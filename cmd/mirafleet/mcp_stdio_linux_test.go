//go:build linux

package main

// TestM5NoHooks (docs/M5.md section 6): the real `mirafleet mcp` entry run as a child process
// with no harness around it (no CLAUDE_* env, no handoff file, no hook), spoken to in hand-written
// newline-delimited JSON-RPC over its stdin and stdout. The child is this test binary re-exec'd:
// the package has no TestMain, so a normal test function (TestM5NoHooksChild) is the hook. It does
// nothing unless nhChildEnv is set, then runs main()'s `mcp` branch and exits before the testing
// framework prints anything on stdout.
//
// The central is in process: SQLite, registry, service, SSH server, one workspace fleet and one
// runner backed by the fake executor.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/central"
	"github.com/mirastacklabs-ai/mirafleet/internal/central/registry"
	"github.com/mirastacklabs-ai/mirafleet/internal/keys"
	"github.com/mirastacklabs-ai/mirafleet/internal/mcpserver"
	"github.com/mirastacklabs-ai/mirafleet/internal/opconf"
	"github.com/mirastacklabs-ai/mirafleet/internal/policy"
	"github.com/mirastacklabs-ai/mirafleet/internal/runner"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/store/sqlite"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/fake"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/wsconfig"
)

const (
	nhChildEnv  = "MIRAFLEET_TEST_MCP_CHILD"
	nhDecoyAnth = "decoy-anthropic-key-s14b"
	nhDecoyAWS  = "decoy-aws-secret-s14b"
	nhImage     = "mirafleet-test/init:1"
	nhFleet     = "dev"
	nhStep      = 15 * time.Second // bound on any single read or wait
)

// TestM5NoHooksChild is the re-exec entry, not a test: skipped unless the parent set nhChildEnv.
func TestM5NoHooksChild(t *testing.T) {
	if os.Getenv(nhChildEnv) != "1" {
		t.Skip("re-exec helper for TestM5NoHooks")
	}
	args := flag.Args() // everything after `--`: "mcp", flags...
	if len(args) == 0 || args[0] != "mcp" {
		fmt.Fprintln(os.Stderr, "child: want `mcp ...`")
		os.Exit(2)
	}
	if err := mcpMain(args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "mirafleet:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// ---- the in-process central ----

type nhCentral struct {
	addr  string // the address operators dial: a counting proxy in front of central
	svc   *service.Service
	db    *sqlite.DB
	conns *atomic.Int64 // live operator connections through the proxy
	seen  *nhSeen
}

// nhSeen records every place a decoy value reached the workspace side.
type nhSeen struct {
	mu   sync.Mutex
	hits []string
}

func (s *nhSeen) check(where string, vals ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range vals {
		for _, d := range []string{nhDecoyAnth, nhDecoyAWS} {
			if strings.Contains(v, d) {
				s.hits = append(s.hits, where+": "+v)
			}
		}
	}
}

func (s *nhSeen) list() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.hits...)
}

type nhFake struct {
	*fake.Executor
	seen *nhSeen
}

func (nhFake) Name() string { return wsconfig.ExecContainerd }

func (f nhFake) Create(ctx context.Context, s workspace.Spec) error {
	f.seen.check("create image", s.Image)
	for k, v := range s.Labels {
		f.seen.check("create label "+k, k, v)
	}
	return f.Executor.Create(ctx, s)
}

// nhProxy forwards TCP to target and counts live connections. A connection is counted until
// both directions have ended, i.e. until central has also closed its side: the count reaching 0
// proves central has noticed the operator going away.
func nhProxy(t *testing.T, target string) (addr string, live *atomic.Int64) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	live = new(atomic.Int64)
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			cc, err := ln.Accept()
			if err != nil {
				return
			}
			uc, err := net.Dial("tcp", target)
			if err != nil {
				cc.Close()
				continue
			}
			live.Add(1)
			go func() {
				defer live.Add(-1)
				var wg sync.WaitGroup
				pipe := func(dst, src net.Conn) {
					defer wg.Done()
					io.Copy(dst, src)
					dst.Close()
					src.Close()
				}
				wg.Add(2)
				go pipe(uc, cc)
				go pipe(cc, uc)
				wg.Wait()
			}()
		}
	}()
	return ln.Addr().String(), live
}

