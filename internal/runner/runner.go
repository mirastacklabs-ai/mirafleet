// Package runner dials out to central, keeps one long-lived SSH connection, heartbeats, and
// serves exec@mirafleet channels through the actions guard. It accepts no other channel type
// and never listens on a port.
package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"runtime"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/actions"
	"github.com/mirastacklabs-ai/mirafleet/internal/audit"
	"github.com/mirastacklabs-ai/mirafleet/internal/keys"
	"github.com/mirastacklabs-ai/mirafleet/internal/protover"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/wsconfig"
)

type Config struct {
	Central    string // host:port
	Name       string // informational only: the SSH user is always "runner"; central names runners by store id
	Signer     ssh.Signer
	KnownHosts string // known-hosts-style file pinning central's host key
	Actions    *actions.Set
	// Workspace and WorkspaceConfig enable workspace mode (docs/M3.md 1.12): both or neither, and never
	// together with Actions. Run returns the error from Validate before connecting.
	Workspace       workspace.Executor
	WorkspaceConfig *wsconfig.Config
	Labels          map[string]string
	// CorporateCA (optional PEM) is added to the system roots the credential broker verifies
	// upstream servers against (host mode, docs/M7.md 4.4).
	CorporateCA []byte
	Version     string
	Audit       *audit.Writer
	Logf        func(format string, args ...any)

	HeartbeatEvery time.Duration // default 15s
	// HeartbeatTimeout is how long to wait for central's heartbeat reply before dropping the
	// connection. Default 2*HeartbeatEvery.
	HeartbeatTimeout time.Duration
	BackoffMin       time.Duration // default 1s
	BackoffMax       time.Duration // default 60s; clamped to >= BackoffMin
	HealthyAfter     time.Duration // connection must stay up this long (with an acked heartbeat) to reset backoff; default 30s
	MaxExec          int           // concurrent actions; default 4

	// Proto is the protocol level sent in every heartbeat. Zero means protover.Current; tests set
	// it to fake version skew.
	Proto int
	// After is the clock for the reconnect wait. Nil means time.After; tests inject one.
	After func(time.Duration) <-chan time.Time
}

// refusedError is central's version-skew refusal (refused@mirafleet). Run logs it and waits
// BackoffMax before the next attempt.
type refusedError struct{ msg string }

func (e *refusedError) Error() string { return "central refused this runner: " + e.msg }

// maxRefusal bounds the refusal text the runner keeps and logs.
const maxRefusal = 512

type Runner struct {
	cfg    Config
	sem    chan struct{}
	ws     *wsState // nil unless workspace mode
	dev    *devMgr  // nil unless the executor has per-session directories (host mode, M7)
	hbKick chan struct{}
	cfgErr error
}

// Validate checks that exactly one mode is configured: actions (exec@) xor workspace (workspace@).
// Neither is allowed (the runner then refuses both channels with Prohibited).
func (c *Config) Validate() error {
	if (c.Workspace == nil) != (c.WorkspaceConfig == nil) {
		return errors.New("runner: workspace executor and workspace config must be set together")
	}
	if c.Actions != nil && c.Workspace != nil {
		return errors.New("runner: actions and workspace mode are mutually exclusive")
	}
	return nil
}

