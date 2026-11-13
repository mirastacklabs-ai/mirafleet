package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/central/registry"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/tokens"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

const (
	imgGo   = "docker.io/library/golang:1.27"
	imgNode = "docker.io/library/node:22"
)

// wsFleet creates a workspace fleet with a small allowlist.
func (e *env) wsFleet(name string, confirm bool, mut func(*store.WorkspaceConfig)) {
	e.t.Helper()
	c := store.WorkspaceConfig{DefaultImage: imgGo, Images: []string{imgGo, "docker.io/library/node:*"}}
	if mut != nil {
		mut(&c)
	}
	if _, err := e.svc.CreateFleet(e.ctx, e.admin, SourceCLI, name, store.ModeWorkspace, confirm, &c); err != nil {
		e.t.Fatal(err)
	}
}

type rspec struct {
	labels map[string]string // trusted token labels
	slots  int
	secure string
	images []string
	exec   string
	off    bool // registered but offline
	hbLbl  map[string]string
}

// wsRunner enrols an active runner with token labels and registers it in the fake registry.
func (e *env) wsRunner(fleet string, sp rspec) store.Runner {
	e.t.Helper()
	e.nameN++
	name := fmt.Sprintf("w%d", e.nameN)
	ct, err := e.svc.CreateToken(e.ctx, e.admin, SourceCLI, TokenParams{Fleet: fleet, Approve: true, Labels: sp.labels})
	if err != nil {
		e.t.Fatal(err)
	}
	id, secret, _, err := tokens.ParseJoinToken(ct.Token)
	if err != nil {
		e.t.Fatal(err)
	}
	h := sha256.Sum256(secret)
	wire, fp := newKey(e.t)
	r, err := e.db.Enroll(e.ctx, id, h[:], t0, store.Runner{Name: name, PubKey: wire, Fingerprint: fp},
		store.AuditEvent{Time: t0, Source: "enroll", ActorKind: "enroll", Action: "enroll.join", Outcome: "ok", Fleet: fleet, Runner: name})
	if err != nil {
		e.t.Fatal(err)
	}
	if sp.exec == "" {
		sp.exec = "containerd"
	}
	if sp.images == nil {
		sp.images = []string{"docker.io/library/golang:*", "docker.io/library/node:*"}
	}
	e.live.snap = append(e.live.snap, registry.SessionInfo{ID: r.ID, Name: name, Fleet: fleet, Status: registry.StatusActive,
		Online: !sp.off, Slots: sp.slots, Executor: sp.exec, SecureRuntime: sp.secure, Images: sp.images,
		Info: transport.Heartbeat{Labels: sp.hbLbl}})
	return r
}

func (e *env) setOnline(id int64, on bool) {
	for i := range e.live.snap {
		if e.live.snap[i].ID == id {
			e.live.snap[i].Online = on
		}
	}
}

func (e *env) wsCalls(op string) []string {
	var out []string
	for _, c := range e.log.get() {
		if strings.HasPrefix(c, "workspace ") && strings.HasSuffix(c, " "+op) {
			out = append(out, c)
		}
	}
	return out
}

func (e *env) wsAudit(action string) []store.AuditEvent {
	var out []store.AuditEvent
	for _, a := range e.audit() {
		if a.Action == action {
			out = append(out, a)
		}
	}
	return out
}

