package central

import (
	"context"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
)

// Admin handlers decode strictly, call the service and map to api DTOs. They never put store
// rows, secrets or hashes on the wire.

func handleFleetCreate(ctx context.Context, c *callCtx, req *api.Request) (any, error) {
	var p api.FleetCreateParams
	if e := api.DecodeParams(req.Params, &p); e != nil {
		return nil, e
	}
	f, err := c.svc.CreateFleet(ctx, c.p, c.src, p.Name, store.Mode(p.Mode), p.Confirm, workspaceFromAPI(p.Workspace))
	if err != nil {
		return nil, err
	}
	return api.FleetCreateResult{Fleet: api.FleetInfo{Name: f.Name, Mode: string(f.Mode), Confirm: f.Confirm, Runners: f.RunnerCount}}, nil
}

func handleTokenCreate(ctx context.Context, c *callCtx, req *api.Request) (any, error) {
	var p api.TokenCreateParams
	if e := api.DecodeParams(req.Params, &p); e != nil {
		return nil, e
	}
	if p.TTLS < 0 || p.TTLS > int(service.MaxTokenTTL/time.Second) {
		return nil, api.Errorf(api.CodeBadRequest, "invalid ttl_s")
	}
	t, err := c.svc.CreateToken(ctx, c.p, c.src, service.TokenParams{
		Fleet: p.Fleet, Labels: p.Labels, Approve: p.Approve, Uses: p.Uses,
		TTL: time.Duration(p.TTLS) * time.Second,
	})
	if err != nil {
		return nil, err
	}
	return api.TokenCreateResult{ID: t.ID, Token: t.Token, Expires: t.Expires}, nil
}

func handleTokenList(ctx context.Context, c *callCtx, req *api.Request) (any, error) {
	var p api.TokenListParams
	if e := api.DecodeParams(req.Params, &p); e != nil {
		return nil, e
	}
	ts, err := c.svc.ListTokens(ctx, c.p, c.src, p.Fleet)
	if err != nil {
		return nil, err
	}
	out := api.TokenListResult{Tokens: make([]api.TokenInfo, len(ts))}
	for i, t := range ts {
		out.Tokens[i] = api.TokenInfo{ID: t.TokenID, Fleet: t.Fleet, Labels: t.Labels, Approve: t.AutoApprove,
			MaxUses: t.MaxUses, Uses: t.Uses, Expires: t.ExpiresAt, Revoked: !t.RevokedAt.IsZero()}
	}
	return out, nil
}

func handleTokenRevoke(ctx context.Context, c *callCtx, req *api.Request) (any, error) {
	var p api.TokenRevokeParams
	if e := api.DecodeParams(req.Params, &p); e != nil {
		return nil, e
	}
	if err := c.svc.RevokeToken(ctx, c.p, c.src, p.ID); err != nil {
		return nil, err
	}
	return struct{}{}, nil
}

func handleRunnerLifecycle(ctx context.Context, c *callCtx, req *api.Request) (any, error) {
	var p api.RunnerLifecycleParams
	if e := api.DecodeParams(req.Params, &p); e != nil {
		return nil, e
	}
	r, err := c.svc.Lifecycle(ctx, c.p, c.src, p.Fleet+"/"+p.Runner, p.Op)
	if err != nil {
		return nil, err
	}
	ri := api.RunnerInfo{Name: r.Name, Status: string(r.Status), Labels: r.Labels}
	if !r.LastSeenAt.IsZero() {
		t := r.LastSeenAt
		ri.LastSeen = &t
	}
	return ri, nil
}

func handleOperatorCode(ctx context.Context, c *callCtx, req *api.Request) (any, error) {
	var none struct{}
	if e := api.DecodeParams(req.Params, &none); e != nil {
		return nil, e
	}
	return c.svc.OperatorCode(ctx, c.p, c.src)
}

func handleOperatorRotateSecret(ctx context.Context, c *callCtx, req *api.Request) (any, error) {
	var none struct{}
	if e := api.DecodeParams(req.Params, &none); e != nil {
		return nil, e
	}
	if err := c.svc.RotateRegistrationSecret(ctx, c.p, c.src); err != nil {
		return nil, err
	}
	return api.OperatorRotateSecretResult{}, nil
}

