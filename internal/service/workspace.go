package service

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/central/registry"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
)

// Defaults for the workspace service settings and for omitted fleet config fields.
const (
	DefaultMaxWorkspacesPerOperator = 8
	DefaultWorkspaceOpenTimeout     = 5 * time.Minute
	workspaceCloseTimeout           = 60 * time.Second
)

var sessionRE = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// WorkspaceDefaults fills the omitted (zero) fields of c with the documented defaults. It never
// invents a default image: that stays required. Used by CreateFleet only; SetFleetWorkspace is a
// full replace.
func WorkspaceDefaults(c store.WorkspaceConfig) store.WorkspaceConfig {
	if c.Executor == "" {
		c.Executor = store.ExecutorContainerd
	}
	if c.Pull == "" {
		c.Pull = store.PullIfNotPresent
	}
	if c.Network == "" {
		c.Network = store.NetworkNone
	}
	if len(c.Images) == 0 && c.DefaultImage != "" {
		c.Images = []string{c.DefaultImage}
	}
	if host := c.Executor == store.ExecutorHost; !host { // the VM is the boundary: no limits
		if c.CPUMilli == 0 {
			c.CPUMilli = 2000
		}
		if c.MemoryMB == 0 {
			c.MemoryMB = 4096
		}
		if c.Pids == 0 {
			c.Pids = 1024
		}
		if c.TmpfsMB == 0 {
			c.TmpfsMB = 512
		}
		if c.DiskMB == 0 {
			c.DiskMB = 10240
		}
	}
	if c.IdleTTL == 0 {
		c.IdleTTL = 24 * time.Hour
		if c.MaxTTL > 0 && c.MaxTTL < c.IdleTTL {
			c.IdleTTL = c.MaxTTL
		}
	}
	if c.MaxTTL == 0 {
		c.MaxTTL = max(168*time.Hour, c.IdleTTL)
	}
	if c.ExecDefault == 0 {
		c.ExecDefault = 120 * time.Second
		if c.ExecMax > 0 && c.ExecMax < c.ExecDefault {
			c.ExecDefault = c.ExecMax
		}
	}
	if c.ExecMax == 0 {
		c.ExecMax = max(1800*time.Second, c.ExecDefault)
	}
	return c
}

// configInvalid maps a config validation failure to bad_request with the (echo-safe) reason.
func configInvalid(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, store.ErrInvalid) {
		msg := err.Error()
		if _, after, ok := strings.Cut(msg, "invalid: "); ok {
			msg = after
		}
		return badRequest("%s", msg)
	}
	return err
}

func wsConfigParams(c store.WorkspaceConfig) map[string]string {
	return map[string]string{"executor": string(c.Executor), "default_image": c.DefaultImage,
		"network": string(c.Network), "images": strings.Join(c.Images, ",")}
}

// SetFleetWorkspace replaces the workspace config of a workspace-mode fleet. Admin only (double
// gate). It is a FULL replace: nothing is merged and no defaults are applied, so lists such as
// egress can be cleared. Existing workspaces keep the spec they were opened with.
func (s *Service) SetFleetWorkspace(ctx context.Context, p auth.Principal, src Source, fleet string, c store.WorkspaceConfig) (store.WorkspaceConfig, error) {
	if err := s.authorize(ctx, p, src, api.MethodFleetWorkspaceSet, true); err != nil {
		return store.WorkspaceConfig{}, err
	}
	if !store.ValidFleetName(fleet) {
		return store.WorkspaceConfig{}, badRequest("invalid fleet name")
	}
	if err := c.Validate(); err != nil {
		return store.WorkspaceConfig{}, configInvalid(err)
	}
	ev := s.event(p, src, api.MethodFleetWorkspaceSet)
	ev.Fleet, ev.Subject = fleet, fleet
	ev.Params = wsConfigParams(c)
	out, err := s.st.PutFleetWorkspace(ctx, fleet, c, s.now(), ev)
	if err != nil {
		return store.WorkspaceConfig{}, s.fail("set fleet workspace", err)
	}
	return out, nil
}

// wsEvent builds an audit event for a workspace call.
func (s *Service) wsEvent(p auth.Principal, src Source, action, fleet, runner, wsID, rid string) store.AuditEvent {
	ev := s.event(p, src, action)
	ev.Fleet, ev.Runner, ev.Workspace, ev.RequestID = fleet, runner, wsID, rid
	return ev
}

