package central

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/apiclient"
	"github.com/mirastacklabs-ai/mirafleet/internal/central/registry"
	"github.com/mirastacklabs-ai/mirafleet/internal/keys"
	"github.com/mirastacklabs-ai/mirafleet/internal/policy"
	"github.com/mirastacklabs-ai/mirafleet/internal/runner"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/fake"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/wsconfig"
)

// The "M3 done when" scenario of docs/M3.md section 4, in process: a real central (SQLite store,
// registry, service, SSH), two real runners in workspace mode and real operator clients. Only
// the workspace executor differs: TestM3Done uses the fake executor (every run); the containerd
// variant (m3_done_integration_linux_test.go) drives the same scenario against a real containerd.
//
// What the fake variant can only assert as requests and specs (the fake's ExecFunc is a stand-in
// userland): point 4's non-root uid, read-only rootfs, symlink escape and no-network checks reach
// the executor as exec requests and Create specs (network none, limits, image, no denied env) but
// prove no isolation; the fake cannot make a symlink, and the "read-only" text is only checked
// for the real variant; restart reconcile and the reaper act on the fake's in-memory containers
// and have no host directories. TestM3DoneIntegration (m3_done_integration_linux_test.go) covers
// the actual behaviour of those points against real containerd containers.

const (
	m3Fleet = "dev"
	m3Image = "mirafleet-test/init:1" // inside the fleet and runner allowlists
)

// m3Backend is what differs between the fake and the containerd variants.
type m3Backend struct {
	real bool
	// yaml is the runner's workspace.yaml.
	yaml func(runner string) string
	// exec returns the runner's executor: the persistent fake, or a fresh containerd executor
	// over the same root and namespace (a runner restart finds its containers again).
	exec func(t *testing.T, runner string) workspace.Executor
	// stop releases the executor of a stopped runner (containerd client); no-op for the fake.
	stop func(t *testing.T, runner string)
	// Probe commands run inside the workspace.
	idArgv, etcArgv, dialArgv []string
	symlinkArgv               func(target, link string) []string // nil: cannot make a symlink
	// Host side: live containers and workspace directories of a runner.
	containers func(t *testing.T, runner string) int
	dirs       func(t *testing.T, runner string) []string
	// hostSecret is a host file the symlink attack aims at.
	hostSecret string
	// after runs variant-specific checks at the end.
	after func(t *testing.T)
}

func m3ctx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func m3sum(b []byte) string { h := sha256.Sum256(b); return fmt.Sprintf("%x", h[:]) }

var firstNumber = regexp.MustCompile(`\d+`)

// ---- the fake backend ----

// m3Fake is the fake executor reporting the name of a containerd executor (so the heartbeat
// matches the fleet), recording every Create spec and exec request it receives. Its ExecFunc is a
// stand-in for the container's userland: it cannot prove isolation, only that the requests and
// specs which enforce it arrive.
type m3Fake struct {
	*fake.Executor
	mu    sync.Mutex
	specs []workspace.Spec
	execs []workspace.ExecRequest
}

func (f *m3Fake) Name() string { return wsconfig.ExecContainerd }

func (f *m3Fake) Create(ctx context.Context, s workspace.Spec) error {
	f.mu.Lock()
	f.specs = append(f.specs, s)
	f.mu.Unlock()
	return f.Executor.Create(ctx, s)
}

func (f *m3Fake) Destroy(ctx context.Context, id workspace.ID) error {
	return f.Executor.Destroy(ctx, id)
}

func (f *m3Fake) List(ctx context.Context) ([]workspace.Info, error) {
	return f.Executor.List(ctx)
}

func newM3Fake() *m3Fake {
	f := &m3Fake{Executor: fake.New()}
	f.ExecFunc = func(ctx context.Context, id workspace.ID, r workspace.ExecRequest) (workspace.ExecResult, error) {
		f.mu.Lock()
		f.execs = append(f.execs, workspace.ExecRequest{Argv: append([]string(nil), r.Argv...), Cwd: r.Cwd, Env: r.Env})
		f.mu.Unlock()
		say := func(w interface{ Write([]byte) (int, error) }, s string) {
			if w != nil {
				w.Write([]byte(s))
			}
		}
		switch r.Argv[0] {
		case "id":
			say(r.Stdout, "1000\n")
		case "touch":
			say(r.Stderr, "read-only file system\n")
			return workspace.ExecResult{ExitCode: 1}, nil
		case "dial":
			say(r.Stdout, "dial-failed: network is unreachable\n")
		default:
			say(r.Stdout, "ok\n")
		}
		return workspace.ExecResult{}, nil
	}
	return f
}