func New(cfg Config) *Runner {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.HeartbeatEvery <= 0 {
		cfg.HeartbeatEvery = 15 * time.Second
	}
	if cfg.HeartbeatTimeout <= 0 {
		cfg.HeartbeatTimeout = 2 * cfg.HeartbeatEvery
	}
	if cfg.BackoffMin <= 0 {
		cfg.BackoffMin = time.Second
	}
	if cfg.BackoffMax <= 0 {
		cfg.BackoffMax = 60 * time.Second
	}
	if cfg.BackoffMax < cfg.BackoffMin {
		cfg.BackoffMax = cfg.BackoffMin
	}
	if cfg.HealthyAfter <= 0 {
		cfg.HealthyAfter = 30 * time.Second
	}
	if cfg.MaxExec <= 0 {
		cfg.MaxExec = 4
	}
	if cfg.Proto <= 0 {
		cfg.Proto = protover.Current
	}
	if cfg.After == nil {
		cfg.After = time.After
	}
	r := &Runner{cfg: cfg, sem: make(chan struct{}, cfg.MaxExec), hbKick: make(chan struct{}, 1), cfgErr: cfg.Validate()}
	if r.cfgErr == nil && cfg.Workspace != nil {
		r.ws = newWSState(cfg.Workspace, cfg.WorkspaceConfig)
		if hooks, ok := cfg.Workspace.(SessionHooks); ok && devCapable(cfg.Workspace) {
			r.dev = newDevMgr(r, hooks)
			r.ws.gone = r.dev.close
			r.ws.adopt = r.dev.adopt
		}
		var hb transport.Heartbeat
		r.fillHeartbeat(&hb)
		r.cfgErr = hb.Validate()
	}
	return r
}

// jitter spreads d by +-20% so runners do not reconnect in lockstep.
func jitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
}

// Run connects and reconnects with exponential backoff until ctx is cancelled.
func (r *Runner) Run(ctx context.Context) error {
	if r.cfgErr != nil {
		return r.cfgErr
	}
	sampler := make(chan struct{})
	sctx, stopSampler := context.WithCancel(ctx)
	go func() { defer close(sampler); r.runDiskSampler(sctx) }()
	defer func() { stopSampler(); <-sampler }()
	if r.dev != nil {
		defer r.dev.closeAll() // sockets and proxies go with the runner; secrets were memory only
	}
	backoff := r.cfg.BackoffMin
	for ctx.Err() == nil {
		healthy, err := r.session(ctx)
		if healthy {
			backoff = r.cfg.BackoffMin // only a connection that stayed healthy resets the backoff
		}
		if ctx.Err() != nil {
			break
		}
		var ref *refusedError
		if errors.As(err, &ref) {
			backoff = r.cfg.BackoffMax // version skew does not heal by itself: no fast reconnects
		}
		wait := jitter(backoff)
		r.cfg.Logf("disconnected: %v; retrying in %s", err, wait.Round(time.Millisecond))
		select {
		case <-ctx.Done():
		case <-r.cfg.After(wait):
		}
		backoff = min(backoff*2, r.cfg.BackoffMax)
	}
	return ctx.Err()
}