// recordBestEffort writes an audit row whose action already happened; a failure is only logged.
func (s *Service) recordBestEffort(ctx context.Context, ev store.AuditEvent) {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), outcomeWriteTimeout)
	defer cancel()
	if _, err := s.st.RecordAudit(wctx, ev); err != nil {
		s.logf("service: audit %s of %s failed: %v", ev.Action, ev.Workspace, err)
	}
}

func (s *Service) requireOperator(ctx context.Context, p auth.Principal, src Source, method string) error {
	if err := s.authorize(ctx, p, src, method, false); err != nil {
		return err
	}
	// Direct kind check, independent of the policy: admins act through the admin methods only.
	if p.Kind != auth.KindOperator {
		return s.deny(ctx, p, src, method)
	}
	return nil
}

func validSpecLabels(labels map[string]string) error {
	if len(labels) > transport.MaxSpecLabels {
		return badRequest("too many labels")
	}
	for k, v := range labels {
		if k == "" || len(k) > transport.MaxSpecLabelBytes || len(v) > transport.MaxSpecLabelBytes ||
			strings.ContainsRune(k, 0) || strings.ContainsRune(v, 0) {
			return badRequest("invalid label")
		}
		if strings.HasPrefix(strings.ToLower(k), "mirafleet.") {
			return badRequest("label names starting with mirafleet. are reserved")
		}
	}
	return nil
}

// OpenWorkspace opens (or reuses) the caller's workspace for (fleet, session). The
// caller must be an active operator and the fleet in workspace mode. Guarded fleets need
// confirm == fleet name and a runner with a secure runtime. A live workspace of the same session
// that is open is returned with reused=true (one still opening is a conflict); if its runner is offline the call fails with unavailable and
// nothing is created elsewhere. Otherwise a runner is picked (most free slots), a row reserved
// (the "start" audit row, fail closed) and the runner asked to create it.
func (s *Service) OpenWorkspace(ctx context.Context, p auth.Principal, src Source, fleet, session, image string,
	labels map[string]string, confirm string) (store.Workspace, bool, error) {
	return s.OpenWorkspaceIn(ctx, p, src, fleet, session, image, labels, confirm, "")
}

// workdirRE is the shared-directory name grammar (the same as a fleet name).
var workdirRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// labelWorkdir is the reserved spec label that carries workdir to the host executor
// (hostx.LabelWorkdir).
const labelWorkdir = "mirafleet.workdir"

// labelSession is the reserved spec label carrying the session id to the host executor
// (hostx.LabelSession), which keys the session directory by it so a reopen reattaches.
const labelSession = "mirafleet.session"

// OpenWorkspaceIn is OpenWorkspace with an optional shared workdir name (host fleets only, Q4).
func (s *Service) OpenWorkspaceIn(ctx context.Context, p auth.Principal, src Source, fleet, session, image string,
	labels map[string]string, confirm, workdir string) (store.Workspace, bool, error) {
	return s.OpenWorkspaceWith(ctx, p, src, fleet, session, image, labels, confirm, workdir, nil)
}

// labelCredentials is the reserved spec label that narrows the credentials a session uses
// (runner.LabelCredentials): a comma-separated list, absent = every credential the runner holds.
const labelCredentials = "mirafleet.credentials"