func TestCreateFleetWorkspaceDefaults(t *testing.T) {
	e := newEnv(t)
	f, err := e.svc.CreateFleet(e.ctx, e.admin, SourceCLI, "dev", store.ModeWorkspace, false,
		&store.WorkspaceConfig{DefaultImage: imgGo})
	if err != nil {
		t.Fatal(err)
	}
	c := *f.Workspace
	want := store.WorkspaceConfig{Executor: store.ExecutorContainerd, DefaultImage: imgGo, Images: []string{imgGo},
		Pull: store.PullIfNotPresent, Network: store.NetworkNone, CPUMilli: 2000, MemoryMB: 4096, Pids: 1024,
		TmpfsMB: 512, DiskMB: 10240, IdleTTL: 24 * time.Hour, MaxTTL: 168 * time.Hour,
		ExecDefault: 120 * time.Second, ExecMax: 1800 * time.Second}
	c.UpdatedAt = time.Time{}
	if fmt.Sprint(c) != fmt.Sprint(want) {
		t.Fatalf("defaults:\n got %+v\nwant %+v", c, want)
	}
	// Explicit values survive; omitted ones default around them.
	g, err := e.svc.CreateFleet(e.ctx, e.admin, SourceCLI, "dev2", store.ModeWorkspace, false,
		&store.WorkspaceConfig{DefaultImage: imgGo, MemoryMB: 8192, IdleTTL: 2 * time.Hour, ExecMax: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if g.Workspace.MemoryMB != 8192 || g.Workspace.IdleTTL != 2*time.Hour || g.Workspace.ExecMax != time.Minute ||
		g.Workspace.ExecDefault != time.Minute || g.Workspace.CPUMilli != 2000 {
		t.Fatalf("explicit: %+v", g.Workspace)
	}
	// Default image is required.
	for _, ws := range []*store.WorkspaceConfig{nil, {}, {Images: []string{imgGo}}} {
		if c := code(t, errOf(e.svc.CreateFleet(e.ctx, e.admin, SourceCLI, "bad", store.ModeWorkspace, false, ws))); c != api.CodeBadRequest {
			t.Errorf("missing default image: %q", c)
		}
	}
}

func TestSetFleetWorkspaceFullReplace(t *testing.T) {
	e := newEnv(t)
	e.wsFleet("dev", false, func(c *store.WorkspaceConfig) {
		c.Network = store.NetworkEgress
		c.Egress = []string{"proxy.golang.org:443"}
	})
	cur, err := e.db.GetFleetWorkspace(e.ctx, "dev")
	if err != nil || len(cur.Egress) != 1 {
		t.Fatalf("setup: %+v %v", cur, err)
	}
	// Full replace: clearing egress and network works; no defaults are merged in.
	cur.Network, cur.Egress = store.NetworkNone, nil
	cur.MemoryMB = 1024
	got, err := e.svc.SetFleetWorkspace(e.ctx, e.admin, SourceCLI, "dev", cur)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Egress) != 0 || got.Network != store.NetworkNone || got.MemoryMB != 1024 {
		t.Fatalf("replaced: %+v", got)
	}
	st, _ := e.db.GetFleetWorkspace(e.ctx, "dev")
	if len(st.Egress) != 0 {
		t.Fatalf("egress not cleared: %+v", st)
	}
	if rows := e.wsAudit(api.MethodFleetWorkspaceSet); len(rows) != 1 || rows[0].Outcome != OutcomeOK || rows[0].Fleet != "dev" {
		t.Fatalf("audit: %+v", rows)
	}
	// A partial config is not completed with defaults.
	if c := code(t, errOf(e.svc.SetFleetWorkspace(e.ctx, e.admin, SourceCLI, "dev", store.WorkspaceConfig{DefaultImage: imgGo}))); c != api.CodeBadRequest {
		t.Errorf("partial set: %q", c)
	}
	// Invalid values: pattern default, default not in allowlist.
	bad := cur
	bad.DefaultImage = "docker.io/library/other:1"
	if c := code(t, errOf(e.svc.SetFleetWorkspace(e.ctx, e.admin, SourceCLI, "dev", bad))); c != api.CodeBadRequest {
		t.Errorf("default outside allowlist: %q", c)
	}
	// Actions fleet and unknown fleet.
	e.fleet("acts")
	if c := code(t, errOf(e.svc.SetFleetWorkspace(e.ctx, e.admin, SourceCLI, "acts", cur))); c != api.CodeBadRequest {
		t.Errorf("actions fleet: %q", c)
	}
	if c := code(t, errOf(e.svc.SetFleetWorkspace(e.ctx, e.admin, SourceCLI, "nope", cur))); c != api.CodeNotFound {
		t.Errorf("unknown fleet: %q", c)
	}
	// Operators are denied (double gate), with a denied audit row, and nothing changes.
	op := e.operator("ann")
	if c := code(t, errOf(e.svc.SetFleetWorkspace(e.ctx, op, SourceAPI, "dev", cur))); c != api.CodeForbidden {
		t.Errorf("operator set: %q", c)
	}
	rows := e.wsAudit(api.MethodFleetWorkspaceSet)
	if rows[len(rows)-1].Outcome != OutcomeDenied {
		t.Errorf("denial not audited: %+v", rows[len(rows)-1])
	}
}

func TestOpenWorkspacePlacement(t *testing.T) {
	type tc struct {
		name    string
		guarded bool
		labels  map[string]string
		image   string
		runners []rspec
		want    int // index of the chosen runner, -1: expect unavailable
	}
	cases := []tc{
		{name: "labels come from the token not the heartbeat", labels: map[string]string{"gpu": "yes"},
			runners: []rspec{{slots: 2, hbLbl: map[string]string{"gpu": "yes"}}, {slots: 1, labels: map[string]string{"gpu": "yes"}}}, want: 1},
		{name: "labels must all match", labels: map[string]string{"a": "1", "b": "2"},
			runners: []rspec{{slots: 2, labels: map[string]string{"a": "1"}}}, want: -1},
		{name: "guarded needs a secure runtime", guarded: true,
			runners: []rspec{{slots: 4}, {slots: 1, secure: "gvisor"}}, want: 1},
		{name: "guarded with none secure", guarded: true, runners: []rspec{{slots: 4}}, want: -1},
		{name: "image patterns", image: imgNode,
			runners: []rspec{{slots: 4, images: []string{"docker.io/library/golang:*"}}, {slots: 1, images: []string{"docker.io/library/node:*"}}}, want: 1},
		{name: "runner advertising no images matches none", runners: []rspec{{slots: 4, images: []string{}}}, want: -1},
		{name: "executor must match", runners: []rspec{{slots: 4, exec: "opensandbox"}, {slots: 1}}, want: 1},
		{name: "offline runner skipped", runners: []rspec{{slots: 4, off: true}, {slots: 1}}, want: 1},
		{name: "most free wins", runners: []rspec{{slots: 1}, {slots: 5}, {slots: 3}}, want: 1},
		{name: "no slots", runners: []rspec{{slots: 0}}, want: -1},
		{name: "no runners", runners: nil, want: -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.wsFleet("dev", c.guarded, nil)
			op := e.operator("ann")
			var rs []store.Runner
			for _, sp := range c.runners {
				rs = append(rs, e.wsRunner("dev", sp))
			}
			confirm := ""
			if c.guarded {
				confirm = "dev"
			}
			w, reused, err := e.svc.OpenWorkspace(e.ctx, op, SourceAPI, "dev", "s1", c.image, c.labels, confirm)
			if c.want < 0 {
				if code(t, err) != api.CodeUnavailable {
					t.Fatalf("err = %v, want unavailable", err)
				}
				if n := len(e.wsCalls("create")); n != 0 {
					t.Fatalf("%d create calls", n)
				}
				return
			}
			if err != nil || reused {
				t.Fatalf("open: %v reused=%v", err, reused)
			}
			if w.RunnerID != rs[c.want].ID || w.State != store.WorkspaceOpen {
				t.Fatalf("placed on %d state %s, want runner %d open", w.RunnerID, w.State, rs[c.want].ID)
			}
		})
	}
}