const nhYAML = `
executor: containerd
concurrent: 1
max_age: 24h
images: ["mirafleet-test/init:*"]
limits: {cpus: 1, memory: 256MiB, pids: 128, tmpfs: 16MiB, disk: 256MiB, max_file: 4MiB, exec_max: 2m}
network: [none]
env_allow: [LANG]
containerd:
  namespace: mirafleet
  root: /var/lib/mirafleet/ws
`

func nhStartCentral(t *testing.T) *nhCentral {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	if _, err := keys.Generate(dir, "host", "host"); err != nil {
		t.Fatal(err)
	}
	host, err := keys.LoadSigner(filepath.Join(dir, "host"))
	if err != nil {
		t.Fatal(err)
	}
	known := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(known, []byte("* "+keys.PubLine(host.PublicKey(), "")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	logf := func(string, ...any) {}
	reg, err := registry.New(registry.Options{StaleAfter: 45 * time.Second, Logf: logf})
	if err != nil {
		t.Fatal(err)
	}
	ops := central.NewOperatorConns()
	svc, err := service.New(service.Config{Store: db, Policy: policy.TablePolicy{}, Live: reg,
		HostKey: host.PublicKey(), Now: time.Now, OperatorConns: ops, Logf: logf, ReaperInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	admin := auth.LocalAdmin(os.Getuid())
	cfg := store.WorkspaceConfig{Executor: store.ExecutorContainerd, DefaultImage: nhImage, Images: []string{"mirafleet-test/init:*"},
		Pull: store.PullNever, Network: store.NetworkNone, CPUMilli: 1000, MemoryMB: 256, Pids: 128, TmpfsMB: 16, DiskMB: 128,
		IdleTTL: time.Hour, MaxTTL: 6 * time.Hour, ExecDefault: 30 * time.Second, ExecMax: 2 * time.Minute}
	if _, err := svc.CreateFleet(ctx, admin, service.SourceCLI, nhFleet, store.ModeWorkspace, false, &cfg); err != nil {
		t.Fatal(err)
	}
	if err := svc.InitRegistrationSecret(ctx, admin, service.SourceCLI); err != nil {
		t.Fatal(err)
	}
	// Enrol the runner through the store (as the repo's other in-process tests do).
	pub, err := keys.Generate(dir, "rw", "rw")
	if err != nil {
		t.Fatal(err)
	}
	pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pub))
	if err != nil {
		t.Fatal(err)
	}
	secret := sha256.Sum256([]byte("secret-s14b"))
	now := time.Now()
	ev := store.AuditEvent{Source: "system", ActorKind: "system", Actor: "test", Action: "token.create", Outcome: "ok"}
	if _, err := db.CreateToken(ctx, store.JoinToken{TokenID: "00000000000014b0", SecretHash: secret[:], Fleet: nhFleet,
		AutoApprove: true, MaxUses: 1, ExpiresAt: now.Add(time.Hour), CreatedBy: "test"}, now, ev); err != nil {
		t.Fatal(err)
	}
	ev.Action = "runner.enrol"
	if _, err := db.Enroll(ctx, "00000000000014b0", secret[:], now,
		store.Runner{Name: "rw", PubKey: pk.Marshal(), Fingerprint: ssh.FingerprintSHA256(pk)}, ev); err != nil {
		t.Fatal(err)
	}
	srv, err := central.New(central.Config{Listen: "127.0.0.1:0", HostKey: host, Store: db, Service: svc,
		Registry: reg, Operators: ops, Logf: logf})
	if err != nil {
		t.Fatal(err)
	}
	addr, err := srv.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)

	fx := fake.New()
	seen := &nhSeen{}
	fx.ExecFunc = func(_ context.Context, _ workspace.ID, r workspace.ExecRequest) (workspace.ExecResult, error) {
		for k, v := range r.Env {
			seen.check("exec env "+k, k, v)
		}
		seen.check("exec argv", r.Argv...)
		seen.check("exec cwd", r.Cwd)
		out := "ok\n"
		if len(r.Argv) == 3 && r.Argv[2] == "echo hi" {
			out = "hi\n"
		}
		if r.Stdout != nil {
			r.Stdout.Write([]byte(out))
		}
		return workspace.ExecResult{}, nil
	}
	wc, err := wsconfig.Parse([]byte(nhYAML))
	if err != nil {
		t.Fatal(err)
	}
	signer, err := keys.LoadSigner(filepath.Join(dir, "rw"))
	if err != nil {
		t.Fatal(err)
	}
	rctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runner.New(runner.Config{Central: addr, Name: "rw", Signer: signer, KnownHosts: known,
			Workspace: nhFake{fx, seen}, WorkspaceConfig: wc, Version: "test",
			HeartbeatEvery: 50 * time.Millisecond, HeartbeatTimeout: 10 * time.Second, BackoffMin: 20 * time.Millisecond, BackoffMax: 100 * time.Millisecond}).Run(rctx)
		close(done)
	}()
	t.Cleanup(func() { cancel(); <-done })
	nhWait(t, "runner online", func() bool {
		for _, si := range reg.Snapshot() {
			if si.Name == "rw" && si.Online && si.Slots == 1 && si.Executor == "containerd" {
				return true
			}
		}
		return false
	})
	paddr, live := nhProxy(t, addr)
	return &nhCentral{addr: paddr, svc: svc, db: db, conns: live, seen: seen}
}

