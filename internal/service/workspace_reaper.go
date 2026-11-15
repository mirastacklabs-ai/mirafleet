package service

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
)

// DefaultReaperInterval is the period of the workspace reaper sweep (M3.md 1.6).
const DefaultReaperInterval = 60 * time.Second

// DefaultOpenMargin is added to the open timeout before reconcile treats an opening row as stuck,
// so a create still finishing on the runner is not destroyed under its caller.
const DefaultOpenMargin = 30 * time.Second

// sweepLimit is the most rows one query of a sweep reads (the store caps lists at 1000). Rows
// beyond it are picked up by later sweeps as earlier ones leave the queried state.
const sweepLimit = 1000

// Audit actions written by the reaper, reconcile and the revoke cascades (source "system").
const (
	actionReap     = "workspace.reap"      // open -> closing: idle, max ttl, revoked owner
	actionDestroy  = "workspace.destroy"   // closing -> closed by the reaper or reconcile
	actionOrphan   = "workspace.orphan"    // a container with no live row was destroyed
	actionLost     = "workspace.lost"      // an open row whose container is gone
	actionRecover  = "workspace.recover"   // opening -> failed at central startup or reconcile
	actionCloseAll = "workspace.close_all" // revoke cascade
)

// Reasons stored on rows the reaper closes.
const (
	reasonIdle      = "idle ttl expired"
	reasonMaxTTL    = "max ttl expired"
	reasonOpRevoked = OperatorRevokeReason
	reasonRunRevoke = RevokeReason
)

var liveStates = []store.WorkspaceState{store.WorkspaceOpening, store.WorkspaceOpen, store.WorkspaceClosing}

// sysEvent builds a source-system audit row about workspace w.
func (s *Service) sysEvent(action string, w store.Workspace, msg string) store.AuditEvent {
	return store.AuditEvent{Time: s.now(), Source: string(SourceSystem), ActorKind: string(SourceSystem), Actor: "reaper",
		Action: action, Outcome: OutcomeOK, Fleet: w.Fleet, Runner: w.Runner, Workspace: w.WsID, Subject: w.Session, Message: msg}
}

// setState moves a row with a system audit row, surviving a cancelled sweep context. A conflict
// (someone else moved the row first) reports false without an error.
func (s *Service) setState(ctx context.Context, w store.Workspace, from, to store.WorkspaceState, reason, action string) bool {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), outcomeWriteTimeout)
	defer cancel()
	_, err := s.st.SetWorkspaceState(wctx, w.WsID, []store.WorkspaceState{from}, to, reason, s.now(), s.sysEvent(action, w, reason))
	if err != nil {
		if !errors.Is(err, store.ErrConflict) && !errors.Is(err, store.ErrNotFound) {
			s.logf("service: workspace %s %s->%s: %v", w.WsID, from, to, err)
		}
		return false
	}
	return true
}

// destroyOn asks runnerID to destroy workspace id. It reports whether the container is gone
// (destroyed, or not found). No lock is held across the call.
func (s *Service) destroyOn(ctx context.Context, runnerID int64, id string) bool {
	dctx, cancel := context.WithTimeout(ctx, workspaceCloseTimeout)
	defer cancel()
	res, err := s.live.Workspace(dctx, runnerID, transport.WorkspaceRequest{Op: transport.OpDestroy, ID: id,
		TimeoutMs: workspaceCloseTimeout.Milliseconds()}, nil, nil)
	if err != nil {
		if ctx.Err() == nil {
			s.logf("service: destroy %s on runner %d: %v", id, runnerID, err)
		}
		return false
	}
	return res != nil && (res.Code == transport.CodeOK || res.Code == transport.CodeNotFound)
}

// RunReaper sweeps every ReaperInterval until ctx is done. It returns only after any sweep in
// progress has finished, so a caller that waits for it can close the store afterwards.
func (s *Service) RunReaper(ctx context.Context) {
	s.markStarted()
	t := time.NewTicker(s.reapEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.ReapOnce(ctx)
		}
	}
}

