package service

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/central/registry"
	"github.com/mirastacklabs-ai/mirafleet/internal/policy"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/store/sqlite"
	"github.com/mirastacklabs-ai/mirafleet/internal/tokens"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

var t0 = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

// callLog is shared by the fake Live and the store wrapper so cross-component order is visible.
type callLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *callLog) add(s string) {
	l.mu.Lock()
	l.calls = append(l.calls, s)
	l.mu.Unlock()
}

func (l *callLog) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.calls...)
}

func (l *callLog) reset() {
	l.mu.Lock()
	l.calls = nil
	l.mu.Unlock()
}

type fakeLive struct {
	execFn    func(ctx context.Context, id int64, req transport.ExecRequest) (*transport.ExecResult, error)
	log       *callLog
	snap      []registry.SessionInfo
	statusErr error
}

func (f *fakeLive) Exec(ctx context.Context, id int64, req transport.ExecRequest) (*transport.ExecResult, error) {
	f.log.add(fmt.Sprintf("exec %d %s", id, req.Action))
	if f.execFn != nil {
		return f.execFn(ctx, id, req)
	}
	return nil, nil
}
func (f *fakeLive) Kick(id int64, reason string) { f.log.add(fmt.Sprintf("kick %d %s", id, reason)) }
func (f *fakeLive) Tombstone(id int64)           { f.log.add(fmt.Sprintf("tombstone %d", id)) }
func (f *fakeLive) ClearTombstone(id int64)      { f.log.add(fmt.Sprintf("cleartombstone %d", id)) }
func (f *fakeLive) SetStatus(id int64, st registry.Status) error {
	f.log.add(fmt.Sprintf("setstatus %d %s", id, st))
	return f.statusErr
}
func (f *fakeLive) Snapshot() []registry.SessionInfo { return f.snap }

// wrapStore logs the commit of SetRunnerStatus and can force it to fail.
type wrapStore struct {
	store.Store
	log     *callLog
	failSet bool
}

func (w *wrapStore) SetRunnerStatus(ctx context.Context, id int64, from []store.Status, to store.Status, now time.Time, ev store.AuditEvent) (store.Runner, error) {
	w.log.add(fmt.Sprintf("commit %d %s", id, to))
	if w.failSet {
		return store.Runner{}, fmt.Errorf("disk on fire: /secret/path/db.sqlite")
	}
	return w.Store.SetRunnerStatus(ctx, id, from, to, now, ev)
}

type env struct {
	t      *testing.T
	svc    *Service
	db     store.Store
	ws     *wrapStore
	live   *fakeLive
	conns  *fakeConns
	log    *callLog
	logs   *bytes.Buffer
	host   ssh.PublicKey
	admin  auth.Principal
	ctx    context.Context
	nameN  int
	logMu  sync.Mutex
	policy policy.PolicyEngine
}

func newKey(t *testing.T) (wire []byte, fp string) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return k.Marshal(), ssh.FingerprintSHA256(k)
}

func newEnv(t *testing.T) *env { return newEnvPolicy(t, policy.TablePolicy{}) }

func newEnvPolicy(t *testing.T, pol policy.PolicyEngine) *env {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "mirafleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	hostPub, _, _ := ed25519.GenerateKey(rand.Reader)
	host, err := ssh.NewPublicKey(hostPub)
	if err != nil {
		t.Fatal(err)
	}
	log := &callLog{}
	e := &env{t: t, db: db, log: log, logs: &bytes.Buffer{}, host: host, ctx: ctx, policy: pol,
		live: &fakeLive{log: log}, admin: auth.LocalAdmin(501)}
	e.ws = &wrapStore{Store: db, log: log}
	e.conns = &fakeConns{}
	svc, err := New(Config{Store: e.ws, Policy: pol, Live: e.live, HostKey: host, OperatorConns: e.conns,
		Now: func() time.Time { return t0 },
		Logf: func(f string, a ...any) {
			e.logMu.Lock()
			fmt.Fprintf(e.logs, f+"\n", a...)
			e.logMu.Unlock()
		}})
	if err != nil {
		t.Fatal(err)
	}
	e.svc = svc
	return e
}

func (e *env) audit() []store.AuditEvent {
	e.t.Helper()
	evs, err := e.db.ListAudit(e.ctx, 0, 10000)
	if err != nil {
		e.t.Fatal(err)
	}
	return evs
}

func (e *env) fleet(name string) {
	e.t.Helper()
	if _, err := e.svc.CreateFleet(e.ctx, e.admin, SourceCLI, name, "", false, nil); err != nil {
		e.t.Fatal(err)
	}
}

// enrollRunner adds a runner via a real service-made token and store.Enroll. Returns the stored
// runner (status pending, or active when approve).
func (e *env) enrollRunner(fleet, name string, approve bool) store.Runner {
	e.t.Helper()
	ct, err := e.svc.CreateToken(e.ctx, e.admin, SourceCLI, TokenParams{Fleet: fleet, Approve: approve})
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
	return r
}