// OpenWorkspaceWith is OpenWorkspaceIn plus an optional credential subset (host fleets only):
// placement needs a runner that holds every named credential, and the names go
// to the runner in the reserved spec label.
func (s *Service) OpenWorkspaceWith(ctx context.Context, p auth.Principal, src Source, fleet, session, image string,
	labels map[string]string, confirm, workdir string, credentials []string) (store.Workspace, bool, error) {

	if err := s.requireOperator(ctx, p, src, api.MethodWorkspaceOpen); err != nil {
		return store.Workspace{}, false, err
	}
	if !store.ValidFleetName(fleet) {
		return store.Workspace{}, false, badRequest("invalid fleet name")
	}
	if !sessionRE.MatchString(session) {
		return store.Workspace{}, false, badRequest("invalid session")
	}
	if err := validSpecLabels(labels); err != nil {
		return store.Workspace{}, false, err
	}
	f, err := s.st.GetFleet(ctx, fleet)
	if err != nil {
		return store.Workspace{}, false, s.fail("get fleet", err)
	}
	if f.Mode != store.ModeWorkspace {
		// F-41: a mode mismatch is audited as one denied row (no start row exists yet).
		den := s.event(p, src, api.MethodWorkspaceOpen)
		den.Fleet, den.Subject = fleet, session
		den.Outcome, den.Message = OutcomeDenied, "fleet is not in workspace mode"
		s.recordBestEffort(ctx, den)
		return store.Workspace{}, false, api.Errorf(api.CodeConflict, "fleet is not in workspace mode")
	}
	cfg := store.WorkspaceConfig{}
	if f.Workspace != nil {
		cfg = *f.Workspace
	} else if cfg, err = s.st.GetFleetWorkspace(ctx, fleet); err != nil {
		return store.Workspace{}, false, s.fail("get fleet workspace", err)
	}
	if f.Confirm && confirm != f.Name {
		den := s.event(p, src, api.MethodWorkspaceOpen)
		den.Fleet, den.Subject = fleet, session
		den.Outcome, den.Message = OutcomeDenied, "confirm missing or wrong"
		if _, aerr := s.st.RecordAudit(ctx, den); aerr != nil {
			s.logf("service: audit of refused confirm failed: %v", aerr)
		}
		return store.Workspace{}, false, badRequest(api.MsgConfirmRequired)
	}
	host := cfg.Executor == store.ExecutorHost
	specLabels := labels
	if host { // no images in host mode; guarding is the typed confirm above, nothing else (Q10)
		if image != "" {
			return store.Workspace{}, false, badRequest("host fleets take no image")
		}
		image = hostImage
		if workdir != "" && !workdirRE.MatchString(workdir) {
			return store.Workspace{}, false, badRequest("invalid workdir name")
		}
		specLabels = make(map[string]string, len(labels)+3)
		for k, v := range labels {
			specLabels[k] = v
		}
		specLabels[labelSession] = session // reserved: the runner keys the session dir by it
		if workdir != "" {
			specLabels[labelWorkdir] = workdir // reserved: not a placement label
		}
		if len(credentials) > 0 {
			seen := map[string]bool{}
			for _, n := range credentials {
				if !transport.ValidCredName(n) || seen[n] || len(seen) >= transport.MaxSyncCreds {
					return store.Workspace{}, false, badRequest("invalid credentials list")
				}
				seen[n] = true
			}
			joined := strings.Join(credentials, ",")
			if len(joined) > transport.MaxSpecLabelBytes {
				return store.Workspace{}, false, badRequest("credentials list is too long")
			}
			specLabels[labelCredentials] = joined
		}
	} else {
		if len(credentials) > 0 {
			return store.Workspace{}, false, badRequest("credentials need a host fleet")
		}
		if workdir != "" {
			return store.Workspace{}, false, badRequest("workdir needs a host fleet")
		}
		if image == "" {
			image = cfg.DefaultImage
		}
		if !cfg.ImageAllowed(image) {
			return store.Workspace{}, false, badRequest("image is not allowed by the fleet")
		}
	}
	rid, err := newRequestID()
	if err != nil {
		return store.Workspace{}, false, s.fail("request id", err)
	}

	// Sticky session, and the per-operator cap, from the operator's live rows.
	live, err := s.st.ListWorkspaces(ctx, store.WorkspaceFilter{OperatorID: p.ID,
		States: []store.WorkspaceState{store.WorkspaceOpening, store.WorkspaceOpen, store.WorkspaceClosing}, Limit: 1000})
	if err != nil {
		return store.Workspace{}, false, s.fail("list workspaces", err)
	}
	for _, w := range live {
		if w.Fleet == fleet && w.Session == session && (w.State == store.WorkspaceOpen || w.State == store.WorkspaceOpening) {
			if host && len(credentials) > 0 && !sameCredSet(w.Spec, specLabels[labelCredentials]) {
				// H-04: a reused session keeps the narrowing it was opened with.
				return store.Workspace{}, false, api.Errorf(api.CodeConflict,
					"session %q is open with a different credentials list", session)
			}
			return s.reuse(ctx, p, src, w, rid)
		}
	}
	maxPerOp := s.maxPerOp
	if maxPerOp > 0 && len(live) >= maxPerOp {
		den := s.wsEvent(p, src, api.MethodWorkspaceOpen, fleet, "", "", rid)
		den.Subject, den.Outcome, den.Message = session, OutcomeDenied, "per-operator workspace cap reached"
		s.recordBestEffort(ctx, den)
		return store.Workspace{}, false, api.Errorf(api.CodeConflict, "workspace limit reached (%d)", maxPerOp)
	}

	preq := placementReq{fleet: fleet, cfg: cfg, guarded: f.Confirm && !host, labels: labels, image: image}
	heldBy, heldRunner := "", ""
	if host { // one operator per runner: skip runners another operator holds
		preq.busyBy, preq.busyRunner = &heldBy, &heldRunner
		if preq.held, err = s.heldRunners(ctx, fleet, p.ID); err != nil {
			return store.Workspace{}, false, s.fail("place workspace", err)
		}
	}
	cands, err := s.candidates(ctx, preq)
	if err != nil {
		return store.Workspace{}, false, s.fail("place workspace", err)
	}
	if len(credentials) > 0 && len(cands) > 0 { // placement needs every named credential (Q33)
		rs := make([]store.Runner, len(cands))
		for i, c := range cands {
			rs[i] = c.runner
		}
		ok, ferr := s.FilterRunnersByCredentials(ctx, rs, credentials)
		if ferr != nil {
			return store.Workspace{}, false, ferr
		}
		keep := make(map[int64]bool, len(ok))
		for _, r := range ok {
			keep[r.ID] = true
		}
		kept := cands[:0]
		for _, c := range cands {
			if keep[c.info.ID] {
				kept = append(kept, c)
			}
		}
		if cands = kept; len(cands) == 0 && heldBy == "" {
			return store.Workspace{}, false, api.Errorf(api.CodeConflict, "no runner holds every requested credential")
		}
	}
	if len(cands) == 0 && heldBy != "" {
		return store.Workspace{}, false, s.refuseBusy(ctx, p, src, fleet, session, rid, heldRunner, heldBy)
	}
	spec := buildSpec(cfg, image, specLabels, f.Confirm && !host)
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return store.Workspace{}, false, s.fail("encode spec", err)
	}

	tried := 0
	for _, c := range cands {
		if tried >= maxPlacementTries {
			break
		}
		tried++
		if len(credentials) > 0 { // narrowing never widens: re-check against what it holds now
			if _, rerr := s.ResolveCredentials(ctx, c.info.ID, credentials); rerr != nil {
				if errors.Is(rerr, ErrCredentialNotHeld) {
					continue
				}
				return store.Workspace{}, false, rerr
			}
		}
		now := s.now()
		id := string(workspace.NewID())
		row := store.Workspace{WsID: id, FleetID: f.ID, RunnerID: c.info.ID, OperatorID: p.ID, Session: session,
			Image: image, Spec: string(specJSON), ExpiresAt: now.Add(cfg.MaxTTL)}
		start := s.wsEvent(p, src, api.MethodWorkspaceOpen, fleet, c.runner.Name, id, rid)
		start.Time, start.Subject, start.Outcome = now, session, OutcomeStart
		start.Params = map[string]string{"image": image, "session": session}
		w, existing, err := s.st.ReserveWorkspace(ctx, row, c.info.Slots, maxPerOp, now, start)
		if errors.Is(err, store.ErrFull) {
			continue
		}
		var held *store.HeldError
		if errors.As(err, &held) { // lost the race to another operator (the store is the authority)
			heldBy, heldRunner = held.Operator, c.runner.Name
			continue
		}
		if err != nil {
			return store.Workspace{}, false, s.fail("reserve workspace", err)
		}
		if existing {
			return s.reuse(ctx, p, src, w, rid)
		}

		cctx, cancel := context.WithTimeout(ctx, s.openTimeout)
		res, lerr := s.live.Workspace(cctx, c.info.ID, transport.WorkspaceRequest{Op: transport.OpCreate, ID: id,
			Spec: &spec, TimeoutMs: s.openTimeout.Milliseconds()}, nil, nil)
		deadline := errors.Is(cctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
		cancel()

		end := s.wsEvent(p, src, api.MethodWorkspaceOpen, fleet, c.runner.Name, id, rid)
		end.Subject = session
		end.DurationMS = s.now().Sub(now).Milliseconds()
		busy := errors.Is(lerr, registry.ErrBusy) || (lerr == nil && res != nil && res.Code == transport.CodeBusy)
		var retErr error
		switch {
		case lerr == nil && res != nil && res.Code == transport.CodeOK:
			end.Outcome = OutcomeOK
			ow, serr := s.finishWorkspace(ctx, id, store.WorkspaceOpening, store.WorkspaceOpen, "", end)
			if serr != nil {
				// Conflict: killed or revoked while the create ran. Any other error (E-16): the row is
				// stuck opening. Either way the container exists and must not outlive the failure.
				why := "row closed while create ran"
				if !errors.Is(serr, store.ErrConflict) {
					why = "recording the open failed"
				}
				s.destroyStray(ctx, c.info.ID, id, why)
				s.failStuckOpening(ctx, p, src, fleet, c.runner.Name, id, session, rid, why)
				return store.Workspace{}, false, s.fail("open workspace", serr)
			}
			ow.Fleet, ow.Runner, ow.Operator = fleet, c.runner.Name, p.Name
			return ow, false, nil
		case busy:
			end.Outcome, end.Message = OutcomeError, "runner busy"
			retErr = api.Errorf(api.CodeUnavailable, "runner busy")
		case deadline:
			end.Outcome, end.Message = OutcomeTimeout, "open timed out"
			retErr = api.Errorf(api.CodeTimeout, "workspace open timed out")
		case ctx.Err() != nil:
			end.Outcome, end.Message = OutcomeCancelled, "request cancelled"
			retErr = api.Errorf(api.CodeUnavailable, "request cancelled")
		case lerr != nil:
			s.logf("service: workspace create %s on %s failed: %v", id, c.runner.Ref(), lerr)
			end.Outcome, end.Message = OutcomeError, "runner unavailable"
			retErr = api.Errorf(api.CodeUnavailable, "runner unavailable")
		case res == nil:
			end.Outcome, end.Message = OutcomeError, "empty result"
			retErr = api.Internal(errors.New("nil workspace result"))
		default:
			msg := clip(res.Message, 200)
			end.Outcome, end.Message = OutcomeError, "runner: "+res.Code+": "+msg
			retErr = runnerResultError(res.Code, msg)
		}
		reason := end.Message
		if _, serr := s.finishWorkspace(ctx, id, store.WorkspaceOpening, store.WorkspaceFailed, reason, end); serr != nil {
			s.logf("service: mark workspace %s failed: %v", id, serr)
		}
		if !busy && (deadline || ctx.Err() != nil || lerr != nil) {
			// The runner may still finish the create: make sure what it builds does not survive.
			s.destroyStray(ctx, c.info.ID, id, reason)
			s.failStuckOpening(ctx, p, src, fleet, c.runner.Name, id, session, rid, reason)
		}
		if busy && tried < maxPlacementTries {
			continue
		}
		return store.Workspace{}, false, retErr
	}
	if heldBy != "" {
		return store.Workspace{}, false, s.refuseBusy(ctx, p, src, fleet, session, rid, heldRunner, heldBy)
	}
	return store.Workspace{}, false, api.Errorf(api.CodeUnavailable, "no runner can take this workspace right now")
}