// ReapOnce runs one sweep: it moves idle, expired and revoked-owner open rows to closing, then
// destroys the closing rows of every online runner. Rows of an offline runner stay closing (they
// hold their slot) until the runner returns and reconcile runs.
func (s *Service) ReapOnce(ctx context.Context) {
	open, err := s.st.ListWorkspaces(ctx, store.WorkspaceFilter{States: []store.WorkspaceState{store.WorkspaceOpen}, Limit: sweepLimit})
	if err != nil {
		if ctx.Err() == nil {
			s.logf("service: reaper list open: %v", err)
		}
		return
	}
	now := s.now()
	idle := map[string]time.Duration{}
	opGone, runGone := map[int64]bool{}, map[int64]bool{}
	for _, w := range open {
		if ctx.Err() != nil {
			return
		}
		reason := ""
		switch {
		case s.operatorRevoked(ctx, w.OperatorID, opGone):
			reason = reasonOpRevoked
		case s.runnerRevoked(ctx, w.RunnerID, runGone):
			reason = reasonRunRevoke
		case !w.ExpiresAt.IsZero() && !now.Before(w.ExpiresAt):
			reason = reasonMaxTTL
		default:
			ttl, ok := idle[w.Fleet]
			if !ok {
				if c, err := s.st.GetFleetWorkspace(ctx, w.Fleet); err == nil {
					ttl = c.IdleTTL
				}
				idle[w.Fleet] = ttl
			}
			if ttl > 0 && s.idleRef(w).Add(ttl).Before(now) && !s.busy(w.WsID) {
				reason = reasonIdle
			}
		}
		if reason != "" {
			s.setState(ctx, w, store.WorkspaceOpen, store.WorkspaceClosing, reason, actionReap)
		}
	}

	s.freeDeadRunnerRows(ctx)
	stale := s.failStaleOpening(ctx)

	// C-13: one goroutine per online runner, at most reapParallel at once, each with its own
	// time budget, so one hung runner cannot stall the others. ReapOnce still returns only after
	// every goroutine has finished (RunReaper's "stops before store close" guarantee).
	var wg sync.WaitGroup
	sem := make(chan struct{}, reapParallel)
	launched := map[int64]bool{}
	for _, si := range s.live.Snapshot() {
		if ctx.Err() != nil {
			break
		}
		if !si.Online {
			continue
		}
		launched[si.ID] = true
		sem <- struct{}{}
		wg.Add(1)
		go func(runnerID int64) {
			defer func() { <-sem; wg.Done() }()
			rctx, cancel := context.WithTimeout(ctx, reapRunnerBudget)
			defer cancel()
			s.reapRunner(rctx, runnerID, stale[runnerID])
		}(si.ID)
	}
	wg.Wait()
	// F-48: stale rows of runners that were offline (or never reached) get a destroy row too.
	for id, rows := range stale {
		if !launched[id] {
			s.auditStaleSkipped(ctx, rows, "stale opening: runner offline; reconcile will retry")
		}
	}
}

// auditStaleSkipped writes the workspace.destroy row (outcome error) for stale rows whose
// container could not be destroyed this sweep (F-48).
func (s *Service) auditStaleSkipped(ctx context.Context, rows []store.Workspace, msg string) {
	for _, w := range rows {
		ev := s.sysEvent(actionDestroy, w, msg)
		ev.Outcome = OutcomeError
		s.recordBestEffort(ctx, ev)
	}
}

// reapParallel bounds the runners destroyed against concurrently; reapRunnerBudget bounds one
// runner's share of a sweep.
const (
	reapParallel     = 8
	reapRunnerBudget = 3 * workspaceCloseTimeout
)