// session runs one connection. healthy reports that it had an acknowledged heartbeat and stayed
// up for at least HealthyAfter (a handshake alone does not count).
func (r *Runner) session(ctx context.Context) (healthy bool, err error) {
	d := net.Dialer{Timeout: 10 * time.Second}
	nc, err := d.DialContext(ctx, "tcp", r.cfg.Central)
	if err != nil {
		return false, err
	}
	nc.SetDeadline(time.Now().Add(30 * time.Second))
	cc, chans, reqs, err := ssh.NewClientConn(nc, r.cfg.Central, &ssh.ClientConfig{
		User:            transport.UserRunner,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(r.cfg.Signer)},
		HostKeyCallback: keys.PinnedHostKeys(r.cfg.KnownHosts),
		Timeout:         30 * time.Second,
	})
	if err != nil {
		nc.Close()
		return false, err
	}
	nc.SetDeadline(time.Time{})
	// We dispatch channel opens ourselves (below). The client gets a channel nobody sends on, so no
	// HandleChannelOpen registration can race an incoming open: the mux queues opens in chans until
	// the dispatcher reads them.
	noChans := make(chan ssh.NewChannel)
	defer close(noChans)
	// Global requests from central pass through this filter: refused@mirafleet is recorded (and
	// acknowledged) before the failed heartbeat reply can be seen, everything else is forwarded.
	var refusal atomic.Pointer[string]
	fwd := make(chan *ssh.Request)
	go func() {
		defer close(fwd)
		for rq := range reqs {
			if rq.Type != transport.ReqRefused {
				fwd <- rq
				continue
			}
			m := string(rq.Payload[:min(len(rq.Payload), maxRefusal)])
			refusal.Store(&m)
			if rq.WantReply {
				rq.Reply(true, nil)
			}
		}
	}()
	client := ssh.NewClient(cc, noChans, fwd)
	defer client.Close()
	sctx, cancel := context.WithCancel(ctx) // cancelled when this connection ends
	defer cancel()
	connectedAt := time.Now()
	acked := false
	defer func() { healthy = acked && time.Since(connectedAt) >= r.cfg.HealthyAfter }()

	// exec@mirafleet and workspace@mirafleet are served (each Prohibited unless configured);
	// every other channel type is rejected.
	go func() {
		for nch := range chans {
			switch nch.ChannelType() {
			case transport.ChanExec:
				go r.serveExec(sctx, nch)
			case transport.ChanWorkspace:
				go r.serveWorkspace(sctx, nch)
			default:
				nch.Reject(ssh.UnknownChannelType, "unknown channel type")
			}
		}
	}()

	dead := make(chan error, 1)
	go func() { dead <- client.Wait() }()
	hostname, _ := os.Hostname()
	base := transport.Heartbeat{Hostname: hostname, OS: runtime.GOOS, Arch: runtime.GOARCH, Version: r.cfg.Version, Labels: r.cfg.Labels, Proto: r.cfg.Proto}
	r.fillActions(&base)
	if r.dev != nil {
		go r.runEvents(sctx, client)
	}
	tick := time.NewTicker(r.cfg.HeartbeatEvery)
	defer tick.Stop()
	for {
		// Rebuilt every time: credentials (names and status only) and busy jobs change.
		hb := base
		r.fillHeartbeat(&hb)
		payload, _ := json.Marshal(hb)
		if err := r.heartbeat(ctx, client, payload); err != nil {
			if m := refusal.Load(); m != nil {
				return false, &refusedError{msg: *m}
			}
			return false, err
		}
		acked = true
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case err := <-dead:
			return false, fmt.Errorf("connection lost: %v", err)
		case <-tick.C:
			if r.dev != nil {
				r.dev.tick()
			}
		case <-r.hbKick:
		}
	}
}

// fillActions adds action discovery (actions mode only), dropping anything that would fail
// Heartbeat.Validate: discovery is informational and must never stop a runner from connecting.
func (r *Runner) fillActions(hb *transport.Heartbeat) {
	if r.cfg.Actions == nil {
		return
	}
	all := r.cfg.Actions.Describe()
	size := 2 // "[]"
	added := 0
	for _, a := range all {
		if len(hb.Actions) >= transport.MaxHeartbeatActions {
			break
		}
		ai := transport.ActionInfo{Name: a.Name}
		for _, p := range a.Params {
			ai.Params = append(ai.Params, transport.ActionParam{Name: p.Name, Pattern: p.Pattern, Default: p.Default})
		}
		if transport.ValidateAction(ai) != nil {
			continue
		}
		b, _ := json.Marshal(ai)
		next := size + len(b)
		if added > 0 {
			next++ // comma
		}
		if next > transport.MaxActionsBytes {
			break
		}
		size = next
		added++
		hb.Actions = append(hb.Actions, ai)
	}
	if left := len(all) - added; left > 0 {
		logf := r.cfg.Logf
		if logf == nil {
			logf = func(string, ...any) {}
		}
		logf("action discovery: %d of %d actions left out (invalid, or over the %d-byte / %d-action budget)",
			left, len(all), transport.MaxActionsBytes, transport.MaxHeartbeatActions)
	}
}

