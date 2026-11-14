// Package orch is the orchestrator core: it resolves which context, fleet and session a tool call
// belongs to, keeps one SSH connection per context, and makes typed operator calls to central.
// It has no MCP code (internal/mcpserver sits on top) and runs on linux, darwin and windows, so
// it never links the server side (service, store, central, runner, containerd, SQLite).
//
// Shape (docs/M5.md §1.2–§1.11):
//
//   - New(Config) builds an Orch. It always succeeds, even with no contexts or key. Until the
//     operator has run `mirafleet mcp login …`, every call returns ErrNotSetUp (no paths).
//   - Resolve picks the context and fleet: tool argument > flags (Config) > .mirafleet.yaml >
//     context default_fleet > none. The result names where each value came from. A guarded fleet
//     that came from the project file or a context default is refused: it must be named
//     explicitly. Guarded status comes from fleets.list, cached per context for GuardedTTL.
//   - SessionFor is re-read at every call: Config.Session (flag), else the handoff file for
//     (PPID, ProjectDir), else a per-process random "p:<32 hex>".
//   - The typed wrappers (FleetsList, RunnersList, ActionRun, WorkspaceOpen, WorkspaceExec,
//     WorkspaceReadFile, WorkspaceWriteFile, WorkspaceClose, WorkspaceList) all go through the
//     private call, the only path to apiclient. It refuses any method for which
//     api.IsOperatorMethod is false (and panics when panicOnGuard is set, which tests do).
//   - Connections: one per context, dialled lazily with mandatory known_hosts pinning (never
//     trust on first use, never a prompt), kept alive every KeepaliveInterval, redialled once on
//     a dead connection. Only idempotent methods are retried (fleets.list, runners.list,
//     workspace.list, workspace.read_file, workspace.open); exec, write_file and action.run never
//     are. At most api.MaxChannelsPerConn calls run at once per context. Close cancels in-flight
//     calls and closes the connections.
//   - Errors are *Error values carrying an api error code. Their text never contains local paths,
//     key fingerprints or internal error details.
//   - Environment is never read or forwarded: orch calls neither os.Environ nor os.Getenv. The
//     only environment-derived input is the default config/state directory from opconf.
//   - CapOutput and Sanitize (output.go) bound and clean command output before it reaches the
//     model; CallLimit (limit.go) is the client-side time limit of a call.
package orch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/opconf"
)

// ErrNotSetUp is returned when there is no context, key or pinned host key yet. It is an *Error
// (code unavailable) whose text names the fix and no paths; errors.Is(err, ErrNotSetUp) works.
var ErrNotSetUp error = &Error{Code: api.CodeUnavailable, Message: "not set up: run `mirafleet mcp login …`"}

// ErrGuardedDefault is the cause of the error for a guarded fleet that came from a default. It
// is an *Error with code bad_request.
var ErrGuardedDefault error = &Error{Code: api.CodeBadRequest, Message: "guarded fleet used as a default"}