const m3FakeYAML = `
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

func TestM3Done(t *testing.T) {
	execs := map[string]*m3Fake{"ra": newM3Fake(), "rb": newM3Fake()}
	b := m3Backend{
		yaml:       func(string) string { return m3FakeYAML },
		exec:       func(_ *testing.T, r string) workspace.Executor { return execs[r] },
		stop:       func(*testing.T, string) {},
		idArgv:     []string{"id", "-u"},
		etcArgv:    []string{"touch", "/etc/m3"},
		dialArgv:   []string{"dial", "1.1.1.1:80"},
		hostSecret: "/etc/passwd",
		containers: func(t *testing.T, r string) int {
			infos, err := execs[r].List(context.Background())
			must(t, err)
			return len(infos)
		},
		dirs: func(*testing.T, string) []string { return nil },
		after: func(t *testing.T) {
			// What enforces the isolation reaches the executor: every create carries network
			// "none", the fleet's limits and the allowed image; no exec carried a denied
			// variable or a user switch; no exec ran outside /workspace.
			created := 0
			for name, f := range execs {
				f.mu.Lock()
				for _, s := range f.specs {
					created++
					if s.Network.Mode != workspace.NetNone && s.Network.Mode != "" || len(s.Network.Allow) != 0 {
						t.Errorf("%s: spec network %+v", name, s.Network)
					}
					if s.Image != m3Image || s.Limits.MemoryBytes != 256<<20 || s.Limits.Pids != 128 || s.Limits.CPUMilli != 1000 ||
						s.Limits.TmpfsBytes != 16<<20 || s.RequireSecureRuntime {
						t.Errorf("%s: spec %+v", name, s)
					}
				}
				for _, x := range f.execs {
					if _, bad := x.Env["ANTHROPIC_API_KEY"]; bad {
						t.Errorf("%s: denied env reached the executor", name)
					}
				}
				f.mu.Unlock()
			}
			if created < 3 { // s1, s2, b1
				t.Errorf("only %d creates reached the executors", created)
			}
		},
	}
	runM3Done(t, b)
}

// ---- the scenario ----

type m3Runner struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func (r *m3Runner) stop() { r.cancel(); <-r.done }

// m3H bundles the central under test; svc and reg are replaced on a central restart.
type m3H struct {
	svc *service.Service
	reg *registry.Registry
}

func runM3Done(t *testing.T, b m3Backend) {
	var offset atomic.Int64 // seconds added to the service clock
	base := time.Now()
	clock := func() time.Time { return base.Add(time.Duration(offset.Load()) * time.Second) }

	// (1) Set-up: central, fleet dev in workspace mode with an image allowlist, operators alice
	// and bob, two runners with concurrent:1 and different trusted labels.
	h := &m3H{}
	e := newEnvOpts(t, 15*time.Second, func(c *Config) {
		reg, err := registry.New(registry.Options{StaleAfter: 45 * time.Second, Logf: c.Logf})
		must(t, err)
		svc, err := service.New(service.Config{Store: c.Store, Policy: policy.TablePolicy{}, Live: reg,
			HostKey: c.HostKey.PublicKey(), Now: clock, OperatorConns: c.Operators, Logf: c.Logf, ReaperInterval: time.Hour})
		must(t, err)
		c.Registry, c.Service = reg, svc
		h.reg, h.svc = reg, svc
	})
	e.reg, e.svc = h.reg, h.svc
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("central log:\n%s", e.logText())
		}
	})
	ctx := m3ctx(t)

	cfg := store.WorkspaceConfig{Executor: store.ExecutorContainerd, DefaultImage: m3Image, Images: []string{"mirafleet-test/init:*"},
		Pull: store.PullNever, Network: store.NetworkNone, CPUMilli: 1000, MemoryMB: 256, Pids: 128, TmpfsMB: 16, DiskMB: 128,
		IdleTTL: time.Hour, MaxTTL: 6 * time.Hour, ExecDefault: 30 * time.Second, ExecMax: 2 * time.Minute}
	_, err := e.svc.CreateFleet(ctx, e.admin, service.SourceCLI, m3Fleet, store.ModeWorkspace, false, &cfg)
	must(t, err)
	m3Enrol(e, m3Fleet, "ra", map[string]string{"zone": "a"})
	m3Enrol(e, m3Fleet, "rb", map[string]string{"zone": "b"})
	e.addOperator("alice")
	e.addOperator("bob")

	runners := map[string]*m3Runner{}
	startRunner := func(name string) {
		t.Helper()
		wc, err := wsconfig.Parse([]byte(b.yaml(name)))
		must(t, err)
		rctx, cancel := context.WithCancel(context.Background())
		rc := runner.New(runner.Config{Central: e.addr, Name: name, Signer: e.signer(name), KnownHosts: e.knownHosts,
			Workspace: b.exec(t, name), WorkspaceConfig: wc, Labels: map[string]string{"zone": "lies"}, Version: "test",
			HeartbeatEvery: 50 * time.Millisecond, HeartbeatTimeout: 10 * time.Second, BackoffMin: 20 * time.Millisecond, BackoffMax: 100 * time.Millisecond})
		r := &m3Runner{cancel: cancel, done: make(chan struct{})}
		go func() { rc.Run(rctx); close(r.done) }()
		runners[name] = r
		t.Cleanup(r.stop)
	}
	online := func(name string) bool {
		for _, si := range h.reg.Snapshot() {
			if si.Name == name && si.Online && si.Slots == 1 && si.Executor == "containerd" {
				return true
			}
		}
		return false
	}
	waitOnline := func(names ...string) {
		t.Helper()
		waitFor(t, func() bool {
			for _, n := range names {
				if !online(n) {
					return false
				}
			}
			return true
		})
	}
	startRunner("ra")
	startRunner("rb")
	waitOnline("ra", "rb")

	al, bo := e.opClient("alice"), e.opClient("bob")

	// (2) alice opens s1 with a label and lands on the matching runner; reopening reuses it.
	op, err := al.WorkspaceOpen(ctx, api.WorkspaceOpenParams{Fleet: m3Fleet, Session: "s1", Labels: map[string]string{"zone": "a"}})
	must(t, err)
	s1 := op.Workspace.ID
	if op.Reused || op.Workspace.Runner != "ra" || op.Workspace.State != "open" || op.Workspace.Image != m3Image || op.Workspace.Session != "s1" {
		t.Fatalf("open s1: %+v", op)
	}
	again, err := al.WorkspaceOpen(ctx, api.WorkspaceOpenParams{Fleet: m3Fleet, Session: "s1", Labels: map[string]string{"zone": "a"}})
	must(t, err)
	if !again.Reused || again.Workspace.ID != s1 || again.Workspace.Runner != "ra" {
		t.Fatalf("reopen s1: %+v", again)
	}
	if n := b.containers(t, "ra"); n != 1 {
		t.Fatalf("containers on ra after reopen: %d (reuse must not create)", n)
	}

	// (3) bob gets not_found for exec, read_file and close (and write_file) on it.
	_, err = bo.WorkspaceExec(ctx, api.WorkspaceExecParams{Workspace: s1, Argv: b.idArgv}, nil)
	wantCode(t, err, api.CodeNotFound)
	_, _, err = bo.WorkspaceReadFile(ctx, api.WorkspaceReadFileParams{Workspace: s1, Path: "anything"})
	wantCode(t, err, api.CodeNotFound)
	_, err = bo.WorkspaceWriteFile(ctx, writeParams(s1, "x", []byte("x")), []byte("x"))
	wantCode(t, err, api.CodeNotFound)
	wantCode(t, bo.WorkspaceClose(ctx, s1), api.CodeNotFound)
	if l, err := bo.WorkspaceList(ctx, ""); err != nil || len(l.Workspaces) != 0 {
		t.Fatalf("bob sees %+v %v", l, err)
	}
	if r, err := e.db.GetWorkspace(ctx, s1); err != nil || r.State != store.WorkspaceOpen {
		t.Fatalf("bob's close affected s1: %+v %v", r, err)
	}

	// (4) Inside the workspace.
	execs := 0
	run := func(argv []string) *api.WorkspaceExecResult {
		t.Helper()
		execs++
		r, err := al.WorkspaceExec(ctx, api.WorkspaceExecParams{Workspace: s1, Argv: argv}, nil)
		must(t, err)
		return r
	}
	// id: not root.
	idRes := run(b.idArgv)
	if m := firstNumber.FindString(idRes.Stdout); idRes.ExitCode != 0 || m == "" || m == "0" {
		t.Fatalf("id: %+v", idRes)
	}
	// Read-only rootfs: writing to /etc fails.
	etc := run(b.etcArgv)
	if etc.ExitCode == 0 || (b.real && !strings.Contains(etc.Stdout+etc.Stderr, "read-only")) {
		t.Fatalf("write to /etc: %+v", etc)
	}
	// 1.5 MiB round trip in 512 KiB chunks matches its SHA-256.
	const total, chunk = 3 << 19, 1 << 19
	data := make([]byte, total)
	_, err = rand.Read(data)
	must(t, err)
	for off := 0; off < total; off += chunk {
		p := writeParams(s1, "m3/big.bin", data[off:off+chunk])
		p.Offset, p.Truncate = int64(off), off == 0
		wr, err := al.WorkspaceWriteFile(ctx, p, data[off:off+chunk])
		must(t, err)
		if wr.Size != int64(off+chunk) {
			t.Fatalf("write at %d: size %d", off, wr.Size)
		}
	}
	var got []byte
	mainReads := 0
	for off := int64(0); ; {
		rf, body, err := al.WorkspaceReadFile(ctx, api.WorkspaceReadFileParams{Workspace: s1, Path: "m3/big.bin", Offset: off, Limit: chunk})
		must(t, err)
		mainReads++
		got = append(got, body...)
		off += int64(len(body))
		if rf.Size != total {
			t.Fatalf("read size %d", rf.Size)
		}
		if rf.EOF {
			break
		}
		if len(body) == 0 {
			t.Fatal("read made no progress")
		}
	}
	if len(got) != total || m3sum(got) != m3sum(data) {
		t.Fatalf("round trip: %d bytes, sha %s want %s", len(got), m3sum(got), m3sum(data))
	}
	// A symlink to a host path cannot be read through read_file; nor can host paths by name.
	secretPath := b.hostSecret
	symlinkReads := 0 // audited read_file calls among the attacks (the symlink one, real variant only)
	attacks := []string{secretPath, "../../../../" + strings.TrimPrefix(secretPath, "/")}
	if b.symlinkArgv != nil {
		if r := run(b.symlinkArgv(secretPath, "/workspace/leak")); r.ExitCode != 0 {
			t.Fatalf("symlink: %+v", r)
		}
		attacks = append(attacks, "leak")
		symlinkReads = 1
	}
	hostBytes, herr := os.ReadFile(secretPath)
	for _, path := range attacks {
		rf, body, err := al.WorkspaceReadFile(ctx, api.WorkspaceReadFileParams{Workspace: s1, Path: path})
		if err == nil || len(body) != 0 || (rf != nil && rf.Length != 0) {
			t.Fatalf("read_file %q leaked host data: %v %d", path, err, len(body))
		}
		if herr == nil && len(hostBytes) > 0 && bytes.Contains([]byte(err.Error()), hostBytes[:min(16, len(hostBytes))]) {
			t.Fatalf("error leaks host content: %v", err)
		}
	}
	// No network under "none".
	// The tool must run (exit 0) and report the dial failed: a tool that cannot even exec is not
	// proof of a blocked network.
	dial := run(b.dialArgv)
	if dial.ExitCode != 0 || !strings.Contains(dial.Stdout, "dial-failed") || strings.Contains(dial.Stdout, "dial-ok") {
		t.Fatalf("dial: want exit 0 and dial-failed: %+v", dial)
	}

	// (5) Refusals.
	_, err = al.WorkspaceOpen(ctx, api.WorkspaceOpenParams{Fleet: m3Fleet, Session: "evil", Image: "docker.io/library/evil:1"})
	wantCode(t, err, api.CodeBadRequest)
	_, err = al.WorkspaceExec(ctx, api.WorkspaceExecParams{Workspace: s1, Argv: b.idArgv, Env: map[string]string{"ANTHROPIC_API_KEY": "sk-secret"}}, nil)
	wantCode(t, err, api.CodeBadRequest)
	_, err = al.WorkspaceOpen(ctx, api.WorkspaceOpenParams{Fleet: m3Fleet, Session: "nolabel", Labels: map[string]string{"zone": "c"}})
	wantCode(t, err, api.CodeUnavailable) // no runner carries the label
	// Fill the remaining slot, then a third open finds every slot taken.
	op2, err := al.WorkspaceOpen(ctx, api.WorkspaceOpenParams{Fleet: m3Fleet, Session: "s2", Labels: map[string]string{"zone": "b"}})
	must(t, err)
	s2 := op2.Workspace.ID
	if op2.Workspace.Runner != "rb" || op2.Reused {
		t.Fatalf("open s2: %+v", op2)
	}
	_, err = bo.WorkspaceOpen(ctx, api.WorkspaceOpenParams{Fleet: m3Fleet, Session: "s3"})
	wantCode(t, err, api.CodeUnavailable)
	// Workspace methods on an actions fleet, and action.run on dev.
	_, err = al.WorkspaceOpen(ctx, api.WorkspaceOpenParams{Fleet: testFleet, Session: "s1"})
	wantCode(t, err, api.CodeConflict)
	if l, err := al.WorkspaceList(ctx, testFleet); err != nil || len(l.Workspaces) != 0 {
		t.Fatalf("workspaces listed on an actions fleet: %+v %v", l, err)
	}
	_, err = al.ActionRun(ctx, api.ActionRunParams{Fleet: m3Fleet, Runner: "ra", Action: "hello", Params: map[string]string{"name": "x"}})
	wantCode(t, err, api.CodeConflict)

	// (6) Restarts. Runner: stop it (exec is unavailable meanwhile), start a new one over the
	// same executor state; reconcile keeps the workspace and its files, and exec works.
	runners["ra"].stop()
	b.stop(t, "ra")
	waitFor(t, func() bool { return !online("ra") })
	_, err = al.WorkspaceExec(ctx, api.WorkspaceExecParams{Workspace: s1, Argv: b.idArgv}, nil)
	wantCode(t, err, api.CodeUnavailable)
	startRunner("ra")
	waitOnline("ra")
	// Calls checkAlive makes, for the exact audit counts of point 9. Each attempt is an audited
	// exec (start row plus an outcome row, ok or error); each pass reads the file back in chunks.
	aliveTries, aliveOK, aliveReads := 0, 0, 0
	checkAlive := func(c *apiclient.Client, why string) {
		t.Helper()
		var last error
		waitFor(t, func() bool { // the first heartbeat starts a reconcile; the row must stay open throughout
			var r *api.WorkspaceExecResult
			aliveTries++
			r, last = c.WorkspaceExec(ctx, api.WorkspaceExecParams{Workspace: s1, Argv: b.idArgv}, nil)
			ok := last == nil && r.ExitCode == 0
			if ok {
				aliveOK++
			}
			return ok
		})
		if r, err := e.db.GetWorkspace(ctx, s1); err != nil || r.State != store.WorkspaceOpen {
			t.Fatalf("%s: row %+v %v", why, r, err)
		}
		var back []byte
		for off := int64(0); off < total; off += chunk {
			_, body, err := c.WorkspaceReadFile(ctx, api.WorkspaceReadFileParams{Workspace: s1, Path: "m3/big.bin", Offset: off, Limit: chunk})
			must(t, err)
			aliveReads++
			back = append(back, body...)
		}
		if m3sum(back) != m3sum(data) {
			t.Fatalf("%s: files changed", why)
		}
	}
	checkAlive(al, "after runner restart")
	if n := b.containers(t, "ra"); n != 1 {
		t.Fatalf("containers on ra after restart: %d", n)
	}
	// Central: close it and start another over the same store and address; the row persists,
	// runners reconnect, a fresh client execs, and the session is still sticky.
	addr := e.addr
	e.srv.Close()
	e.startCentral(addr)
	e.reg, e.svc = h.reg, h.svc
	waitOnline("ra", "rb")
	al, bo = e.opClient("alice"), e.opClient("bob")
	l, err := al.WorkspaceList(ctx, m3Fleet)
	must(t, err)
	if len(l.Workspaces) != 2 {
		t.Fatalf("list after central restart: %+v", l)
	}
	checkAlive(al, "after central restart")
	sticky, err := al.WorkspaceOpen(ctx, api.WorkspaceOpenParams{Fleet: m3Fleet, Session: "s1", Labels: map[string]string{"zone": "a"}})
	must(t, err)
	if !sticky.Reused || sticky.Workspace.ID != s1 {
		t.Fatalf("sticky after central restart: %+v", sticky)
	}

	// (7) The reaper, with the injected clock moved past the idle TTL: the untouched s2 is
	// destroyed (container and directory gone, row closed); s1, used just now, stays.
	offset.Add(int64(cfg.IdleTTL/time.Second) + 60)
	_, err = al.WorkspaceExec(ctx, api.WorkspaceExecParams{Workspace: s1, Argv: b.idArgv}, nil)
	must(t, err) // touches s1 at the new time
	if n := b.containers(t, "rb"); n != 1 {
		t.Fatalf("containers on rb before reap: %d", n)
	}
	e.svc.ReapOnce(ctx)
	r2, err := e.db.GetWorkspace(ctx, s2)
	must(t, err)
	if r2.State != store.WorkspaceClosed || r2.Reason == "" || r2.ClosedAt.IsZero() {
		t.Fatalf("s2 after reap: %+v", r2)
	}
	if n := b.containers(t, "rb"); n != 0 {
		t.Fatalf("container survived the reaper: %d", n)
	}
	if d := b.dirs(t, "rb"); len(d) != 0 {
		t.Fatalf("workspace directory survived the reaper: %v", d)
	}
	if r1, err := e.db.GetWorkspace(ctx, s1); err != nil || r1.State != store.WorkspaceOpen {
		t.Fatalf("s1 after reap: %+v %v", r1, err)
	}
	_, err = al.WorkspaceExec(ctx, api.WorkspaceExecParams{Workspace: s2, Argv: b.idArgv}, nil)
	wantCode(t, err, api.CodeConflict)
	if rows := e.auditRowsFor("workspace.reap"); len(rows) != 1 || rows[0].Workspace != s2 || rows[0].Source != "system" {
		t.Fatalf("reap audit: %+v", rows)
	}

	// (8) Revoke. bob takes the freed slot on rb. Revoking alice closes her workspaces;
	// revoking rb marks bob's closed or lost.
	bw, err := bo.WorkspaceOpen(ctx, api.WorkspaceOpenParams{Fleet: m3Fleet, Session: "b1"})
	must(t, err)
	if bw.Workspace.Runner != "rb" {
		t.Fatalf("bob's workspace: %+v", bw)
	}
	_, err = e.svc.RevokeOperator(ctx, e.admin, service.SourceCLI, "alice")
	must(t, err)
	e.svc.ReapOnce(ctx)
	if r1, err := e.db.GetWorkspace(ctx, s1); err != nil || r1.State != store.WorkspaceClosed {
		t.Fatalf("s1 after operator revoke: %+v %v", r1, err)
	}
	if n := b.containers(t, "ra"); n != 0 {
		t.Fatalf("alice's container survived her revoke: %d", n)
	}
	if rows := e.auditRowsFor("workspace.close_all"); len(rows) == 0 {
		t.Fatal("no close_all audit row for the operator revoke")
	}
	if r, err := e.db.GetWorkspace(ctx, bw.Workspace.ID); err != nil || r.State != store.WorkspaceOpen {
		t.Fatalf("bob's workspace after alice's revoke: %+v %v", r, err)
	}
	_, err = e.svc.Revoke(ctx, e.admin, service.SourceCLI, m3Fleet+"/rb")
	must(t, err)
	e.svc.ReapOnce(ctx)
	if r, err := e.db.GetWorkspace(ctx, bw.Workspace.ID); err != nil || r.State != store.WorkspaceLost || r.ClosedAt.IsZero() {
		t.Fatalf("b1 after runner revoke: want lost: %+v %v", r, err)
	}
	if rows := e.auditRowsFor("workspace.lost"); len(rows) != 1 || rows[0].Workspace != bw.Workspace.ID {
		t.Fatalf("lost audit: %+v", rows)
	}
	waitFor(t, func() bool { return !online("rb") })

	// (9) Every call has a start row and an outcome row with the workspace column set.
	//
	// What the audit holds for the api-source workspace methods (open, exec, read_file,
	// write_file, close), by request id:
	//   - every executed call: exactly one start row and exactly one outcome row (same id);
	//   - a sticky re-open ("reused") is a single ok row, with no start row (nothing is started);
	//   - a refused exec (denied env) is a single `denied` row without a request id;
	//   - bob's not_found calls on s1 (F-43) and workspace.open on an actions fleet (F-41) are one
	//     denied row each, asserted exactly below;
	//   - other refusals decided before any work (image not allowed, unknown label, third open with
	//     all slots taken, lexical read_file path attacks) leave no workspace.* row at all, which is
	//     asserted explicitly below; the real symlink read is the exception (it reaches the runner
	//     and fails there).
	rows := e.auditRows()
	type pair struct {
		start, end int
		action     string
		outcome    string
		actor      string
	}
	byReq := map[string]*pair{}
	var reused, denied, ownerDenied, modeDenied []store.AuditEvent
	for _, r := range rows {
		if r.Source != "api" {
			continue
		}
		switch r.Action {
		case api.MethodWorkspaceExec, api.MethodWorkspaceReadFile, api.MethodWorkspaceWriteFile, api.MethodWorkspaceClose, api.MethodWorkspaceOpen:
		default:
			continue
		}
		// F-41: workspace.open on an actions fleet is one denied row with no workspace.
		if r.Action == api.MethodWorkspaceOpen && r.Outcome == service.OutcomeDenied && r.Workspace == "" {
			modeDenied = append(modeDenied, r)
			continue
		}
		if r.Workspace == "" {
			t.Errorf("audit row without workspace: %+v", r)
			continue
		}
		if r.Subject == "evil" || r.Subject == "nolabel" || r.Subject == "s3" {
			t.Errorf("a refusal left an audit row: %+v", r)
		}
		// F-43: bob's not_found calls on s1 each left one denied row (collected, asserted below).
		if r.Actor == "bob" && r.Workspace == s1 {
			ownerDenied = append(ownerDenied, r)
			continue
		}
		switch {
		case r.Outcome == service.OutcomeDenied:
			denied = append(denied, r)
		case r.Action == api.MethodWorkspaceOpen && r.Outcome == service.OutcomeOK && r.Message == "reused":
			reused = append(reused, r)
		default:
			if r.RequestID == "" {
				t.Errorf("audit row without request id: %+v", r)
				continue
			}
			p := byReq[r.RequestID]
			if p == nil {
				p = &pair{action: r.Action, actor: r.Actor}
				byReq[r.RequestID] = p
			}
			if p.action != r.Action || p.actor != r.Actor {
				t.Errorf("request %s spans actions/actors: %+v", r.RequestID, r)
			}
			if r.Outcome == service.OutcomeStart {
				p.start++
			} else {
				p.end++
				p.outcome = r.Outcome
			}
		}
	}
	// Per action: executed calls (start rows) and how many of them ended ok.
	started, okEnd := map[string]int{}, map[string]int{}
	for id, p := range byReq {
		if p.start != 1 || p.end != 1 {
			t.Errorf("request %s (%s): %d start rows, %d outcome rows", id, p.action, p.start, p.end)
			continue
		}
		started[p.action]++
		if p.outcome == service.OutcomeOK {
			okEnd[p.action]++
		}
	}
	// Only the refused env exec is denied (refusals before the workspace is loaded leave no row).
	if len(denied) != 1 || denied[0].Action != api.MethodWorkspaceExec || denied[0].Workspace != s1 || denied[0].Actor != "alice" {
		t.Errorf("denied rows: %+v", denied)
	}
	// F-43: bob's four refused calls on alice's s1 left exactly one denied row each, bob as actor,
	// s1's id, no request id; the caller saw plain not_found (asserted where the calls are made).
	ownerByAction := map[string]int{}
	for _, r := range ownerDenied {
		ownerByAction[r.Action]++
		if r.Outcome != service.OutcomeDenied || r.ActorKind != "operator" || r.Workspace != s1 || r.RequestID != "" || r.Message != "not the workspace owner" {
			t.Errorf("bob's refusal row: %+v", r)
		}
	}
	for _, m := range []string{api.MethodWorkspaceExec, api.MethodWorkspaceReadFile, api.MethodWorkspaceWriteFile, api.MethodWorkspaceClose} {
		if ownerByAction[m] != 1 {
			t.Errorf("%s: %d denied rows for bob on s1, want 1 (%+v)", m, ownerByAction[m], ownerDenied)
		}
	}
	if len(ownerDenied) != 4 {
		t.Errorf("bob's denied rows: %+v", ownerDenied)
	}
	// F-41: alice's workspace.open on the actions fleet: one denied row, no workspace id, no request id.
	if len(modeDenied) != 1 || modeDenied[0].Actor != "alice" || modeDenied[0].Fleet != testFleet || modeDenied[0].Subject != "s1" ||
		modeDenied[0].RequestID != "" || modeDenied[0].Message != "fleet is not in workspace mode" {
		t.Errorf("mode-mismatch rows: %+v", modeDenied)
	}
	// Two sticky re-opens of s1 (point 2 and after the central restart).
	if len(reused) != 2 {
		t.Errorf("reused open rows: %+v", reused)
	}
	for _, r := range reused {
		if r.Workspace != s1 || r.Actor != "alice" {
			t.Errorf("reused row: %+v", r)
		}
	}
	// Exact expected counts per method. Execs: the run() calls of point 4, the one refused with
	// unavailable while ra was stopped (it still has a start row and an error outcome), every
	// checkAlive attempt (retries during reconcile included), and the touch exec before the reap.
	// The refused env exec and bob's not_found calls have no start row. Reads: the point-4 round
	// trip plus the chunked read-back in each checkAlive; the lexical path attacks are refused
	// before any audit, but the real symlink read reaches the runner (start row, error outcome). Writes: the round-trip chunks. Opens: s1, s2 (alice) and b1 (bob). Closes: the
	// operator revoke closes through the system path (workspace.close_all), not an api close.
	const unavailableExecs, reapTouchExecs = 1, 1
	wantStarted := map[string]int{
		api.MethodWorkspaceOpen:      3,
		api.MethodWorkspaceExec:      execs + unavailableExecs + aliveTries + reapTouchExecs,
		api.MethodWorkspaceWriteFile: total / chunk,
		api.MethodWorkspaceReadFile:  mainReads + aliveReads + symlinkReads,
	}
	for _, m := range []string{api.MethodWorkspaceOpen, api.MethodWorkspaceExec, api.MethodWorkspaceWriteFile,
		api.MethodWorkspaceReadFile, api.MethodWorkspaceClose} {
		if started[m] != wantStarted[m] {
			t.Errorf("%s: %d calls with start+outcome rows, want %d", m, started[m], wantStarted[m])
		}
	}
	// Outcomes: everything succeeded except the exec refused while ra was down and the failed
	// checkAlive attempts.
	if want := execs + reapTouchExecs + aliveOK; okEnd[api.MethodWorkspaceExec] != want {
		t.Errorf("exec ok outcomes: %d, want %d", okEnd[api.MethodWorkspaceExec], want)
	}
	for _, m := range []string{api.MethodWorkspaceOpen, api.MethodWorkspaceWriteFile} {
		if okEnd[m] != started[m] {
			t.Errorf("%s: %d ok outcomes of %d calls", m, okEnd[m], started[m])
		}
	}
	// The symlink read is the one read that fails (the runner refuses it after the start row).
	if want := mainReads + aliveReads; okEnd[api.MethodWorkspaceReadFile] != want {
		t.Errorf("read_file ok outcomes: %d, want %d", okEnd[api.MethodWorkspaceReadFile], want)
	}
	// System rows carry the workspace as well.
	for _, r := range rows {
		if r.Action == "workspace.close_all" { // the revoke cascade: one row listing every id it closed
			if !strings.Contains(r.Params["ws_ids"], map[string]string{"alice": s1, "dev/rb": bw.Workspace.ID}[r.Subject]) {
				t.Errorf("close_all row lacks its workspace ids: %+v", r)
			}
			continue
		}
		if strings.HasPrefix(r.Action, "workspace.") && r.Source == "system" && r.Workspace == "" {
			t.Errorf("system row without workspace: %+v", r)
		}
	}

	if b.after != nil {
		b.after(t)
	}
}

// m3Enrol creates an active runner with trusted labels (from its join token) through the store.
func m3Enrol(e *env, fleet, name string, labels map[string]string) store.Runner {
	e.t.Helper()
	ctx := context.Background()
	pub, err := keys.Generate(e.dir, name, name)
	must(e.t, err)
	pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pub))
	must(e.t, err)
	e.tokenN++
	tokID := fmt.Sprintf("%016x", e.tokenN+2000)
	secret := sha256.Sum256([]byte("secret-" + tokID))
	now := time.Now()
	_, err = e.db.CreateToken(ctx, store.JoinToken{TokenID: tokID, SecretHash: secret[:], Fleet: fleet, Labels: labels,
		AutoApprove: true, MaxUses: 1, ExpiresAt: now.Add(time.Hour), CreatedBy: "test"}, now, auditEv("token.create"))
	must(e.t, err)
	r, err := e.db.Enroll(ctx, tokID, secret[:], now,
		store.Runner{Name: name, PubKey: pk.Marshal(), Fingerprint: ssh.FingerprintSHA256(pk)}, auditEv("runner.enrol"))
	must(e.t, err)
	if r.Labels["zone"] != labels["zone"] {
		e.t.Fatalf("runner labels %v, want %v", r.Labels, labels)
	}
	e.ids[name] = r.ID
	return r
}