// heartbeat sends one heartbeat and waits for the reply for at most HeartbeatTimeout. On timeout
// or ctx cancel it closes the client so the reconnect loop takes over.
func (r *Runner) heartbeat(ctx context.Context, client *ssh.Client, payload []byte) error {
	type reply struct {
		ok  bool
		err error
	}
	c := make(chan reply, 1)
	go func() {
		ok, _, err := client.SendRequest(transport.ReqHeartbeat, true, payload)
		c <- reply{ok, err}
	}()
	t := time.NewTimer(r.cfg.HeartbeatTimeout)
	defer t.Stop()
	select {
	case x := <-c:
		if x.err != nil || !x.ok {
			return fmt.Errorf("heartbeat failed: %v", x.err)
		}
		return nil
	case <-t.C:
		client.Close()
		return errors.New("heartbeat reply timed out")
	case <-ctx.Done():
		client.Close()
		return ctx.Err()
	}
}

// encodeResult marshals without HTML escaping (which would inflate <, >, & six-fold). If the
// encoding still exceeds what central will read, it is replaced by an explicit error result.
func encodeResult(res *transport.ExecResult) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(res); err != nil || buf.Len() > transport.MaxResultBytes {
		buf.Reset()
		enc.Encode(&transport.ExecResult{ExitCode: -1, Message: "result too large to send", DurationMS: res.DurationMS})
	}
	return buf.Bytes()
}

func (r *Runner) serveExec(ctx context.Context, nch ssh.NewChannel) {
	if r.cfg.Actions == nil { // workspace-mode (or unconfigured) runners never run exec@
		nch.Reject(ssh.Prohibited, "this runner does not run actions")
		return
	}
	var req transport.ExecRequest
	if err := json.Unmarshal(nch.ExtraData(), &req); err != nil {
		nch.Reject(ssh.ConnectionFailed, "malformed exec request")
		return
	}
	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	default:
		msg := fmt.Sprintf("runner busy: %d concurrent actions already running (max %d)", cap(r.sem), cap(r.sem))
		r.cfg.Audit.Log(audit.Event{Action: req.Action, Params: req.Params, Outcome: "refused", ExitCode: -1, Message: msg})
		nch.Reject(ssh.ResourceShortage, msg)
		return
	}
	ch, reqs, err := nch.Accept()
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	defer ch.Close()
	// The action lives only as long as its channel and connection: central closing the channel
	// (e.g. its caller gave up) or the connection dropping cancels and kills it.
	ectx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { io.Copy(io.Discard, ch); cancel() }()

	r.cfg.Audit.Log(audit.Event{Action: req.Action, Params: req.Params, Outcome: "start"})
	res, ev := r.execute(ectx, req)
	b := encodeResult(res)
	r.cfg.Audit.Log(ev) // record the outcome before telling central
	ch.Write(b)
	ch.CloseWrite()
}

func (r *Runner) execute(ctx context.Context, req transport.ExecRequest) (*transport.ExecResult, audit.Event) {
	ev := audit.Event{Action: req.Action, Params: req.Params}
	res, err := r.cfg.Actions.Run(ctx, req.Action, req.Params)
	if err != nil {
		ev.Outcome, ev.Message = "refused", err.Error()
		return &transport.ExecResult{Refused: true, Message: err.Error(), ExitCode: -1}, ev
	}
	ev.ExitCode, ev.DurationMS = res.ExitCode, res.Duration.Milliseconds()
	switch {
	case res.TimedOut:
		ev.Outcome = "timeout"
	case res.ExitCode != 0:
		ev.Outcome = "failed"
	default:
		ev.Outcome = "ok"
	}
	out := &transport.ExecResult{Stdout: res.Stdout, Stderr: res.Stderr, StdoutTruncated: res.StdoutTruncated, StderrTruncated: res.StderrTruncated, ExitCode: res.ExitCode, TimedOut: res.TimedOut, DurationMS: res.Duration.Milliseconds()}
	if res.TimedOut {
		out.Message = "timed out; process killed"
	}
	return out, ev
}
