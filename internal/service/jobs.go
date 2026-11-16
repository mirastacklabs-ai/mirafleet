package service

// Background jobs and saved output: workspace.job_start, job_status,
// job_tail, job_cancel and output_read, all dispatched to the runner of the caller's host
// workspace. job_start and job_cancel are audited (start and end rows); the polling reads
// (job_status, job_tail, output_read) write a row only when they fail, so a poll loop does not
// flood the audit log. Every call touches the workspace, so a session that is only polling a job
// stays in use.

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/policy"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// busyTouchEvery is how often a runner's heartbeat busy count refreshes its workspaces' last use.
const busyTouchEvery = 5 * time.Minute

// jobCall runs one job op for the workspace after the gating checks every job method shares.
func (s *Service) jobCall(ctx context.Context, p auth.Principal, src Source, method, wsID, confirm string, audit bool,
	op transport.JobOp, tr string, params map[string]string, want func(*transport.WorkspaceResult) bool) (*transport.WorkspaceResult, error) {

	w, f, err := s.hostTarget(ctx, p, src, method, wsID)
	if err != nil {
		return nil, err
	}
	if f.Confirm && policy.RequiresConfirm(method, "") && confirm != f.Name {
		return nil, s.denyConfirm(ctx, p, src, method, w)
	}
	res, _, err := s.dispatchDev(ctx, p, src, devCall{method: method, w: w, audit: audit, timeout: devOpTimeout, params: params,
		req: transport.WorkspaceRequest{Op: tr, ID: w.WsID, Job: &op},
		verify: func(r *transport.WorkspaceResult, _ int64) error {
			if !want(r) {
				return errors.New("job result missing")
			}
			return nil
		}})
	return res, err
}

// JobStart starts a background shell (bash -lc Command) in the caller's host workspace. On a
// guarded fleet confirm must equal the fleet name. The command is audited (clipped), like exec's argv.
func (s *Service) JobStart(ctx context.Context, p auth.Principal, src Source, params api.JobStartParams) (api.JobStartResult, error) {
	if e := params.Validate(); e != nil {
		return api.JobStartResult{}, e
	}
	audit := map[string]string{"command": clip(params.Command, maxAuditArgv)}
	if len(params.Command) > maxAuditArgv {
		audit["command_clipped"] = "true"
	}
	if params.Description != "" {
		audit["description"] = clip(params.Description, 256)
	}
	if params.TimeoutS > 0 {
		audit["timeout_s"] = strconv.Itoa(params.TimeoutS)
	}
	res, err := s.jobCall(ctx, p, src, api.MethodWorkspaceJobStart, params.Workspace, params.Confirm, true,
		params.Op(), transport.OpJobStart, audit, func(r *transport.WorkspaceResult) bool { return r.Job != nil })
	if err != nil {
		return api.JobStartResult{}, err
	}
	return api.JobStartResult{Job: *res.Job}, nil
}

// JobStatus describes one job, or lists the session's jobs when params.Job is empty.
func (s *Service) JobStatus(ctx context.Context, p auth.Principal, src Source, params api.JobStatusParams) (api.JobStatusResult, error) {
	if e := params.Validate(); e != nil {
		return api.JobStatusResult{}, e
	}
	one := params.Job != ""
	res, err := s.jobCall(ctx, p, src, api.MethodWorkspaceJobStatus, params.Workspace, "", false,
		transport.JobOp{Job: params.Job}, transport.OpJobStatus, nil,
		func(r *transport.WorkspaceResult) bool { return !one || r.Job != nil })
	if err != nil {
		return api.JobStatusResult{}, err
	}
	if one {
		return api.JobStatusResult{Job: res.Job}, nil
	}
	return api.JobStatusResult{Jobs: res.Jobs}, nil
}

// JobTail returns the output the job produced since the previous tail (Filter keeps matching lines).
func (s *Service) JobTail(ctx context.Context, p auth.Principal, src Source, params api.JobTailParams) (api.JobTailResult, error) {
	if e := params.Validate(); e != nil {
		return api.JobTailResult{}, e
	}
	res, err := s.jobCall(ctx, p, src, api.MethodWorkspaceJobTail, params.Workspace, "", false,
		transport.JobOp{Job: params.Job, Filter: params.Filter}, transport.OpJobTail, nil,
		func(r *transport.WorkspaceResult) bool { return r.Job != nil })
	if err != nil {
		return api.JobTailResult{}, err
	}
	return api.JobTailResult{Job: *res.Job}, nil
}

// JobCancel kills the job's process group.
func (s *Service) JobCancel(ctx context.Context, p auth.Principal, src Source, params api.JobCancelParams) (api.JobCancelResult, error) {
	if e := params.Validate(); e != nil {
		return api.JobCancelResult{}, e
	}
	res, err := s.jobCall(ctx, p, src, api.MethodWorkspaceJobCancel, params.Workspace, "", true,
		transport.JobOp{Job: params.Job}, transport.OpJobCancel, map[string]string{"job": params.Job},
		func(r *transport.WorkspaceResult) bool { return r.Job != nil })
	if err != nil {
		return api.JobCancelResult{}, err
	}
	return api.JobCancelResult{Job: *res.Job}, nil
}

// OutputRead pages saved output (a foreground command's Ref, or a job's stream).
func (s *Service) OutputRead(ctx context.Context, p auth.Principal, src Source, params api.OutputReadParams) (api.OutputReadResult, error) {
	if e := params.Validate(); e != nil {
		return api.OutputReadResult{}, e
	}
	w, _, err := s.hostTarget(ctx, p, src, api.MethodWorkspaceOutputRead, params.Workspace)
	if err != nil {
		return api.OutputReadResult{}, err
	}
	op := params.OutputOp
	res, _, err := s.dispatchDev(ctx, p, src, devCall{method: api.MethodWorkspaceOutputRead, w: w, timeout: devOpTimeout,
		req: transport.WorkspaceRequest{Op: transport.OpOutputRead, ID: w.WsID, Out: &op},
		verify: func(r *transport.WorkspaceResult, _ int64) error {
			if r.Output == nil {
				return errors.New("output result missing")
			}
			return nil
		}})
	if err != nil {
		return api.OutputReadResult{}, err
	}
	return *res.Output, nil
}

// TouchBusy refreshes the last use of the open workspaces of a runner whose heartbeat reports
// running background jobs (busy > 0), at most once per busyTouchEvery per workspace, so the idle
// reaper does not close a session that is waiting on a long build. Failures are only logged.
func (s *Service) TouchBusy(ctx context.Context, runnerID int64, busy int) {
	if busy <= 0 {
		return
	}
	ws, err := s.st.ListWorkspaces(ctx, store.WorkspaceFilter{RunnerID: runnerID, Limit: 1000,
		States: []store.WorkspaceState{store.WorkspaceOpen}})
	if err != nil {
		s.logf("service: list busy workspaces of runner %d: %v", runnerID, err)
		return
	}
	now := s.now()
	for _, w := range ws {
		if now.Sub(w.LastUsedAt) >= busyTouchEvery {
			s.touch(ctx, w.WsID)
		}
	}
}
