package central

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/actions"
	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/apiclient"
	"github.com/mirastacklabs-ai/mirafleet/internal/keys"
	"github.com/mirastacklabs-ai/mirafleet/internal/opconf"
	"github.com/mirastacklabs-ai/mirafleet/internal/orch"
	"github.com/mirastacklabs-ai/mirafleet/internal/runner"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/tokens"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/wsconfig"
)

// TestM5Done is the "M5 done when" scenario of docs/M5.md section 6, in process: a real central
// (SQLite, registry, service, SSH on an ephemeral port, reaper on an injected clock), real
// runners, and MCP servers (mcpserver.Serve over io.Pipe, one orch each, one config directory
// per operator) driven by the go-sdk client. Only the workspace executor is the fake one.
// Differences from the text of section 6 are listed in the report of the change that added it;
// the subtests follow the numbered steps.

const m5Actions = `
actions:
  tail-logs:
    run: ["/bin/echo", "tail", "{{lines}}"]
    params: { lines: { pattern: "^[0-9]{1,4}$", default: "200" } }
  noisy:
    run: ["/bin/sh", "-c", "i=0; while [ $i -lt 4500 ]; do printf '\\033[31mnoisy line %05d padding padding padding\\033[0m\\n' $i; i=$((i+1)); done"]
`

// m5Fixture holds what the steps share.
type m5Fixture struct {
	e, cloud       *env
	rec            *m5Rec
	sink           *m5Sink
	execs          map[string]*m3Fake
	started        chan struct{} // a blocking exec began
	cancelled      chan struct{} // a blocking exec saw its context cancelled
	offset         *atomic.Int64
	idleTTL        time.Duration
	aliceDir       string
	bobDir         string
	projDir        string
	deadAddr       string
	decoys         []string
	alice, bob     *m5Server
	aliceWS        string // alice's implicit workspace on dev
	aliceRunner    string
	pubFingerprint []string
	// ownershipRefused counts the calls central refused for ownership (step 7), by method.
	ownershipRefused map[string]int
}

func (f *m5Fixture) cfg(dir string) orch.Config {
	return orch.Config{ConfigDir: dir, StateDir: filepath.Join(filepath.Dir(f.projDir), "state-"+filepath.Base(dir)),
		ProjectDir: f.projDir, Home: filepath.Dir(f.projDir)}
}

func randHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	_, err := rand.Read(b)
	must(t, err)
	return hex.EncodeToString(b)
}

