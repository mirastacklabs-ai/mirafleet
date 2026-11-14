package orch

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/keys"
	"github.com/mirastacklabs-ai/mirafleet/internal/opconf"
)

// setup writes a config dir with a generated key, a known_hosts with one placeholder entry (the
// fake dialer never checks it, but an empty file would be refused before dialling) and contexts.yaml.
func setup(t *testing.T, ctxs ...opconf.Context) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "cfg")
	if _, err := keys.Generate(dir, opconf.KeyName, "test"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(opconf.KnownHostsPath(dir), []byte("* ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPlaceholderPlaceholderPlaceholder00\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := opconf.SaveContexts(dir, &opconf.Contexts{Contexts: ctxs}); err != nil {
		t.Fatal(err)
	}
	return dir
}

// fakeConn is a scriptable Conn.
type fakeConn struct {
	mu      sync.Mutex
	calls   []string
	params  []string // JSON of each call's params
	handler func(ctx context.Context, method string, result any) error
	pings   atomic.Int32
	pingErr atomic.Value // error
	closed  atomic.Bool
	body    []byte
}

func (f *fakeConn) record(method string, params any) {
	b, _ := json.Marshal(params)
	f.mu.Lock()
	f.calls = append(f.calls, method)
	f.params = append(f.params, string(b))
	f.mu.Unlock()
}

func (f *fakeConn) run(ctx context.Context, method string, result any) error {
	if f.handler != nil {
		return f.handler(ctx, method, result)
	}
	return nil
}

func (f *fakeConn) Call(ctx context.Context, method string, params, result any) error {
	f.record(method, params)
	return f.run(ctx, method, result)
}

func (f *fakeConn) CallBody(ctx context.Context, method string, params any, body []byte, result any) error {
	f.record(method, params)
	return f.run(ctx, method, result)
}

func (f *fakeConn) CallReadBody(ctx context.Context, method string, params, result any) ([]byte, error) {
	f.record(method, params)
	return f.body, f.run(ctx, method, result)
}

func (f *fakeConn) Ping(context.Context) error {
	f.pings.Add(1)
	if e, _ := f.pingErr.Load().(error); e != nil {
		return e
	}
	return nil
}

func (f *fakeConn) Close() error { f.closed.Store(true); return nil }

func (f *fakeConn) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// dialer hands out conns from make and counts dials.
type dialer struct {
	n     atomic.Int32
	mk    func(n int) (Conn, error)
	mu    sync.Mutex
	conns []Conn
}

func (d *dialer) dial(_ context.Context, _ string, _ ssh.Signer, hk ssh.HostKeyCallback) (Conn, error) {
	if hk == nil {
		panic("host key callback must be set")
	}
	n := int(d.n.Add(1))
	c, err := d.mk(n)
	if err == nil {
		d.mu.Lock()
		d.conns = append(d.conns, c)
		d.mu.Unlock()
	}
	return c, err
}

func fleetsHandler(fleets ...api.FleetInfo) func(context.Context, string, any) error {
	return func(_ context.Context, method string, result any) error {
		if r, ok := result.(*api.FleetsListResult); ok {
			r.Fleets = fleets
		}
		return nil
	}
}

func newOrch(t *testing.T, dir string, d *dialer, mut ...func(*Config)) *Orch {
	t.Helper()
	cfg := Config{
		ConfigDir:  dir,
		StateDir:   t.TempDir(),
		ProjectDir: t.TempDir(),
		PPID:       4242,
		Dial:       d.dial,
		Logf:       t.Logf,
	}
	for _, m := range mut {
		m(&cfg)
	}
	o, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { o.Close() })
	return o
}

func writeProject(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, opconf.ProjectName), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
