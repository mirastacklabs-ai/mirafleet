package service

import (
	"strings"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
)

// F-47: an unknown workspace id and a foreign one are indistinguishable to the caller, and each
// writes exactly one denied audit row.
func TestF47UnknownAndForeignWorkspaceRefusedAlike(t *testing.T) {
	e := newEnv(t)
	e.wsFleet("dev", false, nil)
	e.wsRunner("dev", rspec{slots: 2})
	ann, bob := e.operator("ann"), e.operator("bob")
	w := e.openWS(ann, "dev", "s")

	unknown := strings.Repeat("ab", 16)
	var errs []error
	for _, id := range []string{w.WsID, unknown} {
		before := len(e.wsAudit(api.MethodWorkspaceClose))
		_, err := e.svc.CloseWorkspace(e.ctx, bob, SourceAPI, id)
		errs = append(errs, err)
		rows := e.wsAudit(api.MethodWorkspaceClose)
		if len(rows) != before+1 || rows[len(rows)-1].Outcome != OutcomeDenied {
			t.Fatalf("id %s: want one new denied row, got %d -> %d", id, before, len(rows))
		}
	}
	for _, err := range errs {
		if err == nil || ToAPIError(err).Code != api.CodeNotFound {
			t.Fatalf("want not_found, got %v", err)
		}
	}
	if errs[0].Error() != errs[1].Error() {
		t.Fatalf("errors differ: %q vs %q", errs[0], errs[1])
	}
}

// F-48: the reaper audits stale opening rows of an offline runner.
func TestF48StaleOpeningOfflineRunnerAudited(t *testing.T) {
	e := newEnv(t)
	clk := e.clock()
	e.wsFleet("dev", false, nil)
	r := e.wsRunner("dev", rspec{slots: 2})
	w := e.rowAt(1, "dev", r, e.operator("ann"), "opening", t0)
	e.setOnline(r.ID, false)
	clk.set(t0.Add(48 * time.Hour))
	e.svc.ReapOnce(e.ctx)
	if got := e.state(w.WsID); got.State != "failed" {
		t.Fatalf("state = %s", got.State)
	}
	var n int
	for _, a := range e.wsAudit(actionDestroy) {
		if a.Workspace == w.WsID && a.Outcome == OutcomeError {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("destroy error rows = %d, want 1", n)
	}
}
