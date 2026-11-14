package orch

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/opconf"
)

func oneCtx(t *testing.T) string {
	return setup(t, opconf.Context{Name: "a", Addr: "127.0.0.1:1"})
}

// apiMethods parses internal/api/api.go and returns every Method* string constant.
func apiMethods(t *testing.T) []string {
	t.Helper()
	fs := token.NewFileSet()
	f, err := parser.ParseFile(fs, filepath.Join("..", "api", "api.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, name := range vs.Names {
			if !strings.HasPrefix(name.Name, "Method") || i >= len(vs.Values) {
				continue
			}
			if bl, ok := vs.Values[i].(*ast.BasicLit); ok && bl.Kind == token.STRING {
				s, _ := strconv.Unquote(bl.Value)
				out = append(out, s)
			}
		}
		return true
	})
	if len(out) < 20 {
		t.Fatalf("found only %d methods in api.go", len(out))
	}
	return out
}

func TestGuardRefusesNonOperatorMethods(t *testing.T) {
	d := &dialer{mk: func(int) (Conn, error) { return &fakeConn{}, nil }}
	o := newOrch(t, oneCtx(t), d)
	refused := 0
	for _, m := range apiMethods(t) {
		if api.IsOperatorMethod(m) {
			continue
		}
		refused++
		err := o.call(context.Background(), "a", m, nil, nil)
		var e *Error
		if !errors.As(err, &e) || e.Code != api.CodeForbidden {
			t.Errorf("%s: err = %v, want forbidden", m, err)
		}
	}
	for _, m := range []string{"", "bogus", "hello ", "FLEETS.LIST"} {
		if err := o.call(context.Background(), "a", m, nil, nil); err == nil {
			t.Errorf("%q was not refused", m)
		}
	}
	if refused < 12 { // 13 admin + 2 enrol today
		t.Fatalf("only %d non-operator methods checked", refused)
	}
	if d.n.Load() != 0 {
		t.Fatal("a refused method must never dial")
	}

	// In tests the guard panics so a violation cannot hide.
	panicOnGuard = true
	defer func() { panicOnGuard = false }()
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic")
		}
	}()
	_ = o.call(context.Background(), "a", api.MethodFleetCreate, nil, nil)
}

func TestNoRetryForNonIdempotent(t *testing.T) {
	type op struct {
		name string
		run  func(o *Orch) error
	}
	ops := []op{
		{"exec", func(o *Orch) error {
			_, err := o.WorkspaceExec(context.Background(), "a", api.WorkspaceExecParams{Workspace: "w", Argv: []string{"x"}}, nil)
			return err
		}},
		{"write_file", func(o *Orch) error {
			_, err := o.WorkspaceWriteFile(context.Background(), "a", api.WorkspaceWriteFileParams{Workspace: "w", Path: "p"}, nil)
			return err
		}},
		{"action.run", func(o *Orch) error {
			_, err := o.ActionRun(context.Background(), "a", api.ActionRunParams{Fleet: "f", Runner: "r", Action: "x"})
			return err
		}},
		{"close", func(o *Orch) error { _, err := o.WorkspaceClose(context.Background(), "a", "w"); return err }},
	}
	for _, tc := range ops {
		t.Run(tc.name, func(t *testing.T) {
			var cs []*fakeConn
			d := &dialer{mk: func(int) (Conn, error) {
				c := &fakeConn{handler: func(context.Context, string, any) error { return io.EOF }}
				cs = append(cs, c)
				return c, nil
			}}
			o := newOrch(t, oneCtx(t), d)
			err := tc.run(o)
			var e *Error
			if !errors.As(err, &e) || e.Code != api.CodeUnavailable {
				t.Fatalf("err = %v", err)
			}
			if len(cs) != 1 || len(cs[0].methods()) != 1 {
				t.Fatalf("dials=%d calls=%v: a non-idempotent call must run at most once", len(cs), cs[0].methods())
			}
			if !cs[0].closed.Load() {
				t.Fatal("dead connection must be closed")
			}
		})
	}
}