func TestOpenWorkspaceSpecAndAudit(t *testing.T) {
	e := newEnv(t)
	e.wsFleet("dev", true, func(c *store.WorkspaceConfig) {
		c.Network = store.NetworkEgress
		c.Egress = []string{"proxy.golang.org", "*.example.org:8443", "10.1.0.0/16:5432"}
	})
	r := e.wsRunner("dev", rspec{slots: 2, secure: "kata", labels: map[string]string{"zone": "a"}})
	var got transport.WorkspaceRequest
	var gotDeadline time.Time
	e.live.wsFn = func(ctx context.Context, id int64, req transport.WorkspaceRequest, _ io.Reader, _ io.Writer) (*transport.WorkspaceResult, error) {
		got = req
		gotDeadline, _ = ctx.Deadline()
		return &transport.WorkspaceResult{Code: transport.CodeOK}, nil
	}
	op := e.operator("ann")
	w, _, err := e.svc.OpenWorkspace(e.ctx, op, SourceAPI, "dev", "sess:1", "", map[string]string{"zone": "a"}, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if got.Op != transport.OpCreate || got.ID != w.WsID || got.Spec == nil || got.Spec.Image != imgGo {
		t.Fatalf("create req: %+v", got)
	}
	sp := got.Spec
	if sp.CPUMilli != 2000 || sp.MemoryBytes != 4096<<20 || sp.Pids != 1024 || sp.TmpfsBytes != 512<<20 ||
		sp.DiskBytes != 10240<<20 || sp.NetworkMode != "egress" || !sp.RequireSecureRuntime ||
		sp.Labels["zone"] != "a" || sp.MaxAgeMs != (168*time.Hour).Milliseconds() || sp.Pull != "if-not-present" {
		t.Fatalf("spec: %+v", sp)
	}
	wantAllow := []transport.EgressRule{{Host: "proxy.golang.org", Port: 443}, {Host: "*.example.org", Port: 8443}, {Host: "10.1.0.0/16", Port: 5432}}
	if fmt.Sprint(sp.Allow) != fmt.Sprint(wantAllow) {
		t.Fatalf("allow: %+v", sp.Allow)
	}
	if d := gotDeadline.Sub(time.Now()); d <= 0 || d > DefaultWorkspaceOpenTimeout {
		t.Fatalf("deadline %v", d)
	}
	if !w.ExpiresAt.Equal(t0.Add(168 * time.Hour)) {
		t.Fatalf("expires %v", w.ExpiresAt)
	}
	rows := e.wsAudit(api.MethodWorkspaceOpen)
	if len(rows) != 2 || rows[0].Outcome != OutcomeStart || rows[1].Outcome != OutcomeOK {
		t.Fatalf("audit: %+v", rows)
	}
	for _, a := range rows {
		if a.Workspace != w.WsID || a.RequestID == "" || a.RequestID != rows[0].RequestID || a.Fleet != "dev" ||
			a.Runner != r.Name || a.Actor != "ann" || a.Source != "api" || a.ActorKind != "operator" {
			t.Fatalf("row: %+v", a)
		}
	}
	// Row start precedes the create call in the log (reserve then call).
	if len(e.wsCalls("create")) != 1 {
		t.Fatal("create calls")
	}
}

func TestOpenWorkspaceValidation(t *testing.T) {
	e := newEnv(t)
	e.wsFleet("dev", false, nil)
	e.fleet("acts")
	e.wsRunner("dev", rspec{slots: 8})
	op := e.operator("ann")
	for name, c := range map[string]struct {
		fleet, session, image string
		labels                map[string]string
		want                  string
	}{
		"bad session":      {"dev", "has space", "", nil, api.CodeBadRequest},
		"empty session":    {"dev", "", "", nil, api.CodeBadRequest},
		"long session":     {"dev", strings.Repeat("a", 129), "", nil, api.CodeBadRequest},
		"image not listed": {"dev", "s", "docker.io/library/evil:1", nil, api.CodeBadRequest},
		"reserved label":   {"dev", "s", "", map[string]string{"mirafleet.x": "1"}, api.CodeBadRequest},
		"actions fleet":    {"acts", "s", "", nil, api.CodeConflict},
		"unknown fleet":    {"nope", "s", "", nil, api.CodeNotFound},
		"bad fleet name":   {"A b", "s", "", nil, api.CodeBadRequest},
	} {
		if got := code(t, errOf3(e.svc.OpenWorkspace(e.ctx, op, SourceAPI, c.fleet, c.session, c.image, c.labels, ""))); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
	// Admins and revoked operators are refused with a denied audit row.
	if c := code(t, errOf3(e.svc.OpenWorkspace(e.ctx, e.admin, SourceCLI, "dev", "s", "", nil, ""))); c != api.CodeForbidden {
		t.Errorf("admin: %q", c)
	}
	rev := e.operator("rev")
	if _, err := e.svc.RevokeOperator(e.ctx, e.admin, SourceCLI, "rev"); err != nil {
		t.Fatal(err)
	}
	if c := code(t, errOf3(e.svc.OpenWorkspace(e.ctx, rev, SourceAPI, "dev", "s", "", nil, ""))); c != api.CodeForbidden {
		t.Errorf("revoked operator: %q", c)
	}
	den := 0
	for _, a := range e.wsAudit(api.MethodWorkspaceOpen) {
		if a.Outcome == OutcomeDenied {
			den++
		}
	}
	if den != 2 {
		t.Errorf("denied rows = %d", den)
	}
	if n := len(e.wsCalls("create")); n != 0 {
		t.Errorf("%d create calls", n)
	}
}

func TestOpenWorkspaceGuardedConfirm(t *testing.T) {
	e := newEnv(t)
	e.wsFleet("prod", true, nil)
	e.wsRunner("prod", rspec{slots: 2, secure: "gvisor"})
	op := e.operator("ann")
	for _, cf := range []string{"", "other", "PROD"} {
		if c := code(t, errOf3(e.svc.OpenWorkspace(e.ctx, op, SourceAPI, "prod", "s", "", nil, cf))); c != api.CodeBadRequest {
			t.Errorf("confirm %q: %q", cf, c)
		}
	}
	if n := len(e.wsCalls("create")); n != 0 {
		t.Fatalf("%d create calls on refused confirm", n)
	}
	den := e.wsAudit(api.MethodWorkspaceOpen)
	if len(den) != 3 || den[0].Outcome != OutcomeDenied || !strings.Contains(den[0].Message, "confirm") {
		t.Fatalf("audit: %+v", den)
	}
	if _, _, err := e.svc.OpenWorkspace(e.ctx, op, SourceAPI, "prod", "s", "", nil, "prod"); err != nil {
		t.Fatal(err)
	}
}

func TestOpenWorkspaceSticky(t *testing.T) {
	e := newEnv(t)
	e.wsFleet("dev", false, nil)
	r := e.wsRunner("dev", rspec{slots: 2})
	e.wsRunner("dev", rspec{slots: 5})
	ann, bob := e.operator("ann"), e.operator("bob")
	w1, reused, err := e.svc.OpenWorkspace(e.ctx, ann, SourceAPI, "dev", "s", "", nil, "")
	if err != nil || reused {
		t.Fatal(err, reused)
	}
	w2, reused, err := e.svc.OpenWorkspace(e.ctx, ann, SourceAPI, "dev", "s", "", nil, "")
	if err != nil || !reused || w2.WsID != w1.WsID {
		t.Fatalf("reuse: %+v %v %v", w2, reused, err)
	}
	if n := len(e.wsCalls("create")); n != 1 {
		t.Fatalf("%d creates", n)
	}
	var last store.AuditEvent
	rows := e.wsAudit(api.MethodWorkspaceOpen)
	last = rows[len(rows)-1]
	if last.Message != "reused" || last.Outcome != OutcomeOK || last.Workspace != w1.WsID {
		t.Fatalf("reuse not audited: %+v", last)
	}
	// Another operator with the same session gets their own workspace.
	w3, reused, err := e.svc.OpenWorkspace(e.ctx, bob, SourceAPI, "dev", "s", "", nil, "")
	if err != nil || reused || w3.WsID == w1.WsID {
		t.Fatalf("bob: %+v %v %v", w3, reused, err)
	}
	// Offline runner of the sticky workspace: unavailable, nothing created elsewhere.
	e.setOnline(w1.RunnerID, false)
	_ = r
	before := len(e.wsCalls("create"))
	if c := code(t, errOf3(e.svc.OpenWorkspace(e.ctx, ann, SourceAPI, "dev", "s", "", nil, ""))); c != api.CodeUnavailable {
		t.Fatalf("offline sticky: %q", c)
	}
	if len(e.wsCalls("create")) != before {
		t.Fatal("created a replacement elsewhere")
	}
	rows = e.wsAudit(api.MethodWorkspaceOpen)
	if l := rows[len(rows)-1]; l.Outcome != OutcomeError || !strings.Contains(l.Message, "offline") {
		t.Fatalf("offline not audited: %+v", l)
	}
	// After a close, the session may be opened afresh.
	e.setOnline(w1.RunnerID, true)
	if _, err := e.svc.CloseWorkspace(e.ctx, ann, SourceAPI, w1.WsID); err != nil {
		t.Fatal(err)
	}
	w4, reused, err := e.svc.OpenWorkspace(e.ctx, ann, SourceAPI, "dev", "s", "", nil, "")
	if err != nil || reused || w4.WsID == w1.WsID {
		t.Fatalf("after close: %+v %v %v", w4, reused, err)
	}
}

func TestOpenWorkspaceFullThenNext(t *testing.T) {
	e := newEnv(t)
	e.wsFleet("dev", false, nil)
	a := e.wsRunner("dev", rspec{slots: 1})
	b := e.wsRunner("dev", rspec{slots: 1})
	op := e.operator("ann")
	seen := map[int64]bool{}
	for i := 0; i < 2; i++ {
		w, _, err := e.svc.OpenWorkspace(e.ctx, op, SourceAPI, "dev", fmt.Sprintf("s%d", i), "", nil, "")
		if err != nil {
			t.Fatal(err)
		}
		seen[w.RunnerID] = true
	}
	if !seen[a.ID] || !seen[b.ID] {
		t.Fatalf("not spread over both runners: %v", seen)
	}
	// All full -> unavailable.
	if c := code(t, errOf3(e.svc.OpenWorkspace(e.ctx, op, SourceAPI, "dev", "s9", "", nil, ""))); c != api.CodeUnavailable {
		t.Fatalf("all full: %q", c)
	}
}

// staleCount makes the placement view stale: the first runner looks free but the store (the
// authority, re-checked in ReserveWorkspace) is full, so the service must try the next one.
type staleCount struct{ store.Store }

func (s staleCount) CountLiveWorkspaces(ctx context.Context, ids []int64) (map[int64]int, error) {
	return map[int64]int{}, nil // pretend everything is empty
}

func TestOpenWorkspaceReserveFullTriesNext(t *testing.T) {
	e := newEnv(t)
	e.wsFleet("dev", false, nil)
	big := e.wsRunner("dev", rspec{slots: 3})
	small := e.wsRunner("dev", rspec{slots: 2})
	op := e.operator("ann")
	// Fill the big runner's three slots in the store directly via the service.
	e.live.snap = e.live.snap[:0]
	e.live.snap = append(e.live.snap, registry.SessionInfo{ID: big.ID, Fleet: "dev", Status: registry.StatusActive, Online: true, Slots: 3, Executor: "containerd", Images: []string{"docker.io/library/golang:*"}})
	for i := 0; i < 3; i++ {
		if _, _, err := e.svc.OpenWorkspace(e.ctx, op, SourceAPI, "dev", fmt.Sprintf("f%d", i), "", nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	e.live.snap = append(e.live.snap, registry.SessionInfo{ID: small.ID, Fleet: "dev", Status: registry.StatusActive, Online: true, Slots: 2, Executor: "containerd", Images: []string{"docker.io/library/golang:*"}})
	e.svc.st = staleCount{e.svc.st}
	w, _, err := e.svc.OpenWorkspace(e.ctx, op, SourceAPI, "dev", "next", "", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if w.RunnerID != small.ID {
		t.Fatalf("placed on %d, want %d (big is full in the store)", w.RunnerID, small.ID)
	}
}

func TestOpenWorkspaceRunnerBusyTriesNextThenFails(t *testing.T) {
	e := newEnv(t)
	e.wsFleet("dev", false, nil)
	a := e.wsRunner("dev", rspec{slots: 4})
	b := e.wsRunner("dev", rspec{slots: 2})
	op := e.operator("ann")
	e.live.wsFn = func(_ context.Context, id int64, req transport.WorkspaceRequest, _ io.Reader, _ io.Writer) (*transport.WorkspaceResult, error) {
		if id == a.ID {
			return nil, registry.ErrBusy
		}
		return &transport.WorkspaceResult{Code: transport.CodeOK}, nil
	}
	w, _, err := e.svc.OpenWorkspace(e.ctx, op, SourceAPI, "dev", "s", "", nil, "")
	if err != nil || w.RunnerID != b.ID {
		t.Fatalf("busy then next: %+v %v", w, err)
	}
	// The busy attempt left a failed row with an error outcome.
	failed, _ := e.db.ListWorkspaces(e.ctx, store.WorkspaceFilter{States: []store.WorkspaceState{store.WorkspaceFailed}})
	if len(failed) != 1 || failed[0].RunnerID != a.ID {
		t.Fatalf("failed rows: %+v", failed)
	}
	// Both busy: unavailable.
	e.live.wsFn = func(context.Context, int64, transport.WorkspaceRequest, io.Reader, io.Writer) (*transport.WorkspaceResult, error) {
		return &transport.WorkspaceResult{Code: transport.CodeBusy}, nil
	}
	if c := code(t, errOf3(e.svc.OpenWorkspace(e.ctx, op, SourceAPI, "dev", "t", "", nil, ""))); c != api.CodeUnavailable {
		t.Fatalf("all busy: %q", c)
	}
}

func TestOpenWorkspaceCreateFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		fn   func(context.Context, int64, transport.WorkspaceRequest, io.Reader, io.Writer) (*transport.WorkspaceResult, error)
		want string
	}{
		"refused": {func(context.Context, int64, transport.WorkspaceRequest, io.Reader, io.Writer) (*transport.WorkspaceResult, error) {
			return &transport.WorkspaceResult{Code: transport.CodeRefused, Message: "no secure runtime\x00"}, nil
		}, api.CodeBadRequest},
		"link down": {func(context.Context, int64, transport.WorkspaceRequest, io.Reader, io.Writer) (*transport.WorkspaceResult, error) {
			return nil, errors.New("connection reset: /secret/path")
		}, api.CodeUnavailable},
		"unavailable": {func(context.Context, int64, transport.WorkspaceRequest, io.Reader, io.Writer) (*transport.WorkspaceResult, error) {
			return &transport.WorkspaceResult{Code: transport.CodeUnavailable}, nil
		}, api.CodeUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.wsFleet("dev", false, nil)
			e.wsRunner("dev", rspec{slots: 2})
			e.wsRunner("dev", rspec{slots: 2})
			e.live.wsFn = tc.fn
			op := e.operator("ann")
			err := errOf3(e.svc.OpenWorkspace(e.ctx, op, SourceAPI, "dev", "s", "", nil, ""))
			if code(t, err) != tc.want {
				t.Fatalf("err = %v, want %s", err, tc.want)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("leaked: %v", err)
			}
			if n := len(e.wsCalls("create")); n != 1 {
				t.Fatalf("%d create calls, want 1 (no retry)", n)
			}
			failed, _ := e.db.ListWorkspaces(e.ctx, store.WorkspaceFilter{States: []store.WorkspaceState{store.WorkspaceFailed}})
			if len(failed) != 1 {
				t.Fatalf("failed rows: %d", len(failed))
			}
			rows := e.wsAudit(api.MethodWorkspaceOpen)
			if len(rows) != 2 || rows[1].Outcome != OutcomeError || rows[1].RequestID != rows[0].RequestID {
				t.Fatalf("audit: %+v", rows)
			}
			// The slot is free again.
			e.live.wsFn = nil
			if _, _, err := e.svc.OpenWorkspace(e.ctx, op, SourceAPI, "dev", "s", "", nil, ""); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOpenWorkspaceTimeout(t *testing.T) {
	e := newEnv(t)
	e.svc.openTimeout = 20 * time.Millisecond
	e.wsFleet("dev", false, nil)
	e.wsRunner("dev", rspec{slots: 2})
	e.live.wsFn = func(ctx context.Context, _ int64, _ transport.WorkspaceRequest, _ io.Reader, _ io.Writer) (*transport.WorkspaceResult, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	op := e.operator("ann")
	if c := code(t, errOf3(e.svc.OpenWorkspace(e.ctx, op, SourceAPI, "dev", "s", "", nil, ""))); c != api.CodeTimeout {
		t.Fatalf("timeout: %q", c)
	}
	rows := e.wsAudit(api.MethodWorkspaceOpen)
	if rows[len(rows)-1].Outcome != OutcomeTimeout {
		t.Fatalf("audit: %+v", rows)
	}
}

func TestOpenWorkspaceOperatorCap(t *testing.T) {
	e := newEnv(t)
	e.svc.maxPerOp = 2
	e.wsFleet("dev", false, nil)
	e.wsRunner("dev", rspec{slots: 10})
	ann, bob := e.operator("ann"), e.operator("bob")
	var first store.Workspace
	for i := 0; i < 2; i++ {
		w, _, err := e.svc.OpenWorkspace(e.ctx, ann, SourceAPI, "dev", fmt.Sprintf("s%d", i), "", nil, "")
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = w
		}
	}
	if c := code(t, errOf3(e.svc.OpenWorkspace(e.ctx, ann, SourceAPI, "dev", "s2", "", nil, ""))); c != api.CodeConflict {
		t.Fatalf("over cap: %q", c)
	}
	// Reuse of an existing session still works at the cap; other operators are unaffected.
	if _, reused, err := e.svc.OpenWorkspace(e.ctx, ann, SourceAPI, "dev", "s0", "", nil, ""); err != nil || !reused {
		t.Fatalf("reuse at cap: %v %v", reused, err)
	}
	if _, _, err := e.svc.OpenWorkspace(e.ctx, bob, SourceAPI, "dev", "s0", "", nil, ""); err != nil {
		t.Fatal(err)
	}
	// Closing frees a slot.
	if _, err := e.svc.CloseWorkspace(e.ctx, ann, SourceAPI, first.WsID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.svc.OpenWorkspace(e.ctx, ann, SourceAPI, "dev", "s2", "", nil, ""); err != nil {
		t.Fatal(err)
	}
	// Default cap.
	if e2 := newEnv(t); e2.svc.maxPerOp != 8 || e2.svc.openTimeout != 5*time.Minute {
		t.Fatalf("defaults: %d %v", e2.svc.maxPerOp, e2.svc.openTimeout)
	}
}

func TestCloseWorkspace(t *testing.T) {
	e := newEnv(t)
	e.wsFleet("dev", false, nil)
	r := e.wsRunner("dev", rspec{slots: 4})
	ann, bob := e.operator("ann"), e.operator("bob")
	w, _, err := e.svc.OpenWorkspace(e.ctx, ann, SourceAPI, "dev", "s", "", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	// Non-owner (and admin, malformed and unknown ids): not_found, never forbidden, no runner call.
	for name, p := range map[string]auth.Principal{"bob": bob} {
		if c := code(t, errOf(e.svc.CloseWorkspace(e.ctx, p, SourceAPI, w.WsID))); c != api.CodeNotFound {
			t.Errorf("%s: %q", name, c)
		}
	}
	for _, id := range []string{strings.Repeat("0", 32), "short", ""} {
		if c := code(t, errOf(e.svc.CloseWorkspace(e.ctx, ann, SourceAPI, id))); c != api.CodeNotFound {
			t.Errorf("id %q: %q", id, c)
		}
	}
	if c := code(t, errOf(e.svc.CloseWorkspace(e.ctx, e.admin, SourceCLI, w.WsID))); c != api.CodeForbidden {
		t.Errorf("admin: %q", c)
	}
	if n := len(e.wsCalls("destroy")); n != 0 {
		t.Fatalf("%d destroys before owner close", n)
	}
	got, err := e.svc.CloseWorkspace(e.ctx, ann, SourceAPI, w.WsID)
	if err != nil || got.State != store.WorkspaceClosed {
		t.Fatalf("close: %+v %v", got, err)
	}
	if n := e.wsCalls("destroy"); len(n) != 1 || n[0] != fmt.Sprintf("workspace %d destroy", r.ID) {
		t.Fatalf("destroy calls: %v", n)
	}
	rows := e.wsAudit(api.MethodWorkspaceClose)
	var starts, oks int
	for _, a := range rows {
		if a.Outcome == OutcomeStart {
			starts++
		}
		if a.Outcome == OutcomeOK {
			oks++
			if a.RequestID == "" || a.Workspace != w.WsID {
				t.Fatalf("row: %+v", a)
			}
		}
	}
	if starts != 1 || oks != 1 {
		t.Fatalf("audit: %+v", rows)
	}
	// Closing a closed workspace is a no-op.
	if g, err := e.svc.CloseWorkspace(e.ctx, ann, SourceAPI, w.WsID); err != nil || g.State != store.WorkspaceClosed {
		t.Fatalf("again: %+v %v", g, err)
	}
	if n := len(e.wsCalls("destroy")); n != 1 {
		t.Fatalf("%d destroys", n)
	}
}

func TestCloseWorkspaceRunnerOffline(t *testing.T) {
	e := newEnv(t)
	e.wsFleet("dev", false, nil)
	r := e.wsRunner("dev", rspec{slots: 1})
	ann := e.operator("ann")
	w, _, err := e.svc.OpenWorkspace(e.ctx, ann, SourceAPI, "dev", "s", "", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	e.live.wsFn = func(context.Context, int64, transport.WorkspaceRequest, io.Reader, io.Writer) (*transport.WorkspaceResult, error) {
		return nil, fmt.Errorf("runner %d is not online", r.ID)
	}
	got, err := e.svc.CloseWorkspace(e.ctx, ann, SourceAPI, w.WsID)
	if err != nil || got.State != store.WorkspaceClosing {
		t.Fatalf("offline close: %+v %v", got, err)
	}
	st, _ := e.db.GetWorkspace(e.ctx, w.WsID)
	if st.State != store.WorkspaceClosing {
		t.Fatalf("state %s", st.State)
	}
	// It still holds its slot.
	if c := code(t, errOf3(e.svc.OpenWorkspace(e.ctx, ann, SourceAPI, "dev", "t", "", nil, ""))); c != api.CodeUnavailable {
		t.Fatalf("slot freed early: %q", c)
	}
	// A retry once the runner is back finishes it.
	e.live.wsFn = nil
	got, err = e.svc.CloseWorkspace(e.ctx, ann, SourceAPI, w.WsID)
	if err != nil || got.State != store.WorkspaceClosed {
		t.Fatalf("retry: %+v %v", got, err)
	}
	// Runner says not_found: the container is gone, still closed.
	w2, _, err := e.svc.OpenWorkspace(e.ctx, ann, SourceAPI, "dev", "u", "", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	e.live.wsFn = func(context.Context, int64, transport.WorkspaceRequest, io.Reader, io.Writer) (*transport.WorkspaceResult, error) {
		return &transport.WorkspaceResult{Code: transport.CodeNotFound}, nil
	}
	if g, err := e.svc.CloseWorkspace(e.ctx, ann, SourceAPI, w2.WsID); err != nil || g.State != store.WorkspaceClosed {
		t.Fatalf("not_found destroy: %+v %v", g, err)
	}
}

// failStore fails chosen mutations to prove the service fails closed.
type failStore struct {
	store.Store
	failReserve bool
	failClosing bool
}

func (f failStore) ReserveWorkspace(ctx context.Context, w store.Workspace, slots, maxPerOp int, now time.Time, ev store.AuditEvent) (store.Workspace, bool, error) {
	if f.failReserve {
		return store.Workspace{}, false, errors.New("audit disk full")
	}
	return f.Store.ReserveWorkspace(ctx, w, slots, maxPerOp, now, ev)
}

func (f failStore) SetWorkspaceState(ctx context.Context, id string, from []store.WorkspaceState, to store.WorkspaceState, reason string, now time.Time, ev store.AuditEvent) (store.Workspace, error) {
	if f.failClosing && to == store.WorkspaceClosing {
		return store.Workspace{}, errors.New("audit disk full")
	}
	return f.Store.SetWorkspaceState(ctx, id, from, to, reason, now, ev)
}

func TestWorkspaceStartRowFailsClosed(t *testing.T) {
	e := newEnv(t)
	e.wsFleet("dev", false, nil)
	e.wsRunner("dev", rspec{slots: 2})
	ann := e.operator("ann")
	w, _, err := e.svc.OpenWorkspace(e.ctx, ann, SourceAPI, "dev", "s", "", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	e.live.log.reset()
	real := e.svc.st

	e.svc.st = failStore{Store: real, failReserve: true}
	err = errOf3(e.svc.OpenWorkspace(e.ctx, ann, SourceAPI, "dev", "t", "", nil, ""))
	if code(t, err) != api.CodeInternal || strings.Contains(err.Error(), "disk") {
		t.Fatalf("open err = %v", err)
	}
	e.svc.st = failStore{Store: real, failClosing: true}
	err = errOf(e.svc.CloseWorkspace(e.ctx, ann, SourceAPI, w.WsID))
	if code(t, err) != api.CodeInternal {
		t.Fatalf("close err = %v", err)
	}
	if calls := e.log.get(); len(calls) != 0 {
		t.Fatalf("runner called without a start row: %v", calls)
	}
	e.svc.st = real
	if st, _ := e.db.GetWorkspace(e.ctx, w.WsID); st.State != store.WorkspaceOpen {
		t.Fatalf("state changed: %s", st.State)
	}
}

func errOf3[A, B any](_ A, _ B, err error) error { return err }