func TestM5Done(t *testing.T) {
	ctx := m3ctx(t)
	f := &m5Fixture{rec: &m5Rec{}, ownershipRefused: map[string]int{}, sink: &m5Sink{}, idleTTL: time.Hour, offset: new(atomic.Int64),
		execs: map[string]*m3Fake{"build-1": newM3Fake(), "build-2": newM3Fake()}}

	// ---- 1. Set-up ----
	// Only decoys live in the environment of the servers.
	f.decoys = []string{"sk-ant-m5decoy-" + randHex(t, 12), "m5decoy-aws-" + randHex(t, 12)}
	t.Setenv("ANTHROPIC_API_KEY", f.decoys[0])
	t.Setenv("AWS_SECRET_ACCESS_KEY", f.decoys[1])

	// A clock fixed inside a registration step (operator codes are valid for 30 s), moved by offset.
	base := time.Unix(tokens.Step(time.Now())*tokens.StepSeconds+10, 0)
	clock := func() time.Time { return base.Add(time.Duration(f.offset.Load()) * time.Second) }

	e := newM5Env(t, clock)
	f.e = e
	cloud := newM5Env(t, clock)
	f.cloud = cloud
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("central log:\n%s", e.logText())
		}
	})

	cfg := store.WorkspaceConfig{Executor: store.ExecutorContainerd, DefaultImage: m3Image, Images: []string{"mirafleet-test/init:*"},
		Pull: store.PullNever, Network: store.NetworkNone, CPUMilli: 1000, MemoryMB: 256, Pids: 128, TmpfsMB: 16, DiskMB: 128,
		IdleTTL: f.idleTTL, MaxTTL: 6 * time.Hour, ExecDefault: 30 * time.Second, ExecMax: 2 * time.Minute}
	_, err := e.svc.CreateFleet(ctx, e.admin, service.SourceCLI, "dev", store.ModeWorkspace, false, &cfg)
	must(t, err)
	_, err = e.svc.CreateFleet(ctx, e.admin, service.SourceCLI, "prod", store.ModeActions, true, nil)
	must(t, err)
	for _, fl := range []struct {
		name    string
		confirm bool
	}{{"dev", false}, {"prod", true}} {
		_, err = cloud.svc.CreateFleet(ctx, cloud.admin, service.SourceCLI, fl.name, store.ModeActions, fl.confirm, nil)
		must(t, err)
	}
	m3Enrol(e, "dev", "build-1", nil)
	m3Enrol(e, "dev", "build-2", nil)
	m3Enrol(e, "prod", "db-1", nil)

	// The fake executors: a command "m5-block" waits for its context; "echo hi" prints hi.
	f.started, f.cancelled = make(chan struct{}, 8), make(chan struct{}, 8)
	for _, fk := range f.execs {
		prev := fk.ExecFunc
		fk.ExecFunc = func(ctx context.Context, id workspace.ID, r workspace.ExecRequest) (workspace.ExecResult, error) {
			if len(r.Argv) == 3 && r.Argv[2] == "m5-block" {
				fk.mu.Lock()
				fk.execs = append(fk.execs, workspace.ExecRequest{Argv: append([]string(nil), r.Argv...), Cwd: r.Cwd, Env: r.Env})
				fk.mu.Unlock()
				f.started <- struct{}{}
				<-ctx.Done()
				f.cancelled <- struct{}{}
				return workspace.ExecResult{}, ctx.Err()
			}
			res, err := prev(ctx, id, r)
			if err == nil && len(r.Argv) == 3 && r.Argv[2] == "echo hi" && r.Stdout != nil {
				r.Stdout.Write([]byte("hi\n"))
			}
			if err == nil && len(r.Argv) == 3 && r.Argv[2] == "m5-noisy" && r.Stdout != nil {
				line := []byte(strings.Repeat("n", 99) + "\n") // F-42: 400 KiB of output
				for i := 0; i < 4096; i++ {
					r.Stdout.Write(line)
				}
			}
			return res, err
		}
	}
	wc, err := wsconfig.Parse([]byte(m3FakeYAML))
	must(t, err)
	for _, name := range []string{"build-1", "build-2"} {
		e.startRunner(name, func(c *runner.Config) { c.Workspace, c.WorkspaceConfig, c.Actions = f.execs[name], wc, nil })
	}
	acts, err := actions.Parse([]byte(m5Actions))
	must(t, err)
	e.startRunner("db-1", func(c *runner.Config) { c.Actions = acts })
	waitFor(t, func() bool {
		up := map[string]bool{}
		for _, si := range e.reg.Snapshot() {
			up[si.Name] = si.Online && (si.Name == "db-1" || (si.Slots == 1 && si.Executor == "containerd"))
		}
		return up["build-1"] && up["build-2"] && up["db-1"]
	})

	// Operators in separate config directories, registered through apiclient.Register with the
	// current code of each central; the host keys are pinned from the code.
	must(t, e.svc.InitRegistrationSecret(ctx, e.admin, service.SourceCLI))
	must(t, cloud.svc.InitRegistrationSecret(ctx, cloud.admin, service.SourceCLI))
	register := func(dir, name string, targets ...*env) {
		t.Helper()
		_, err := keys.Generate(dir, opconf.KeyName, name)
		must(t, err)
		signer, err := keys.LoadSigner(opconf.KeyPath(dir))
		must(t, err)
		f.pubFingerprint = append(f.pubFingerprint, ssh.FingerprintSHA256(signer.PublicKey()))
		var lines []string
		for _, te := range targets {
			waitCodeWindow()
			code, err := te.svc.OperatorCode(ctx, te.admin, service.SourceCLI)
			must(t, err)
			rr, hk, err := apiclient.Register(ctx, te.addr, code.Code, name, signer)
			must(t, err)
			if rr.Operator != name || rr.Status != "active" {
				t.Fatalf("register %s: %+v", name, rr)
			}
			lines = append(lines, apiclient.KnownHostsLine(te.addr, hk))
			f.pubFingerprint = append(f.pubFingerprint, ssh.FingerprintSHA256(hk))
		}
		must(t, os.WriteFile(opconf.KnownHostsPath(dir), []byte(strings.Join(lines, "\n")+"\n"), 0o600))
	}
	f.aliceDir, f.bobDir = filepath.Join(t.TempDir(), "alice-cfg"), filepath.Join(t.TempDir(), "bob-cfg")
	register(f.aliceDir, "alice", e, cloud)
	register(f.bobDir, "bob", e)

	// The dead third context: a listener that accepts and closes at once (the SSH handshake fails),
	// so no other process can come to own the address during the test.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	f.deadAddr = l.Addr().String()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	t.Cleanup(func() { l.Close() })
	must(t, opconf.SaveContexts(f.aliceDir, &opconf.Contexts{Current: "work", Contexts: []opconf.Context{
		{Name: "work", Addr: e.addr}, {Name: "cloud", Addr: cloud.addr}, {Name: "dead", Addr: f.deadAddr}}}))
	must(t, opconf.SaveContexts(f.bobDir, &opconf.Contexts{Current: "work", Contexts: []opconf.Context{{Name: "work", Addr: e.addr}}}))

	// The project directory: fleet dev by default.
	f.projDir = filepath.Join(t.TempDir(), "proj")
	must(t, os.MkdirAll(f.projDir, 0o755))
	must(t, os.WriteFile(filepath.Join(f.projDir, opconf.ProjectName), []byte("fleet: dev\n"), 0o644))
	must(t, os.MkdirAll(filepath.Join(filepath.Dir(f.projDir), "state-"+filepath.Base(f.aliceDir)), 0o700))

	f.alice = m5Start(t, f.cfg(f.aliceDir), f.rec, f.sink)
	f.bob = m5Start(t, f.cfg(f.bobDir), f.rec, f.sink)

	// ---- 2 .. 12 ----
	steps := []struct {
		name string
		fn   func(*testing.T, *m5Fixture)
	}{
		{"02_tools", m5Tools},
		{"03_lists", m5Lists},
		{"04_guarded_action", m5Guarded},
		{"05_implicit_open_exec", m5ImplicitOpen},
		{"06_files", m5Files},
		{"07_ownership", m5Ownership},
		{"08_mode_mismatch", m5ModeMismatch},
		{"09_cancel", m5Cancel},
		{"10_idle_on_exit", m5IdleOnExit},
		{"11_reattach", m5Reattach},
		{"12_hygiene", m5Hygiene},
	}
	for _, s := range steps {
		if !t.Run(s.name, func(t *testing.T) { s.fn(t, f) }) {
			t.Logf("step %s failed; later steps depend on its state and are skipped", s.name)
			break
		}
	}
}

// ---- 2. Tools ----

