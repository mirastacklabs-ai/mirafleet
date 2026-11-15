package service

// Dev-workspace dispatch (docs/M7.md 3.5 to 3.7): workspace.fs and the shared call path of the
// job and output methods (jobs.go). All of them need a host-mode fleet; container fleets answer
// bad_request. The guard of a guarded host fleet is the typed fleet-name confirm only (Q10),
// decided by policy.RequiresConfirm.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"strconv"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/policy"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

const (
	// fsOpTimeout bounds one file-tool call (grep itself is bounded to 30 s on the runner).
	fsOpTimeout = 60 * time.Second
	// devOpTimeout bounds the job and output calls.
	devOpTimeout = 60 * time.Second
)

// requireHostFleet refuses (bad_request) a fleet that is not a host-mode fleet.
func (s *Service) requireHostFleet(ctx context.Context, fleet string) error {
	cfg, err := s.st.GetFleetWorkspace(ctx, fleet)
	if err != nil {
		return s.fail("get fleet workspace", err)
	}
	if cfg.Executor != store.ExecutorHost {
		return badRequest("only host-mode fleets support this")
	}
	return nil
}

// hostTarget is wsTarget (policy, owner, state open) plus the host-executor requirement.
func (s *Service) hostTarget(ctx context.Context, p auth.Principal, src Source, method, wsID string) (store.Workspace, store.Fleet, error) {
	w, f, cfg, err := s.wsTarget(ctx, p, src, method, wsID)
	if err != nil {
		return store.Workspace{}, store.Fleet{}, err
	}
	if cfg.Executor != store.ExecutorHost {
		return store.Workspace{}, store.Fleet{}, badRequest("only host-mode fleets support this")
	}
	return w, f, nil
}

// devCall is one runner call made through dispatchDev.
type devCall struct {
	method    string
	w         store.Workspace
	req       transport.WorkspaceRequest
	params    map[string]string // start-row params (never content)
	audit     bool              // start and end rows; otherwise one row, only on failure
	timeout   time.Duration
	body      io.Reader
	out       io.Writer                                          // receives the reply body when set
	verify    func(*transport.WorkspaceResult, int64) error      // checks an ok result (body bytes received)
	endParams func(*transport.WorkspaceResult) map[string]string // end-row params of an ok result
}

// dispatchDev runs c on the workspace's runner: start row (fail closed, when audited), touch,
// call, classify, end row, touch. It returns the runner's result for an ok call.
func (s *Service) dispatchDev(ctx context.Context, p auth.Principal, src Source, c devCall) (*transport.WorkspaceResult, string, error) {
	rid, err := newRequestID()
	if err != nil {
		return nil, "", s.fail("request id", err)
	}
	start := s.now()
	ev := s.wsEvent(p, src, c.method, c.w.Fleet, c.w.Runner, c.w.WsID, rid)
	ev.Subject = c.w.Session
	if c.audit {
		ev.Time, ev.Outcome, ev.Params = start, OutcomeStart, c.params
		if err := s.startRow(ctx, ev); err != nil {
			return nil, rid, err
		}
	}
	s.touch(ctx, c.w.WsID)
	defer s.beginOp(c.w.WsID)()
	dctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	cw := &countWriter{w: c.out}
	var ow io.Writer
	if c.out != nil {
		ow = cw
	}
	res, lerr := s.live.Workspace(dctx, c.w.RunnerID, c.req, c.body, ow)

	end := ev
	end.Time = s.now()
	end.Params = nil
	end.DurationMS = end.Time.Sub(start).Milliseconds()
	outcome, msg, retErr := classify(ctx, dctx, res, lerr, s.logf)
	if retErr == nil && c.verify != nil {
		if verr := c.verify(res, cw.n); verr != nil {
			outcome, msg, retErr = OutcomeError, "runner result inconsistent", api.Internal(verr)
		}
	}
	if retErr == nil && c.endParams != nil {
		end.Params = c.endParams(res)
	}
	end.Outcome, end.Message = outcome, msg
	if c.audit || retErr != nil {
		s.recordBestEffort(ctx, end)
	}
	if res != nil && lerr == nil {
		s.touch(ctx, c.w.WsID)
	}
	if retErr != nil {
		return nil, rid, retErr
	}
	return res, rid, nil
}

// fsAuditParams describes an fs call for the audit row: op, path, sizes and the SHA-256 of
// written data; never file content, edit strings or patterns.
func fsAuditParams(o *transport.FSOp) map[string]string {
	m := map[string]string{"op": o.Op}
	if o.Path != "" {
		m["path"] = clip(o.Path, 512)
	}
	switch o.Op {
	case transport.FSWrite:
		m["bytes"] = strconv.FormatInt(o.Length, 10)
		m["sha256"] = o.SHA256
	case transport.FSEdit:
		m["old_len"], m["new_len"] = strconv.Itoa(len(o.OldString)), strconv.Itoa(len(o.NewString))
		m["replace_all"] = strconv.FormatBool(o.ReplaceAll)
	case transport.FSMultiEdit:
		m["edits"] = strconv.Itoa(len(o.Edits))
	case transport.FSNotebookEdit:
		m["edit_mode"] = o.EditMode
		m["source_len"] = strconv.Itoa(len(o.NewSource))
	case transport.FSRead:
		m["offset"], m["limit"] = strconv.FormatInt(o.Offset, 10), strconv.Itoa(o.Limit)
	case transport.FSGrep, transport.FSGlob:
		m["pattern_len"] = strconv.Itoa(len(o.Pattern))
	}
	return m
}