func handleOperatorList(ctx context.Context, c *callCtx, req *api.Request) (any, error) {
	var none struct{}
	if e := api.DecodeParams(req.Params, &none); e != nil {
		return nil, e
	}
	ops, err := c.svc.ListOperators(ctx, c.p, c.src, true)
	if err != nil {
		return nil, err
	}
	out := api.OperatorListResult{Operators: make([]api.OperatorInfo, len(ops))}
	for i, o := range ops {
		oi := api.OperatorInfo{Name: o.Name, Fingerprint: o.Fingerprint, Status: string(o.Status)}
		if !o.LastSeenAt.IsZero() {
			t := o.LastSeenAt
			oi.LastSeen = &t
		}
		out.Operators[i] = oi
	}
	return out, nil
}

func handleOperatorRevoke(ctx context.Context, c *callCtx, req *api.Request) (any, error) {
	var p api.OperatorRevokeParams
	if e := api.DecodeParams(req.Params, &p); e != nil {
		return nil, e
	}
	if _, err := c.svc.RevokeOperator(ctx, c.p, c.src, p.Name); err != nil {
		return nil, err
	}
	return struct{}{}, nil
}

// Audit list paging: default and maximum rows per call.
const (
	auditDefaultLimit = 100
	auditMaxLimit     = 1000
)

func handleAuditList(ctx context.Context, c *callCtx, req *api.Request) (any, error) {
	var p api.AuditListParams
	if e := api.DecodeParams(req.Params, &p); e != nil {
		return nil, e
	}
	// The service has no audit read; keep the admin-only gate here, independent of the table.
	if c.p.Kind != auth.KindAdmin {
		return nil, api.Errorf(api.CodeForbidden, "forbidden")
	}
	if p.SinceID < 0 || p.Limit < 0 {
		return nil, api.Errorf(api.CodeBadRequest, "since_id and limit must be >= 0")
	}
	limit := p.Limit
	if limit == 0 {
		limit = auditDefaultLimit
	}
	if limit > auditMaxLimit {
		limit = auditMaxLimit
	}
	evs, err := c.st.ListAudit(ctx, p.SinceID, limit)
	if err != nil {
		c.logf("admin: list audit: %v", err)
		return nil, api.Internal(err)
	}
	out := api.AuditListResult{Events: make([]api.AuditEvent, len(evs))}
	for i, e := range evs {
		out.Events[i] = api.AuditEvent{ID: e.ID, Time: e.Time, Source: e.Source, ActorKind: e.ActorKind,
			Actor: e.Actor, Remote: e.Remote, Fleet: e.Fleet, Runner: e.Runner, Subject: e.Subject,
			Action: e.Action, Params: e.Params, Outcome: e.Outcome, ExitCode: e.ExitCode,
			DurationMS: e.DurationMS, Message: e.Message, RequestID: e.RequestID}
	}
	return out, nil
}

// workspaceFromAPI maps the wire config to the store config field by field (seconds to
// durations). nil stays nil; the store validates and rejects invalid or misplaced configs.
func workspaceFromAPI(w *api.WorkspaceConfig) *store.WorkspaceConfig {
	if w == nil {
		return nil
	}
	sec := func(n int) time.Duration { return time.Duration(n) * time.Second }
	return &store.WorkspaceConfig{
		Executor:     store.Executor(w.Executor),
		DefaultImage: w.DefaultImage,
		Images:       w.Images,
		Pull:         store.PullPolicy(w.Pull),
		Network:      store.NetworkMode(w.Network),
		Egress:       w.Egress,
		CPUMilli:     w.CPUMilli,
		MemoryMB:     w.MemoryMB,
		Pids:         w.Pids,
		TmpfsMB:      w.TmpfsMB,
		DiskMB:       w.DiskMB,
		IdleTTL:      sec(w.IdleTTLS),
		MaxTTL:       sec(w.MaxTTLS),
		ExecDefault:  sec(w.ExecDefaultS),
		ExecMax:      sec(w.ExecMaxS),
	}
}