// runnerAt returns a runner in the wanted status.
func (e *env) runnerAt(fleet string, st store.Status) store.Runner {
	e.t.Helper()
	e.nameN++
	name := fmt.Sprintf("r%d", e.nameN)
	r := e.enrollRunner(fleet, name, st == store.StatusActive || st == store.StatusDisabled)
	ref := fleet + "/" + name
	switch st {
	case store.StatusDisabled:
		if _, err := e.svc.Disable(e.ctx, e.admin, SourceCLI, ref); err != nil {
			e.t.Fatal(err)
		}
	case store.StatusRevoked:
		if _, err := e.svc.Revoke(e.ctx, e.admin, SourceCLI, ref); err != nil {
			e.t.Fatal(err)
		}
	}
	return r
}

func (e *env) operator(name string) auth.Principal {
	e.t.Helper()
	wire, fp := newKey(e.t)
	op, _, err := e.db.RegisterOperator(e.ctx, name, wire, fp, t0,
		store.AuditEvent{Time: t0, Source: "enroll", ActorKind: "enroll", Action: "enroll.register", Outcome: "ok"})
	if err != nil {
		e.t.Fatal(err)
	}
	return auth.Principal{Kind: auth.KindOperator, ID: op.ID, Name: name, Fingerprint: fp, Remote: "10.0.0.1:2222", Status: "active"}
}

func code(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		return ""
	}
	ae, ok := err.(*api.Error)
	if !ok {
		t.Fatalf("error is %T (%v), want *api.Error", err, err)
	}
	return ae.Code
}

func TestNewRequiresDeps(t *testing.T) {
	e := newEnv(t)
	good := Config{Store: e.db, Policy: policy.TablePolicy{}, Live: e.live, HostKey: e.host, OperatorConns: e.conns, Now: func() time.Time { return t0 }}
	if _, err := New(good); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*Config){
		"store":  func(c *Config) { c.Store = nil },
		"policy": func(c *Config) { c.Policy = nil },
		"live":   func(c *Config) { c.Live = nil },
		"host":   func(c *Config) { c.HostKey = nil },
		"now":    func(c *Config) { c.Now = nil },
		"conns":  func(c *Config) { c.OperatorConns = nil },
	} {
		c := good
		mut(&c)
		if _, err := New(c); err == nil {
			t.Errorf("missing %s accepted", name)
		}
	}
}

func TestFleets(t *testing.T) {
	e := newEnv(t)
	f, err := e.svc.CreateFleet(e.ctx, e.admin, SourceCLI, "prod", store.ModeWorkspace, true, wsCfg())
	if err != nil {
		t.Fatal(err)
	}
	if f.Mode != store.ModeWorkspace || !f.Confirm || !f.CreatedAt.Equal(t0) {
		t.Fatalf("fleet = %+v", f)
	}
	evs := e.audit()
	if len(evs) != 1 {
		t.Fatalf("audit rows = %d", len(evs))
	}
	ev := evs[0]
	if ev.Source != "cli" || ev.ActorKind != "admin" || ev.Actor != e.admin.Name || ev.Subject != "prod" ||
		ev.Fleet != "prod" || ev.Action != api.MethodFleetCreate || ev.Outcome != "ok" || !ev.Time.Equal(t0) {
		t.Fatalf("audit = %+v", ev)
	}
	if c := code(t, errOf(e.svc.CreateFleet(e.ctx, e.admin, SourceCLI, "nows", store.ModeWorkspace, false, nil))); c != api.CodeBadRequest {
		t.Fatalf("workspace fleet without config: %q", c)
	}
	if c := code(t, errOf(e.svc.CreateFleet(e.ctx, e.admin, SourceCLI, "ax", store.ModeActions, false, wsCfg()))); c != api.CodeBadRequest {
		t.Fatalf("actions fleet with config: %q", c)
	}
	// Default mode.
	g, err := e.svc.CreateFleet(e.ctx, e.admin, SourceGUI, "stage", "", false, nil)
	if err != nil || g.Mode != store.ModeActions {
		t.Fatalf("default mode: %+v %v", g, err)
	}
	// Errors.
	if c := code(t, errOf(e.svc.CreateFleet(e.ctx, e.admin, SourceCLI, "prod", "", false, nil))); c != api.CodeConflict {
		t.Errorf("duplicate fleet code = %q", c)
	}
	if c := code(t, errOf(e.svc.CreateFleet(e.ctx, e.admin, SourceCLI, "Bad Name", "", false, nil))); c != api.CodeBadRequest {
		t.Errorf("bad name code = %q", c)
	}
	if c := code(t, errOf(e.svc.CreateFleet(e.ctx, e.admin, SourceCLI, "x", "weird", false, nil))); c != api.CodeBadRequest {
		t.Errorf("bad mode code = %q", c)
	}
	if c := code(t, errOf(e.svc.CreateFleet(e.ctx, e.admin, "bogus", "y", "", false, nil))); c != api.CodeBadRequest {
		t.Errorf("bad source code = %q", c)
	}
	if n := len(e.audit()); n != 2 {
		t.Errorf("failed creates wrote audit rows: %d", n)
	}
	// Counts and get.
	e.runnerAt("prod", store.StatusPending)
	e.runnerAt("prod", store.StatusActive)
	e.runnerAt("prod", store.StatusRevoked)
	fs, err := e.svc.ListFleets(e.ctx, e.admin, SourceCLI)
	if err != nil || len(fs) != 2 || fs[0].Name != "prod" || fs[0].RunnerCount != 2 || fs[1].RunnerCount != 0 {
		t.Fatalf("list = %+v %v", fs, err)
	}
	if got, err := e.svc.GetFleet(e.ctx, e.admin, SourceCLI, "stage"); err != nil || got.Name != "stage" {
		t.Fatalf("get = %+v %v", got, err)
	}
	if c := code(t, errOf(e.svc.GetFleet(e.ctx, e.admin, SourceCLI, "nope"))); c != api.CodeNotFound {
		t.Errorf("missing fleet code = %q", c)
	}
	if c := code(t, errOf(e.svc.GetFleet(e.ctx, e.admin, SourceCLI, "../x"))); c != api.CodeBadRequest {
		t.Errorf("bad fleet name code = %q", c)
	}
}