// reapRunner destroys the closing rows of one runner and marks them closed. staleFailed are rows
// of this runner just failed by failStaleOpening (F-28): their containers get a best-effort
// destroy here, with the result audited.
func (s *Service) reapRunner(ctx context.Context, runnerID int64, staleFailed []store.Workspace) {
	for i, w := range staleFailed {
		if ctx.Err() != nil {
			s.auditStaleSkipped(ctx, staleFailed[i:], "stale opening: sweep budget exhausted; reconcile will retry")
			return
		}
		ev := s.sysEvent(actionDestroy, w, "stale opening: destroyed")
		if !s.destroyOn(ctx, w.RunnerID, w.WsID) {
			ev.Outcome, ev.Message = OutcomeError, "stale opening: destroy failed; reconcile will retry"
		}
		s.recordBestEffort(ctx, ev)
	}
	rows, err := s.st.ListWorkspaces(ctx, store.WorkspaceFilter{RunnerID: runnerID,
		States: []store.WorkspaceState{store.WorkspaceClosing}, Limit: sweepLimit})
	if err != nil {
		if ctx.Err() == nil {
			s.logf("service: reaper list closing: %v", err)
		}
		return
	}
	for _, w := range rows {
		if ctx.Err() != nil {
			return
		}
		if s.destroyOn(ctx, w.RunnerID, w.WsID) {
			s.setState(ctx, w, store.WorkspaceClosing, store.WorkspaceClosed, w.Reason, actionDestroy)
		}
	}
}

// freeDeadRunnerRows moves closing rows of a revoked or vanished runner to lost: that runner can
// never come back to be told to destroy them, and they would count toward the owner's cap forever.
// Its host keeps the containers (cleaned by `runner workspace prune`).
func (s *Service) freeDeadRunnerRows(ctx context.Context) {
	rows, err := s.st.ListWorkspaces(ctx, store.WorkspaceFilter{States: []store.WorkspaceState{store.WorkspaceClosing}, Limit: sweepLimit})
	if err != nil {
		if ctx.Err() == nil {
			s.logf("service: reaper list closing (dead runners): %v", err)
		}
		return
	}
	dead := map[int64]bool{}
	for _, w := range rows {
		if ctx.Err() != nil {
			return
		}
		gone, ok := dead[w.RunnerID]
		if !ok {
			r, err := s.st.GetRunnerByID(ctx, w.RunnerID)
			switch {
			case err == nil:
				gone = r.Status == store.StatusRevoked
			case errors.Is(err, store.ErrNotFound):
				gone = true
			default:
				continue
			}
			dead[w.RunnerID] = gone
		}
		if gone {
			s.setState(ctx, w, store.WorkspaceClosing, store.WorkspaceLost, "runner revoked or gone; container left on its host", actionLost)
		}
	}
}

// failStaleOpening moves opening rows older than the open timeout plus margin to failed (E-16).
// A live create is bounded by the open timeout, so such a row is stuck: its OpenWorkspace call
// died after the create, or never recorded the result. ReconcileRunner already fails these when
// the runner lists the container, but only on a runner's first heartbeat and not when the create
// never produced a container; this sweep covers the rest. A best-effort destroy follows when the
// runner is online (the row is already failed, so reconcile also reaps any leftover container).
//
// F-28: the destroys are not done here. The rows just failed are returned per runner and
// destroyed (and audited) inside the budgeted, parallel reapRunner phase.
func (s *Service) failStaleOpening(ctx context.Context) map[int64][]store.Workspace {
	rows, err := s.st.ListWorkspaces(ctx, store.WorkspaceFilter{States: []store.WorkspaceState{store.WorkspaceOpening}, Limit: sweepLimit})
	if err != nil {
		if ctx.Err() == nil {
			s.logf("service: reaper list opening: %v", err)
		}
		return nil
	}
	limit := s.openTimeout + s.openMargin
	failed := map[int64][]store.Workspace{}
	for _, w := range rows {
		if ctx.Err() != nil {
			return failed
		}
		if s.now().Sub(w.CreatedAt) <= limit {
			continue
		}
		if !s.setState(ctx, w, store.WorkspaceOpening, store.WorkspaceFailed, "open timed out", actionRecover) {
			continue
		}
		failed[w.RunnerID] = append(failed[w.RunnerID], w)
	}
	return failed
}