func TestRetryIdempotentRedials(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	d := &dialer{mk: func(n int) (Conn, error) {
		return &fakeConn{handler: func(_ context.Context, m string, res any) error {
			if n == 1 {
				return io.ErrUnexpectedEOF // first connection is dead
			}
			if r, ok := res.(*api.FleetsListResult); ok {
				r.Fleets = []api.FleetInfo{{Name: "dev"}}
			}
			return nil
		}}, nil
	}}
	o := newOrch(t, oneCtx(t), d)
	r, err := o.FleetsList(context.Background(), "a")
	if err != nil || len(r.Fleets) != 1 {
		t.Fatalf("got %+v %v", r, err)
	}
	if d.n.Load() != 2 {
		t.Fatalf("dials = %d, want 2 (redial once)", d.n.Load())
	}
	if !d.conns[0].(*fakeConn).closed.Load() {
		t.Fatal("the dead connection must be closed")
	}

	// Every idempotent method is retried; a second failure is returned (only one retry).
	for _, run := range []func(*Orch) error{
		func(o *Orch) error { _, err := o.RunnersList(context.Background(), "a", "f"); return err },
		func(o *Orch) error { _, err := o.WorkspaceList(context.Background(), "a", ""); return err },
		func(o *Orch) error {
			_, _, err := o.WorkspaceReadFile(context.Background(), "a", api.WorkspaceReadFileParams{Workspace: "w", Path: "p"})
			return err
		},
		func(o *Orch) error {
			_, err := o.WorkspaceOpen(context.Background(), "a", api.WorkspaceOpenParams{Fleet: "f", Session: "s"})
			return err
		},
	} {
		d2 := &dialer{mk: func(int) (Conn, error) {
			return &fakeConn{handler: func(context.Context, string, any) error { return io.EOF }}, nil
		}}
		o2 := newOrch(t, oneCtx(t), d2)
		if err := run(o2); err == nil {
			t.Fatal("expected an error")
		}
		if d2.n.Load() != 2 {
			t.Fatalf("dials = %d, want exactly 2", d2.n.Load())
		}
	}
}

func TestAPIErrorPassesThroughAndKeepsConn(t *testing.T) {
	d := &dialer{mk: func(int) (Conn, error) {
		return &fakeConn{handler: func(context.Context, string, any) error {
			return api.Errorf(api.CodeForbidden, "fleet %q is guarded\x1b[31m", "prod")
		}}, nil
	}}
	o := newOrch(t, oneCtx(t), d)
	for i := 0; i < 2; i++ {
		_, err := o.ActionRun(context.Background(), "a", api.ActionRunParams{})
		var e *Error
		if !errors.As(err, &e) || e.Code != api.CodeForbidden || strings.Contains(e.Message, "\x1b") {
			t.Fatalf("err = %#v", err)
		}
	}
	if d.n.Load() != 1 {
		t.Fatalf("dials = %d: a wire error must not drop the connection", d.n.Load())
	}
}

func TestErrorTextHasNoInternals(t *testing.T) {
	cfg := oneCtx(t)
	d := &dialer{mk: func(int) (Conn, error) {
		return nil, errors.New("dial tcp /Users/someone/.config/mirafleet/operator_ed25519: SHA256:abcdef internal")
	}}
	o := newOrch(t, cfg, d)
	_, err := o.FleetsList(context.Background(), "a")
	if err == nil {
		t.Fatal("expected error")
	}
	for _, bad := range []string{"/Users", "SHA256", "operator_ed25519", cfg} {
		if strings.Contains(err.Error(), bad) {
			t.Fatalf("error leaks %q: %v", bad, err)
		}
	}
}