// destroyStray sends a best-effort destroy for a workspace whose create outcome could not be
// recorded as open. It is bounded and detached from ctx (which may be cancelled); the runner
// defers the destroy until an in-flight create settles, and ReconcileRunner remains the backstop.
func (s *Service) destroyStray(ctx context.Context, runnerID int64, id, why string) {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workspaceCloseTimeout)
	defer cancel()
	if s.destroyOn(dctx, runnerID, id) {
		s.logf("service: destroyed stray workspace %s on runner %d (%s)", id, runnerID, why)
	} else {
		s.logf("service: could not destroy stray workspace %s on runner %d (%s); reconcile will retry", id, runnerID, why)
	}
}

// failStuckOpening (F-27) re-reads the row after destroyStray and, if it is still opening (the
// open could not be recorded), moves it to failed with an audit row, so session reuse is not
// blocked until the stale sweep. Best effort: errors are logged only.
func (s *Service) failStuckOpening(ctx context.Context, p auth.Principal, src Source, fleet, runner, id, session, rid, why string) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), outcomeWriteTimeout)
	defer cancel()
	w, err := s.st.GetWorkspace(rctx, id)
	if err != nil {
		s.logf("service: re-read stray workspace %s: %v", id, err)
		return
	}
	if w.State != store.WorkspaceOpening {
		return
	}
	ev := s.wsEvent(p, src, api.MethodWorkspaceOpen, fleet, runner, id, rid)
	ev.Subject, ev.Outcome, ev.Message = session, OutcomeError, "open failed: "+why
	if _, err := s.finishWorkspace(ctx, id, store.WorkspaceOpening, store.WorkspaceFailed, why, ev); err != nil &&
		!errors.Is(err, store.ErrConflict) {
		s.logf("service: mark stray workspace %s failed: %v", id, err)
	}
}