func m5Tools(t *testing.T, f *m5Fixture) {
	al := f.alice.at(t)
	ir := al.cs.InitializeResult()
	if ir.ProtocolVersion == "" || ir.ServerInfo.Name != "mirafleet" || ir.Capabilities.Tools == nil ||
		ir.Capabilities.Logging != nil || ir.Capabilities.Prompts != nil || ir.Capabilities.Resources != nil {
		t.Fatalf("initialize: %+v", ir)
	}
	// Negotiation as the SDK answers (the Facts section): 2026-07-28 is not served by initialize.
	for ask, want := range map[string]string{"2025-06-18": "2025-06-18", "2026-07-28": "2025-11-25"} {
		if got := m5RawInitialize(t, f.cfg(f.aliceDir), f.rec, ask); got != want {
			t.Errorf("initialize asking %s: got %s, want %s", ask, got, want)
		}
	}
	res, err := al.cs.ListTools(m5ctx(t), nil)
	must(t, err)
	type ann struct{ ro, destructive, idem, open bool }
	want := map[string]ann{
		"list_fleets":     {true, false, true, true},
		"list_runners":    {true, false, true, true},
		"run_action":      {false, true, false, true},
		"workspace_list":  {true, false, true, true},
		"workspace_open":  {false, false, true, true},
		"exec":            {false, true, false, true},
		"read_file":       {true, false, true, true},
		"write_file":      {false, true, false, true},
		"workspace_close": {false, true, true, true},
		// M7 (docs/M7.md §3.5, §3.7): Claude Code tool parity. Annotations as in mcpserver.
		"read":          {true, false, true, true},
		"write":         {false, true, false, true},
		"edit":          {false, true, false, true},
		"multi_edit":    {false, true, false, true},
		"grep":          {true, false, true, true},
		"glob":          {true, false, true, true},
		"ls":            {true, false, true, true},
		"notebook_edit": {false, true, false, true},
		"upload":        {false, true, true, true},
		"download":      {false, true, true, true},
		"bash":          {false, true, false, true},
		"bash_output":   {false, false, false, true},
		"kill_shell":    {false, true, true, true},
		"job_list":      {true, false, true, true},
		"read_output":   {true, false, true, true},
	}
	if len(res.Tools) != len(want) {
		var names []string
		for _, x := range res.Tools {
			names = append(names, x.Name)
		}
		t.Fatalf("%d tools, want %d: %v", len(res.Tools), len(want), names)
	}
	for _, tl := range res.Tools {
		w, ok := want[tl.Name]
		if !ok {
			t.Errorf("unexpected tool %q", tl.Name)
			continue
		}
		a := tl.Annotations
		if tl.InputSchema == nil || tl.OutputSchema == nil || tl.Description == "" || a == nil ||
			a.DestructiveHint == nil || a.OpenWorldHint == nil {
			t.Errorf("%s: schemas or annotations missing: %+v", tl.Name, tl)
			continue
		}
		if got := (ann{a.ReadOnlyHint, *a.DestructiveHint, a.IdempotentHint, *a.OpenWorldHint}); got != w {
			t.Errorf("%s: annotations %+v, want %+v", tl.Name, got, w)
		}
	}
	// fleet_create is not a tool.
	r, err := al.callCtx(m5ctx(t), "fleet_create", map[string]any{"name": "x"})
	if err == nil && !r.IsErr {
		t.Fatalf("fleet_create was served: %+v", r)
	}
	// Strict arguments: an unknown field is refused.
	if r := al.call("list_fleets", map[string]any{"bogus": 1}); !r.IsErr || r.code() != api.CodeBadRequest {
		t.Fatalf("unknown argument: %+v", r)
	}
}

// ---- 3. Lists ----

func m5Lists(t *testing.T, f *m5Fixture) {
	al := f.alice.at(t)
	r := al.call("list_fleets", nil)
	if r.IsErr {
		t.Fatalf("list_fleets: %s", r.Text)
	}
	type key struct{ ctx, name string }
	got := map[key]map[string]any{}
	for _, x := range r.get("fleets").([]any) {
		m := x.(map[string]any)
		got[key{m["context"].(string), m["name"].(string)}] = m
	}
	for _, c := range []string{"work", "cloud"} {
		dev, prod := got[key{c, "dev"}], got[key{c, "prod"}]
		if dev == nil || prod == nil {
			t.Fatalf("context %s lacks dev or prod: %s", c, r.Text)
		}
		if dev["default"] != true || dev["guarded"] != false || prod["guarded"] != true || prod["default"] == true {
			t.Errorf("context %s: dev %+v prod %+v", c, dev, prod)
		}
	}
	if got[key{"work", "dev"}]["mode"] != "workspace" || got[key{"work", "prod"}]["mode"] != "actions" {
		t.Errorf("modes: %+v %+v", got[key{"work", "dev"}], got[key{"work", "prod"}])
	}
	errs, _ := r.get("errors").([]any)
	if len(errs) != 1 {
		t.Fatalf("errors: %s", r.Text)
	}
	if em := errs[0].(map[string]any); em["context"] != "dead" || em["code"] == "" || em["message"] == "" {
		t.Errorf("dead context entry: %+v", em)
	}
	if r.boolean("truncated") {
		t.Error("list_fleets truncated")
	}

	// list_runners prod: db-1 with tail-logs (and its pattern) and noisy.
	r = al.call("list_runners", map[string]any{"fleet": "prod"})
	if r.IsErr {
		t.Fatalf("list_runners prod: %s", r.Text)
	}
	rs := r.get("runners").([]any)
	if len(rs) != 1 {
		t.Fatalf("prod runners: %s", r.Text)
	}
	db := rs[0].(map[string]any)
	if db["name"] != "db-1" || db["online"] != true {
		t.Fatalf("db-1: %+v", db)
	}
	seen := map[string]string{}
	for _, a := range db["actions"].([]any) {
		am := a.(map[string]any)
		pat := ""
		if ps, _ := am["params"].([]any); len(ps) == 1 {
			pm := ps[0].(map[string]any)
			pat = pm["pattern"].(string)
			if am["name"] == "tail-logs" && pm["default"] != "200" {
				t.Errorf("tail-logs default: %+v", pm)
			}
		}
		seen[am["name"].(string)] = pat
	}
	if p, ok := seen["tail-logs"]; !ok || !strings.Contains(p, "[0-9]{1,4}") {
		t.Errorf("tail-logs pattern: %v", seen)
	}
	if _, ok := seen["noisy"]; !ok {
		t.Errorf("noisy missing: %v", seen)
	}
	if r.str("where", "context") != "work" || r.str("where", "context_source") != "current context" || r.str("where", "fleet_source") != "argument" {
		t.Errorf("where: %+v", r.get("where"))
	}

	// list_runners dev (the project file's fleet): free_slots.
	r = al.call("list_runners", nil)
	if r.IsErr {
		t.Fatalf("list_runners dev: %s", r.Text)
	}
	if r.str("where", "fleet") != "dev" || r.str("where", "fleet_source") != "project file" {
		t.Errorf("where: %+v", r.get("where"))
	}
	rs = r.get("runners").([]any)
	if len(rs) != 2 {
		t.Fatalf("dev runners: %s", r.Text)
	}
	for _, x := range rs {
		m := x.(map[string]any)
		if m["free_slots"] != float64(1) || m["slots"] != float64(1) || m["online"] != true || m["executor"] != "containerd" {
			t.Errorf("runner %+v", m)
		}
	}
}

