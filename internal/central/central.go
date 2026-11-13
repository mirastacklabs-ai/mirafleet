// Package central is the embedded SSH server. Runners dial in and authenticate with their
// Ed25519 key (listed in a flat authorized_keys-format file; the key comment is the runner
// name). Central then opens exec@mirafleet channels back over the runner's own connection.
//
// Package boundaries for later milestones: the key lookup (authorize) becomes the AuthProvider/
// Store, the in-memory registry becomes Store-backed, and audit.Writer becomes an AuditSink.
package central

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs/mirafleet/internal/audit"
	"github.com/mirastacklabs/mirafleet/internal/keys"
	"github.com/mirastacklabs/mirafleet/internal/transport"
)

type Config struct {
	Listen                string // host:port; ":0"-style ports are fine
	HostKey               ssh.Signer
	AuthorizedRunnersFile string // authorized_keys format; comment = runner name. Re-read per login.
	Audit                 *audit.Writer
	Logf                  func(format string, args ...any)

	// HeartbeatEvery is the heartbeat interval runners are expected to use (default 15s).
	// A runner silent for 3 intervals is treated as offline and its connection is closed.
	HeartbeatEvery time.Duration
}

// RunnerInfo is a registry snapshot entry.
type RunnerInfo struct {
	Name      string
	Heartbeat transport.Heartbeat
	LastSeen  time.Time
	Since     time.Time
}

type session struct {
	name  string
	conn  *ssh.ServerConn
	since time.Time

	mu       sync.Mutex
	hb       transport.Heartbeat
	lastSeen time.Time
	ready    bool // set by the first heartbeat; exec is only allowed once ready
}

type Server struct {
	cfg Config
	ssh *ssh.ServerConfig

	mu      sync.Mutex
	ln      net.Listener
	runners map[string]*session
	conns   map[*ssh.ServerConn]struct{}
	wg      sync.WaitGroup
	closed  bool
}

func New(cfg Config) *Server {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.HeartbeatEvery <= 0 {
		cfg.HeartbeatEvery = 15 * time.Second
	}
	s := &Server{cfg: cfg, runners: map[string]*session{}, conns: map[*ssh.ServerConn]struct{}{}}
	sc := &ssh.ServerConfig{PublicKeyCallback: s.authorize}
	sc.AddHostKey(cfg.HostKey)
	s.ssh = sc
	return s
}

// authorize accepts only keys present in the authorized-runners file.
func (s *Server) authorize(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
	data, err := os.ReadFile(s.cfg.AuthorizedRunnersFile)
	if err != nil {
		return nil, fmt.Errorf("authorized runners: %w", err)
	}
	entries, errs := keys.ParseAuthorized(data)
	for _, e := range errs {
		s.cfg.Logf("WARNING authorized runners %s: %v", s.cfg.AuthorizedRunnersFile, e)
	}
	for _, a := range entries {
		if string(a.Key.Marshal()) == string(key.Marshal()) {
			return &ssh.Permissions{Extensions: map[string]string{"runner": a.Name}}, nil
		}
	}
	return nil, errors.New("unknown runner key")
}

// Start binds the listener and serves in the background. It returns the bound address.
func (s *Server) Start() (string, error) {
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			s.wg.Add(1)
			go func() { defer s.wg.Done(); s.handle(nc) }()
		}
	}()
	return ln.Addr().String(), nil
}

// Close stops listening, drops all runner connections and waits for handlers.
func (s *Server) Close() {
	s.mu.Lock()
	s.closed = true
	if s.ln != nil {
		s.ln.Close()
	}
	for c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *Server) handle(nc net.Conn) {
	nc.SetDeadline(time.Now().Add(30 * time.Second)) // handshake deadline
	sconn, chans, reqs, err := ssh.NewServerConn(nc, s.ssh)
	if err != nil {
		nc.Close()
		s.cfg.Logf("handshake from %s failed: %v", nc.RemoteAddr(), err)
		return
	}
	nc.SetDeadline(time.Time{})
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		sconn.Close()
		return
	}
	s.conns[sconn] = struct{}{}
	s.mu.Unlock()
	defer func() {
		sconn.Close()
		s.mu.Lock()
		delete(s.conns, sconn)
		s.mu.Unlock()
	}()

	name := sconn.Permissions.Extensions["runner"]
	sess := &session{name: name, conn: sconn, since: time.Now()}
	// Runners never open channels toward central.
	go func() {
		for nch := range chans {
			nch.Reject(ssh.UnknownChannelType, "channels from runners are not accepted")
		}
	}()
	defer s.deregister(sess)
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go s.watch(sess, stopWatch)
	for r := range reqs {
		if r.Type != transport.ReqHeartbeat {
			r.Reply(false, nil)
			continue
		}
		var hb transport.Heartbeat
		if err := json.Unmarshal(r.Payload, &hb); err != nil {
			r.Reply(false, nil)
			continue
		}
		s.heartbeat(sess, hb)
		r.Reply(true, nil)
	}
}

