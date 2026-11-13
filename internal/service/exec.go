package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/central/registry"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// Audit outcomes of an exec (§1.7). "start" is the row written before dispatch.
const (
	OutcomeStart     = "start"
	OutcomeError     = "error"
	OutcomeCancelled = "cancelled"
	OutcomeTimeout   = "timeout"
)

// outcomeWriteTimeout bounds the final audit write, which must survive a cancelled request ctx.
const outcomeWriteTimeout = 10 * time.Second

func newRequestID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// RunActionResult is the wire result of RunAction.
type RunActionResult = api.ActionRunResult

// RunAction runs an action on a runner (§1.4, §1.7). Checks, in order: policy (operator must be
// active), input limits, fleet mode "actions", server-side confirm (guarded fleets need
// confirm == fleet name), runner active. A "start" audit row is written BEFORE dispatch; if that
// write fails nothing is executed (fail closed). The outcome row (ok/error/cancelled/timeout) has
// the same request_id. A kick of the runner (revoke) ends the exec with outcome "cancelled" and
// message from the kick reason ("runner revoked"). A failed outcome write is logged only: the
// exec already happened.
//
// Action params are stored verbatim in the start audit row: actions must not take secrets as
// params (a redaction seam can be added later).
//
// For an operator's exec the ctx must be tied to the operator's connection: revoking the operator
// closes the connection, which cancels ctx, which cancels the in-flight exec (outcome cancelled).
func (s *Service) RunAction(ctx context.Context, p auth.Principal, src Source, fleet, runner, action string,
	params map[string]string, timeoutS int, confirm string) (RunActionResult, error) {

	if err := s.authorize(ctx, p, src, api.MethodActionRun, false); err != nil {
		return RunActionResult{}, err
	}
	// Direct kind check, independent of the policy: only admins and operators run actions.
	if p.Kind != auth.KindAdmin && p.Kind != auth.KindOperator {
		return RunActionResult{}, s.deny(ctx, p, src, api.MethodActionRun)
	}
	ref, err := store.ParseRef(fleet + "/" + runner)
	if err != nil {
		return RunActionResult{}, badRequest("invalid fleet or runner name")
	}
	if action == "" || len(action) > api.MaxParamKey {
		return RunActionResult{}, badRequest("invalid action")
	}
	if e := api.ValidateActionParams(params); e != nil {
		return RunActionResult{}, e
	}
	if e := api.ValidateTimeoutS(timeoutS); e != nil {
		return RunActionResult{}, e
	}
	timeout := api.ActionTimeout(timeoutS)

	f, err := s.st.GetFleet(ctx, ref.Fleet)
	if err != nil {
		return RunActionResult{}, s.fail("get fleet", err)
	}
	if f.Mode != store.ModeActions {
		return RunActionResult{}, api.Errorf(api.CodeConflict, "fleet is not in actions mode")
	}
	if f.Confirm && confirm != f.Name {
		// Audited as a refusal; the row carries the action name but no params.
		den := s.event(p, src, api.MethodActionRun)
		den.Fleet, den.Runner, den.Subject = ref.Fleet, ref.Runner, action
		den.Outcome, den.Message = OutcomeDenied, "confirm missing or wrong"
		if _, aerr := s.st.RecordAudit(ctx, den); aerr != nil {
			s.logf("service: audit of refused confirm failed: %v", aerr)
		}
		return RunActionResult{}, badRequest("fleet requires confirm to equal the fleet name")
	}
	r, err := s.st.GetRunner(ctx, ref)
	if err != nil {
		return RunActionResult{}, s.fail("get runner", err)
	}
	if r.Status != store.StatusActive {
		return RunActionResult{}, api.Errorf(api.CodeConflict, "runner is %s", r.Status)
	}

	rid, err := newRequestID()
	if err != nil {
		return RunActionResult{}, s.fail("request id", err)
	}
	start := s.now()
	ev := s.event(p, src, api.MethodActionRun)
	ev.Time = start
	ev.Fleet, ev.Runner, ev.Subject = ref.Fleet, ref.Runner, action
	ev.Params = params
	ev.RequestID = rid
	ev.Outcome = OutcomeStart
	if _, err := s.st.RecordAudit(ctx, ev); err != nil {
		s.logf("service: start audit for %s failed, refusing exec: %v", rid, err)
		return RunActionResult{}, api.Errorf(api.CodeUnavailable, "audit unavailable")
	}

	ectx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, execErr := s.live.Exec(ectx, r.ID, transport.ExecRequest{Action: action, Params: params})

	end := ev
	end.Time = s.now()
	end.Params = nil
	end.DurationMS = end.Time.Sub(start).Milliseconds()
	var out RunActionResult
	var retErr error
	switch {
	case execErr != nil:
		var ke *registry.KickError
		switch {
		case errors.As(execErr, &ke):
			end.Outcome, end.Message = OutcomeCancelled, ke.Reason
			retErr = api.Errorf(api.CodeUnavailable, "%s", ke.Reason)
		case errors.Is(ectx.Err(), context.DeadlineExceeded) && ctx.Err() == nil:
			end.Outcome, end.Message = OutcomeTimeout, "timed out"
			retErr = api.Errorf(api.CodeTimeout, "action timed out")
		case ctx.Err() != nil:
			end.Outcome, end.Message = OutcomeCancelled, "request cancelled"
			retErr = api.Errorf(api.CodeUnavailable, "request cancelled")
		default:
			s.logf("service: exec %s on %s failed: %v", rid, ref, execErr)
			end.Outcome, end.Message = OutcomeError, "runner unavailable"
			retErr = api.Errorf(api.CodeUnavailable, "runner unavailable")
		}
	case res == nil:
		end.Outcome, end.Message = OutcomeError, "empty result"
		retErr = api.Internal(errors.New("nil exec result"))
	case res.Refused:
		end.Outcome, end.Message = OutcomeError, "refused by runner: "+clip(res.Message, 200)
		retErr = api.Errorf(api.CodeBadRequest, "refused by runner: %s", clip(res.Message, 200))
	default:
		code := res.ExitCode
		end.ExitCode = &code
		if res.DurationMS > 0 {
			end.DurationMS = res.DurationMS
		}
		end.Outcome = OutcomeOK
		if res.TimedOut {
			end.Outcome, end.Message = OutcomeTimeout, "timed out"
		}
		out = RunActionResult{
			ExitCode: res.ExitCode, Stdout: res.Stdout, Stderr: res.Stderr,
			Truncated: res.StdoutTruncated || res.StderrTruncated, DurationMS: end.DurationMS, RequestID: rid,
		}
	}
	wctx, wcancel := context.WithTimeout(context.WithoutCancel(ctx), outcomeWriteTimeout)
	defer wcancel()
	if _, err := s.st.RecordAudit(wctx, end); err != nil {
		s.logf("service: outcome audit for %s failed: %v", rid, err)
	}
	if retErr != nil {
		return RunActionResult{}, retErr
	}
	return out, nil
}

// clip sanitises runner-supplied text for the operator: control characters other than \n and \t
// (and invalid UTF-8) are dropped, and the result is cut to at most n bytes on a rune boundary.
func clip(s string, n int) string {
	var b strings.Builder
	for _, r := range s {
		if r == utf8.RuneError || (unicode.IsControl(r) && r != '\n' && r != '\t') {
			continue
		}
		if b.Len()+utf8.RuneLen(r) > n {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}
