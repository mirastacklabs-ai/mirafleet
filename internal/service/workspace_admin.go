package service

import (
	"context"
	"io"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
)

// StdinDeferred as ExecWorkspace's stdinLen means "the stdin body is not read yet": stdin must
// then implement DeferredBody. The front end uses it so that a body frame is read from the wire
// only after policy, ownership, state, input and confirm checks have passed (docs/M3.md 1.1).
const StdinDeferred int64 = -1

// DeferredBody supplies a request body (and its exact length) on demand. ExecWorkspace calls
// Body at most once, after all gating checks. An error from Body is returned to the caller as is.
type DeferredBody interface {
	io.Reader
	Body() (r io.Reader, length int64, err error)
}

// adminListLimit is the most rows workspace.admin_list returns.
const adminListLimit = 1000

// AdminListWorkspaces lists workspaces of every operator, optionally of one fleet, in any state
// (live and recent). Admin only (double gate).
func (s *Service) AdminListWorkspaces(ctx context.Context, p auth.Principal, src Source, fleet string) ([]store.Workspace, error) {
	if err := s.authorize(ctx, p, src, api.MethodWorkspaceAdminList, true); err != nil {
		return nil, err
	}
	if fleet != "" && !store.ValidFleetName(fleet) {
		return nil, badRequest("invalid fleet name")
	}
	ws, err := s.st.ListWorkspaces(ctx, store.WorkspaceFilter{Fleet: fleet, Limit: adminListLimit})
	if err != nil {
		return nil, s.fail("list workspaces", err)
	}
	return ws, nil
}

// KillWorkspace closes any workspace regardless of its owner: opening or open -> closing ->
// destroy on the runner -> closed, like CloseWorkspace (an offline runner leaves it closing for
// the reaper). Admin only (double gate); both audit rows carry the admin as actor.
func (s *Service) KillWorkspace(ctx context.Context, p auth.Principal, src Source, wsID string) (store.Workspace, error) {
	return s.KillWorkspaceDelete(ctx, p, src, wsID, false)
}

// KillWorkspaceDelete is KillWorkspace; with del (host fleets only) the runner also deletes the
// session directory.
func (s *Service) KillWorkspaceDelete(ctx context.Context, p auth.Principal, src Source, wsID string, del bool) (store.Workspace, error) {
	if err := s.authorize(ctx, p, src, api.MethodWorkspaceKill, true); err != nil {
		return store.Workspace{}, err
	}
	if !workspace.ValidID(workspace.ID(wsID)) {
		return store.Workspace{}, api.Errorf(api.CodeNotFound, "not found")
	}
	w, err := s.st.GetWorkspace(ctx, wsID)
	if err != nil {
		return store.Workspace{}, s.fail("get workspace", err)
	}
	if del && !w.State.Terminal() {
		if err := s.requireHostFleet(ctx, w.Fleet); err != nil {
			return store.Workspace{}, err
		}
	}
	if w.State.Terminal() {
		return w, nil
	}
	rid, err := newRequestID()
	if err != nil {
		return store.Workspace{}, s.fail("request id", err)
	}
	start := s.now()
	ev := s.wsEvent(p, src, api.MethodWorkspaceKill, w.Fleet, w.Runner, w.WsID, rid)
	ev.Time, ev.Subject, ev.Outcome = start, w.Session, OutcomeStart
	if del {
		ev.Params = map[string]string{"delete": "true"}
	}
	if w.State == store.WorkspaceOpen || w.State == store.WorkspaceOpening {
		w, err = s.st.SetWorkspaceState(ctx, w.WsID, []store.WorkspaceState{store.WorkspaceOpen, store.WorkspaceOpening},
			store.WorkspaceClosing, "killed by admin", start, ev)
		if err != nil {
			return store.Workspace{}, s.fail("kill workspace", err)
		}
	} else if _, err := s.st.RecordAudit(ctx, ev); err != nil { // retry of a closing workspace
		s.logf("service: start audit for %s failed, refusing destroy: %v", rid, err)
		return store.Workspace{}, api.Errorf(api.CodeUnavailable, "audit unavailable")
	}

	dctx, cancel := context.WithTimeout(ctx, workspaceCloseTimeout)
	res, lerr := s.live.Workspace(dctx, w.RunnerID, transport.WorkspaceRequest{Op: transport.OpDestroy, ID: w.WsID,
		TimeoutMs: workspaceCloseTimeout.Milliseconds(), Delete: del}, nil, nil)
	cancel()

	end := s.wsEvent(p, src, api.MethodWorkspaceKill, w.Fleet, w.Runner, w.WsID, rid)
	end.Subject = w.Session
	end.DurationMS = s.now().Sub(start).Milliseconds()
	switch {
	case lerr == nil && res != nil && (res.Code == transport.CodeOK || res.Code == transport.CodeNotFound):
		end.Outcome = OutcomeOK
		cw, serr := s.finishWorkspace(ctx, w.WsID, store.WorkspaceClosing, store.WorkspaceClosed, "killed by admin", end)
		if serr != nil {
			return store.Workspace{}, s.fail("kill workspace", serr)
		}
		return cw, nil
	case lerr != nil:
		s.logf("service: destroy %s on runner %d failed: %v", w.WsID, w.RunnerID, lerr)
		end.Outcome, end.Message = OutcomeError, "runner unavailable; workspace stays closing"
		s.recordBestEffort(ctx, end)
		return w, nil
	default:
		code := "empty result"
		if res != nil {
			code = res.Code
		}
		end.Outcome, end.Message = OutcomeError, "destroy refused: "+code+"; workspace stays closing"
		s.recordBestEffort(ctx, end)
		return w, nil // E-04: the closing row, as for an offline runner; the reaper finishes it
	}
}