// runnerResultError maps a runner's non-ok result code to the operator-facing error.
func runnerResultError(code, msg string) error {
	switch code {
	case transport.CodeBadRequest, transport.CodeRefused, transport.CodeUnsupported, transport.CodeTooLarge, transport.CodeQuota:
		return api.Errorf(api.CodeBadRequest, "refused by runner: %s", msg)
	case transport.CodeNotFound:
		return api.Errorf(api.CodeNotFound, "not found")
	case transport.CodeTimeout:
		return api.Errorf(api.CodeTimeout, "timed out")
	}
	return api.Errorf(api.CodeUnavailable, "runner unavailable")
}

// finishWorkspace moves a row between states with its outcome audit row, surviving a cancelled
// request context.
func (s *Service) finishWorkspace(ctx context.Context, id string, from, to store.WorkspaceState, reason string, ev store.AuditEvent) (store.Workspace, error) {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), outcomeWriteTimeout)
	defer cancel()
	ev.Time = s.now()
	return s.st.SetWorkspaceState(wctx, id, []store.WorkspaceState{from}, to, reason, ev.Time, ev)
}

// reuse returns a sticky workspace, or unavailable when its runner is offline.
// sameCredSet reports whether the credentials label stored in specJSON names the same set as
// want (a comma-separated list; order and repeats do not matter, absent = empty).
func sameCredSet(specJSON, want string) bool {
	var spec transport.WorkspaceSpec
	_ = json.Unmarshal([]byte(specJSON), &spec)
	norm := func(l string) string {
		if l == "" {
			return ""
		}
		parts := strings.Split(l, ",")
		slices.Sort(parts)
		return strings.Join(slices.Compact(parts), ",")
	}
	return norm(spec.Labels[labelCredentials]) == norm(want)
}