func (s *Server) staleAfter() time.Duration { return 3 * s.cfg.HeartbeatEvery }

// lastActivity is the last heartbeat, or the connect time if none arrived yet.
func (sess *session) lastActivity() time.Time {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.lastSeen.IsZero() {
		return sess.since
	}
	return sess.lastSeen
}

func (s *Server) stale(sess *session) bool { return time.Since(sess.lastActivity()) > s.staleAfter() }

// watch closes the connection of a runner that stopped heartbeating (dead peer, half-open TCP).
func (s *Server) watch(sess *session, stop <-chan struct{}) {
	t := time.NewTicker(max(s.cfg.HeartbeatEvery/2, time.Millisecond))
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if s.stale(sess) {
				s.cfg.Logf("runner %q silent for >%s; closing connection", sess.name, s.staleAfter())
				sess.conn.Close()
				return
			}
		}
	}
}

func (s *Server) heartbeat(sess *session, hb transport.Heartbeat) {
	sess.mu.Lock()
	first := !sess.ready
	sess.hb, sess.lastSeen, sess.ready = hb, time.Now(), true
	sess.mu.Unlock()
	if !first {
		return
	}
	// Register on first heartbeat: by then the runner has installed its channel handler.
	s.mu.Lock()
	old := s.runners[sess.name]
	s.runners[sess.name] = sess
	s.mu.Unlock()
	if old != nil && old != sess {
		old.conn.Close() // the new connection wins (e.g. runner reconnected after a silent drop)
	}
	s.cfg.Logf("runner %q online (%s/%s)", sess.name, hb.OS, hb.Arch)
}

func (s *Server) deregister(sess *session) {
	s.mu.Lock()
	if s.runners[sess.name] == sess {
		delete(s.runners, sess.name)
		s.cfg.Logf("runner %q offline", sess.name)
	}
	s.mu.Unlock()
}

// Runners returns a snapshot of online runners, sorted by name.
func (s *Server) Runners() []RunnerInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []RunnerInfo
	for _, r := range s.runners {
		if s.stale(r) {
			continue
		}
		r.mu.Lock()
		out = append(out, RunnerInfo{r.name, r.hb, r.lastSeen, r.since})
		r.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Server) sessionFor(name string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.runners[name]; r != nil && !s.stale(r) {
		return r
	}
	return nil
}

// Exec asks a runner to run an action over a channel opened on the runner's own connection.
// A guard refusal comes back as a result with Refused set, not as an error.
func (s *Server) Exec(ctx context.Context, runner string, req transport.ExecRequest) (*transport.ExecResult, error) {
	start := time.Now()
	res, err := s.exec(ctx, runner, req)
	ev := audit.Event{Runner: runner, Action: req.Action, Params: req.Params, DurationMS: time.Since(start).Milliseconds()}
	switch {
	case err != nil:
		ev.Outcome, ev.Message = "error", err.Error()
	case res.Refused:
		ev.Outcome, ev.Message = "refused", res.Message
	case res.TimedOut:
		ev.Outcome, ev.ExitCode = "timeout", res.ExitCode
	case res.ExitCode != 0:
		ev.Outcome, ev.ExitCode = "failed", res.ExitCode
	default:
		ev.Outcome = "ok"
	}
	s.cfg.Audit.Log(ev)
	return res, err
}

func (s *Server) exec(ctx context.Context, runner string, req transport.ExecRequest) (*transport.ExecResult, error) {
	sess := s.sessionFor(runner)
	if sess == nil {
		return nil, fmt.Errorf("runner %q is not online", runner)
	}
	payload, _ := json.Marshal(req)
	// OpenChannel itself cannot be cancelled: run it aside and give up on ctx. If it opens late,
	// the orphan channel is closed.
	type opened struct {
		ch   ssh.Channel
		reqs <-chan *ssh.Request
		err  error
	}
	done := make(chan opened, 1)
	go func() {
		ch, reqs, err := sess.conn.OpenChannel(transport.ChanExec, payload)
		done <- opened{ch, reqs, err}
	}()
	var o opened
	select {
	case o = <-done:
	case <-ctx.Done():
		go func() {
			if late := <-done; late.err == nil {
				late.ch.Close()
				go ssh.DiscardRequests(late.reqs)
			}
		}()
		return nil, ctx.Err()
	}
	if o.err != nil {
		return nil, fmt.Errorf("open exec channel: %w", o.err)
	}
	ch := o.ch
	go ssh.DiscardRequests(o.reqs)
	defer ch.Close()
	stop := context.AfterFunc(ctx, func() { ch.Close() })
	defer stop()
	data, err := io.ReadAll(io.LimitReader(ch, transport.MaxResultBytes+1))
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	if len(data) > transport.MaxResultBytes {
		return nil, fmt.Errorf("result too large: runner sent more than %d bytes", transport.MaxResultBytes)
	}
	var res transport.ExecResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("runner sent no valid result (%d bytes)", len(data))
	}
	return &res, nil
}
