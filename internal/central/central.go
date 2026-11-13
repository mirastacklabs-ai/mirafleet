// Package central is the embedded SSH server. Runners dial in and authenticate with their
// Ed25519 key (listed in a flat authorized_keys-format file; the key comment is the runner
// name). Central then opens exec@mirafleet channels back over the runner's own connection.
//
// Live sessions live in internal/central/registry, keyed by runner id and also indexed by name.
// M1 shim (until the store-backed auth of M2 S7a): the runner id is the key's line number in the
// authorized-runners file; it travels in the verified ssh.Permissions and is read only after the
// handshake has authenticated the peer. Later steps replace the file with the store and the
// audit.Writer with an audit.Sink.
package central

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/audit"
	"github.com/mirastacklabs-ai/mirafleet/internal/central/registry"
	"github.com/mirastacklabs-ai/mirafleet/internal/keys"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
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

type Server struct {
	cfg Config
	ssh *ssh.ServerConfig

	mu     sync.Mutex
	ln     net.Listener
	reg    *registry.Registry
	conns  map[*ssh.ServerConn]struct{}
	wg     sync.WaitGroup
	closed bool
}

func New(cfg Config) *Server {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.HeartbeatEvery <= 0 {
		cfg.HeartbeatEvery = 15 * time.Second
	}
	s := &Server{cfg: cfg, conns: map[*ssh.ServerConn]struct{}{}}
	reg, err := registry.New(registry.Options{StaleAfter: 3 * cfg.HeartbeatEvery, Logf: cfg.Logf})
	if err != nil {
		panic(err) // unreachable: HeartbeatEvery is defaulted above, so StaleAfter > 0
	}
	s.reg = reg
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
			// M1 shim: the id is the key's line number in the flat file (stable until edited).
			// Only returned here; this callback runs before the signature is verified, so it
			// must not write any server state.
			return &ssh.Permissions{Extensions: map[string]string{"runner": a.Name, "runner_id": strconv.Itoa(a.Line)}}, nil
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

	name, id, ok := runnerIdentity(sconn.Permissions)
	if !ok {
		s.cfg.Logf("handshake from %s: missing or invalid runner identity; closing", nc.RemoteAddr())
		return // the deferred Close drops the connection
	}
	// Runners never open channels toward central.
	go func() {
		for nch := range chans {
			nch.Reject(ssh.UnknownChannelType, "channels from runners are not accepted")
		}
	}()
	// A connection that never heartbeats is never registered; close it after the stale window.
	var sess *registry.Session
	var registered atomic.Bool
	guard := time.AfterFunc(s.staleAfter(), func() {
		if !registered.Load() {
			s.cfg.Logf("runner %q silent for >%s; closing connection", name, s.staleAfter())
			sconn.Close()
		}
	})
	defer guard.Stop()
	defer func() {
		if sess != nil {
			s.reg.Unregister(sess)
		}
	}()
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
		if sess == nil {
			// Register on first heartbeat: by then the runner has installed its channel handler.
			// The registry refuses tombstoned ids. TODO(S7a): status/fleet come from the store.
			var err error
			sess, err = s.reg.Register(id, name, "", sconn, registry.StatusActive, hb)
			if err != nil {
				s.cfg.Logf("runner %q refused: %v", name, err)
				r.Reply(false, nil)
				sconn.Close()
				continue
			}
			registered.Store(true)
			s.cfg.Logf("runner %q online (%s/%s)", name, hb.OS, hb.Arch)
		} else {
			sess.Heartbeat(hb)
		}
		r.Reply(true, nil)
	}
}

// runnerIdentity reads the runner name and id from verified permissions (strict: canonical
// decimal, id > 0, name non-empty). The M1 shim uses its own extension keys, not auth.Decode.
func runnerIdentity(p *ssh.Permissions) (name string, id int64, ok bool) {
	if p == nil || p.Extensions == nil {
		return "", 0, false
	}
	name = p.Extensions["runner"]
	raw := p.Extensions["runner_id"]
	id, err := strconv.ParseInt(raw, 10, 64)
	if name == "" || err != nil || id <= 0 || strconv.FormatInt(id, 10) != raw {
		return "", 0, false
	}
	return name, id, true
}

func (s *Server) staleAfter() time.Duration { return 3 * s.cfg.HeartbeatEvery }

// Runners returns a snapshot of online runners, sorted by name.
func (s *Server) Runners() []RunnerInfo {
	var out []RunnerInfo
	for _, si := range s.reg.Snapshot() {
		if si.Online {
			out = append(out, RunnerInfo{si.Name, si.Info, si.LastHeartbeat, si.Since})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Server) sessionFor(name string) *registry.Session {
	return s.reg.GetByName(name)
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
	return s.reg.Exec(ctx, sess.ID, req)
}