func TestNotSetUp(t *testing.T) {
	root := t.TempDir()
	want := "not set up: run `mirafleet mcp login …`"
	check := func(name string, err error, paths ...string) {
		t.Helper()
		var oe *Error
		if !errors.Is(err, ErrNotSetUp) || !errors.As(err, &oe) || oe.Code != api.CodeUnavailable {
			t.Fatalf("%s: err = %v", name, err)
		}
		if code, msg := oe.PublicError(); code != api.CodeUnavailable || msg != want {
			t.Fatalf("%s: err = %v", name, err)
		}
		for _, p := range paths {
			if strings.Contains(err.Error(), p) {
				t.Fatalf("%s: leaks path %q", name, p)
			}
		}
	}
	// No config dir at all.
	o, err := New(Config{ConfigDir: filepath.Join(root, "nothing"), StateDir: root, ProjectDir: root})
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	_, err = o.FleetsList(context.Background(), "a")
	check("fleets", err, root)
	_, err = o.Resolve(context.Background(), "", "")
	check("resolve", err, root)
	_, err = o.WorkspaceExec(context.Background(), "a", api.WorkspaceExecParams{}, nil)
	check("exec", err, root)
	if _, err := o.ContextNames(); !errors.Is(err, ErrNotSetUp) {
		t.Fatal(err)
	}
	if o.OutputCap() != opconf.DefaultOutputCap {
		t.Fatal("default cap expected")
	}

	// A context but no key / no pinned host key: still not set up, never trust on first use.
	dir := filepath.Join(root, "cfg")
	if err := opconf.SaveContexts(dir, &opconf.Contexts{Contexts: []opconf.Context{{Name: "a", Addr: "127.0.0.1:1"}}}); err != nil {
		t.Fatal(err)
	}
	dialled := false
	o2, _ := New(Config{ConfigDir: dir, StateDir: root, ProjectDir: root,
		Dial: func(context.Context, string, ssh.Signer, ssh.HostKeyCallback) (Conn, error) {
			dialled = true
			return &fakeConn{}, nil
		}})
	defer o2.Close()
	_, err = o2.FleetsList(context.Background(), "a")
	check("no key", err, root)
	if dialled {
		t.Fatal("dialled without key and known_hosts")
	}
	// Key present, known_hosts missing.
	full := setup(t, opconf.Context{Name: "a", Addr: "127.0.0.1:1"})
	if err := os.Remove(opconf.KnownHostsPath(full)); err != nil {
		t.Fatal(err)
	}
	o3, _ := New(Config{ConfigDir: full, StateDir: root, ProjectDir: root,
		Dial: func(context.Context, string, ssh.Signer, ssh.HostKeyCallback) (Conn, error) {
			dialled = true
			return &fakeConn{}, nil
		}})
	defer o3.Close()
	_, err = o3.FleetsList(context.Background(), "a")
	check("no known_hosts", err, full)
	if dialled {
		t.Fatal("dialled without a pinned host key")
	}
}

func TestSemaphoreLimitsConcurrency(t *testing.T) {
	var cur, max atomic.Int32
	release := make(chan struct{})
	d := &dialer{mk: func(int) (Conn, error) {
		return &fakeConn{handler: func(ctx context.Context, m string, res any) error {
			n := cur.Add(1)
			for {
				m := max.Load()
				if n <= m || max.CompareAndSwap(m, n) {
					break
				}
			}
			select {
			case <-release:
			case <-ctx.Done():
			}
			cur.Add(-1)
			return nil
		}}, nil
	}}
	o := newOrch(t, oneCtx(t), d)
	var wg sync.WaitGroup
	for i := 0; i < 3*api.MaxChannelsPerConn; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := o.WorkspaceList(context.Background(), "a", ""); err != nil {
				t.Error(err)
			}
		}()
	}
	eventually(t, "semaphore full", func() bool { return cur.Load() == api.MaxChannelsPerConn })
	time.Sleep(50 * time.Millisecond) // give excess callers the chance to (wrongly) enter
	if cur.Load() != api.MaxChannelsPerConn {
		t.Fatalf("in flight = %d, want %d", cur.Load(), api.MaxChannelsPerConn)
	}
	close(release)
	wg.Wait()
	if max.Load() != api.MaxChannelsPerConn {
		t.Fatalf("max in flight = %d, want %d", max.Load(), api.MaxChannelsPerConn)
	}
	if d.n.Load() != 1 {
		t.Fatalf("dials = %d: concurrent callers must share one connection", d.n.Load())
	}
}