func (s *Service) reuse(ctx context.Context, p auth.Principal, src Source, w store.Workspace, rid string) (store.Workspace, bool, error) {
	if w.State == store.WorkspaceOpening { // C-14: the create is still in flight; do not hand out a half-made workspace
		ev := s.wsEvent(p, src, api.MethodWorkspaceOpen, w.Fleet, w.Runner, w.WsID, rid)
		ev.Subject, ev.Outcome, ev.Message = w.Session, OutcomeError, "sticky workspace is still opening"
		s.recordBestEffort(ctx, ev)
		return store.Workspace{}, false, api.Errorf(api.CodeConflict, "workspace is still opening")
	}
	ev := s.wsEvent(p, src, api.MethodWorkspaceOpen, w.Fleet, w.Runner, w.WsID, rid)
	ev.Subject = w.Session
	if !s.runnerOnline(w.RunnerID) {
		ev.Outcome, ev.Message = OutcomeError, "sticky workspace's runner is offline"
		if _, err := s.st.RecordAudit(ctx, ev); err != nil {
			s.logf("service: audit of offline sticky open failed: %v", err)
		}
		return store.Workspace{}, false, api.Errorf(api.CodeUnavailable, "the runner holding this session's workspace is offline")
	}
	ev.Message = "reused"
	if _, err := s.st.RecordAudit(ctx, ev); err != nil {
		s.logf("service: audit of workspace reuse failed: %v", err)
	}
	return w, true, nil
}