// ---- 4. Guarded action ----

func m5Guarded(t *testing.T, f *m5Fixture) {
	al := f.alice.at(t)
	args := map[string]any{"fleet": "prod", "runner": "db-1", "action": "tail-logs"}
	before := len(f.e.auditRowsFor(api.MethodActionRun))
	r := al.call("run_action", args)
	if !r.IsErr || r.code() != api.CodeBadRequest || !strings.Contains(r.Text, "Ask the user") ||
		!strings.Contains(r.Text, "context `work`") || !strings.Contains(r.Text, "fleet `prod`") {
		t.Fatalf("guarded without confirm: %s", r.Text)
	}
	rows := f.e.auditRowsFor(api.MethodActionRun)[before:]
	if len(rows) != 1 || rows[0].Outcome != service.OutcomeDenied || rows[0].Actor != "alice" || rows[0].Fleet != "prod" || rows[0].Runner != "db-1" {
		t.Fatalf("denied audit: %+v", rows)
	}

	args["confirm"] = "prod"
	r = al.call("run_action", args)
	if r.IsErr || r.num("exit_code") != 0 || strings.TrimSpace(r.str("stdout")) != "tail 200" {
		t.Fatalf("confirmed run: %s", r.Text)
	}
	// Confirm pass-through: the unconfirmed call reached central without a confirm, the second with "prod".
	var confirms []string
	for _, q := range f.rec.snapshot() {
		if q.method != api.MethodActionRun || !strings.Contains(q.params, `"tail-logs"`) {
			continue
		}
		var p api.ActionRunParams
		must(t, json.Unmarshal([]byte(q.params), &p))
		confirms = append(confirms, p.Confirm)
	}
	if len(confirms) != 2 || confirms[0] != "" || confirms[1] != "prod" {
		t.Fatalf("confirm values of the recorded tail-logs action.run calls: %q, want [\"\" \"prod\"]", confirms)
	}
	if r.str("where", "fleet") != "prod" || r.str("where", "context") != "work" {
		t.Errorf("where: %+v", r.get("where"))
	}

	args["action"] = "noisy"
	r = al.call("run_action", args)
	if r.IsErr {
		t.Fatalf("noisy: %s", r.Text)
	}
	out := r.str("stdout")
	if len(out)+len(r.str("stderr")) > opconf.DefaultOutputCap || !r.boolean("truncated") || r.num("omitted_bytes") <= 0 ||
		!strings.Contains(out, "bytes omitted]") || strings.ContainsRune(out, 0x1b) || strings.Contains(r.Text, `\u001b`) {
		t.Fatalf("noisy: len %d truncated %v omitted %v esc %v", len(out), r.get("truncated"), r.get("omitted_bytes"), strings.ContainsRune(out, 0x1b))
	}
	if !strings.Contains(out, "noisy line 00000") {
		t.Errorf("noisy output lost its head: %.80q", out)
	}
}

// ---- 5. Implicit open + exec ----

