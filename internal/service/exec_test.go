package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/central/registry"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// failAudit makes RecordAudit fail for rows with the given outcome ("" = all).
type failAudit struct {
	store.Store
	outcome string
	n       atomic.Int32
}

func (f *failAudit) RecordAudit(ctx context.Context, ev store.AuditEvent) (int64, error) {
	if f.outcome == "" || f.outcome == ev.Outcome {
		f.n.Add(1)
		return 0, errors.New("audit disk on fire: /secret/path")
	}
	return f.Store.RecordAudit(ctx, ev)
}

func (e *env) execAudit() []store.AuditEvent {
	var out []store.AuditEvent
	for _, a := range e.audit() {
		if a.Action == api.MethodActionRun {
			out = append(out, a)
		}
	}
	return out
}

func (e *env) execCalls() int {
	n := 0
	for _, c := range e.log.get() {
		if strings.HasPrefix(c, "exec ") {
			n++
		}
	}
	return n
}

func okExec(code int) func(context.Context, int64, transport.ExecRequest) (*transport.ExecResult, error) {
	return func(context.Context, int64, transport.ExecRequest) (*transport.ExecResult, error) {
		return &transport.ExecResult{Stdout: "hi", ExitCode: code, DurationMS: 7}, nil
	}
}

func TestRunActionHappyPath(t *testing.T) {
	e := newEnv(t)
	e.fleet("prod")
	r := e.runnerAt("prod", store.StatusActive)
	op := e.operator("alice")
	e.live.execFn = okExec(3)
	res, err := e.svc.RunAction(e.ctx, op, SourceAPI, "prod", r.Name, "uptime", map[string]string{"a": "b"}, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 || res.Stdout != "hi" || res.RequestID == "" {
		t.Fatalf("result %+v", res)
	}
	rows := e.execAudit()
	if len(rows) != 2 || rows[0].Outcome != "start" || rows[1].Outcome != "ok" {
		t.Fatalf("rows %+v", rows)
	}
	if rows[0].RequestID != rows[1].RequestID || rows[0].RequestID != res.RequestID {
		t.Errorf("request ids differ: %q %q %q", rows[0].RequestID, rows[1].RequestID, res.RequestID)
	}
	if rows[1].ExitCode == nil || *rows[1].ExitCode != 3 || rows[1].DurationMS != 7 {
		t.Errorf("outcome row %+v", rows[1])
	}
	if rows[0].Actor != "alice" || rows[0].Fleet != "prod" || rows[0].Runner != r.Name || rows[0].Subject != "uptime" {
		t.Errorf("start row %+v", rows[0])
	}
}

func TestRunActionStartAuditFailureNeverExecs(t *testing.T) {
	e := newEnv(t)
	e.fleet("prod")
	r := e.runnerAt("prod", store.StatusActive)
	op := e.operator("alice")
	fa := &failAudit{Store: e.svc.st, outcome: "start"}
	e.svc.st = fa
	_, err := e.svc.RunAction(e.ctx, op, SourceAPI, "prod", r.Name, "uptime", nil, 0, "")
	if code(t, err) != api.CodeUnavailable {
		t.Fatalf("err %v", err)
	}
	if e.execCalls() != 0 {
		t.Error("exec dispatched despite start-audit failure")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Error("error leaks internals")
	}
}

func TestRunActionOutcomeAuditFailureLogged(t *testing.T) {
	e := newEnv(t)
	e.fleet("prod")
	r := e.runnerAt("prod", store.StatusActive)
	op := e.operator("alice")
	e.live.execFn = okExec(0)
	e.svc.st = &failAudit{Store: e.svc.st, outcome: "ok"}
	if _, err := e.svc.RunAction(e.ctx, op, SourceAPI, "prod", r.Name, "uptime", nil, 0, ""); err != nil {
		t.Fatalf("exec result must survive an outcome audit failure: %v", err)
	}
	if !strings.Contains(e.logs.String(), "outcome audit") {
		t.Error("outcome failure not logged")
	}
}

func TestRunActionChecks(t *testing.T) {
	e := newEnv(t)
	e.fleet("prod")
	if _, err := e.svc.CreateFleet(e.ctx, e.admin, SourceCLI, "guarded", "actions", true, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CreateFleet(e.ctx, e.admin, SourceCLI, "ws", "workspace", false, wsCfg()); err != nil {
		t.Fatal(err)
	}
	act := e.runnerAt("prod", store.StatusActive)
	pend := e.runnerAt("prod", store.StatusPending)
	dis := e.runnerAt("prod", store.StatusDisabled)
	rev := e.runnerAt("prod", store.StatusRevoked)
	gr := e.runnerAt("guarded", store.StatusActive)
	wr := e.runnerAt("ws", store.StatusActive)
	op := e.operator("alice")
	e.live.execFn = okExec(0)

	run := func(p auth.Principal, fleet, runner string, params map[string]string, to int, confirm string) error {
		_, err := e.svc.RunAction(e.ctx, p, SourceAPI, fleet, runner, "uptime", params, to, confirm)
		return err
	}
	tooMany := map[string]string{}
	for i := 0; i <= api.MaxParams; i++ {
		tooMany[fmt.Sprint("k", i)] = "v"
	}
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"workspace mode", run(op, "ws", wr.Name, nil, 0, ""), api.CodeConflict},
		{"guarded no confirm", run(op, "guarded", gr.Name, nil, 0, ""), api.CodeBadRequest},
		{"guarded wrong confirm", run(op, "guarded", gr.Name, nil, 0, "prod"), api.CodeBadRequest},
		{"guarded right confirm", run(op, "guarded", gr.Name, nil, 0, "guarded"), ""},
		{"pending", run(op, "prod", pend.Name, nil, 0, ""), api.CodeConflict},
		{"disabled", run(op, "prod", dis.Name, nil, 0, ""), api.CodeConflict},
		{"revoked", run(op, "prod", rev.Name, nil, 0, ""), api.CodeNotFound},
		{"unknown runner", run(op, "prod", "nope", nil, 0, ""), api.CodeNotFound},
		{"unknown fleet", run(op, "nofleet", "x", nil, 0, ""), api.CodeNotFound},
		{"bad name", run(op, "prod", "../x", nil, 0, ""), api.CodeBadRequest},
		{"too many params", run(op, "prod", act.Name, tooMany, 0, ""), api.CodeBadRequest},
		{"big param", run(op, "prod", act.Name, map[string]string{"k": strings.Repeat("x", api.MaxParamValue+1)}, 0, ""), api.CodeBadRequest},
		{"negative timeout", run(op, "prod", act.Name, nil, -1, ""), api.CodeBadRequest},
		{"ok", run(op, "prod", act.Name, nil, 5, ""), ""},
		{"admin ok", run(e.admin, "prod", act.Name, nil, 0, ""), ""},
	}
	for _, c := range cases {
		if got := ""; c.err != nil {
			got = code(t, c.err)
			if got != c.want {
				t.Errorf("%s: code %q, want %q (%v)", c.name, got, c.want, c.err)
			}
		} else if c.want != "" {
			t.Errorf("%s: no error, want %q", c.name, c.want)
		}
	}
	// Refusals never exec: only the 3 successful calls did.
	if n := e.execCalls(); n != 3 {
		t.Errorf("exec calls %d, want 3", n)
	}
	// Non-operator kinds and revoked operators are refused and audited as denied.
	if _, err := e.svc.RevokeOperator(e.ctx, e.admin, SourceCLI, "alice"); err != nil {
		t.Fatal(err)
	}
	if code(t, run(op, "prod", act.Name, nil, 0, "")) != api.CodeForbidden {
		t.Error("revoked operator ran an action")
	}
	if code(t, run(auth.Principal{Kind: auth.KindRunner, ID: act.ID}, "prod", act.Name, nil, 0, "")) != api.CodeForbidden {
		t.Error("runner ran an action")
	}
	if e.execCalls() != 3 {
		t.Error("exec after denial")
	}
}