func (s *Service) operatorRevoked(ctx context.Context, id int64, cache map[int64]bool) bool {
	if v, ok := cache[id]; ok {
		return v
	}
	op, err := s.st.GetOperatorByID(ctx, id)
	v := err == nil && op.Status == store.OperatorRevoked
	if err == nil || errors.Is(err, store.ErrNotFound) {
		cache[id] = v
	}
	return v
}

func (s *Service) runnerRevoked(ctx context.Context, id int64, cache map[int64]bool) bool {
	if v, ok := cache[id]; ok {
		return v
	}
	r, err := s.st.GetRunnerByID(ctx, id)
	v := err == nil && r.Status == store.StatusRevoked
	if err == nil || errors.Is(err, store.ErrNotFound) {
		cache[id] = v
	}
	return v
}

// closeWorkspacesOf is the revoke cascade: it moves the owner's opening and open workspaces to
// closing in one transaction with its own system audit row. Nothing is written when the owner
// holds none. A failure is only logged: the reaper is the backstop.
func (s *Service) closeWorkspacesOf(ctx context.Context, who store.WorkspaceOwner, reason, fleet, runner, subject string) {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), outcomeWriteTimeout)
	defer cancel()
	ws, err := s.st.ListWorkspaces(wctx, store.WorkspaceFilter{RunnerID: who.RunnerID, OperatorID: who.OperatorID,
		States: []store.WorkspaceState{store.WorkspaceOpening, store.WorkspaceOpen}, Limit: 1})
	if err != nil {
		s.logf("service: cascade list (%s): %v", reason, err)
		return
	}
	if len(ws) == 0 {
		return
	}
	now := s.now()
	ev := store.AuditEvent{Time: now, Source: string(SourceSystem), ActorKind: string(SourceSystem), Actor: "revoke",
		Action: actionCloseAll, Outcome: OutcomeOK, Fleet: fleet, Runner: runner, Subject: subject, Message: reason}
	if _, err := s.st.CloseWorkspacesOf(wctx, who, reason, now, ev); err != nil {
		s.logf("service: close workspaces of %s (%s): %v", subject, reason, err)
	}
}

// RecoverWorkspaces is the central startup step: opening rows are from a create that central
// crashed in the middle of, so they become failed. Reconcile destroys any container left behind
// when the runner returns. It returns how many rows it moved.
func (s *Service) RecoverWorkspaces(ctx context.Context) (int, error) {
	s.markStarted()
	n := 0
	for {
		rows, err := s.st.ListWorkspaces(ctx, store.WorkspaceFilter{States: []store.WorkspaceState{store.WorkspaceOpening}, Limit: sweepLimit})
		if err != nil {
			return n, err
		}
		moved := 0
		for _, w := range rows {
			if s.setState(ctx, w, store.WorkspaceOpening, store.WorkspaceFailed, "central restarted during open", actionRecover) {
				moved++
			}
		}
		n += moved
		if moved == 0 || len(rows) < sweepLimit {
			return n, nil
		}
	}
}