func m5ImplicitOpen(t *testing.T, f *m5Fixture) {
	al := f.alice.at(t)
	r := al.call("exec", map[string]any{"command": "echo hi"})
	if r.IsErr || r.num("exit_code") != 0 || !strings.Contains(r.str("stdout"), "hi") {
		t.Fatalf("exec: %s", r.Text)
	}
	f.aliceWS = r.str("workspace")
	if f.aliceWS == "" || r.str("where", "fleet") != "dev" || r.str("where", "fleet_source") != "project file" || r.str("where", "context") != "work" {
		t.Fatalf("exec where/workspace: %s", r.Text)
	}
	l := al.call("workspace_list", nil)
	ws := l.get("workspaces").([]any)
	if len(ws) != 1 {
		t.Fatalf("workspace_list: %s", l.Text)
	}
	w := ws[0].(map[string]any)
	if w["id"] != f.aliceWS || w["state"] != "open" || !strings.HasPrefix(w["session"].(string), "p:") || w["fleet"] != "dev" {
		t.Fatalf("workspace: %+v", w)
	}
	f.aliceRunner = w["runner"].(string)
	if f.execs[f.aliceRunner] == nil {
		t.Fatalf("workspace on runner %q", f.aliceRunner)
	}

	// The fake received sh -c "echo hi"; nothing of the server's environment reached it.
	fk := f.execs[f.aliceRunner]
	fk.mu.Lock()
	found := false
	for _, x := range fk.execs {
		if len(x.Argv) == 3 && x.Argv[0] == "/bin/sh" && x.Argv[1] == "-c" && x.Argv[2] == "echo hi" {
			found = true
		}
	}
	fk.mu.Unlock()
	if !found {
		t.Fatalf("fake executor did not receive [/bin/sh -c echo hi]: %+v", fk.execs)
	}
	f.assertNoDecoyAtExecutors(t)

	// F-42: a noisy exec is capped: truncated is set and the result is within the output cap.
	r = al.call("exec", map[string]any{"command": "m5-noisy"})
	if r.IsErr || !r.boolean("truncated") || r.num("omitted_bytes") <= 0 ||
		len(r.str("stdout"))+len(r.str("stderr")) > opconf.DefaultOutputCap {
		t.Fatalf("noisy exec: err %v truncated %v omitted %v stdout %d stderr %d (cap %d)", r.IsErr, r.get("truncated"),
			r.get("omitted_bytes"), len(r.str("stdout")), len(r.str("stderr")), opconf.DefaultOutputCap)
	}

	// A denied variable is refused, with the server's value never involved.
	r = al.call("exec", map[string]any{"command": "echo hi", "env": map[string]any{"ANTHROPIC_API_KEY": "x"}})
	if !r.IsErr || r.code() != api.CodeBadRequest {
		t.Fatalf("exec with ANTHROPIC_API_KEY: %s", r.Text)
	}

	// No implicit open on a guarded fleet: no confirm and no workspace -> refused before any open.
	// (prod is a guarded actions fleet; the guard check precedes the open, so no mode conflict comes first.)
	opens := f.rec.count(api.MethodWorkspaceOpen)
	r = al.call("exec", map[string]any{"fleet": "prod", "command": "echo hi"})
	// guardedErr spells it "ask the user" inside the sentence; match case-insensitively.
	if !r.IsErr || r.code() != api.CodeBadRequest || !strings.Contains(strings.ToLower(r.Text), "ask the user") ||
		!strings.Contains(r.Text, "context `work`") || !strings.Contains(r.Text, "fleet `prod`") {
		t.Fatalf("exec on the guarded fleet: %s", r.Text)
	}
	if n := f.rec.count(api.MethodWorkspaceOpen); n != opens {
		t.Fatalf("a guarded implicit open sent %d workspace.open calls", n-opens)
	}
}

func (f *m5Fixture) assertNoDecoyAtExecutors(t *testing.T) {
	t.Helper()
	for name, fk := range f.execs {
		fk.mu.Lock()
		dump := fmt.Sprintf("%+v %+v", fk.specs, fk.execs)
		for _, x := range fk.execs {
			for k := range x.Env {
				if k == "ANTHROPIC_API_KEY" || k == "AWS_SECRET_ACCESS_KEY" {
					t.Errorf("%s: denied variable %s reached the executor", name, k)
				}
			}
		}
		fk.mu.Unlock()
		for _, d := range f.decoys {
			if strings.Contains(dump, d) {
				t.Errorf("%s: decoy %q reached the executor", name, d)
			}
		}
	}
}

// ---- 6. Files ----

func m5Files(t *testing.T, f *m5Fixture) {
	al := f.alice.at(t)
	const total = 3 << 19 // 1.5 MiB
	data := make([]byte, total)
	_, err := rand.Read(data)
	must(t, err)
	writes := f.rec.count(api.MethodWorkspaceWriteFile)
	r := al.call("write_file", map[string]any{"path": "m5/big.bin", "encoding": "base64", "content": base64.StdEncoding.EncodeToString(data)})
	if r.IsErr || r.num("bytes_written") != total || r.num("size") != total || r.str("sha256") != m3sum(data) {
		t.Fatalf("write_file: %s", r.Text)
	}
	if n := f.rec.count(api.MethodWorkspaceWriteFile) - writes; n != 2 {
		t.Fatalf("1.5 MiB write took %d central calls, want 2 chunks", n)
	}

	// Paged read matches the SHA-256 of the whole.
	var got []byte
	offset, pages := float64(0), 0
	for {
		r = al.call("read_file", map[string]any{"path": "m5/big.bin", "encoding": "base64", "offset": offset, "limit": 512 << 10})
		if r.IsErr {
			t.Fatalf("read_file: %s", r.Text)
		}
		page, err := base64.StdEncoding.DecodeString(r.str("content"))
		must(t, err)
		if r.str("sha256") != m3sum(page) || int(r.num("length")) != len(page) || r.num("size") != total {
			t.Fatalf("page at %v: %s", offset, r.Text[:min(len(r.Text), 300)])
		}
		got = append(got, page...)
		pages++
		if r.boolean("eof") {
			break
		}
		next := r.num("next_offset")
		if next <= offset {
			t.Fatalf("no progress at %v", offset)
		}
		offset = next
	}
	if len(got) != total || m3sum(got) != m3sum(data) || pages != 3 {
		t.Fatalf("paged read: %d bytes in %d pages, sha %s want %s", len(got), pages, m3sum(got), m3sum(data))
	}

	// The default read is capped at the output cap and says where to continue.
	r = al.call("read_file", map[string]any{"path": "m5/big.bin", "encoding": "base64"})
	if r.IsErr || int(r.num("length")) > opconf.DefaultOutputCap || r.boolean("eof") || r.num("next_offset") != r.num("length") {
		t.Fatalf("default read: length %v eof %v next %v", r.get("length"), r.get("eof"), r.get("next_offset"))
	}
	// A small text file round-trips as text.
	if r := al.call("write_file", map[string]any{"path": "m5/note.txt", "content": "hello m5\n"}); r.IsErr {
		t.Fatalf("write note: %s", r.Text)
	}
	if r := al.call("read_file", map[string]any{"path": "m5/note.txt"}); r.IsErr || r.str("content") != "hello m5\n" || !r.boolean("eof") {
		t.Fatalf("read note: %s", r.Text)
	}
}