// Error is the error type of every orch call. Code is an api error code (api.CodeBadRequest, …).
type Error struct {
	Code    string
	Message string
	cause   error
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Unwrap returns the sentinel cause (ErrNotSetUp, ErrGuardedDefault) if any.
func (e *Error) Unwrap() error { return e.cause }

// PublicError returns the api error code and the message that are safe to show the model.
func (e *Error) PublicError() (code, message string) { return e.Code, e.Message }

func errf(code string, cause error, msg string) *Error {
	return &Error{Code: code, Message: msg, cause: cause}
}

// Defaults.
const (
	DefaultKeepalive = 30 * time.Second
	GuardedTTL       = 30 * time.Second
)

// Conn is one authenticated connection to central. apiclient.Client satisfies it through a thin
// adapter; tests inject fakes.
type Conn interface {
	Call(ctx context.Context, method string, params, result any) error
	CallBody(ctx context.Context, method string, params any, body []byte, result any) error
	CallReadBody(ctx context.Context, method string, params, result any) ([]byte, error)
	// Ping proves the connection is alive (the keepalive).
	Ping(ctx context.Context) error
	Close() error
}

// DialFunc connects to a central at addr as the operator.
type DialFunc func(ctx context.Context, addr string, signer ssh.Signer, hostKey ssh.HostKeyCallback) (Conn, error)

// Config configures an Orch. Zero values are usable: empty ConfigDir/StateDir use opconf's
// defaults, empty ProjectDir the working directory.
type Config struct {
	ConfigDir  string // keys, known_hosts, contexts.yaml
	StateDir   string // handoff files
	ProjectDir string // where .mirafleet.yaml is looked up and the handoff is keyed
	Home       string // upper bound of the project-file search; empty: only ProjectDir itself
	Context    string // -context flag
	Fleet      string // -fleet flag
	Session    string // -session flag
	PPID       int    // parent (harness) PID, the handoff key

	// ConfigErr / StateErr: the caller already tried to resolve the directory and failed. When set,
	// orch never falls back to opconf for that directory and treats it as unavailable.
	ConfigErr error
	StateErr  error

	Now  func() time.Time
	Logf func(format string, args ...any) // diagnostics for the operator (stderr); never shown to the model

	// Test hooks. Dial replaces the pinned dial; it stays exported because tests in other
	// packages (internal/central's M5 tests) drive a real orch over a fake central, so it cannot
	// be unexported without changing those tests (docs/M5.md F-33).
	Dial              DialFunc
	KeepaliveInterval time.Duration
}

// Orch is the orchestrator core. Safe for concurrent use.
type Orch struct {
	cfg         Config
	cfgErr      error // directory defaults failed: reported as ErrNotSetUp
	procSession string

	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	closed  bool
	pools   map[string]*pool
	guarded map[string]guardEntry // key: context name + address (see guardKey)

	guardFlights map[string]*guardFlight // in-flight fleets.list refreshes, one per key
	wg           sync.WaitGroup
}

// New builds an Orch. It reads nothing from disk, so it succeeds with no contexts or keys; the
// only error is an invalid Config.Session or a failing random source.
func New(cfg Config) (*Orch, error) {
	if cfg.Session != "" {
		if e := api.ValidateSession(cfg.Session); e != nil {
			return nil, errors.New("invalid -session value")
		}
	}
	o := &Orch{cfg: cfg, pools: map[string]*pool{}, guarded: map[string]guardEntry{}, guardFlights: map[string]*guardFlight{}}
	if o.cfg.Now == nil {
		o.cfg.Now = time.Now
	}
	if o.cfg.Logf == nil {
		o.cfg.Logf = func(string, ...any) {}
	}
	if o.cfg.KeepaliveInterval <= 0 {
		o.cfg.KeepaliveInterval = DefaultKeepalive
	}
	if o.cfg.Dial == nil {
		o.cfg.Dial = defaultDial
	}
	switch {
	case o.cfg.ConfigErr != nil:
		o.cfg.ConfigDir = ""
		o.cfgErr = o.cfg.ConfigErr
	case o.cfg.ConfigDir == "":
		d, err := opconf.Dir()
		if err != nil {
			o.cfgErr = err
		} else {
			o.cfg.ConfigDir = d
		}
	}
	if o.cfg.StateErr != nil {
		o.cfg.StateDir = "" // no handoff; the per-process session still works
	} else if o.cfg.StateDir == "" {
		if d, err := opconf.StateDir(); err == nil {
			o.cfg.StateDir = d
		} // else: no handoff; the per-process session still works
	}
	if o.cfg.ProjectDir == "" {
		if wd, err := os.Getwd(); err == nil {
			o.cfg.ProjectDir = wd
		}
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, errors.New("no random source")
	}
	o.procSession = "p:" + hex.EncodeToString(b[:])
	o.ctx, o.cancel = context.WithCancel(context.Background())
	return o, nil
}

// Close cancels in-flight calls, closes every connection and waits for the background
// goroutines. Calls after Close fail. It never closes workspaces.
func (o *Orch) Close() error {
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return nil
	}
	o.closed = true
	pools := o.pools
	o.pools = map[string]*pool{}
	o.mu.Unlock()
	o.cancel()
	for _, p := range pools {
		p.closeAll()
	}
	o.wg.Wait()
	return nil
}

// goStart registers a background goroutine unless Close has begun.
func (o *Orch) goStart() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return false
	}
	o.wg.Add(1)
	return true
}

func (o *Orch) isClosed() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.closed
}

var errClosed = errf(api.CodeUnavailable, nil, "the orchestrator is shutting down")

// ContextNames lists the configured contexts (ErrNotSetUp when there are none).
func (o *Orch) ContextNames() ([]string, error) {
	cs, err := o.loadContexts()
	if err != nil {
		return nil, err
	}
	return cs.Names(), nil
}

// OutputCap is the effective output cap in bytes (the default when contexts.yaml is unusable).
func (o *Orch) OutputCap() int {
	cs, err := o.loadContexts()
	if err != nil {
		return opconf.DefaultOutputCap
	}
	return cs.OutputCap()
}

// dirUnavailable is true when no config directory can be used. An empty directory is never
// joined into a path (that would resolve relative to the working directory).
func (o *Orch) dirUnavailable() bool { return o.cfgErr != nil || o.cfg.ConfigDir == "" }

func (o *Orch) loadContexts() (*opconf.Contexts, error) {
	if o.dirUnavailable() {
		return nil, ErrNotSetUp
	}
	cs, err := opconf.LoadContexts(o.cfg.ConfigDir)
	if err != nil {
		o.cfg.Logf("contexts: %v", err)
		return nil, errf(api.CodeBadRequest, nil, "the contexts file is invalid: check it with `mirafleet mcp context list` or run `mirafleet mcp login …`")
	}
	if len(cs.Contexts) == 0 {
		return nil, ErrNotSetUp
	}
	return cs, nil
}
