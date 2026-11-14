package service

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// F-34: the guarded refusal text is the shared constant.
func TestMsgConfirmRequired(t *testing.T) {
	e := newEnv(t)
	e.fleet("prod")
	if _, err := e.svc.CreateFleet(e.ctx, e.admin, SourceCLI, "g", "actions", true, nil); err != nil {
		t.Fatal(err)
	}
	r := e.runnerAt("g", store.StatusActive)
	_, err := e.svc.RunAction(e.ctx, e.operator("ann"), SourceAPI, "g", r.Name, "x", nil, 0, "")
	if err == nil || !strings.Contains(err.Error(), api.MsgConfirmRequired) || api.MsgConfirmRequired != "fleet requires confirm to equal the fleet name" {
		t.Fatalf("err = %v", err)
	}
}

// F-43: another operator's exec/read/write/close is refused as not_found and audited as one denied
// row (requester as actor, the workspace id); an unknown id leaves no row; the owner's rows are unharmed.
func TestOwnershipRefusalAuditedDenied(t *testing.T) {
	e, ann, w := opsEnv(t, false, nil)
	bob := e.operator("bob")
	calls := map[string]func() error{
		api.MethodWorkspaceExec: func() error {
			return errOf(e.svc.ExecWorkspace(e.ctx, bob, SourceAPI, w.WsID, []string{"id"}, "", nil, 0, nil, 0, ""))
		},
		api.MethodWorkspaceReadFile: func() error {
			return errOf(e.svc.ReadWorkspaceFile(e.ctx, bob, SourceAPI, w.WsID, "a.txt", 0, 0, nil))
		},
		api.MethodWorkspaceWriteFile: func() error {
			return errOf(e.svc.WriteWorkspaceFile(e.ctx, bob, SourceAPI, w.WsID, "a.txt", 0, true, 0o644, 1, sum([]byte("x")), strings.NewReader("x"), ""))
		},
		api.MethodWorkspaceClose: func() error {
			return errOf(e.svc.CloseWorkspace(e.ctx, bob, SourceAPI, w.WsID))
		},
	}
	for m, call := range calls {
		if c := code(t, call()); c != api.CodeNotFound {
			t.Errorf("%s: code %q, want not_found", m, c)
		}
		rows := e.wsAudit(m)
		var mine []store.AuditEvent
		for _, r := range rows {
			if r.Actor == "bob" {
				mine = append(mine, r)
			}
		}
		if len(mine) != 1 || mine[0].Outcome != OutcomeDenied || mine[0].Workspace != w.WsID || mine[0].ActorKind != "operator" {
			t.Errorf("%s: bob's rows %+v", m, mine)
		}
		// Guarded refusals use the same shape: denied, requester as actor, workspace id.
	}
	// Unknown and malformed ids: not_found, no row.
	n := len(e.audit())
	for _, id := range []string{strings.Repeat("0", 32), "short"} {
		if c := code(t, errOf(e.svc.CloseWorkspace(e.ctx, bob, SourceAPI, id))); c != api.CodeNotFound {
			t.Errorf("id %q: %q", id, c)
		}
	}
	for _, a := range e.audit()[n:] {
		if a.Outcome == OutcomeDenied && a.Workspace != "" {
			t.Errorf("row for an unknown id: %+v", a)
		}
	}
	// The owner still works.
	e.live.wsFn = wsExecOK(0)
	if err := errOf(e.svc.ExecWorkspace(e.ctx, ann, SourceAPI, w.WsID, []string{"id"}, "", nil, 0, nil, 0, "")); err != nil {
		t.Fatalf("owner exec: %v", err)
	}
}

// F-41: a mode mismatch is one denied row in both directions.
func TestModeMismatchAuditedDenied(t *testing.T) {
	e := newEnv(t)
	e.fleet("prod")
	e.wsFleet("dev", false, nil)
	wr := e.wsRunner("dev", rspec{slots: 1})
	pr := e.runnerAt("prod", store.StatusActive)
	ann := e.operator("ann")

	if c := code(t, errOf(e.svc.RunAction(e.ctx, ann, SourceAPI, "dev", wr.Name, "uptime", nil, 0, ""))); c != api.CodeConflict {
		t.Fatalf("run_action on workspace fleet: %q", c)
	}
	if c := code(t, errOf3(e.svc.OpenWorkspace(e.ctx, ann, SourceAPI, "prod", "s", "", nil, ""))); c != api.CodeConflict {
		t.Fatalf("workspace.open on actions fleet: %q", c)
	}
	_ = pr
	for _, c := range []struct{ action, fleet string }{{api.MethodActionRun, "dev"}, {api.MethodWorkspaceOpen, "prod"}} {
		rows := e.wsAudit(c.action)
		if len(rows) != 1 || rows[0].Outcome != OutcomeDenied || rows[0].Actor != "ann" || rows[0].Fleet != c.fleet || rows[0].RequestID != "" {
			t.Errorf("%s: rows %+v", c.action, rows)
		}
	}
}

