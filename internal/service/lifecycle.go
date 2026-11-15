package service

import (
	"context"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/central/registry"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// RevokeReason is the kick reason and audit message for a revoked runner.
const RevokeReason = "runner revoked"

// RunnerView is a stored runner merged with live state.
type RunnerView struct {
	store.Runner
	Online        bool
	LastHeartbeat time.Time // zero when not connected
	// Workspace runners (zero otherwise): advertised slots, slots not held by a live workspace
	// row, and the executor name, all from the latest heartbeat and the store.
	Slots     int
	FreeSlots int
	Executor  string
	// Actions an actions-mode runner advertised (informational, from the latest heartbeat,
	// bounded by Heartbeat.Validate). Never used for authorisation.
	Actions []transport.ActionInfo
	// Host-mode runners (M7): the features and running-job count of the latest heartbeat, and the
	// credentials the runner is assigned (name, kind, hosts, status only).
	Features    []string
	Busy        int
	Credentials []api.RunnerCredential
}

// ListRunners lists a fleet's non-revoked runners merged with the registry snapshot. Allowed
// for admins and active operators.
func (s *Service) ListRunners(ctx context.Context, p auth.Principal, src Source, fleet string) ([]RunnerView, error) {
	if err := s.authorize(ctx, p, src, api.MethodRunnersList, false); err != nil {
		return nil, err
	}
	return s.listRunners(ctx, fleet, false)
}

// ListRunnersAll is ListRunners with an optional includeRevoked. Including revoked runners is
// admin only (double gate); otherwise it behaves exactly like ListRunners.
func (s *Service) ListRunnersAll(ctx context.Context, p auth.Principal, src Source, fleet string, includeRevoked bool) ([]RunnerView, error) {
	if err := s.authorize(ctx, p, src, api.MethodRunnersList, includeRevoked); err != nil {
		return nil, err
	}
	return s.listRunners(ctx, fleet, includeRevoked)
}

func (s *Service) listRunners(ctx context.Context, fleet string, includeRevoked bool) ([]RunnerView, error) {
	if !store.ValidFleetName(fleet) {
		return nil, badRequest("invalid fleet name")
	}
	rs, err := s.st.ListRunners(ctx, fleet, includeRevoked)
	if err != nil {
		return nil, s.fail("list runners", err)
	}
	live := map[int64]registry.SessionInfo{}
	for _, si := range s.live.Snapshot() {
		live[si.ID] = si
	}
	out := make([]RunnerView, len(rs))
	var ids []int64
	for i, r := range rs {
		v := RunnerView{Runner: r}
		if si, ok := live[r.ID]; ok {
			v.Online = si.Online
			v.LastHeartbeat = si.LastHeartbeat
			if si.LastHeartbeat.After(v.LastSeenAt) {
				v.LastSeenAt = si.LastHeartbeat
			}
			v.Actions = si.Info.Actions
			v.Features, v.Busy = si.Info.Features, si.Info.Busy
			if si.Slots > 0 {
				v.Slots, v.Executor, v.FreeSlots = si.Slots, si.Executor, si.Slots
				ids = append(ids, r.ID)
			}
		}
		for _, f := range v.Features {
			if f == transport.FeatureHost {
				if cv, err := s.RunnerCredentialViews(ctx, r.ID); err == nil {
					v.Credentials = cv
				}
				break
			}
		}
		out[i] = v
	}
	if len(ids) > 0 {
		held, err := s.st.CountLiveWorkspaces(ctx, ids)
		if err != nil {
			return nil, s.fail("count workspaces", err)
		}
		for i := range out {
			if out[i].Slots > 0 {
				out[i].FreeSlots = max(0, out[i].Slots-held[out[i].ID])
			}
		}
	}
	return out, nil
}

// Approve moves a pending runner to active. Admin only.
func (s *Service) Approve(ctx context.Context, p auth.Principal, src Source, ref string) (store.Runner, error) {
	return s.lifecycle(ctx, p, src, ref, api.OpApprove)
}

// Disable moves an active runner to disabled; the live connection stays up and in-flight work
// finishes. Admin only.
func (s *Service) Disable(ctx context.Context, p auth.Principal, src Source, ref string) (store.Runner, error) {
	return s.lifecycle(ctx, p, src, ref, api.OpDisable)
}

// Enable moves a disabled runner back to active. Admin only.
func (s *Service) Enable(ctx context.Context, p auth.Principal, src Source, ref string) (store.Runner, error) {
	return s.lifecycle(ctx, p, src, ref, api.OpEnable)
}

// Revoke permanently revokes a non-revoked runner and kicks its connection (§1.6). Admin only.
func (s *Service) Revoke(ctx context.Context, p auth.Principal, src Source, ref string) (store.Runner, error) {
	return s.lifecycle(ctx, p, src, ref, api.OpRevoke)
}

// Lifecycle applies op (api.OpApprove|OpDisable|OpEnable|OpRevoke) to the runner "fleet/name".
func (s *Service) Lifecycle(ctx context.Context, p auth.Principal, src Source, ref, op string) (store.Runner, error) {
	return s.lifecycle(ctx, p, src, ref, op)
}

func (s *Service) lifecycle(ctx context.Context, p auth.Principal, src Source, refStr, op string) (store.Runner, error) {
	// Double gate: the policy AND a direct kind check inside authorize(adminOnly).
	if err := s.authorize(ctx, p, src, api.MethodRunnerLifecycle, true); err != nil {
		return store.Runner{}, err
	}
	var from []store.Status
	var to store.Status
	switch op {
	case api.OpApprove:
		from, to = []store.Status{store.StatusPending}, store.StatusActive
	case api.OpDisable:
		from, to = []store.Status{store.StatusActive}, store.StatusDisabled
	case api.OpEnable:
		from, to = []store.Status{store.StatusDisabled}, store.StatusActive
	case api.OpRevoke:
		from, to = []store.Status{store.StatusPending, store.StatusActive, store.StatusDisabled}, store.StatusRevoked
	default:
		return store.Runner{}, badRequest("unknown lifecycle op")
	}
	ref, err := store.ParseRef(refStr)
	if err != nil {
		return store.Runner{}, badRequest("invalid runner reference")
	}
	r, err := s.st.GetRunner(ctx, ref)
	if err != nil {
		return store.Runner{}, s.fail("get runner", err)
	}
	allowed := false
	for _, f := range from {
		if f == r.Status {
			allowed = true
		}
	}
	if !allowed || !store.CanTransition(r.Status, to) {
		return store.Runner{}, api.Errorf(api.CodeConflict, "runner is %s: cannot %s", r.Status, op)
	}

	now := s.now()
	ev := s.event(p, src, "runner."+op)
	ev.Fleet, ev.Runner, ev.Subject = ref.Fleet, ref.Runner, ref.String()
	ev.Params = map[string]string{"from": string(r.Status), "to": string(to)}

	if to == store.StatusRevoked {
		// §1.6: tombstone + kick first so nothing can slip in or keep running while the commit
		// is in flight; undo the tombstone if the commit fails.
		ev.Message = RevokeReason
		s.live.Tombstone(r.ID)
		s.live.Kick(r.ID, RevokeReason)
		out, err := s.st.SetRunnerStatus(ctx, r.ID, from, to, now, ev)
		if err != nil {
			// Undo the tombstone only if the runner is verifiably NOT revoked: a conflict
			// because a concurrent revoke already committed must keep it. If the read itself
			// fails we cannot tell, so fail closed and keep the tombstone.
			if cur, gerr := s.st.GetRunnerByID(context.WithoutCancel(ctx), r.ID); gerr == nil && cur.Status != store.StatusRevoked {
				s.live.ClearTombstone(r.ID)
			}
			return store.Runner{}, s.fail("revoke runner", err)
		}
		// The tombstone stays: it, not a status, is what keeps a revoked id out of the registry
		// (registry.Status has no "revoked"), and ids are never reused. Tombstones live in memory
		// only; after a restart revoked runners are rejected at auth from the DB status, so
		// losing them is safe.
		s.closeWorkspacesOf(ctx, store.WorkspaceOwner{RunnerID: r.ID}, RevokeReason, ref.Fleet, ref.Runner, ref.String())
		return out, nil
	}

	out, err := s.st.SetRunnerStatus(ctx, r.ID, from, to, now, ev)
	if err != nil {
		return store.Runner{}, s.fail("set runner status", err)
	}
	if err := s.live.SetStatus(r.ID, registry.Status(to)); err != nil {
		// Fail closed: the live session may hold a stale status. Kick it; the runner reconnects
		// and registers with the status recorded in the DB.
		s.logf("service: live SetStatus(%d, %s) failed, kicking: %v", r.ID, to, err)
		s.live.Kick(r.ID, "status sync failed")
	}
	switch to {
	case store.StatusDisabled:
		s.withdrawRunner(ctx, r.ID) // H-02: the runner drops its secrets
	case store.StatusActive:
		if r.Status == store.StatusDisabled && s.cred != nil { // re-enable: deliver again
			if err := s.DeliverRunner(context.WithoutCancel(ctx), r.ID); err != nil {
				s.logf("service: deliver credentials to runner %d: %v", r.ID, err)
			}
		}
	}
	return out, nil
}