// errOf drops the first result of a two-value call.
func errOf[T any](_ T, err error) error { return err }

func TestLifecycleTable(t *testing.T) {
	e := newEnv(t)
	e.fleet("prod")
	type tc struct {
		from store.Status
		op   string
		to   store.Status // "" = must fail
		code string
	}
	cases := []tc{
		{store.StatusPending, api.OpApprove, store.StatusActive, ""},
		{store.StatusPending, api.OpDisable, "", api.CodeConflict},
		{store.StatusPending, api.OpEnable, "", api.CodeConflict},
		{store.StatusPending, api.OpRevoke, store.StatusRevoked, ""},
		{store.StatusActive, api.OpApprove, "", api.CodeConflict},
		{store.StatusActive, api.OpDisable, store.StatusDisabled, ""},
		{store.StatusActive, api.OpEnable, "", api.CodeConflict},
		{store.StatusActive, api.OpRevoke, store.StatusRevoked, ""},
		{store.StatusDisabled, api.OpApprove, "", api.CodeConflict},
		{store.StatusDisabled, api.OpDisable, "", api.CodeConflict},
		{store.StatusDisabled, api.OpEnable, store.StatusActive, ""},
		{store.StatusDisabled, api.OpRevoke, store.StatusRevoked, ""},
		// Revoked runners are no longer addressable by name.
		{store.StatusRevoked, api.OpApprove, "", api.CodeNotFound},
		{store.StatusRevoked, api.OpDisable, "", api.CodeNotFound},
		{store.StatusRevoked, api.OpEnable, "", api.CodeNotFound},
		{store.StatusRevoked, api.OpRevoke, "", api.CodeNotFound},
	}
	for _, c := range cases {
		t.Run(string(c.from)+"_"+c.op, func(t *testing.T) {
			r := e.runnerAt("prod", c.from)
			before := len(e.audit())
			e.log.reset()
			ref := "prod/" + r.Name
			got, err := e.svc.Lifecycle(e.ctx, e.admin, SourceGUI, ref, c.op)
			after := e.audit()
			if c.to == "" {
				if got := code(t, err); got != c.code {
					t.Fatalf("code = %q, want %q (err %v)", got, c.code, err)
				}
				if len(after) != before {
					t.Fatalf("failed op wrote audit rows")
				}
				if len(e.log.get()) != 0 {
					t.Fatalf("failed op touched live: %v", e.log.get())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != c.to {
				t.Fatalf("status = %s, want %s", got.Status, c.to)
			}
			if len(after) != before+1 {
				t.Fatalf("audit rows %d -> %d, want +1", before, len(after))
			}
			ev := after[len(after)-1]
			if ev.Source != "gui" || ev.ActorKind != "admin" || ev.Actor != e.admin.Name || ev.Action != "runner."+c.op ||
				ev.Subject != ref || ev.Fleet != "prod" || ev.Runner != r.Name || ev.Outcome != "ok" ||
				ev.Params["from"] != string(c.from) || ev.Params["to"] != string(c.to) || !ev.Time.Equal(t0) {
				t.Fatalf("audit = %+v", ev)
			}
			// Store agrees.
			if c.to != store.StatusRevoked {
				cur, err := e.db.GetRunner(e.ctx, store.Ref{Fleet: "prod", Runner: r.Name})
				if err != nil || cur.Status != c.to {
					t.Fatalf("stored = %+v %v", cur, err)
				}
			} else if ev.Message != RevokeReason {
				t.Fatalf("revoke message = %q", ev.Message)
			}
		})
	}
}

func TestLifecycleValidation(t *testing.T) {
	e := newEnv(t)
	e.fleet("prod")
	for _, ref := range []string{"", "prod", "prod/", "/x", "Prod/x", "prod/../x", strings.Repeat("a", 500) + "/x"} {
		if c := code(t, errOf(e.svc.Approve(e.ctx, e.admin, SourceCLI, ref))); c != api.CodeBadRequest {
			t.Errorf("ref %.20q code = %q", ref, c)
		}
	}
	if c := code(t, errOf(e.svc.Approve(e.ctx, e.admin, SourceCLI, "prod/ghost"))); c != api.CodeNotFound {
		t.Errorf("unknown runner code = %q", c)
	}
	if c := code(t, errOf(e.svc.Lifecycle(e.ctx, e.admin, SourceCLI, "prod/x", "explode"))); c != api.CodeBadRequest {
		t.Errorf("unknown op code = %q", c)
	}
}

func TestRevokeOrdersTombstoneKickBeforeCommit(t *testing.T) {
	e := newEnv(t)
	e.fleet("prod")
	r := e.runnerAt("prod", store.StatusActive)
	e.log.reset()
	if _, err := e.svc.Revoke(e.ctx, e.admin, SourceCLI, "prod/"+r.Name); err != nil {
		t.Fatal(err)
	}
	want := []string{
		fmt.Sprintf("tombstone %d", r.ID),
		fmt.Sprintf("kick %d runner revoked", r.ID),
		fmt.Sprintf("commit %d revoked", r.ID),
	}
	if got := e.log.get(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	// Revoked key never re-enrols: the id is gone from GetRunner.
	if _, err := e.db.GetRunner(e.ctx, store.Ref{Fleet: "prod", Runner: r.Name}); err == nil {
		t.Fatal("revoked runner still addressable")
	}
}

func TestRevokeCommitFailureClearsTombstone(t *testing.T) {
	e := newEnv(t)
	e.fleet("prod")
	r := e.runnerAt("prod", store.StatusActive)
	rows := len(e.audit())
	e.log.reset()
	e.ws.failSet = true
	err := errOf(e.svc.Revoke(e.ctx, e.admin, SourceCLI, "prod/"+r.Name))
	if c := code(t, err); c != api.CodeInternal {
		t.Fatalf("code = %q", c)
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "sqlite") {
		t.Fatalf("internal detail leaked: %v", err)
	}
	want := []string{
		fmt.Sprintf("tombstone %d", r.ID),
		fmt.Sprintf("kick %d runner revoked", r.ID),
		fmt.Sprintf("commit %d revoked", r.ID),
		fmt.Sprintf("cleartombstone %d", r.ID),
	}
	if got := e.log.get(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	if len(e.audit()) != rows {
		t.Fatal("failed revoke wrote an audit row")
	}
	cur, err := e.db.GetRunner(e.ctx, store.Ref{Fleet: "prod", Runner: r.Name})
	if err != nil || cur.Status != store.StatusActive {
		t.Fatalf("runner = %+v %v", cur, err)
	}
	if !strings.Contains(e.logs.String(), "disk on fire") {
		t.Error("internal error not logged server-side")
	}
}

func TestDisableEnableLiveCalls(t *testing.T) {
	e := newEnv(t)
	e.fleet("prod")
	r := e.runnerAt("prod", store.StatusActive)
	e.log.reset()
	if _, err := e.svc.Disable(e.ctx, e.admin, SourceCLI, "prod/"+r.Name); err != nil {
		t.Fatal(err)
	}
	if got := e.log.get(); len(got) != 2 || got[0] != fmt.Sprintf("commit %d disabled", r.ID) || got[1] != fmt.Sprintf("setstatus %d disabled", r.ID) {
		t.Fatalf("disable calls = %v", got)
	}
	e.log.reset()
	if _, err := e.svc.Enable(e.ctx, e.admin, SourceCLI, "prod/"+r.Name); err != nil {
		t.Fatal(err)
	}
	if got := e.log.get(); len(got) != 2 || got[1] != fmt.Sprintf("setstatus %d active", r.ID) {
		t.Fatalf("enable calls = %v", got)
	}
	p := e.runnerAt("prod", store.StatusPending)
	e.log.reset()
	if _, err := e.svc.Approve(e.ctx, e.admin, SourceCLI, "prod/"+p.Name); err != nil {
		t.Fatal(err)
	}
	if got := e.log.get(); len(got) != 2 || got[1] != fmt.Sprintf("setstatus %d active", p.ID) {
		t.Fatalf("approve calls = %v", got)
	}
	for _, c := range e.log.get() {
		if strings.HasPrefix(c, "kick") || strings.HasPrefix(c, "tombstone") {
			t.Fatalf("unexpected %q", c)
		}
	}
}

func TestLiveSetStatusErrorIsLoggedNotFatal(t *testing.T) {
	e := newEnv(t)
	e.fleet("prod")
	r := e.runnerAt("prod", store.StatusPending)
	e.live.statusErr = fmt.Errorf("registry exploded")
	if _, err := e.svc.Approve(e.ctx, e.admin, SourceCLI, "prod/"+r.Name); err != nil {
		t.Fatalf("approve failed: %v", err)
	}
	if !strings.Contains(e.logs.String(), "registry exploded") {
		t.Error("SetStatus error not logged")
	}
}

// callAll invokes every admin-only method; each must fail with forbidden.
func callAll(e *env, p auth.Principal) map[string]error {
	ctx := e.ctx
	return map[string]error{
		"CreateFleet": errOf(e.svc.CreateFleet(ctx, p, SourceAPI, "zzz", "", false, nil)),
		"CreateToken": errOf(e.svc.CreateToken(ctx, p, SourceAPI, TokenParams{Fleet: "prod"})),
		"RevokeToken": e.svc.RevokeToken(ctx, p, SourceAPI, "0123456789abcdef"),
		"ListTokens":  errOf(e.svc.ListTokens(ctx, p, SourceAPI, "")),
		"Approve":     errOf(e.svc.Approve(ctx, p, SourceAPI, "prod/x")),
		"Disable":     errOf(e.svc.Disable(ctx, p, SourceAPI, "prod/x")),
		"Enable":      errOf(e.svc.Enable(ctx, p, SourceAPI, "prod/x")),
		"Revoke":      errOf(e.svc.Revoke(ctx, p, SourceAPI, "prod/x")),
		"Lifecycle":   errOf(e.svc.Lifecycle(ctx, p, SourceAPI, "prod/x", api.OpApprove)),

		"OperatorCode":             errOf(e.svc.OperatorCode(ctx, p, SourceAPI)),
		"RotateRegistrationSecret": e.svc.RotateRegistrationSecret(ctx, p, SourceAPI),
		"InitRegistrationSecret":   e.svc.InitRegistrationSecret(ctx, p, SourceAPI),
		"ListOperators":            errOf(e.svc.ListOperators(ctx, p, SourceAPI, true)),
		"RevokeOperator":           errOf(e.svc.RevokeOperator(ctx, p, SourceAPI, "alice")),
	}
}

// Tests here and in enroll_test.go swap e.svc.now on a per-test env and never call t.Parallel:
// keep them serial.
func TestNonAdminForbiddenAndAudited(t *testing.T) {
	// Run under the real policy and under an allow-everything policy: the service's own kind
	// check must still refuse (double gate).
	for _, pc := range []struct {
		name string
		pol  policy.PolicyEngine
	}{{"table", policy.TablePolicy{}}, {"allow-all", allowAll{}}} {
		t.Run(pc.name, func(t *testing.T) {
			e := newEnvPolicy(t, pc.pol)
			e.fleet("prod")
			r := e.runnerAt("prod", store.StatusPending)
			e.log.reset()
			ops := map[string]auth.Principal{
				"operator": e.operator("alice"),
				"enroll":   {Kind: auth.KindEnroll, Name: "", Fingerprint: "SHA256:x", Remote: "9.9.9.9:1"},
				"runner":   {Kind: auth.KindRunner, ID: r.ID, Name: r.Name, Fingerprint: "SHA256:y", Remote: "8.8.8.8:1", Status: "pending"},
			}
			for pname, p := range ops {
				before := len(e.audit())
				errs := callAll(e, p)
				if p.Kind != auth.KindOperator { // operators may legitimately run actions
					_, errs["RunAction"] = e.svc.RunAction(e.ctx, p, SourceAPI, "prod", r.Name, "uptime", nil, 0, "")
				}
				for m, err := range errs {
					if c := code(t, err); c != api.CodeForbidden {
						t.Errorf("%s %s: code %q", pname, m, c)
					}
				}
				evs := e.audit()
				if len(evs) != before+len(errs) {
					t.Fatalf("%s: audit rows +%d, want +%d", pname, len(evs)-before, len(errs))
				}
				for _, ev := range evs[before:] {
					if ev.Outcome != OutcomeDenied || ev.Source != "api" || ev.ActorKind != string(p.Kind) ||
						ev.Actor != p.Name || ev.Remote != p.Remote {
						t.Errorf("%s: denied audit = %+v", pname, ev)
					}
				}
			}
			if len(e.log.get()) != 0 {
				t.Fatalf("denied calls touched live: %v", e.log.get())
			}
			cur, _ := e.db.GetRunner(e.ctx, store.Ref{Fleet: "prod", Runner: r.Name})
			if cur.Status != store.StatusPending {
				t.Fatalf("status changed to %s", cur.Status)
			}
			if _, err := e.db.GetFleet(e.ctx, "zzz"); err == nil {
				t.Fatal("fleet created by non-admin")
			}
			if _, err := e.db.GetSetting(e.ctx, store.SettingRegistrationSecret); err == nil {
				t.Fatal("registration secret created by non-admin")
			}
		})
	}
}

type allowAll struct{}

func (allowAll) Authorize(context.Context, auth.Principal, string, policy.Resource) error { return nil }

func TestReadMethodsByPrincipal(t *testing.T) {
	e := newEnv(t)
	e.fleet("prod")
	e.runnerAt("prod", store.StatusActive)
	op := e.operator("alice")
	if _, err := e.svc.ListFleets(e.ctx, op, SourceAPI); err != nil {
		t.Errorf("active operator ListFleets: %v", err)
	}
	if _, err := e.svc.ListRunners(e.ctx, op, SourceAPI, "prod"); err != nil {
		t.Errorf("active operator ListRunners: %v", err)
	}
	if _, err := e.svc.GetFleet(e.ctx, op, SourceAPI, "prod"); err != nil {
		t.Errorf("active operator GetFleet: %v", err)
	}
	// Operator revoked after authentication: forbidden and audited.
	if _, err := e.db.RevokeOperator(e.ctx, "alice", t0, store.AuditEvent{Time: t0, Source: "cli", ActorKind: "admin", Action: "operator.revoke", Outcome: "ok"}); err != nil {
		t.Fatal(err)
	}
	before := len(e.audit())
	if c := code(t, errOf(e.svc.ListFleets(e.ctx, op, SourceAPI))); c != api.CodeForbidden {
		t.Errorf("revoked operator code = %q", c)
	}
	if evs := e.audit(); len(evs) != before+1 || evs[len(evs)-1].Outcome != OutcomeDenied {
		t.Errorf("revoked operator denial not audited")
	}
	for name, p := range map[string]auth.Principal{
		"enroll": {Kind: auth.KindEnroll, Fingerprint: "SHA256:x"},
		"runner": {Kind: auth.KindRunner, ID: 1, Name: "x", Fingerprint: "SHA256:y"},
		"ghost":  {Kind: auth.KindOperator, ID: 9999, Name: "ghost"},
	} {
		if c := code(t, errOf(e.svc.ListFleets(e.ctx, p, SourceAPI))); c != api.CodeForbidden {
			t.Errorf("%s ListFleets code = %q", name, c)
		}
		if c := code(t, errOf(e.svc.ListRunners(e.ctx, p, SourceAPI, "prod"))); c != api.CodeForbidden {
			t.Errorf("%s ListRunners code = %q", name, c)
		}
	}
}

func TestListRunnersMergesLive(t *testing.T) {
	e := newEnv(t)
	e.fleet("prod")
	a := e.runnerAt("prod", store.StatusActive)
	b := e.runnerAt("prod", store.StatusPending)
	gone := e.runnerAt("prod", store.StatusRevoked)
	hb := t0.Add(-5 * time.Second)
	e.live.snap = []registry.SessionInfo{
		{ID: a.ID, Fleet: "prod", Name: a.Name, Online: true, LastHeartbeat: hb},
		{ID: gone.ID, Online: true},
	}
	rs, err := e.svc.ListRunners(e.ctx, e.admin, SourceCLI, "prod")
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 {
		t.Fatalf("runners = %d (revoked must be hidden)", len(rs))
	}
	if !rs[0].Online || !rs[0].LastHeartbeat.Equal(hb) || !rs[0].LastSeenAt.Equal(hb) || rs[0].Name != a.Name {
		t.Errorf("online runner = %+v", rs[0])
	}
	if rs[1].Online || !rs[1].LastHeartbeat.IsZero() || rs[1].Name != b.Name {
		t.Errorf("offline runner = %+v", rs[1])
	}
	if c := code(t, errOf(e.svc.ListRunners(e.ctx, e.admin, SourceCLI, "nope"))); c != api.CodeNotFound {
		t.Errorf("unknown fleet code = %q", c)
	}
	if c := code(t, errOf(e.svc.ListRunners(e.ctx, e.admin, SourceCLI, "BAD"))); c != api.CodeBadRequest {
		t.Errorf("bad fleet code = %q", c)
	}
}

func TestTokenDefaultsAndCaps(t *testing.T) {
	e := newEnv(t)
	e.fleet("prod")
	ct, err := e.svc.CreateToken(e.ctx, e.admin, SourceCLI, TokenParams{Fleet: "prod", Labels: map[string]string{"role": "web"}, Approve: true})
	if err != nil {
		t.Fatal(err)
	}
	if ct.MaxUses != 1 || !ct.Expires.Equal(t0.Add(24*time.Hour)) {
		t.Fatalf("defaults: %+v", ct)
	}
	id, _, hash, err := tokens.ParseJoinToken(ct.Token)
	if err != nil || id != ct.ID {
		t.Fatalf("token id mismatch: %v", err)
	}
	if want := tokens.HostKeyHash(e.host); hash != want {
		t.Fatalf("host key hash %q, want %q", hash, want)
	}
	ts, err := e.svc.ListTokens(e.ctx, e.admin, SourceCLI, "prod")
	if err != nil || len(ts) != 1 {
		t.Fatalf("list = %+v %v", ts, err)
	}
	got := ts[0]
	if got.TokenID != ct.ID || got.MaxUses != 1 || !got.AutoApprove || got.CreatedBy != e.admin.Name ||
		got.Labels["role"] != "web" || got.SecretHash != nil || !got.ExpiresAt.Equal(ct.Expires) {
		t.Fatalf("stored token = %+v", got)
	}
	// Boundaries.
	if ct, err := e.svc.CreateToken(e.ctx, e.admin, SourceCLI, TokenParams{Fleet: "prod", Uses: MaxTokenUses, TTL: MaxTokenTTL}); err != nil || ct.MaxUses != MaxTokenUses || !ct.Expires.Equal(t0.Add(MaxTokenTTL)) {
		t.Fatalf("max boundary: %+v %v", ct, err)
	}
	bad := []TokenParams{
		{Fleet: "prod", Uses: MaxTokenUses + 1},
		{Fleet: "prod", Uses: -1},
		{Fleet: "prod", TTL: MaxTokenTTL + time.Second},
		{Fleet: "prod", TTL: -time.Hour},
		{Fleet: "BAD!"},
		{Fleet: "prod", Labels: map[string]string{"": "x"}},
		{Fleet: "prod", Labels: map[string]string{"k": strings.Repeat("v", 1000)}},
	}
	rows := len(e.audit())
	for i, p := range bad {
		if c := code(t, errOf(e.svc.CreateToken(e.ctx, e.admin, SourceCLI, p))); c != api.CodeBadRequest {
			t.Errorf("bad[%d] code = %q", i, c)
		}
	}
	if c := code(t, errOf(e.svc.CreateToken(e.ctx, e.admin, SourceCLI, TokenParams{Fleet: "ghost"}))); c != api.CodeNotFound {
		t.Errorf("unknown fleet code = %q", c)
	}
	if len(e.audit()) != rows {
		t.Error("failed token creation wrote audit rows")
	}
	// Audit of a good one.
	ev := e.audit()[1]
	if ev.Action != api.MethodTokenCreate || ev.Subject != ct.ID || ev.Fleet != "prod" || ev.Source != "cli" || ev.Params["uses"] != "1" || ev.Params["ttl_s"] != "86400" {
		t.Fatalf("token audit = %+v", ev)
	}
}

func TestRevokeTokenAndList(t *testing.T) {
	e := newEnv(t)
	e.fleet("prod")
	e.fleet("stage")
	a, _ := e.svc.CreateToken(e.ctx, e.admin, SourceCLI, TokenParams{Fleet: "prod"})
	if _, err := e.svc.CreateToken(e.ctx, e.admin, SourceCLI, TokenParams{Fleet: "stage"}); err != nil {
		t.Fatal(err)
	}
	if ts, _ := e.svc.ListTokens(e.ctx, e.admin, SourceCLI, ""); len(ts) != 2 {
		t.Fatalf("all tokens = %d", len(ts))
	}
	before := len(e.audit())
	if err := e.svc.RevokeToken(e.ctx, e.admin, SourceGUI, a.ID); err != nil {
		t.Fatal(err)
	}
	evs := e.audit()
	if len(evs) != before+1 {
		t.Fatalf("audit rows +%d", len(evs)-before)
	}
	ev := evs[len(evs)-1]
	if ev.Action != api.MethodTokenRevoke || ev.Subject != a.ID || ev.Source != "gui" || ev.Outcome != "ok" {
		t.Fatalf("audit = %+v", ev)
	}
	if err := e.svc.RevokeToken(e.ctx, e.admin, SourceGUI, a.ID); err != nil {
		t.Fatalf("second revoke: %v", err)
	}
	if c := code(t, e.svc.RevokeToken(e.ctx, e.admin, SourceGUI, "0123456789abcdef")); c != api.CodeNotFound {
		t.Errorf("unknown token code = %q", c)
	}
	if c := code(t, e.svc.RevokeToken(e.ctx, e.admin, SourceGUI, "NOTANID")); c != api.CodeBadRequest {
		t.Errorf("bad id code = %q", c)
	}
	if c := code(t, errOf(e.svc.ListTokens(e.ctx, e.admin, SourceCLI, "BAD!"))); c != api.CodeBadRequest {
		t.Errorf("bad fleet code = %q", c)
	}
	// A revoked token is stale at enrol.
	_, secret, _, _ := tokens.ParseJoinToken(a.Token)
	h := sha256.Sum256(secret)
	wire, fp := newKey(t)
	_, err := e.db.Enroll(e.ctx, a.ID, h[:], t0, store.Runner{Name: "x", PubKey: wire, Fingerprint: fp}, store.AuditEvent{Time: t0, Action: "enroll.join"})
	if ToAPIError(err).Code != api.CodeConflict {
		t.Errorf("stale enrol maps to %v", ToAPIError(err))
	}
}

func TestTokenSecretNeverLeaks(t *testing.T) {
	e := newEnv(t)
	e.fleet("prod")
	ct, err := e.svc.CreateToken(e.ctx, e.admin, SourceCLI, TokenParams{Fleet: "prod", Approve: true, Uses: 3})
	if err != nil {
		t.Fatal(err)
	}
	_, secret, _, _ := tokens.ParseJoinToken(ct.Token)
	parts := strings.Split(ct.Token, ".")
	secretB64 := parts[2]
	if len(secret) != 32 {
		t.Fatal("bad secret")
	}
	// Exercise paths that log or error, then scan everything.
	e.enrollRunner("prod", "web1", true)
	e.ws.failSet = true
	_, _ = e.svc.Revoke(e.ctx, e.admin, SourceCLI, "prod/web1")
	e.ws.failSet = false
	_ = e.svc.RevokeToken(e.ctx, e.admin, SourceCLI, ct.ID)
	_, _ = e.svc.CreateToken(e.ctx, e.admin, SourceCLI, TokenParams{Fleet: "ghost"})
	ts, _ := e.svc.ListTokens(e.ctx, e.admin, SourceCLI, "")
	var haystack strings.Builder
	for _, ev := range e.audit() {
		fmt.Fprintf(&haystack, "%+v\n", ev)
	}
	haystack.WriteString(e.logs.String())
	for _, tk := range ts {
		fmt.Fprintf(&haystack, "%+v\n", tk)
	}
	h := haystack.String()
	for what, needle := range map[string]string{"token": ct.Token, "secret": secretB64, "hash": fmt.Sprintf("%x", sha256.Sum256(secret))} {
		if strings.Contains(h, needle) {
			t.Errorf("%s leaked into audit/logs/list", what)
		}
	}
	if strings.Contains(h, "mfj1.") {
		t.Error("token prefix in audit/logs")
	}
	if !strings.Contains(h, ct.ID) {
		t.Error("token id should be audited")
	}
}

func TestToAPIError(t *testing.T) {
	apiErr := api.Errorf(api.CodeTooLarge, "big")
	cases := []struct {
		err  error
		code string
	}{
		{nil, ""},
		{apiErr, api.CodeTooLarge},
		{fmt.Errorf("wrap: %w", apiErr), api.CodeTooLarge},
		{store.ErrNotFound, api.CodeNotFound},
		{fmt.Errorf("sql: no rows /var/db/x: %w", store.ErrNotFound), api.CodeNotFound},
		{store.ErrConflict, api.CodeConflict},
		{store.ErrStale, api.CodeConflict},
		{store.ErrInvalid, api.CodeBadRequest},
		{policy.ErrForbidden, api.CodeForbidden},
		{auth.ErrDenied, api.CodeForbidden},
		{fmt.Errorf("UNIQUE constraint failed: runners.pubkey"), api.CodeInternal},
	}
	for _, c := range cases {
		got := ToAPIError(c.err)
		if c.err == nil {
			if got != nil {
				t.Errorf("nil -> %v", got)
			}
			continue
		}
		if got.Code != c.code {
			t.Errorf("%v -> %q, want %q", c.err, got.Code, c.code)
		}
		for _, leak := range []string{"/var/db", "sql", "UNIQUE", "pubkey", "wrap"} {
			if c.code != api.CodeTooLarge && strings.Contains(got.Message, leak) {
				t.Errorf("%v: message %q leaks %q", c.err, got.Message, leak)
			}
		}
	}
	if got := ToAPIError(store.ErrStale); got.Message != "conflict" {
		t.Errorf("stale message = %q, want generic conflict", got.Message)
	}
	if got := ToAPIError(fmt.Errorf("boom")); got.Message != "internal error" {
		t.Errorf("internal message = %q", got.Message)
	}
}

func TestConcurrentRevokeKeepsTombstone(t *testing.T) {
	e := newEnv(t)
	e.fleet("prod")
	for i := 0; i < 20; i++ {
		r := e.runnerAt("prod", store.StatusActive)
		e.log.reset()
		revokesBefore := 0
		for _, a := range e.audit() {
			if a.Action == "runner."+api.OpRevoke && a.Runner == r.Name {
				revokesBefore++
			}
		}
		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make([]error, 2)
		for j := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, errs[j] = e.svc.Revoke(e.ctx, e.admin, SourceCLI, "prod/"+r.Name)
			}()
		}
		close(start)
		wg.Wait()
		if (errs[0] == nil) == (errs[1] == nil) {
			t.Fatalf("round %d: errs = %v, want exactly one success", i, errs)
		}
		cur, err := e.db.GetRunnerByID(e.ctx, r.ID)
		if err != nil || cur.Status != store.StatusRevoked {
			t.Fatalf("round %d: runner = %+v %v", i, cur, err)
		}
		tomb := fmt.Sprintf("tombstone %d", r.ID)
		clear := fmt.Sprintf("cleartombstone %d", r.ID)
		sawTomb := false
		for _, c := range e.log.get() {
			if c == tomb {
				sawTomb = true
			}
			if c == clear {
				t.Fatalf("round %d: tombstone cleared after a committed revoke: %v", i, e.log.get())
			}
		}
		if !sawTomb {
			t.Fatalf("round %d: no tombstone", i)
		}
		revokes := 0
		for _, a := range e.audit() {
			if a.Action == "runner."+api.OpRevoke && a.Runner == r.Name && a.Outcome == OutcomeOK {
				revokes++
			}
		}
		if revokes-revokesBefore != 1 {
			t.Fatalf("round %d: %d revoke audit rows, want 1", i, revokes-revokesBefore)
		}
	}
}

// wsCfg returns a valid workspace-fleet configuration.
func wsCfg() *store.WorkspaceConfig {
	return &store.WorkspaceConfig{
		Executor: store.ExecutorContainerd, DefaultImage: "docker.io/library/golang:1.27",
		Images: []string{"docker.io/library/golang:1.27", "ghcr.io/acme/dev@sha256:*"},
		Pull:   store.PullIfNotPresent, Network: store.NetworkNone,
		CPUMilli: 2000, MemoryMB: 4096, Pids: 1024, TmpfsMB: 512, DiskMB: 10240,
		IdleTTL: time.Hour, MaxTTL: 2 * time.Hour,
		ExecDefault: 120 * time.Second, ExecMax: 30 * time.Minute,
	}
}