// ---- 7. Ownership ----

func m5Ownership(t *testing.T, f *m5Fixture) {
	bo := f.bob.at(t)
	ctx := m5ctx(t)
	// Through the tools: bob's own list lacks the id, so the tool layer answers not_found locally.
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"exec", map[string]any{"workspace": f.aliceWS, "command": "echo hi"}},
		{"read_file", map[string]any{"workspace": f.aliceWS, "path": "m5/note.txt"}},
		{"workspace_close", map[string]any{"workspace": f.aliceWS}},
	} {
		r := bo.call(c.tool, c.args)
		if !r.IsErr || r.code() != api.CodeNotFound {
			t.Errorf("bob %s on alice's workspace: %s", c.tool, r.Text)
		}
	}
	if l := bo.call("workspace_list", nil); l.IsErr || len(l.get("workspaces").([]any)) != 0 {
		t.Fatalf("bob sees %s", l.Text)
	}
	before, err := f.e.db.GetWorkspace(ctx, f.aliceWS)
	must(t, err)

	// Straight at central through bob's orch (the tool layer is bypassed): central itself must refuse,
	// and each request must provably have reached it (the recorder counts it).
	probes := []struct {
		method string
		call   func() error
	}{
		{api.MethodWorkspaceExec, func() error {
			_, err := f.bob.o.WorkspaceExec(ctx, "work", api.WorkspaceExecParams{Workspace: f.aliceWS, Argv: []string{"/bin/sh", "-c", "echo hi"}}, nil)
			return err
		}},
		{api.MethodWorkspaceReadFile, func() error {
			_, _, err := f.bob.o.WorkspaceReadFile(ctx, "work", api.WorkspaceReadFileParams{Workspace: f.aliceWS, Path: "m5/note.txt"})
			return err
		}},
		{api.MethodWorkspaceClose, func() error {
			_, err := f.bob.o.WorkspaceClose(ctx, "work", f.aliceWS)
			return err
		}},
	}
	for _, p := range probes {
		n := f.rec.count(p.method)
		err := p.call()
		if c := m5Code(err); c != api.CodeNotFound {
			t.Errorf("central, %s by bob on alice's workspace: %v (code %q), want %s", p.method, err, c, api.CodeNotFound)
		}
		if got := f.rec.count(p.method) - n; got != 1 {
			t.Errorf("%s: %d requests reached central, want exactly 1", p.method, got)
		}
		f.ownershipRefused[p.method]++ // refused before any work: one denied row each (F-43, step 12)
	}
	after, err := f.e.db.GetWorkspace(ctx, f.aliceWS)
	must(t, err)
	if after.State != store.WorkspaceOpen || !reflect.DeepEqual(after, before) {
		t.Fatalf("alice's workspace after bob's calls: %+v, before %+v", after, before)
	}
	// F-43: each refusal left exactly one denied row (bob as actor, alice's workspace id), nothing else.
	deniedBy := map[string]int{}
	for _, row := range f.e.auditRows() {
		if row.Actor == "bob" && row.Workspace == f.aliceWS {
			if row.Outcome != service.OutcomeDenied || row.Source != "api" {
				t.Errorf("bob's refused call left a non-denied row on alice's workspace: %+v", row)
			}
			deniedBy[row.Action]++
		}
	}
	for _, p := range probes {
		if deniedBy[p.method] != 1 {
			t.Errorf("%s: %d denied rows for bob on alice's workspace, want 1", p.method, deniedBy[p.method])
		}
	}
}

// ---- 8. Mode mismatch ----

func m5ModeMismatch(t *testing.T, f *m5Fixture) {
	al := f.alice.at(t)
	r := al.call("workspace_open", map[string]any{"fleet": "prod", "confirm": "prod"})
	if !r.IsErr || r.code() != api.CodeConflict {
		t.Errorf("workspace_open on the actions fleet: %s", r.Text)
	}
	r = al.call("run_action", map[string]any{"fleet": "dev", "runner": "build-1", "action": "hello", "params": map[string]any{"name": "x"}})
	if !r.IsErr || r.code() != api.CodeConflict {
		t.Errorf("run_action on the workspace fleet: %s", r.Text)
	}
}

// ---- 9. Cancel ----

