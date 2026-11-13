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
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/actions"
	"github.com/mirastacklabs-ai/mirafleet/internal/audit"
	"github.com/mirastacklabs-ai/mirafleet/internal/keys"
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
	Version         string
	Audit           *audit.Writer
	Logf            func(format string, args ...any)

	HeartbeatEvery time.Duration // default 15s
	BackoffMin     time.Duration // default 1s
	BackoffMax     time.Duration // default 60s; clamped to >= BackoffMin
	HealthyAfter   time.Duration // connection must stay up this long (with an acked heartbeat) to reset backoff; default 30s
	MaxExec        int           // concurrent actions; default 4
}

type Runner struct {
	cfg    Config
	sem    chan struct{}
	ws     *wsState // nil unless workspace mode
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
	r := &Runner{cfg: cfg, sem: make(chan struct{}, cfg.MaxExec), cfgErr: cfg.Validate()}
	if r.cfgErr == nil && cfg.Workspace != nil {
		r.ws = newWSState(cfg.Workspace, cfg.WorkspaceConfig)
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
	backoff := r.cfg.BackoffMin
	for ctx.Err() == nil {
		healthy, err := r.session(ctx)
		if healthy {
			backoff = r.cfg.BackoffMin // only a connection that stayed healthy resets the backoff
		}
		if ctx.Err() != nil {
			break
		}
		wait := jitter(backoff)
		r.cfg.Logf("disconnected: %v; retrying in %s", err, wait.Round(time.Millisecond))
		select {
		case <-ctx.Done():
		case <-time.After(wait):
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
	client := ssh.NewClient(cc, noChans, reqs)
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
	hb := transport.Heartbeat{Hostname: hostname, OS: runtime.GOOS, Arch: runtime.GOARCH, Version: r.cfg.Version, Labels: r.cfg.Labels}
	r.fillHeartbeat(&hb)
	payload, _ := json.Marshal(hb)
	tick := time.NewTicker(r.cfg.HeartbeatEvery)
	defer tick.Stop()
	for {
		if err := r.heartbeat(ctx, client, payload); err != nil {
			return false, err
		}
		acked = true
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case err := <-dead:
			return false, fmt.Errorf("connection lost: %v", err)
		case <-tick.C:
		}
	}
}

// heartbeat sends one heartbeat and waits for the reply for at most 2x the interval. On timeout
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
	t := time.NewTimer(2 * r.cfg.HeartbeatEvery)
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