func TestSemaphoreWaitHonoursContext(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	d := &dialer{mk: func(int) (Conn, error) {
		return &fakeConn{handler: func(ctx context.Context, m string, res any) error {
			select {
			case <-block:
			case <-ctx.Done():
			}
			return nil
		}}, nil
	}}
	o := newOrch(t, oneCtx(t), d)
	bg, cancelBG := context.WithCancel(context.Background())
	defer cancelBG()
	for i := 0; i < api.MaxChannelsPerConn; i++ {
		go o.WorkspaceList(bg, "a", "")
	}
	eventually(t, "slots taken", func() bool {
		o.mu.Lock()
		defer o.mu.Unlock()
		p := o.pools["a"]
		return p != nil && len(p.sem) == api.MaxChannelsPerConn
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := o.WorkspaceList(ctx, "a", "")
	var e *Error
	if !errors.As(err, &e) || e.Code != api.CodeTimeout {
		t.Fatalf("err = %v", err)
	}
}

func TestKeepaliveAndDropOnFailure(t *testing.T) {
	d := &dialer{mk: func(int) (Conn, error) { return &fakeConn{}, nil }}
	o := newOrch(t, oneCtx(t), d, func(c *Config) { c.KeepaliveInterval = 10 * time.Millisecond })
	if _, err := o.WorkspaceList(context.Background(), "a", ""); err != nil {
		t.Fatal(err)
	}
	c1 := d.conns[0].(*fakeConn)
	eventually(t, "3 pings", func() bool { return c1.pings.Load() >= 3 })
	if d.n.Load() != 1 {
		t.Fatal("healthy keepalive must not redial")
	}
	c1.pingErr.Store(io.EOF)
	eventually(t, "conn dropped", func() bool { return c1.closed.Load() })
	// The next call transparently redials, even a non-idempotent one.
	if _, err := o.WorkspaceClose(context.Background(), "a", "w"); err != nil {
		t.Fatal(err)
	}
	if d.n.Load() != 2 {
		t.Fatalf("dials = %d, want 2", d.n.Load())
	}
}

func TestCloseCancelsInFlight(t *testing.T) {
	started := make(chan struct{})
	d := &dialer{mk: func(int) (Conn, error) {
		return &fakeConn{handler: func(ctx context.Context, m string, res any) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		}}, nil
	}}
	o := newOrch(t, oneCtx(t), d)
	errc := make(chan error, 1)
	go func() {
		_, err := o.WorkspaceExec(context.Background(), "a", api.WorkspaceExecParams{Workspace: "w", Argv: []string{"sleep"}}, nil)
		errc <- err
	}()
	<-started
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errc:
		var e *Error
		if !errors.As(err, &e) || e.Code != api.CodeUnavailable {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight call not cancelled by Close")
	}
	if !d.conns[0].(*fakeConn).closed.Load() {
		t.Fatal("connection not closed")
	}
	if _, err := o.FleetsList(context.Background(), "a"); err == nil {
		t.Fatal("calls after Close must fail")
	}
}

func TestCallerCancelIsReturnedAsIs(t *testing.T) {
	d := &dialer{mk: func(int) (Conn, error) {
		return &fakeConn{handler: func(ctx context.Context, m string, res any) error { <-ctx.Done(); return ctx.Err() }}, nil
	}}
	o := newOrch(t, oneCtx(t), d)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	_, err := o.WorkspaceList(ctx, "a", "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if d.n.Load() != 1 {
		t.Fatal("a cancelled call must not redial or retry")
	}
}

func TestDecoyEnvNeverForwarded(t *testing.T) {
	decoys := map[string]string{
		"ANTHROPIC_API_KEY":     "sk-decoy-anthropic-9f2",
		"AWS_SECRET_ACCESS_KEY": "decoy-aws-secret-77a",
		"GITHUB_TOKEN":          "ghp_decoytoken123",
	}
	for k, v := range decoys {
		t.Setenv(k, v)
	}
	fc := &fakeConn{}
	o := newOrch(t, oneCtx(t), &dialer{mk: func(int) (Conn, error) { return fc, nil }})
	if _, err := o.WorkspaceOpen(context.Background(), "a", api.WorkspaceOpenParams{Fleet: "f", Session: "s"}); err != nil {
		t.Fatal(err)
	}
	if _, err := o.WorkspaceExec(context.Background(), "a",
		api.WorkspaceExecParams{Workspace: "w", Argv: []string{"env"}, Env: map[string]string{"FOO": "bar"}}, nil); err != nil {
		t.Fatal(err)
	}
	fc.mu.Lock()
	all := strings.Join(fc.params, "\n")
	fc.mu.Unlock()
	if !strings.Contains(all, `"FOO":"bar"`) {
		t.Fatalf("explicit env missing: %s", all)
	}
	for k, v := range decoys {
		if strings.Contains(all, v) || strings.Contains(all, k) {
			t.Fatalf("decoy %s reached a request: %s", k, all)
		}
	}
	// Denylisted env is refused before any call.
	n := len(fc.methods())
	_, err := o.WorkspaceExec(context.Background(), "a",
		api.WorkspaceExecParams{Workspace: "w", Argv: []string{"x"}, Env: map[string]string{"LD_PRELOAD": "/x"}}, nil)
	var e *Error
	if !errors.As(err, &e) || e.Code != api.CodeBadRequest || len(fc.methods()) != n {
		t.Fatalf("err = %v", err)
	}
	// Invalid session id refused locally.
	if _, err := o.WorkspaceOpen(context.Background(), "a", api.WorkspaceOpenParams{Fleet: "f", Session: "bad id"}); err == nil {
		t.Fatal("invalid session accepted")
	}
}

func TestNoEnvAccessInSource(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		// Code only: strip line comments, which may mention the names.
		var code []string
		for _, ln := range strings.Split(string(b), "\n") {
			if i := strings.Index(ln, "//"); i >= 0 {
				ln = ln[:i]
			}
			code = append(code, ln)
		}
		src := strings.Join(code, "\n")
		for _, bad := range []string{"os.Environ", "os.Getenv", "os.LookupEnv", "os.ExpandEnv", "syscall.Getenv"} {
			if strings.Contains(src, bad) {
				t.Errorf("%s uses %s", f, bad)
			}
		}
	}
}

func TestReadFileReturnsBody(t *testing.T) {
	fc := &fakeConn{body: []byte("hello"), handler: func(_ context.Context, m string, res any) error {
		if r, ok := res.(*api.WorkspaceReadFileResult); ok {
			r.Length, r.Size, r.EOF = 5, 5, true
		}
		return nil
	}}
	o := newOrch(t, oneCtx(t), &dialer{mk: func(int) (Conn, error) { return fc, nil }})
	r, b, err := o.WorkspaceReadFile(context.Background(), "a", api.WorkspaceReadFileParams{Workspace: "w", Path: "p"})
	if err != nil || string(b) != "hello" || r.Length != 5 {
		t.Fatalf("got %+v %q %v", r, b, err)
	}
}

func TestCallLimitMoved(t *testing.T) {
	if got := CallLimit(api.MethodFleetsList, nil); got != CallTimeout {
		t.Fatal(got)
	}
	if got := CallLimit(api.MethodActionRun, []byte(`{"timeout_s":10}`)); got != 10*time.Second+RunMargin {
		t.Fatal(got)
	}
}
