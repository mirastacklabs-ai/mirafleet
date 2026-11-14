package service

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// clock gives a test an adjustable service clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

func (e *env) clock() *clock {
	c := &clock{t: t0}
	e.svc.now = c.now
	return c
}

func (e *env) openWS(p auth.Principal, fleet, session string) store.Workspace {
	e.t.Helper()
	w, _, err := e.svc.OpenWorkspace(e.ctx, p, SourceAPI, fleet, session, "", nil, "")
	if err != nil {
		e.t.Fatal(err)
	}
	return w
}

func (e *env) state(id string) store.Workspace {
	e.t.Helper()
	w, err := e.db.GetWorkspace(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return w
}

// rowAt inserts a workspace row in the given state directly through the store.
func (e *env) rowAt(n int, fleet string, r store.Runner, op auth.Principal, state store.WorkspaceState, created time.Time) store.Workspace {
	e.t.Helper()
	f, err := e.db.GetFleet(e.ctx, fleet)
	if err != nil {
		e.t.Fatal(err)
	}
	ev := store.AuditEvent{Time: created, Source: "system", ActorKind: "system", Action: "test.row", Outcome: "ok"}
	w, _, err := e.db.ReserveWorkspace(e.ctx, store.Workspace{WsID: fmt.Sprintf("%032x", n), FleetID: f.ID, RunnerID: r.ID,
		OperatorID: op.ID, Session: fmt.Sprintf("s%d", n), Image: imgGo, Spec: `{}`, ExpiresAt: created.Add(1000 * time.Hour)}, 100, 0, created, ev)
	if err != nil {
		e.t.Fatal(err)
	}
	path := map[store.WorkspaceState][]store.WorkspaceState{
		store.WorkspaceOpening: nil,
		store.WorkspaceOpen:    {store.WorkspaceOpen},
		store.WorkspaceClosing: {store.WorkspaceOpen, store.WorkspaceClosing},
		store.WorkspaceClosed:  {store.WorkspaceOpen, store.WorkspaceClosing, store.WorkspaceClosed},
		store.WorkspaceFailed:  {store.WorkspaceFailed},
	}[state]
	cur := store.WorkspaceOpening
	for _, st := range path {
		if _, err := e.db.SetWorkspaceState(e.ctx, w.WsID, []store.WorkspaceState{cur}, st, "setup", created, ev); err != nil {
			e.t.Fatal(err)
		}
		cur = st
	}
	return e.state(w.WsID)
}

// listing makes the fake runner answer list with the given ids.
func (e *env) listing(ids ...string) {
	e.live.wsFn = func(_ context.Context, _ int64, req transport.WorkspaceRequest, _ io.Reader, _ io.Writer) (*transport.WorkspaceResult, error) {
		if req.Op != transport.OpList {
			return &transport.WorkspaceResult{Code: transport.CodeOK}, nil
		}
		res := &transport.WorkspaceResult{Code: transport.CodeOK}
		for _, id := range ids {
			res.List = append(res.List, transport.WorkspaceInfo{ID: id, State: "running"})
		}
		return res, nil
	}
}

func TestReapIdleAndMaxTTL(t *testing.T) {
	e := newEnv(t)
	clk := e.clock()
	e.wsFleet("dev", false, func(c *store.WorkspaceConfig) { c.IdleTTL, c.MaxTTL = time.Hour, 10*time.Hour })
	e.wsRunner("dev", rspec{slots: 4})
	ann := e.operator("ann")
	w1, w2 := e.openWS(ann, "dev", "s1"), e.openWS(ann, "dev", "s2")

	clk.set(t0.Add(30 * time.Minute))
	e.svc.ReapOnce(e.ctx)
	if e.state(w1.WsID).State != store.WorkspaceOpen || len(e.wsCalls("destroy")) != 0 {
		t.Fatal("reaped too early")
	}
	// w2 is used; w1 goes idle.
	if err := e.db.TouchWorkspace(e.ctx, w2.WsID, t0.Add(50*time.Minute)); err != nil {
		t.Fatal(err)
	}
	clk.set(t0.Add(90 * time.Minute))
	e.svc.ReapOnce(e.ctx)
	got := e.state(w1.WsID)
	if got.State != store.WorkspaceClosed || got.Reason != reasonIdle {
		t.Fatalf("idle: %+v", got)
	}
	if e.state(w2.WsID).State != store.WorkspaceOpen {
		t.Fatal("active workspace reaped")
	}
	if n := len(e.wsCalls("destroy")); n != 1 {
		t.Fatalf("destroy calls: %d", n)
	}
	// Kept busy, w2 still dies at its hard expiry.
	if err := e.db.TouchWorkspace(e.ctx, w2.WsID, t0.Add(10*time.Hour-time.Minute)); err != nil {
		t.Fatal(err)
	}
	clk.set(t0.Add(10 * time.Hour))
	e.svc.ReapOnce(e.ctx)
	got = e.state(w2.WsID)
	if got.State != store.WorkspaceClosed || got.Reason != reasonMaxTTL {
		t.Fatalf("max ttl: %+v", got)
	}
	// Every audit row of the reaper has source system.
	n := 0
	for _, a := range e.audit() {
		if a.Action == actionReap || a.Action == actionDestroy {
			n++
			if a.Source != "system" || a.Workspace == "" {
				t.Errorf("audit %+v", a)
			}
		}
	}
	if n != 4 {
		t.Fatalf("reaper audit rows = %d, want 4", n)
	}
}

func TestReapOfflineRunnerKeepsClosing(t *testing.T) {
	e := newEnv(t)
	clk := e.clock()
	e.wsFleet("dev", false, func(c *store.WorkspaceConfig) { c.IdleTTL, c.MaxTTL = time.Hour, 10*time.Hour })
	r := e.wsRunner("dev", rspec{slots: 1})
	w := e.openWS(e.operator("ann"), "dev", "s")
	e.setOnline(r.ID, false)
	clk.set(t0.Add(2 * time.Hour))
	e.svc.ReapOnce(e.ctx)
	e.svc.ReapOnce(e.ctx)
	if got := e.state(w.WsID); got.State != store.WorkspaceClosing {
		t.Fatalf("offline: %+v", got)
	}
	if len(e.wsCalls("destroy")) != 0 {
		t.Fatal("destroy attempted on an offline runner")
	}
	if c, _ := e.db.CountLiveWorkspaces(e.ctx, []int64{r.ID}); c[r.ID] != 1 {
		t.Fatalf("closing row must keep its slot: %v", c)
	}
	// Back online: the next sweep finishes it.
	e.setOnline(r.ID, true)
	e.svc.ReapOnce(e.ctx)
	if e.state(w.WsID).State != store.WorkspaceClosed {
		t.Fatal("not closed after the runner returned")
	}
}

func TestReapDestroyFailureStaysClosing(t *testing.T) {
	e := newEnv(t)
	clk := e.clock()
	e.wsFleet("dev", false, func(c *store.WorkspaceConfig) { c.IdleTTL, c.MaxTTL = time.Hour, 10*time.Hour })
	e.wsRunner("dev", rspec{slots: 1})
	w := e.openWS(e.operator("ann"), "dev", "s")
	e.live.wsFn = func(context.Context, int64, transport.WorkspaceRequest, io.Reader, io.Writer) (*transport.WorkspaceResult, error) {
		return &transport.WorkspaceResult{Code: transport.CodeBusy}, nil
	}
	clk.set(t0.Add(2 * time.Hour))
	e.svc.ReapOnce(e.ctx)
	if e.state(w.WsID).State != store.WorkspaceClosing {
		t.Fatal("refused destroy must leave the row closing")
	}
	e.live.wsFn = nil
	e.svc.ReapOnce(e.ctx)
	if e.state(w.WsID).State != store.WorkspaceClosed {
		t.Fatal("retry did not close")
	}
}

func TestRevokeOperatorCascade(t *testing.T) {
	e := newEnv(t)
	e.clock()
	e.wsFleet("dev", false, nil)
	e.wsRunner("dev", rspec{slots: 4})
	ann, bob := e.operator("ann"), e.operator("bob")
	wa, wb := e.openWS(ann, "dev", "s"), e.openWS(bob, "dev", "s")
	if _, err := e.svc.RevokeOperator(e.ctx, e.admin, SourceCLI, "ann"); err != nil {
		t.Fatal(err)
	}
	if got := e.state(wa.WsID); got.State != store.WorkspaceClosing || got.Reason != OperatorRevokeReason {
		t.Fatalf("cascade: %+v", got)
	}
	if e.state(wb.WsID).State != store.WorkspaceOpen {
		t.Fatal("bob's workspace touched")
	}
	ca := e.wsAudit(actionCloseAll)
	if len(ca) != 1 || ca[0].Source != "system" {
		t.Fatalf("cascade audit: %+v", ca)
	}
	e.svc.ReapOnce(e.ctx)
	if e.state(wa.WsID).State != store.WorkspaceClosed || e.state(wb.WsID).State != store.WorkspaceOpen {
		t.Fatal("sweep did not destroy the revoked operator's workspace")
	}
}

// If the cascade write never happened, the sweep still catches a revoked operator.
func TestReapBackstopRevokedOperator(t *testing.T) {
	e := newEnv(t)
	e.clock()
	e.wsFleet("dev", false, nil)
	e.wsRunner("dev", rspec{slots: 4})
	ann := e.operator("ann")
	w := e.openWS(ann, "dev", "s")
	if _, err := e.db.RevokeOperator(e.ctx, "ann", t0, store.AuditEvent{Time: t0, Source: "cli", ActorKind: "admin", Action: "operator.revoke", Outcome: "ok"}); err != nil {
		t.Fatal(err)
	}
	e.svc.ReapOnce(e.ctx)
	if got := e.state(w.WsID); got.State != store.WorkspaceClosed || got.Reason != reasonOpRevoked {
		t.Fatalf("backstop: %+v", got)
	}
}

func TestRevokeRunnerCascade(t *testing.T) {
	e := newEnv(t)
	e.clock()
	e.wsFleet("dev", false, nil)
	r1 := e.wsRunner("dev", rspec{slots: 4})
	ann := e.operator("ann")
	w := e.openWS(ann, "dev", "s")
	if _, err := e.svc.Revoke(e.ctx, e.admin, SourceCLI, "dev/"+r1.Name); err != nil {
		t.Fatal(err)
	}
	if got := e.state(w.WsID); got.State != store.WorkspaceClosing || got.Reason != RevokeReason {
		t.Fatalf("cascade: %+v", got)
	}
	ca := e.wsAudit(actionCloseAll)
	if len(ca) != 1 || ca[0].Source != "system" || ca[0].Runner != r1.Name {
		t.Fatalf("cascade audit: %+v", ca)
	}
	// The revoked runner never reconnects: no destroy is attempted and the sweep frees the row.
	e.setOnline(r1.ID, false)
	e.svc.ReapOnce(e.ctx)
	if got := e.state(w.WsID); got.State != store.WorkspaceLost || len(e.wsCalls("destroy")) != 0 {
		t.Fatalf("offline revoked runner: %+v", got)
	}
	if n := len(e.wsAudit(actionLost)); n != 1 {
		t.Fatalf("%d lost audit rows", n)
	}
}

// Closing rows on a revoked runner must not hold the operator's cap forever.
func TestRevokedRunnerFreesOperatorCap(t *testing.T) {
	e := newEnv(t)
	e.clock()
	e.svc.maxPerOp = 1
	e.wsFleet("dev", false, nil)
	r1 := e.wsRunner("dev", rspec{slots: 4})
	ann := e.operator("ann")
	w := e.openWS(ann, "dev", "s1")
	if _, err := e.svc.Revoke(e.ctx, e.admin, SourceCLI, "dev/"+r1.Name); err != nil {
		t.Fatal(err)
	}
	e.wsRunner("dev", rspec{slots: 4})
	if _, _, err := e.svc.OpenWorkspace(e.ctx, ann, SourceAPI, "dev", "s2", "", nil, ""); err == nil {
		t.Fatal("cap not enforced before the sweep")
	}
	e.setOnline(r1.ID, false)
	e.svc.ReapOnce(e.ctx)
	if got := e.state(w.WsID); got.State != store.WorkspaceLost {
		t.Fatalf("row: %+v", got)
	}
	e.openWS(ann, "dev", "s2")
}

// A runner revoke with no workspaces writes no cascade audit row.
func TestRevokeRunnerNoWorkspacesNoAudit(t *testing.T) {
	e := newEnv(t)
	e.wsFleet("dev", false, nil)
	r1 := e.wsRunner("dev", rspec{slots: 4})
	if _, err := e.svc.Revoke(e.ctx, e.admin, SourceCLI, "dev/"+r1.Name); err != nil {
		t.Fatal(err)
	}
	if n := len(e.wsAudit(actionCloseAll)); n != 0 {
		t.Fatalf("%d cascade rows", n)
	}
}

func TestReapBackstopRevokedRunner(t *testing.T) {
	e := newEnv(t)
	e.clock()
	e.wsFleet("dev", false, nil)
	r1 := e.wsRunner("dev", rspec{slots: 4})
	w := e.openWS(e.operator("ann"), "dev", "s")
	if _, err := e.db.SetRunnerStatus(e.ctx, r1.ID, []store.Status{store.StatusActive}, store.StatusRevoked, t0,
		store.AuditEvent{Time: t0, Source: "cli", ActorKind: "admin", Action: "runner.revoke", Outcome: "ok"}); err != nil {
		t.Fatal(err)
	}
	e.setOnline(r1.ID, false)
	e.svc.ReapOnce(e.ctx)
	if got := e.state(w.WsID); got.State != store.WorkspaceLost {
		t.Fatalf("backstop: %+v", got)
	}
}

func TestRecoverWorkspaces(t *testing.T) {
	e := newEnv(t)
	e.clock()
	e.wsFleet("dev", false, nil)
	r := e.wsRunner("dev", rspec{slots: 10})
	ann := e.operator("ann")
	op := e.rowAt(1, "dev", r, ann, store.WorkspaceOpening, t0)
	ok := e.rowAt(2, "dev", r, ann, store.WorkspaceOpen, t0)
	cl := e.rowAt(3, "dev", r, ann, store.WorkspaceClosing, t0)
	n, err := e.svc.RecoverWorkspaces(e.ctx)
	if err != nil || n != 1 {
		t.Fatalf("recover: %d %v", n, err)
	}
	if e.state(op.WsID).State != store.WorkspaceFailed || e.state(ok.WsID).State != store.WorkspaceOpen ||
		e.state(cl.WsID).State != store.WorkspaceClosing {
		t.Fatal("wrong rows moved")
	}
	a := e.wsAudit(actionRecover)
	if len(a) != 1 || a[0].Source != "system" || a[0].Workspace != op.WsID {
		t.Fatalf("audit: %+v", a)
	}
	// Idempotent.
	if n, err := e.svc.RecoverWorkspaces(e.ctx); err != nil || n != 0 {
		t.Fatalf("second recover: %d %v", n, err)
	}
}

func TestReconcileTable(t *testing.T) {
	e := newEnv(t)
	clk := e.clock()
	e.wsFleet("dev", false, nil)
	r := e.wsRunner("dev", rspec{slots: 100})
	ann := e.operator("ann")
	old := t0.Add(-time.Hour) // older than the open timeout
	clk.set(t0)

	openPresent := e.rowAt(1, "dev", r, ann, store.WorkspaceOpen, t0)
	openMissing := e.rowAt(2, "dev", r, ann, store.WorkspaceOpen, t0)
	closingPresent := e.rowAt(3, "dev", r, ann, store.WorkspaceClosing, t0)
	closingMissing := e.rowAt(4, "dev", r, ann, store.WorkspaceClosing, t0)
	openingOld := e.rowAt(5, "dev", r, ann, store.WorkspaceOpening, old)
	openingFresh := e.rowAt(6, "dev", r, ann, store.WorkspaceOpening, t0)
	closedPresent := e.rowAt(7, "dev", r, ann, store.WorkspaceClosed, t0)
	failedPresent := e.rowAt(8, "dev", r, ann, store.WorkspaceFailed, t0)
	orphan := fmt.Sprintf("%032x", 99)

	e.listing(openPresent.WsID, closingPresent.WsID, openingOld.WsID, openingFresh.WsID, closedPresent.WsID, failedPresent.WsID, orphan)
	var destroyedIDs []string
	inner := e.live.wsFn
	e.live.wsFn = func(ctx context.Context, id int64, req transport.WorkspaceRequest, b io.Reader, w io.Writer) (*transport.WorkspaceResult, error) {
		if req.Op == transport.OpDestroy {
			destroyedIDs = append(destroyedIDs, req.ID)
		}
		return inner(ctx, id, req, b, w)
	}

	e.svc.ReconcileRunner(e.ctx, r.ID)

	want := map[string]store.WorkspaceState{
		openPresent.WsID:    store.WorkspaceOpen,
		openMissing.WsID:    store.WorkspaceLost,
		closingPresent.WsID: store.WorkspaceClosed,
		closingMissing.WsID: store.WorkspaceClosed,
		openingOld.WsID:     store.WorkspaceFailed,
		openingFresh.WsID:   store.WorkspaceOpening,
		closedPresent.WsID:  store.WorkspaceClosed,
		failedPresent.WsID:  store.WorkspaceFailed,
	}
	for id, st := range want {
		if got := e.state(id).State; got != st {
			t.Errorf("%s: %s, want %s", id, got, st)
		}
	}
	wantDestroy := map[string]bool{closingPresent.WsID: true, openingOld.WsID: true, closedPresent.WsID: true, failedPresent.WsID: true, orphan: true}
	if len(destroyedIDs) != len(wantDestroy) {
		t.Fatalf("destroyed %v", destroyedIDs)
	}
	for _, id := range destroyedIDs {
		if !wantDestroy[id] {
			t.Errorf("unexpected destroy of %s", id)
		}
	}
	// System audit rows: lost, the orphan, and the transitions.
	if a := e.wsAudit(actionLost); len(a) != 1 || a[0].Source != "system" || a[0].Workspace != openMissing.WsID {
		t.Errorf("lost audit: %+v", a)
	}
	if a := e.wsAudit(actionOrphan); len(a) != 3 {
		t.Errorf("orphan audit rows: %+v", a)
	}
	for _, a := range e.audit() {
		if strings.HasPrefix(a.Action, "workspace.") && a.Action != "workspace.open" && a.Source != "system" {
			t.Errorf("non-system audit: %+v", a)
		}
	}
	// A lost row frees its slot.
	if c, _ := e.db.CountLiveWorkspaces(e.ctx, []int64{r.ID}); c[r.ID] != 2 { // openPresent + openingFresh
		t.Fatalf("live after reconcile: %v", c)
	}
	// Idempotent: a second pass leaves terminal rows terminal.
	destroyedIDs = nil
	e.svc.ReconcileRunner(e.ctx, r.ID)
	for id, st := range want {
		if st == store.WorkspaceClosed || st == store.WorkspaceFailed || st == store.WorkspaceLost {
			if got := e.state(id).State; got != st {
				t.Errorf("second pass %s: %s", id, got)
			}
		}
	}
}

func TestReconcileListFailureChangesNothing(t *testing.T) {
	e := newEnv(t)
	e.clock()
	e.wsFleet("dev", false, nil)
	r := e.wsRunner("dev", rspec{slots: 10})
	w := e.rowAt(1, "dev", r, e.operator("ann"), store.WorkspaceOpen, t0)
	e.live.wsFn = func(context.Context, int64, transport.WorkspaceRequest, io.Reader, io.Writer) (*transport.WorkspaceResult, error) {
		return nil, fmt.Errorf("runner not online")
	}
	e.svc.ReconcileRunner(e.ctx, r.ID)
	e.live.wsFn = func(context.Context, int64, transport.WorkspaceRequest, io.Reader, io.Writer) (*transport.WorkspaceResult, error) {
		return &transport.WorkspaceResult{Code: transport.CodeUnsupported}, nil
	}
	e.svc.ReconcileRunner(e.ctx, r.ID)
	if e.state(w.WsID).State != store.WorkspaceOpen {
		t.Fatal("a failed list must not mark rows lost")
	}
}

// No service lock is held across a Live call: a reconcile stuck in a runner call blocks neither
// another runner's reconcile nor a reaper sweep.
func TestNoLockAcrossLiveCalls(t *testing.T) {
	e := newEnv(t)
	e.clock()
	e.wsFleet("dev", false, nil)
	r1 := e.wsRunner("dev", rspec{slots: 10})
	r2 := e.wsRunner("dev", rspec{slots: 10})
	ann := e.operator("ann")
	e.rowAt(1, "dev", r2, ann, store.WorkspaceOpen, t0)

	entered, release := make(chan struct{}), make(chan struct{})
	e.live.wsFn = func(ctx context.Context, id int64, req transport.WorkspaceRequest, _ io.Reader, _ io.Writer) (*transport.WorkspaceResult, error) {
		if id == r1.ID && req.Op == transport.OpList {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		return &transport.WorkspaceResult{Code: transport.CodeOK}, nil
	}
	done := make(chan struct{})
	go func() { defer close(done); e.svc.ReconcileRunner(e.ctx, r1.ID) }()
	<-entered
	fin := make(chan struct{})
	go func() {
		defer close(fin)
		e.svc.ReconcileRunner(e.ctx, r2.ID)
		e.svc.ReapOnce(e.ctx)
	}()
	select {
	case <-fin:
	case <-time.After(5 * time.Second):
		t.Fatal("blocked behind a runner call")
	}
	close(release)
	<-done
	// The second runner's missing open row went lost.
	if got := e.state(fmt.Sprintf("%032x", 1)).State; got != store.WorkspaceLost {
		t.Fatalf("r2 row: %s", got)
	}
}

func TestRunReaperStopsOnCancel(t *testing.T) {
	e := newEnv(t)
	e.svc.reapEvery = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(e.ctx)
	done := make(chan struct{})
	go func() { e.svc.RunReaper(ctx); close(done) }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunReaper did not stop")
	}
}

func TestReconcileOpeningBoundary(t *testing.T) {
	e := newEnv(t)
	clk := e.clock()
	e.wsFleet("dev", false, nil)
	r := e.wsRunner("dev", rspec{slots: 100})
	ann := e.operator("ann")
	if e.svc.openMargin != DefaultOpenMargin {
		t.Fatalf("margin %v", e.svc.openMargin)
	}
	limit := e.svc.openTimeout + e.svc.openMargin
	atLimit := e.rowAt(1, "dev", r, ann, store.WorkspaceOpening, t0)
	inMargin := e.rowAt(2, "dev", r, ann, store.WorkspaceOpening, t0.Add(limit-e.svc.openTimeout-time.Second))
	past := e.rowAt(3, "dev", r, ann, store.WorkspaceOpening, t0.Add(-time.Second))
	clk.set(t0.Add(limit)) // atLimit: age == limit; inMargin: past the open timeout; past: limit+1s
	e.listing(atLimit.WsID, inMargin.WsID, past.WsID)
	e.svc.ReconcileRunner(e.ctx, r.ID)
	for w, want := range map[string]store.WorkspaceState{atLimit.WsID: store.WorkspaceOpening,
		inMargin.WsID: store.WorkspaceOpening, past.WsID: store.WorkspaceFailed} {
		if got := e.state(w).State; got != want {
			t.Errorf("%s: %s, want %s", w, got, want)
		}
	}
}

// E-16: the sweep fails opening rows older than open timeout + margin (even when the runner
// lists no container) with a system audit row and a best-effort destroy; younger rows and rows
// of an offline runner are handled without a runner call.
func TestReapFailsStaleOpening(t *testing.T) {
	e := newEnv(t)
	clk := e.clock()
	e.wsFleet("dev", false, nil)
	r := e.wsRunner("dev", rspec{slots: 100})
	off := e.wsRunner("dev", rspec{slots: 100, off: true})
	ann := e.operator("ann")
	limit := e.svc.openTimeout + e.svc.openMargin
	atLimit := e.rowAt(1, "dev", r, ann, store.WorkspaceOpening, t0)
	stale := e.rowAt(2, "dev", r, ann, store.WorkspaceOpening, t0.Add(-time.Second))
	staleOff := e.rowAt(3, "dev", off, ann, store.WorkspaceOpening, t0.Add(-time.Second))
	clk.set(t0.Add(limit)) // atLimit: age == limit (kept); the others: limit+1s
	e.svc.ReapOnce(e.ctx)
	for id, want := range map[string]store.WorkspaceState{atLimit.WsID: store.WorkspaceOpening,
		stale.WsID: store.WorkspaceFailed, staleOff.WsID: store.WorkspaceFailed} {
		if got := e.state(id).State; got != want {
			t.Errorf("%s: %s, want %s", id, got, want)
		}
	}
	if d := e.wsCalls("destroy"); len(d) != 1 || d[0] != fmt.Sprintf("workspace %d destroy", r.ID) {
		t.Fatalf("destroys %v, want one on the online runner", d)
	}
	if evs := e.wsAudit(actionRecover); len(evs) != 2 || evs[0].Source != string(SourceSystem) {
		t.Fatalf("recover audit: %+v", evs)
	}
}

func TestReapIdleNotBeforeCentralStart(t *testing.T) {
	e := newEnv(t)
	clk := e.clock()
	e.wsFleet("dev", false, func(c *store.WorkspaceConfig) { c.IdleTTL = time.Hour })
	e.wsRunner("dev", rspec{slots: 4})
	ann := e.operator("ann")
	w := e.openWS(ann, "dev", "s1") // last_used = t0

	// Central was down for 10 h; it restarts at t0+10h.
	clk.set(t0.Add(10 * time.Hour))
	e.svc.markStarted()
	clk.set(t0.Add(10*time.Hour + 30*time.Minute))
	e.svc.ReapOnce(e.ctx)
	if e.state(w.WsID).State != store.WorkspaceOpen {
		t.Fatal("reaped on a pre-restart last_used_at")
	}
	clk.set(t0.Add(11*time.Hour + time.Minute))
	e.svc.ReapOnce(e.ctx)
	if got := e.state(w.WsID); got.State != store.WorkspaceClosed || got.Reason != reasonIdle {
		t.Fatalf("after a full ttl since start: %+v", got)
	}
}

func TestReapSkipsInFlightOp(t *testing.T) {
	e, op, w := opsEnv(t, false, func(c *store.WorkspaceConfig) { c.IdleTTL = time.Hour })
	clk := e.clock()
	var during store.Workspace
	e.live.wsFn = func(ctx context.Context, id int64, req transport.WorkspaceRequest, b io.Reader, o io.Writer) (*transport.WorkspaceResult, error) {
		if req.Op != transport.OpExec {
			return &transport.WorkspaceResult{Code: transport.CodeOK}, nil
		}
		// A long exec: the clock runs past idle_ttl while it is in flight.
		clk.set(t0.Add(3 * time.Hour))
		e.svc.ReapOnce(e.ctx)
		during = e.state(w.WsID)
		return wsExecOK(0)(ctx, id, req, b, o)
	}
	clk.set(t0.Add(2 * time.Hour)) // dispatch time
	if _, err := e.svc.ExecWorkspace(e.ctx, op, SourceAPI, w.WsID, []string{"sleep", "1"}, "", nil, 0, nil, 0, ""); err != nil {
		t.Fatal(err)
	}
	if during.State != store.WorkspaceOpen {
		t.Fatalf("reaped mid-exec: %+v", during)
	}
	if !during.LastUsedAt.Equal(t0.Add(2 * time.Hour)) {
		t.Fatalf("not touched at dispatch: %v", during.LastUsedAt)
	}
	if e.svc.busy(w.WsID) {
		t.Fatal("in-flight marker leaked")
	}
}

func TestReconcileDeadContainerStateLost(t *testing.T) {
	e := newEnv(t)
	clk := e.clock()
	e.wsFleet("dev", false, nil)
	r := e.wsRunner("dev", rspec{slots: 100})
	ann := e.operator("ann")
	clk.set(t0)
	ws := map[string]store.Workspace{}
	for i, st := range []string{"stopped", "unknown", "terminated", "running", "created"} {
		ws[st] = e.rowAt(i+1, "dev", r, ann, store.WorkspaceOpen, t0)
	}
	e.live.wsFn = func(_ context.Context, _ int64, req transport.WorkspaceRequest, _ io.Reader, _ io.Writer) (*transport.WorkspaceResult, error) {
		res := &transport.WorkspaceResult{Code: transport.CodeOK}
		if req.Op == transport.OpList {
			for st, w := range ws {
				res.List = append(res.List, transport.WorkspaceInfo{ID: w.WsID, State: st})
			}
		}
		return res, nil
	}
	e.svc.ReconcileRunner(e.ctx, r.ID)
	for st, w := range ws {
		want := store.WorkspaceOpen
		if st == "stopped" || st == "unknown" || st == "terminated" {
			want = store.WorkspaceLost
		}
		if got := e.state(w.WsID).State; got != want {
			t.Errorf("%s: %s, want %s", st, got, want)
		}
	}
	if a := e.wsAudit(actionLost); len(a) != 3 || a[0].Source != "system" {
		t.Errorf("lost audit: %+v", a)
	}
}