func TestRunActionTimeoutClampedAndOutcomes(t *testing.T) {
	e := newEnv(t)
	e.fleet("prod")
	r := e.runnerAt("prod", store.StatusActive)
	op := e.operator("alice")

	var gotDeadline time.Duration
	e.live.execFn = func(ctx context.Context, _ int64, _ transport.ExecRequest) (*transport.ExecResult, error) {
		d, _ := ctx.Deadline()
		gotDeadline = time.Until(d)
		return &transport.ExecResult{TimedOut: true, ExitCode: -1}, nil
	}
	res, err := e.svc.RunAction(e.ctx, op, SourceAPI, "prod", r.Name, "uptime", nil, 100000, "")
	if err != nil {
		t.Fatal(err)
	}
	if gotDeadline > api.MaxActionTimeout || gotDeadline < api.MaxActionTimeout-time.Minute {
		t.Errorf("deadline %v not clamped to %v", gotDeadline, api.MaxActionTimeout)
	}
	_ = res
	rows := e.execAudit()
	if rows[len(rows)-1].Outcome != OutcomeTimeout {
		t.Errorf("runner-reported timeout: %+v", rows[len(rows)-1])
	}

	// Our own deadline fires while the exec hangs.
	e.live.execFn = func(ctx context.Context, _ int64, _ transport.ExecRequest) (*transport.ExecResult, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	_, err = e.svc.RunAction(e.ctx, op, SourceAPI, "prod", r.Name, "uptime", nil, 1, "")
	if code(t, err) != api.CodeTimeout {
		t.Fatalf("err %v", err)
	}
	rows = e.execAudit()
	if rows[len(rows)-1].Outcome != OutcomeTimeout {
		t.Errorf("deadline outcome: %+v", rows[len(rows)-1])
	}

	// Refused by the guard on the runner.
	e.live.execFn = func(context.Context, int64, transport.ExecRequest) (*transport.ExecResult, error) {
		return &transport.ExecResult{Refused: true, Message: "unknown action"}, nil
	}
	_, err = e.svc.RunAction(e.ctx, op, SourceAPI, "prod", r.Name, "bogus", nil, 0, "")
	if code(t, err) != api.CodeBadRequest {
		t.Fatalf("err %v", err)
	}
	rows = e.execAudit()
	if l := rows[len(rows)-1]; l.Outcome != OutcomeError || !strings.Contains(l.Message, "unknown action") {
		t.Errorf("refused outcome: %+v", l)
	}
}

func TestRunActionKickCancels(t *testing.T) {
	e := newEnv(t)
	e.fleet("prod")
	r := e.runnerAt("prod", store.StatusActive)
	op := e.operator("alice")
	started := make(chan struct{})
	release := make(chan struct{})
	e.live.execFn = func(ctx context.Context, _ int64, _ transport.ExecRequest) (*transport.ExecResult, error) {
		close(started)
		<-release
		return nil, &registry.KickError{Reason: RevokeReason}
	}
	done := make(chan error, 1)
	go func() {
		_, err := e.svc.RunAction(e.ctx, op, SourceAPI, "prod", r.Name, "long", nil, 0, "")
		done <- err
	}()
	<-started
	close(release)
	err := <-done
	if code(t, err) != api.CodeUnavailable {
		t.Fatalf("err %v", err)
	}
	rows := e.execAudit()
	if len(rows) != 2 {
		t.Fatalf("rows %+v", rows)
	}
	if o := rows[1]; o.Outcome != OutcomeCancelled || o.Message != "runner revoked" || o.RequestID != rows[0].RequestID {
		t.Errorf("outcome %+v", o)
	}
}

func TestRunActionRequestCancelledStillAuditsOutcome(t *testing.T) {
	e := newEnv(t)
	e.fleet("prod")
	r := e.runnerAt("prod", store.StatusActive)
	op := e.operator("alice")
	ctx, cancel := context.WithCancel(e.ctx)
	e.live.execFn = func(c context.Context, _ int64, _ transport.ExecRequest) (*transport.ExecResult, error) {
		cancel() // e.g. the operator's connection died
		<-c.Done()
		return nil, c.Err()
	}
	if _, err := e.svc.RunAction(ctx, op, SourceAPI, "prod", r.Name, "long", nil, 0, ""); err == nil {
		t.Fatal("want error")
	}
	rows := e.execAudit()
	if len(rows) != 2 || rows[1].Outcome != OutcomeCancelled {
		t.Fatalf("rows %+v", rows)
	}
}

func TestLifecycleSetStatusFailureKicks(t *testing.T) {
	e := newEnv(t)
	e.fleet("prod")
	r := e.runnerAt("prod", store.StatusPending)
	e.live.statusErr = errors.New("registry exploded")
	e.log.reset()
	if _, err := e.svc.Approve(e.ctx, e.admin, SourceCLI, "prod/"+r.Name); err != nil {
		t.Fatal(err)
	}
	calls := strings.Join(e.log.get(), ";")
	want := fmt.Sprintf("commit %d active;setstatus %d active;kick %d status sync failed", r.ID, r.ID, r.ID)
	if calls != want {
		t.Errorf("calls %q\nwant  %q", calls, want)
	}
}

func TestRunActionGuardedRefusalAudited(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.CreateFleet(e.ctx, e.admin, SourceCLI, "guarded", "actions", true, nil); err != nil {
		t.Fatal(err)
	}
	gr := e.runnerAt("guarded", store.StatusActive)
	op := e.operator("alice")
	e.live.execFn = okExec(0)
	for _, confirm := range []string{"", "prod"} {
		before := len(e.audit())
		_, err := e.svc.RunAction(e.ctx, op, SourceAPI, "guarded", gr.Name, "uptime", map[string]string{"k": "v"}, 0, confirm)
		if c := code(t, err); c != api.CodeBadRequest {
			t.Fatalf("confirm %q: code %q", confirm, c)
		}
		evs := e.audit()
		if len(evs) != before+1 {
			t.Fatalf("confirm %q: %d new audit rows, want 1", confirm, len(evs)-before)
		}
		a := evs[len(evs)-1]
		if a.Action != api.MethodActionRun || a.Outcome != OutcomeDenied || a.Subject != "uptime" ||
			a.Fleet != "guarded" || a.Runner != gr.Name || len(a.Params) != 0 || a.RequestID != "" {
			t.Errorf("refusal audit = %+v", a)
		}
	}
	if e.execCalls() != 0 {
		t.Error("refused call executed")
	}
}

func TestRunnerRefusalTextSanitised(t *testing.T) {
	e := newEnv(t)
	e.fleet("prod")
	r := e.runnerAt("prod", store.StatusActive)
	op := e.operator("alice")
	msg := "bad\x1b[31m red\x00 ok\ttab\nline " + strings.Repeat("é", 300)
	e.live.execFn = func(context.Context, int64, transport.ExecRequest) (*transport.ExecResult, error) {
		return &transport.ExecResult{Refused: true, Message: msg}, nil
	}
	_, err := e.svc.RunAction(e.ctx, op, SourceAPI, "prod", r.Name, "uptime", nil, 0, "")
	if code(t, err) != api.CodeBadRequest {
		t.Fatalf("err = %v", err)
	}
	m := err.Error()
	for _, bad := range []string{"\x1b", "\x00"} {
		if strings.Contains(m, bad) {
			t.Errorf("control char %q survived: %q", bad, m)
		}
	}
	if !strings.Contains(m, "\ttab\nline ") {
		t.Errorf("tab/newline stripped: %q", m)
	}
	if !utf8.ValidString(m) {
		t.Errorf("invalid utf-8 after clip: %q", m)
	}
}

func TestClip(t *testing.T) {
	if got := clip("héllo", 2); got != "h" { // é is 2 bytes: would be cut mid-rune at 2
		t.Errorf("clip = %q", got)
	}
	if got := clip("abc", 10); got != "abc" {
		t.Errorf("clip = %q", got)
	}
	if got := clip("a\x07b\x7fc\nd\te", 100); got != "abc\nd\te" {
		t.Errorf("clip = %q", got)
	}
	if got := clip("a\xffb", 10); got != "ab" {
		t.Errorf("invalid utf-8: %q", got)
	}
}