func nhWait(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(nhStep)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ---- the child process ----

type nhChild struct {
	t      *testing.T
	cmd    *exec.Cmd
	in     *os.File
	lines  chan string
	stderr *lockedBuf
	exited chan struct{}
	done   chan struct{} // closed in Cleanup: releases the reader goroutine
	waitEr error
	all    []string
	nextID int
}

type nhEnv struct{ cfg, state, home, project string }

func nhStartChild(t *testing.T, e nhEnv) *nhChild {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	c := &nhChild{t: t, in: inW, lines: make(chan string, 256), stderr: &lockedBuf{}, exited: make(chan struct{}), done: make(chan struct{})}
	c.cmd = exec.Command(exe, "-test.run=^TestM5NoHooksChild$", "--", "mcp", "-project-dir", e.project)
	c.cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + e.home,
		opconf.EnvDir + "=" + e.cfg,
		opconf.EnvStateDir + "=" + e.state,
		"ANTHROPIC_API_KEY=" + nhDecoyAnth,
		"AWS_SECRET_ACCESS_KEY=" + nhDecoyAWS,
		nhChildEnv + "=1",
	}
	c.cmd.Dir = e.project
	c.cmd.WaitDelay = nhStep
	c.cmd.Stdin, c.cmd.Stdout, c.cmd.Stderr = inR, outW, c.stderr
	if err := c.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	inR.Close()
	outW.Close()
	// The reader goroutine is the only owner of outR: it reads to EOF (every byte the child
	// wrote, even after it exited) and closes it. The Wait goroutine never touches it.
	go func() {
		defer close(c.lines)
		defer outR.Close()
		br := bufio.NewReaderSize(outR, 1<<20)
		for {
			l, err := br.ReadString('\n')
			if l != "" {
				select {
				case c.lines <- l:
				case <-c.done:
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	go func() { c.waitEr = c.cmd.Wait(); close(c.exited) }()
	t.Cleanup(func() {
		c.in.Close()
		c.cmd.Process.Kill()
		<-c.exited
		close(c.done)
	})
	return c
}

// checkLine asserts one stdout line is a JSON-RPC 2.0 message.
func (c *nhChild) checkLine(l string) map[string]any {
	c.t.Helper()
	c.all = append(c.all, l)
	var m map[string]any
	if err := json.Unmarshal([]byte(l), &m); err != nil || m["jsonrpc"] != "2.0" {
		c.t.Fatalf("stdout line is not JSON-RPC 2.0: %q (%v); stderr: %q", l, err, c.stderr.String())
	}
	return m
}

// drain reads every stdout line left after the child exited (until the reader closes the
// channel) and applies the same JSON-RPC check to each.
func (c *nhChild) drain() {
	c.t.Helper()
	for {
		select {
		case l, ok := <-c.lines:
			if !ok {
				return
			}
			c.checkLine(l)
		case <-time.After(nhStep):
			c.t.Fatalf("stdout not closed %s after child exit; stderr: %q", nhStep, c.stderr.String())
		}
	}
}

// obj, arr and str are comma-ok assertions that fail the test (never panic) with the child's stderr.
func (c *nhChild) obj(v any, what string) map[string]any {
	c.t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		c.t.Fatalf("%s: got %T (%v), want object; stderr: %q", what, v, v, c.stderr.String())
	}
	return m
}

func (c *nhChild) arr(v any, what string) []any {
	c.t.Helper()
	a, ok := v.([]any)
	if !ok {
		c.t.Fatalf("%s: got %T (%v), want array; stderr: %q", what, v, v, c.stderr.String())
	}
	return a
}

func (c *nhChild) str(v any, what string) string {
	c.t.Helper()
	s, ok := v.(string)
	if !ok {
		c.t.Fatalf("%s: got %T (%v), want string; stderr: %q", what, v, v, c.stderr.String())
	}
	return s
}

// recv returns the next stdout line, asserted to be a JSON-RPC 2.0 message.
func (c *nhChild) recv() map[string]any {
	c.t.Helper()
	select {
	case l, ok := <-c.lines:
		if !ok {
			c.t.Fatalf("child stdout closed; stderr: %q", c.stderr.String())
		}
		return c.checkLine(l)
	case <-time.After(nhStep):
		c.t.Fatalf("timeout reading child stdout; stderr: %q", c.stderr.String())
	}
	return nil
}

func (c *nhChild) send(m map[string]any) {
	c.t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		c.t.Fatal(err)
	}
	if _, err := c.in.Write(append(b, '\n')); err != nil {
		c.t.Fatal(err)
	}
}

// rpc sends a request and returns the matching response, skipping interleaved notifications.
func (c *nhChild) rpc(method string, params any) map[string]any {
	c.t.Helper()
	c.nextID++
	id := c.nextID
	c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	for {
		m := c.recv()
		if m["method"] != nil {
			continue // server notification or request
		}
		if m["id"] != float64(id) {
			c.t.Fatalf("response id %v, want %d: %v", m["id"], id, m)
		}
		return m
	}
}

// handshake runs initialize + notifications/initialized and returns the negotiated version.
func (c *nhChild) handshake(ask string) string {
	c.t.Helper()
	r := c.rpc("initialize", map[string]any{"protocolVersion": ask, "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "nohooks", "version": "0"}})
	res, ok := r["result"].(map[string]any)
	if !ok {
		c.t.Fatalf("initialize: %v", r)
	}
	c.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	v, _ := res["protocolVersion"].(string)
	return v
}

// call runs a tool and returns its structured result; isError fails the test.
func (c *nhChild) call(name string, args map[string]any) map[string]any {
	c.t.Helper()
	r := c.rpc("tools/call", map[string]any{"name": name, "arguments": args})
	res, ok := r["result"].(map[string]any)
	if !ok {
		c.t.Fatalf("%s: %v", name, r)
	}
	if res["isError"] == true {
		c.t.Fatalf("%s: tool error: %v", name, res)
	}
	if sc, ok := res["structuredContent"].(map[string]any); ok {
		return sc
	}
	if cs, ok := res["content"].([]any); ok && len(cs) > 0 {
		if first, ok := cs[0].(map[string]any); ok {
			var out map[string]any
			if json.Unmarshal([]byte(fmt.Sprint(first["text"])), &out) == nil {
				return out
			}
		}
	}
	c.t.Fatalf("%s: no structured result: %v", name, res)
	return nil
}

// exitCode waits (bounded) for the child to exit and returns its exit code.
func (c *nhChild) exitCode(within time.Duration) int {
	c.t.Helper()
	select {
	case <-c.exited:
		return c.cmd.ProcessState.ExitCode()
	case <-time.After(within):
		c.t.Fatalf("child still running after %s; stderr: %q", within, c.stderr.String())
	}
	return -1
}

// noDecoys, called after the exit code is known, drains and validates the rest of stdout, then
// asserts no decoy value is in any stdout line, in the full stderr, or was seen workspace-side.
func (c *nhChild) noDecoys(seen *nhSeen) {
	c.t.Helper()
	c.drain()
	out, errOut := strings.Join(c.all, ""), c.stderr.String()
	for _, d := range []string{nhDecoyAnth, nhDecoyAWS} {
		if strings.Contains(out, d) {
			c.t.Errorf("decoy %q leaked to the child's stdout", d)
		}
		if strings.Contains(errOut, d) {
			c.t.Errorf("decoy %q leaked to the child's stderr", d)
		}
	}
	if h := seen.list(); len(h) > 0 {
		c.t.Errorf("decoy reached the workspace: %q", h)
	}
}

func TestM5NoHooks(t *testing.T) {
	c := nhStartCentral(t)

	// Operator registration into a temp config dir, through the real `mcp login` path.
	cfg := filepath.Join(t.TempDir(), "cfg")
	t.Setenv(opconf.EnvDir, cfg)
	code, err := c.svc.OperatorCode(context.Background(), auth.LocalAdmin(os.Getuid()), service.SourceCLI)
	if err != nil {
		t.Fatal(err)
	}
	if err := mcpMain([]string{"login", c.addr, "-name", "alice", "-code", "-"}, strings.NewReader(code.Code+"\n"), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, ".mirafleet.yaml"), []byte("fleet: "+nhFleet+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := nhEnv{cfg: cfg, state: t.TempDir(), home: t.TempDir(), project: project}

	// Run 1: full tool flow, then stdin close.
	t.Run("full flow and stdin close", func(t *testing.T) {
		ch := nhStartChild(t, env)
		if v := ch.handshake("2025-06-18"); v != "2025-06-18" {
			t.Errorf("asked 2025-06-18, got %q", v)
		}
		lt := ch.rpc("tools/list", map[string]any{})
		tools := ch.arr(ch.obj(lt["result"], "tools/list result")["tools"], "tools/list tools")
		got := map[string]bool{}
		for i, x := range tools {
			got[ch.str(ch.obj(x, fmt.Sprintf("tool %d", i))["name"], "tool name")] = true
		}
		// Expected names come from the tool definitions themselves (no backend needed to list them).
		want := map[string]bool{}
		for _, tl := range append(mcpserver.ListTools(nil), mcpserver.WorkspaceTools(nil)...) {
			want[tl.Name] = true
		}
		for n := range want {
			if !got[n] {
				t.Errorf("tools/list lacks %s", n)
			}
		}
		for n := range got {
			if !want[n] {
				t.Errorf("tools/list has unexpected %s", n)
			}
		}
		if len(tools) != len(want) || len(want) == 0 {
			t.Errorf("tools/list returned %d tools, want %d", len(tools), len(want))
		}

		lf := ch.call("list_fleets", map[string]any{})
		found := false
		for i, f := range ch.arr(lf["fleets"], "list_fleets fleets") {
			fm := ch.obj(f, fmt.Sprintf("fleet %d", i))
			if fm["name"] == nhFleet && fm["mode"] == "workspace" {
				found = true
			}
		}
		if !found {
			t.Errorf("list_fleets lacks %s: %v", nhFleet, lf)
		}

		op := ch.call("workspace_open", map[string]any{})
		wsID := ch.str(ch.obj(op["workspace"], "workspace_open workspace")["id"], "workspace id")
		if wsID == "" || op["reused"] != false {
			t.Fatalf("workspace_open: %v", op)
		}
		// "process" = the session came from this process's own id: no handoff file, no hook.
		if op["session_source"] != "process" {
			t.Errorf("workspace_open session_source = %v, want process (a handoff or hook was used)", op["session_source"])
		}

		ex := ch.call("exec", map[string]any{"command": "echo hi"})
		if ex["exit_code"] != float64(0) || strings.TrimSpace(fmt.Sprint(ex["stdout"])) != "hi" || ex["workspace"] != wsID {
			t.Errorf("exec: %v", ex)
		}
		ex = ch.call("exec", map[string]any{"argv": []string{"id"}})
		if ex["exit_code"] != float64(0) {
			t.Errorf("exec argv: %v", ex)
		}

		wr := ch.call("write_file", map[string]any{"path": "/workspace/hello.txt", "content": "hello nohooks\n"})
		if wr["bytes_written"] != float64(14) {
			t.Errorf("write_file: %v", wr)
		}
		rd := ch.call("read_file", map[string]any{"path": "/workspace/hello.txt"})
		sum := sha256.Sum256([]byte("hello nohooks\n"))
		if rd["content"] != "hello nohooks\n" || rd["sha256"] != fmt.Sprintf("%x", sum[:]) {
			t.Errorf("read_file: %v", rd)
		}

		cl := ch.call("workspace_close", map[string]any{})
		if cl["workspace"] != wsID {
			t.Errorf("workspace_close: %v", cl)
		}
		nhWait(t, "workspace closed in the store", func() bool {
			r, err := c.db.GetWorkspace(context.Background(), wsID)
			return err == nil && r.State == store.WorkspaceClosed
		})

		ch.in.Close()
		if code := ch.exitCode(5 * time.Second); code != 0 {
			t.Errorf("exit code after stdin close = %d; stderr: %q", code, ch.stderr.String())
		}
		ch.noDecoys(c.seen)
	})

	// Run 2: version above the SDK's, then SIGTERM.
	t.Run("sigterm", func(t *testing.T) {
		ch := nhStartChild(t, env)
		if v := ch.handshake("2026-07-28"); v != "2025-11-25" {
			t.Errorf("asked 2026-07-28, got %q, want 2025-11-25", v)
		}
		ch.call("list_fleets", map[string]any{})
		if err := ch.cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		if code := ch.exitCode(5 * time.Second); code != 0 {
			t.Errorf("exit code after SIGTERM = %d; stderr: %q", code, ch.stderr.String())
		}
		ch.noDecoys(c.seen)
	})

	// Run 3: leave a workspace open; it stays open after the server exits.
	t.Run("unclosed workspace stays open", func(t *testing.T) {
		ch := nhStartChild(t, env)
		ch.handshake("2025-11-25")
		op := ch.call("workspace_open", map[string]any{})
		wsID := ch.str(ch.obj(op["workspace"], "workspace_open workspace")["id"], "workspace id")
		if wsID == "" || op["reused"] != false {
			t.Fatalf("workspace_open: %v", op)
		}
		if op["session_source"] != "process" {
			t.Errorf("workspace_open session_source = %v, want process (a handoff or hook was used)", op["session_source"])
		}
		// The operator connection is live (so the counting proxy is really in the path).
		if c.conns.Load() == 0 {
			t.Fatal("no operator connection through the proxy while the child is running")
		}
		ch.in.Close()
		if code := ch.exitCode(5 * time.Second); code != 0 {
			t.Errorf("exit code = %d; stderr: %q", code, ch.stderr.String())
		}
		ch.noDecoys(c.seen)
		// Sync point: central has seen the operator go (its side of every operator connection
		// is closed). Any close it would do on disconnect has been triggered by then.
		nhWait(t, "central to register the operator disconnect", func() bool { return c.conns.Load() == 0 })
		r, err := c.db.GetWorkspace(context.Background(), wsID)
		if err != nil || r.State != store.WorkspaceOpen {
			t.Errorf("workspace after server exit: %+v %v, want open", r, err)
		}
	})
}