func m5Cancel(t *testing.T, f *m5Fixture) {
	al := f.alice.at(t)
	execStarts := func() (n int) {
		for _, row := range f.e.auditRowsFor(api.MethodWorkspaceExec) {
			if row.Outcome == service.OutcomeStart {
				n++
			}
		}
		return
	}
	before := execStarts()
	cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	type outcome struct {
		r   m5Res
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		r, err := al.callCtx(cctx, "exec", map[string]any{"command": "m5-block"})
		done <- outcome{r, err}
	}()
	select {
	case <-f.started:
	case <-time.After(15 * time.Second):
		t.Fatal("the blocking exec never reached the executor")
	}
	cancel() // the client sends notifications/cancelled
	select {
	case <-f.cancelled:
	case <-time.After(15 * time.Second):
		t.Fatal("the cancel did not reach the fake executor")
	}
	select {
	case o := <-done:
		if o.err == nil && !o.r.IsErr {
			t.Errorf("cancelled call returned success: %s", o.r.Text)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the cancelled call never returned")
	}
	// Audited: the new start row has exactly one outcome row, "cancelled".
	var rows []store.AuditEvent
	waitFor(t, func() bool {
		rows = f.e.auditRowsFor(api.MethodWorkspaceExec)
		for _, row := range rows {
			if row.Outcome == service.OutcomeCancelled {
				return true
			}
		}
		return false
	})
	if n := execStarts() - before; n != 1 {
		t.Fatalf("%d new exec start rows", n)
	}
	var cancelledRows []store.AuditEvent
	for _, row := range rows {
		if row.Outcome == service.OutcomeCancelled {
			cancelledRows = append(cancelledRows, row)
		}
	}
	if len(cancelledRows) != 1 || cancelledRows[0].Actor != "alice" || cancelledRows[0].Workspace != f.aliceWS || cancelledRows[0].RequestID == "" {
		t.Fatalf("cancelled rows: %+v", cancelledRows)
	}
	hasStart := false
	for _, row := range rows {
		hasStart = hasStart || (row.Outcome == service.OutcomeStart && row.RequestID == cancelledRows[0].RequestID && row.ID < cancelledRows[0].ID)
	}
	if !hasStart {
		t.Fatalf("cancelled row without its start row: %+v", rows)
	}
	// The workspace is unharmed.
	if r := al.call("exec", map[string]any{"command": "echo hi"}); r.IsErr || r.str("workspace") != f.aliceWS {
		t.Fatalf("exec after cancel: %s", r.Text)
	}
}

// ---- 10. Idle on exit ----

func m5IdleOnExit(t *testing.T, f *m5Fixture) {
	ctx := m5ctx(t)
	al := f.alice.at(t)
	// closeStdin fails the test if Serve does not return within 5 s; it returns Serve's own error.
	if err := al.closeStdin(5 * time.Second); err != nil {
		t.Errorf("Serve returned %v on stdin EOF", err)
	}
	must(t, al.o.Close()) // the process exits; workspaces are not touched
	if w, err := f.e.db.GetWorkspace(ctx, f.aliceWS); err != nil || w.State != store.WorkspaceOpen {
		t.Fatalf("row after stdin close: %+v %v", w, err)
	}

	// A new server (a new random session) gets the other runner.
	s2 := m5Start(t, f.cfg(f.aliceDir), f.rec, f.sink).at(t)
	r := s2.call("exec", map[string]any{"command": "echo hi"})
	if r.IsErr {
		t.Fatalf("second server exec: %s", r.Text)
	}
	ws2 := r.str("workspace")
	if ws2 == "" || ws2 == f.aliceWS {
		t.Fatalf("second server reused the first workspace: %s", r.Text)
	}
	w2, err := f.e.db.GetWorkspace(ctx, ws2)
	must(t, err)
	if w2.Runner == f.aliceRunner || w2.Runner == "" {
		t.Fatalf("second workspace on %q, first on %q", w2.Runner, f.aliceRunner)
	}
	// A third has no slot left.
	s3 := m5Start(t, f.cfg(f.aliceDir), f.rec, f.sink).at(t)
	if r := s3.call("exec", map[string]any{"command": "echo hi"}); !r.IsErr || r.code() != api.CodeUnavailable {
		t.Fatalf("third server exec: %s", r.Text)
	}

	// The clock moves past the idle TTL; the second workspace is used (touched), the first is not.
	f.offset.Add(int64(f.idleTTL/time.Second) + 60)
	if r := s2.call("exec", map[string]any{"command": "echo hi"}); r.IsErr {
		t.Fatalf("touch: %s", r.Text)
	}
	f.e.svc.ReapOnce(ctx)
	w1, err := f.e.db.GetWorkspace(ctx, f.aliceWS)
	must(t, err)
	if w1.State != store.WorkspaceClosed || w1.Reason == "" || w1.ClosedAt.IsZero() {
		t.Fatalf("first row after reap: %+v", w1)
	}
	if w2, err = f.e.db.GetWorkspace(ctx, ws2); err != nil || w2.State != store.WorkspaceOpen {
		t.Fatalf("second row after reap: %+v %v", w2, err)
	}
	if infos, err := f.execs[f.aliceRunner].List(ctx); err != nil || len(infos) != 0 {
		t.Fatalf("container of the first workspace survived the reaper: %v %v", infos, err)
	}
	if rows := f.e.auditRowsFor("workspace.reap"); len(rows) != 1 || rows[0].Workspace != f.aliceWS || rows[0].Source != "system" {
		t.Fatalf("reap audit: %+v", rows)
	}
	// The freed slot serves the third server now.
	if r := s3.call("exec", map[string]any{"command": "echo hi"}); r.IsErr {
		t.Fatalf("third server after reap: %s", r.Text)
	}
	// Free both slots again for the next step.
	for _, s := range []*m5Server{s2, s3} {
		if r := s.call("workspace_close", nil); r.IsErr || r.str("state") != "closed" {
			t.Fatalf("close: %s", r.Text)
		}
	}
}

// ---- 11. Reattach, no hooks ----

func m5Reattach(t *testing.T, f *m5Fixture) {
	cfg := f.cfg(f.bobDir)
	cfg.Session = "s1"
	a := m5Start(t, cfg, f.rec, f.sink).at(t)
	b := m5Start(t, cfg, f.rec, f.sink).at(t)
	ra := a.call("workspace_open", nil)
	if ra.IsErr || ra.boolean("reused") || ra.str("session_source") != "flag" || ra.str("workspace", "session") != "s1" {
		t.Fatalf("first open: %s", ra.Text)
	}
	rb := b.call("workspace_open", nil)
	if rb.IsErr || !rb.boolean("reused") || rb.str("workspace", "id") != ra.str("workspace", "id") {
		t.Fatalf("second open: %s", rb.Text)
	}
	if r := b.call("exec", map[string]any{"command": "echo hi"}); r.IsErr || r.str("workspace") != ra.str("workspace", "id") {
		t.Fatalf("exec on the reattached workspace: %s", r.Text)
	}
	if r := a.call("workspace_close", nil); r.IsErr {
		t.Fatalf("close: %s", r.Text)
	}
}

// ---- 12. Hygiene ----

func m5Hygiene(t *testing.T, f *m5Fixture) {
	rows := f.e.auditRows()
	crows, err := f.cloud.db.ListAudit(m5ctx(t), 0, 10000)
	must(t, err)

	// Everything the servers sent is an operator method.
	sent := map[string]int{}
	for _, q := range f.rec.snapshot() {
		if !api.IsOperatorMethod(q.method) || api.IsAdminMethod(q.method) {
			t.Errorf("an orchestrator sent %q", q.method)
		}
		sent[q.method]++
	}

	// Every executed call has a start row and one outcome row (same request id, later id).
	type pair struct{ start, end int }
	byReq := map[string]*pair{}
	started, denied := map[string]int{}, map[string]int{}
	for _, row := range rows {
		if row.Source != "api" {
			continue
		}
		switch row.Action {
		case api.MethodActionRun, api.MethodWorkspaceExec, api.MethodWorkspaceReadFile, api.MethodWorkspaceWriteFile,
			api.MethodWorkspaceClose, api.MethodWorkspaceOpen:
		default:
			continue
		}
		if row.Outcome == service.OutcomeDenied {
			denied[row.Action]++
			continue
		}
		if row.Action == api.MethodWorkspaceOpen && row.Message == "reused" {
			continue
		}
		p := byReq[row.RequestID]
		if row.RequestID == "" {
			t.Errorf("audit row without request id: %+v", row)
			continue
		}
		if p == nil {
			p = &pair{}
			byReq[row.RequestID] = p
		}
		if row.Outcome == service.OutcomeStart {
			p.start++
			started[row.Action]++
		} else {
			p.end++
		}
	}
	for id, p := range byReq {
		if p.start != 1 || p.end != 1 {
			t.Errorf("request %s: %d start rows, %d outcome rows", id, p.start, p.end)
		}
	}
	// Every call sent has a start row or a single denied row, and the denied rows are exactly the
	// known refusals: step 7's ownership probes (F-43) per method, step 8's mode-mismatch
	// run_action on the workspace fleet (F-41) plus the one guarded refusal for action.run.
	modeRefused := 0 // action.run requests aimed at fleet dev (the workspace fleet), from the recorder
	for _, q := range f.rec.snapshot() {
		if q.method != api.MethodActionRun {
			continue
		}
		var p api.ActionRunParams
		must(t, json.Unmarshal([]byte(q.params), &p))
		if p.Fleet == "dev" {
			modeRefused++
		}
	}
	wantDenied := map[string]int{
		api.MethodWorkspaceExec:      f.ownershipRefused[api.MethodWorkspaceExec],
		api.MethodWorkspaceReadFile:  f.ownershipRefused[api.MethodWorkspaceReadFile],
		api.MethodWorkspaceWriteFile: f.ownershipRefused[api.MethodWorkspaceWriteFile],
		api.MethodWorkspaceClose:     f.ownershipRefused[api.MethodWorkspaceClose],
		api.MethodActionRun:          1 + modeRefused,
		api.MethodWorkspaceOpen:      1, // step 8: workspace_open on the actions fleet (F-41)
	}
	for _, m := range []string{api.MethodWorkspaceExec, api.MethodWorkspaceReadFile, api.MethodWorkspaceWriteFile, api.MethodActionRun} {
		if started[m]+denied[m] != sent[m] {
			t.Errorf("%s: %d calls sent, %d start rows + %d denied rows", m, sent[m], started[m], denied[m])
		}
	}
	for m, want := range wantDenied {
		if denied[m] != want {
			t.Errorf("%s: %d denied rows, want %d", m, denied[m], want)
		}
	}

	// No admin method in alice's (or bob's) audit; operators act through api only.
	for _, set := range [][]store.AuditEvent{rows, crows} {
		for _, row := range set {
			if row.Actor != "alice" && row.Actor != "bob" {
				continue
			}
			if api.IsAdminMethod(row.Action) || (row.Source != "api" && row.Source != "enroll") {
				t.Errorf("operator row looks administrative: %+v", row)
			}
		}
	}

	// No secret reached anything central or an executor received, or anything the model saw.
	f.assertNoDecoyAtExecutors(t)
	var all strings.Builder
	for _, q := range f.rec.snapshot() {
		all.WriteString(q.method + " " + q.params + " " + q.body + "\n")
	}
	for _, row := range append(rows, crows...) {
		fmt.Fprintf(&all, "%+v\n", row)
	}
	for _, d := range f.decoys {
		if strings.Contains(all.String(), d) {
			t.Errorf("decoy %q reached central", d)
		}
	}
	results := f.sink.all()
	if len(results) < 30 {
		t.Errorf("only %d tool results recorded", len(results))
	}
	secrets := append([]string{f.aliceDir, f.bobDir, f.e.dir, f.cloud.dir}, f.pubFingerprint...)
	secrets = append(secrets, f.decoys...)
	secrets = append(secrets, ssh.FingerprintSHA256(f.e.hostSigner.PublicKey()), ssh.FingerprintSHA256(f.cloud.hostSigner.PublicKey()))
	sort.Strings(secrets)
	for _, text := range results {
		for _, s := range secrets {
			if s != "" && strings.Contains(text, s) {
				t.Errorf("a tool result contains %q: %.300s", s, text)
			}
		}
		if strings.Contains(text, "SHA256:") {
			t.Errorf("a tool result contains a key fingerprint: %.300s", text)
		}
	}
}