// ownedWorkspace loads a workspace the principal owns; anyone else gets not_found so existence
// does not leak.
//
// F-43: a refusal on an existing workspace of another operator is audited as denied (requester as
// actor, the workspace id); the caller still sees plain not_found.
func (s *Service) ownedWorkspace(ctx context.Context, p auth.Principal, src Source, method, wsID string) (store.Workspace, error) {
	// F-47: an unknown id takes the same path as a foreign one: one denied row, plain not_found.
	// A malformed id is never copied into the audit row (caller-controlled, unbounded).
	deny := func(w store.Workspace, id string) (store.Workspace, error) {
		den := s.wsEvent(p, src, method, w.Fleet, w.Runner, id, "")
		den.Subject, den.Outcome, den.Message = w.Session, OutcomeDenied, "not found or not owner"
		s.recordBestEffort(ctx, den)
		return store.Workspace{}, api.Errorf(api.CodeNotFound, "not found")
	}
	if !workspace.ValidID(workspace.ID(wsID)) {
		return deny(store.Workspace{}, "")
	}
	w, err := s.st.GetWorkspace(ctx, wsID)
	if errors.Is(err, store.ErrNotFound) {
		return deny(store.Workspace{}, wsID)
	}
	if err != nil {
		return store.Workspace{}, s.fail("get workspace", err)
	}
	if w.OperatorID != p.ID {
		return deny(w, w.WsID)
	}
	return w, nil
}

// CloseWorkspace closes the caller's workspace: open -> closing -> destroy on the runner ->
// closed. Only the owner may close; anyone else gets not_found. If the runner is offline or the
// destroy fails, the row stays closing (it still holds its slot) and the reaper or reconcile
// finishes it; the returned workspace then has state closing. Closing a terminal workspace is a
// no-op returning it.
func (s *Service) CloseWorkspace(ctx context.Context, p auth.Principal, src Source, wsID string) (store.Workspace, error) {
	return s.CloseWorkspaceDelete(ctx, p, src, wsID, false)
}

// CloseWorkspaceDelete is CloseWorkspace; with del (host fleets only) the runner also deletes the
// session directory. A close without del keeps it.
func (s *Service) CloseWorkspaceDelete(ctx context.Context, p auth.Principal, src Source, wsID string, del bool) (store.Workspace, error) {
	if err := s.requireOperator(ctx, p, src, api.MethodWorkspaceClose); err != nil {
		return store.Workspace{}, err
	}
	w, err := s.ownedWorkspace(ctx, p, src, api.MethodWorkspaceClose, wsID)
	if err != nil {
		return store.Workspace{}, err
	}
	if del {
		if err := s.requireHostFleet(ctx, w.Fleet); err != nil {
			return store.Workspace{}, err
		}
	}
	if w.State.Terminal() {
		return w, nil
	}
	if w.State == store.WorkspaceOpening {
		return store.Workspace{}, api.Errorf(api.CodeConflict, "workspace is still opening")
	}
	rid, err := newRequestID()
	if err != nil {
		return store.Workspace{}, s.fail("request id", err)
	}
	start := s.now()
	ev := s.wsEvent(p, src, api.MethodWorkspaceClose, w.Fleet, w.Runner, w.WsID, rid)
	ev.Time, ev.Subject, ev.Outcome = start, w.Session, OutcomeStart
	if del {
		ev.Params = map[string]string{"delete": "true"}
	}
	if w.State == store.WorkspaceOpen {
		w, err = s.st.SetWorkspaceState(ctx, w.WsID, []store.WorkspaceState{store.WorkspaceOpen}, store.WorkspaceClosing, "closed by operator", start, ev)
		if err != nil {
			return store.Workspace{}, s.fail("close workspace", err)
		}
	} else if _, err := s.st.RecordAudit(ctx, ev); err != nil { // retry of a closing workspace
		s.logf("service: start audit for %s failed, refusing destroy: %v", rid, err)
		return store.Workspace{}, api.Errorf(api.CodeUnavailable, "audit unavailable")
	}

	dctx, cancel := context.WithTimeout(ctx, workspaceCloseTimeout)
	res, lerr := s.live.Workspace(dctx, w.RunnerID, transport.WorkspaceRequest{Op: transport.OpDestroy, ID: w.WsID,
		TimeoutMs: workspaceCloseTimeout.Milliseconds(), Delete: del}, nil, nil)
	cancel()

	end := s.wsEvent(p, src, api.MethodWorkspaceClose, w.Fleet, w.Runner, w.WsID, rid)
	end.Subject = w.Session
	end.DurationMS = s.now().Sub(start).Milliseconds()
	switch {
	case lerr == nil && res != nil && (res.Code == transport.CodeOK || res.Code == transport.CodeNotFound):
		end.Outcome = OutcomeOK
		cw, serr := s.finishWorkspace(ctx, w.WsID, store.WorkspaceClosing, store.WorkspaceClosed, "closed by operator", end)
		if serr != nil {
			return store.Workspace{}, s.fail("close workspace", serr)
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