// F-27: a create that succeeded but could not be recorded as open leaves the row failed (not stuck
// opening), with an audit row, so the session can be reused at once.
func TestOpenWorkspaceStuckOpeningMarkedFailed(t *testing.T) {
	e := newEnv(t)
	e.wsFleet("dev", false, nil)
	e.wsRunner("dev", rspec{slots: 2})
	ann := e.operator("ann")
	real := e.svc.st
	e.svc.st = failStore{Store: real, failOpen: true}
	e.live.wsFn = func(context.Context, int64, transport.WorkspaceRequest, io.Reader, io.Writer) (*transport.WorkspaceResult, error) {
		return &transport.WorkspaceResult{Code: transport.CodeOK}, nil
	}
	if err := errOf3(e.svc.OpenWorkspace(e.ctx, ann, SourceAPI, "dev", "s", "", nil, "")); err == nil {
		t.Fatal("open succeeded")
	}
	e.svc.st = real
	ws, err := e.db.ListWorkspaces(e.ctx, store.WorkspaceFilter{Limit: 10})
	if err != nil || len(ws) != 1 || ws[0].State != store.WorkspaceFailed {
		t.Fatalf("rows after the failed open: %+v %v", ws, err)
	}
	var found bool
	for _, a := range e.wsAudit(api.MethodWorkspaceOpen) {
		if a.Workspace == ws[0].WsID && a.Outcome == OutcomeError && strings.HasPrefix(a.Message, "open failed") {
			found = true
			// The outcome row pairs with the start row through the request id.
			paired := false
			for _, b := range e.wsAudit(api.MethodWorkspaceOpen) {
				if b.Workspace == a.Workspace && b.Outcome != a.Outcome && a.RequestID != "" && b.RequestID == a.RequestID {
					paired = true
				}
			}
			if !paired {
				t.Errorf("failure row request id %q does not pair with a start row: %+v", a.RequestID, e.wsAudit(api.MethodWorkspaceOpen))
			}
		}
	}
	if !found {
		t.Fatalf("no failure audit row: %+v", e.wsAudit(api.MethodWorkspaceOpen))
	}
	// Session reuse is not blocked.
	w, reused, err := e.svc.OpenWorkspace(e.ctx, ann, SourceAPI, "dev", "s", "", nil, "")
	if err != nil || reused || w.State != store.WorkspaceOpen {
		t.Fatalf("reopen: %+v reused=%v %v", w, reused, err)
	}
}

// F-28: failStaleOpening only fails the rows; the destroy runs in the per-runner phase of the
// sweep, and its result is audited (ok, or error when the destroy fails).
func TestStaleOpeningDestroyInReapRunner(t *testing.T) {
	for _, destroyOK := range []bool{true, false} {
		t.Run(fmt.Sprint("destroyOK=", destroyOK), func(t *testing.T) {
			e := newEnv(t)
			clk := e.clock()
			e.wsFleet("dev", false, nil)
			r := e.wsRunner("dev", rspec{slots: 100})
			ann := e.operator("ann")
			stale := e.rowAt(1, "dev", r, ann, store.WorkspaceOpening, t0)
			clk.set(t0.Add(e.svc.openTimeout + e.svc.openMargin + 1))

			failed := e.svc.failStaleOpening(e.ctx)
			if len(failed[r.ID]) != 1 || failed[r.ID][0].WsID != stale.WsID || e.state(stale.WsID).State != store.WorkspaceFailed {
				t.Fatalf("failStaleOpening: %+v", failed)
			}
			if d := e.wsCalls("destroy"); len(d) != 0 {
				t.Fatalf("failStaleOpening destroyed inline: %v", d)
			}
			if !destroyOK {
				e.live.wsFn = func(context.Context, int64, transport.WorkspaceRequest, io.Reader, io.Writer) (*transport.WorkspaceResult, error) {
					return &transport.WorkspaceResult{Code: transport.CodeBusy}, nil
				}
			}
			e.svc.reapRunner(e.ctx, r.ID, failed[r.ID])
			rows := e.wsAudit(actionDestroy)
			if len(rows) != 1 || rows[0].Workspace != stale.WsID || rows[0].Source != string(SourceSystem) {
				t.Fatalf("destroy audit: %+v", rows)
			}
			wantOutcome := OutcomeOK
			if !destroyOK {
				wantOutcome = OutcomeError
			}
			if rows[0].Outcome != wantOutcome {
				t.Fatalf("destroy outcome %q, want %q", rows[0].Outcome, wantOutcome)
			}
		})
	}
}