// FSWorkspace runs one file-tool call (workspace.fs) in the caller's host workspace. Checks, in
// order: policy, ownership, state open, host fleet, params, guarded confirm (mutating ops, via
// policy.RequiresConfirm), then for a write the body is read and its SHA-256 verified BEFORE the
// runner is contacted. body supplies Length bytes for a write (a DeferredBody is read only now,
// after every gating check). An image result streams its raw bytes to out (at most 1 MiB, the
// transport body limit; the runner returns larger images as kind=binary metadata).
func (s *Service) FSWorkspace(ctx context.Context, p auth.Principal, src Source, params api.FSParams, body io.Reader, out io.Writer) (api.FSResult, error) {
	w, f, err := s.hostTarget(ctx, p, src, api.MethodWorkspaceFS, params.Workspace)
	if err != nil {
		return api.FSResult{}, err
	}
	if e := params.Validate(); e != nil {
		return api.FSResult{}, s.refuseInput(ctx, p, src, api.MethodWorkspaceFS, w, e.Message)
	}
	if f.Confirm && policy.RequiresConfirm(api.MethodWorkspaceFS, params.Op) && params.Confirm != f.Name {
		return api.FSResult{}, s.denyConfirm(ctx, p, src, api.MethodWorkspaceFS, w)
	}
	op := params.FSOp
	audit := fsAuditParams(&op)
	var wbody io.Reader
	bodyLen := int64(0)
	if op.Op == transport.FSWrite {
		r := body
		if d, ok := body.(DeferredBody); ok {
			var n int64
			var berr error
			if r, n, berr = d.Body(); berr != nil {
				return api.FSResult{}, berr
			}
			if n != op.Length {
				return api.FSResult{}, badRequest("body length does not match length")
			}
		}
		if op.Length < 0 || op.Length > transport.MaxBodyBytes || (op.Length > 0 && r == nil) {
			return api.FSResult{}, badRequest("invalid body length")
		}
		buf := make([]byte, op.Length)
		if op.Length > 0 {
			if _, rerr := io.ReadFull(r, buf); rerr != nil {
				return api.FSResult{}, s.fsRefused(ctx, p, src, w, audit, "body shorter than length")
			}
		}
		want, derr := hex.DecodeString(op.SHA256)
		got := sha256.Sum256(buf)
		if derr != nil || len(want) != sha256.Size || subtle.ConstantTimeCompare(got[:], want) != 1 {
			return api.FSResult{}, s.fsRefused(ctx, p, src, w, audit, "sha256 mismatch")
		}
		wbody, bodyLen = bytes.NewReader(buf), op.Length
	}

	var reply capBuffer
	reply.max = transport.MaxBodyBytes
	res, _, err := s.dispatchDev(ctx, p, src, devCall{method: api.MethodWorkspaceFS, w: w, audit: true, timeout: fsOpTimeout,
		req:    transport.WorkspaceRequest{Op: transport.OpFS, ID: w.WsID, FS: &op, StdinLen: bodyLen},
		params: audit, body: wbody, out: &reply,
		verify: func(r *transport.WorkspaceResult, n int64) error {
			if r.FS == nil || r.FS.Length != n || (n > 0 && r.FS.Kind != transport.KindImage) {
				return errors.New("fs result missing or inconsistent")
			}
			return nil
		},
		endParams: func(r *transport.WorkspaceResult) map[string]string {
			m := map[string]string{"op": op.Op, "kind": r.FS.Kind}
			if r.FS.Count > 0 {
				m["count"] = strconv.Itoa(r.FS.Count)
			}
			if r.FS.WrittenBytes > 0 {
				m["written_bytes"] = strconv.FormatInt(r.FS.WrittenBytes, 10)
			}
			return m
		}})
	if err != nil {
		return api.FSResult{}, err
	}
	if len(reply.b) > 0 && out != nil {
		if _, werr := out.Write(reply.b); werr != nil {
			return api.FSResult{}, api.Internal(werr)
		}
	}
	return *res.FS, nil
}

// fsRefused audits an fs call refused before dispatch (one error row, never content).
func (s *Service) fsRefused(ctx context.Context, p auth.Principal, src Source, w store.Workspace, params map[string]string, msg string) error {
	ev := s.wsEvent(p, src, api.MethodWorkspaceFS, w.Fleet, w.Runner, w.WsID, "")
	ev.Subject, ev.Outcome, ev.Message, ev.Params = w.Session, OutcomeError, msg, params
	s.recordBestEffort(ctx, ev)
	return badRequest("%s", msg)
}