// ReconcileRunner compares the runner's own list of workspaces with the store (M3.md 1.6). It
// is idempotent, never holds a lock across a runner call, and is meant to run in a background
// goroutine on the runner's first heartbeat.
func (s *Service) ReconcileRunner(ctx context.Context, runnerID int64) {
	if _, busy := s.recon.LoadOrStore(runnerID, struct{}{}); busy { // a fast reconnect
		return
	}
	defer s.recon.Delete(runnerID)

	// Rows first, list second: a row that opens between the two is not in rows, so it cannot be
	// mistaken for a missing container.
	rows, err := s.st.ListWorkspaces(ctx, store.WorkspaceFilter{RunnerID: runnerID, States: liveStates, Limit: sweepLimit})
	if err != nil {
		if ctx.Err() == nil {
			s.logf("service: reconcile runner %d: list rows: %v", runnerID, err)
		}
		return
	}
	lctx, cancel := context.WithTimeout(ctx, workspaceCloseTimeout)
	res, err := s.live.Workspace(lctx, runnerID, transport.WorkspaceRequest{Op: transport.OpList,
		TimeoutMs: workspaceCloseTimeout.Milliseconds()}, nil, nil)
	cancel()
	if err != nil || res == nil || res.Code != transport.CodeOK {
		if ctx.Err() == nil {
			s.logf("service: reconcile runner %d: list failed: %v", runnerID, err)
		}
		return
	}
	present := make(map[string]bool, len(res.List))
	for _, in := range res.List {
		if ctx.Err() != nil {
			return
		}
		if !workspace.ValidID(workspace.ID(in.ID)) {
			continue
		}
		present[in.ID] = true
		w, err := s.st.GetWorkspace(ctx, in.ID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			s.destroyOrphan(ctx, runnerID, store.Workspace{WsID: in.ID}, "no row")
		case err != nil:
			s.logf("service: reconcile runner %d: get %s: %v", runnerID, in.ID, err)
		case w.RunnerID != runnerID || w.State.Terminal():
			s.destroyOrphan(ctx, runnerID, w, "row is "+string(w.State))
		case w.State == store.WorkspaceClosing:
			if s.destroyOn(ctx, runnerID, w.WsID) {
				s.setState(ctx, w, store.WorkspaceClosing, store.WorkspaceClosed, w.Reason, actionDestroy)
			}
		case w.State == store.WorkspaceOpen && deadState(in.State):
			s.setState(ctx, w, store.WorkspaceOpen, store.WorkspaceLost, "container "+in.State+" on runner", actionLost)
		case w.State == store.WorkspaceOpening && s.now().Sub(w.CreatedAt) > s.openTimeout+s.openMargin:
			if s.destroyOn(ctx, runnerID, w.WsID) {
				s.setState(ctx, w, store.WorkspaceOpening, store.WorkspaceFailed, "open timed out", actionRecover)
			}
		}
	}
	for _, w := range rows {
		if present[w.WsID] {
			continue
		}
		switch w.State {
		case store.WorkspaceOpen:
			s.setState(ctx, w, store.WorkspaceOpen, store.WorkspaceLost, "container missing on runner", actionLost)
		case store.WorkspaceClosing:
			s.setState(ctx, w, store.WorkspaceClosing, store.WorkspaceClosed, w.Reason, actionDestroy)
		}
	}
}

// destroyOrphan destroys a container no live row owns and records it.
func (s *Service) destroyOrphan(ctx context.Context, runnerID int64, w store.Workspace, why string) {
	if !s.destroyOn(ctx, runnerID, w.WsID) {
		return
	}
	if w.Runner == "" {
		if r, err := s.st.GetRunnerByID(ctx, runnerID); err == nil {
			w.Runner, w.Fleet = r.Name, r.Fleet
		}
	}
	s.recordBestEffort(ctx, s.sysEvent(actionOrphan, w, "orphan destroyed: "+why))
}

// markStarted records the start of this process's workspace housekeeping (the first call wins).
// The idle clock never runs from before it: after a central outage longer than idle_ttl, every
// workspace would otherwise look idle at the first sweep although nobody could reach it.
func (s *Service) markStarted() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.startedAt.IsZero() {
		s.startedAt = s.now()
	}
}

// idleRef is the time the idle ttl counts from: the later of last use and central's start.
func (s *Service) idleRef(w store.Workspace) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.startedAt.After(w.LastUsedAt) {
		return s.startedAt
	}
	return w.LastUsedAt
}

// beginOp marks an op in flight on workspace id and returns the function that ends it. The
// reaper never idles out a workspace with an op in flight.
func (s *Service) beginOp(id string) func() {
	s.mu.Lock()
	if s.inflight == nil {
		s.inflight = map[string]int{}
	}
	s.inflight[id]++
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.inflight[id]--; s.inflight[id] <= 0 {
			delete(s.inflight, id)
		}
	}
}

func (s *Service) busy(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inflight[id] > 0
}

// deadState reports a runner-reported container state that can no longer serve an open row.
func deadState(st string) bool {
	switch st {
	case string(workspace.StateStopped), string(workspace.StateUnknown), "terminated":
		return true
	}
	return false
}
