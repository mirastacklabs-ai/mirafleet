package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/orch"
)

func TestListToolsSchemasAndAnnotations(t *testing.T) {
	h := startTools(t, ListTools(&fakeBackend{}))
	res, err := h.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	type want struct{ ro, destr, idem bool }
	wants := map[string]want{
		"list_fleets":    {true, false, true},
		"list_runners":   {true, false, true},
		"run_action":     {false, true, false},
		"workspace_list": {true, false, true},
	}
	if len(res.Tools) != len(wants) {
		t.Fatalf("got %d tools", len(res.Tools))
	}
	for _, tl := range res.Tools {
		w, ok := wants[tl.Name]
		if !ok {
			t.Errorf("unexpected tool %s", tl.Name)
			continue
		}
		in, _ := json.Marshal(tl.InputSchema)
		var m map[string]any
		if err := json.Unmarshal(in, &m); err != nil || m["type"] != "object" || m["additionalProperties"] != false {
			t.Errorf("%s: input schema %s", tl.Name, in)
		}
		if tl.OutputSchema == nil {
			t.Errorf("%s: no output schema", tl.Name)
		}
		a := tl.Annotations
		if a == nil || a.ReadOnlyHint != w.ro || a.IdempotentHint != w.idem ||
			a.DestructiveHint == nil || *a.DestructiveHint != w.destr || a.OpenWorldHint == nil {
			t.Errorf("%s: annotations %+v", tl.Name, a)
		}
		if tl.Description == "" {
			t.Errorf("%s: no description", tl.Name)
		}
		if tl.Name == "run_action" && !strings.Contains(tl.Description, "explicit agreement") {
			t.Errorf("run_action description lacks the guarded warning: %q", tl.Description)
		}
	}
}

func TestListFleetsPartialFailure(t *testing.T) {
	fb := &fakeBackend{Contexts: []string{"alpha", "beta", "gamma"}}
	fb.FleetsListFn = func(_ context.Context, c string) (*api.FleetsListResult, error) {
		if c == "beta" {
			return nil, &orch.Error{Code: api.CodeUnavailable, Message: "cannot reach central"}
		}
		return &api.FleetsListResult{Fleets: []api.FleetInfo{
			{Name: "dev", Mode: "actions", Runners: 2},
			{Name: "prod", Mode: "actions", Confirm: true},
		}}, nil
	}
	fb.ResolveFn = func(_ context.Context, c, _ string) (orch.Target, error) {
		if c == "alpha" {
			return orch.Target{Context: c, Fleet: "dev", FleetSource: orch.SrcCtxDefault}, nil
		}
		return orch.Target{Context: c, FleetSource: orch.SrcNone}, nil
	}
	h := startTools(t, ListTools(fb))
	var out fleetsOut
	okJSON(t, callJSON(t, h, "list_fleets", `{}`), &out)
	if len(out.Errors) != 1 || out.Errors[0].Context != "beta" || out.Errors[0].Code != api.CodeUnavailable ||
		out.Errors[0].Message != "cannot reach central" {
		t.Fatalf("errors: %+v", out.Errors)
	}
	if len(out.Fleets) != 4 {
		t.Fatalf("fleets: %+v", out.Fleets)
	}
	for _, f := range out.Fleets {
		wantDef := f.Context == "alpha" && f.Name == "dev"
		if f.Default != wantDef {
			t.Errorf("%s/%s default=%v", f.Context, f.Name, f.Default)
		}
		if f.Guarded != (f.Name == "prod") {
			t.Errorf("%s/%s guarded=%v", f.Context, f.Name, f.Guarded)
		}
	}
}

func TestListFleetsOneContextAndBadName(t *testing.T) {
	fb := &fakeBackend{Contexts: []string{"alpha", "beta"}}
	h := startTools(t, ListTools(fb))
	okJSON(t, callJSON(t, h, "list_fleets", `{"context":"beta"}`), nil)
	if n := len(fb.calls("FleetsList")); n != 1 || fb.calls("FleetsList")[0] != "beta" {
		t.Fatalf("calls: %v", fb.calls("FleetsList"))
	}
	code, _ := errorOf(t, callJSON(t, h, "list_fleets", `{"context":"../x"}`))
	if code != "bad_request" {
		t.Fatalf("code %s", code)
	}
}

func TestWhereAndRunnerList(t *testing.T) {
	fb := &fakeBackend{}
	fb.ResolveFn = func(_ context.Context, c, f string) (orch.Target, error) {
		return orch.Target{Context: "work", ContextSource: orch.SrcProject, Fleet: "dev", FleetSource: orch.SrcProject}, nil
	}
	def := "main"
	fb.RunnersListFn = func(_ context.Context, c, f string) (*api.RunnersListResult, error) {
		if c != "work" || f != "dev" {
			t.Errorf("RunnersList(%s,%s)", c, f)
		}
		return &api.RunnersListResult{Runners: []api.RunnerInfo{{
			Name: "r1", Status: "online", Online: true, Labels: map[string]string{"az": "a"}, FreeSlots: 2, Executor: "containerd",
			Actions:        []api.ActionInfo{{Name: "deploy", Params: []api.ActionParamInfo{{Name: "branch", Pattern: "^[a-z]+$", Default: &def}}}},
			ActionsOmitted: true,
		}}}, nil
	}
	h := startTools(t, ListTools(fb))
	var out runnersOut
	okJSON(t, callJSON(t, h, "list_runners", `{}`), &out)
	want := where{Context: "work", ContextSource: "project file", Fleet: "dev", FleetSource: "project file"}
	if out.Where != want {
		t.Fatalf("where %+v", out.Where)
	}
	r := out.Runners[0]
	if r.FreeSlots != 2 || r.Executor != "containerd" || !r.ActionsOmitted || r.Actions[0].Params[0].Pattern != "^[a-z]+$" ||
		r.Actions[0].Params[0].Default == nil || *r.Actions[0].Params[0].Default != "main" {
		t.Fatalf("runner %+v", r)
	}
	// No fleet resolved: a clear error.
	fb.ResolveFn = nil
	code, msg := errorOf(t, callJSON(t, h, "list_runners", `{}`))
	if code != "bad_request" || !strings.Contains(msg, "no fleet") {
		t.Fatalf("%s %s", code, msg)
	}
}

func TestRunActionCapsNoisyOutput(t *testing.T) {
	noisy := strings.Repeat("\x1b[31mred <tag> & \"q\"\x1b[0m\r\nline\n", 7000) // ~200 KiB
	if len(noisy) < 200<<10 {
		t.Fatalf("setup: %d", len(noisy))
	}
	fb := &fakeBackend{Cap: 8 << 10}
	fb.ActionRunFn = func(context.Context, string, api.ActionRunParams) (*api.ActionRunResult, error) {
		return &api.ActionRunResult{ExitCode: 3, Stdout: noisy, Stderr: noisy, DurationMS: 1500}, nil
	}
	h := startTools(t, ListTools(fb))
	res := callJSON(t, h, "run_action", `{"fleet":"dev","runner":"r1","action":"a"}`)
	var out runActionOut
	text := okJSON(t, res, &out)
	if strings.ContainsRune(text, 0x1b) || strings.ContainsRune(out.Stdout+out.Stderr, 0x1b) || strings.Contains(text, `\u001b`) {
		t.Fatal("ESC byte in result")
	}
	enc, _ := json.Marshal(out.Stdout + out.Stderr)
	if len(enc)-2 > 8<<10 {
		t.Fatalf("encoded output %d > cap", len(enc))
	}
	if !out.Truncated || out.OmittedBytes == 0 || out.ExitCode != 3 || out.Duration != "1.5s" {
		t.Fatalf("out: exit=%d trunc=%v omitted=%d dur=%s", out.ExitCode, out.Truncated, out.OmittedBytes, out.Duration)
	}
	if out.Where.Fleet != "dev" || out.Where.Context != "main" {
		t.Fatalf("where %+v", out.Where)
	}
}

func TestRunActionConfirmPassThroughAndRewrite(t *testing.T) {
	fb := &fakeBackend{}
	fb.ActionRunFn = func(_ context.Context, _ string, p api.ActionRunParams) (*api.ActionRunResult, error) {
		if p.Confirm != "prod" {
			return nil, &orch.Error{Code: api.CodeBadRequest, Message: "fleet requires confirm to equal the fleet name"}
		}
		return &api.ActionRunResult{Stdout: "ok"}, nil
	}
	h := startTools(t, ListTools(fb))
	code, msg := errorOf(t, callJSON(t, h, "run_action", `{"fleet":"prod","runner":"r","action":"a"}`))
	want := "fleet `prod` in context `main` (only context) is guarded. Ask the user. Only after the user explicitly agrees, repeat the call with `confirm` set to the fleet name."
	if code != "bad_request" || msg != want {
		t.Fatalf("%s %q", code, msg)
	}
	if p := fb.calls("ActionRun")[0].(api.ActionRunParams); p.Confirm != "" {
		t.Fatalf("confirm was filled in: %q", p.Confirm)
	}
	// Wrong value is passed verbatim (central refuses), right value reaches central unchanged.
	callJSON(t, h, "run_action", `{"fleet":"prod","runner":"r","action":"a","confirm":"wrong"}`)
	if p := fb.calls("ActionRun")[1].(api.ActionRunParams); p.Confirm != "wrong" {
		t.Fatalf("confirm altered: %q", p.Confirm)
	}
	var out runActionOut
	okJSON(t, callJSON(t, h, "run_action", `{"fleet":"prod","runner":"r","action":"a","confirm":"prod","params":{"k":"v"},"timeout_s":99999}`), &out)
	p := fb.calls("ActionRun")[2].(api.ActionRunParams)
	if p.Confirm != "prod" || p.Params["k"] != "v" || p.TimeoutS != int(api.MaxActionTimeout.Seconds()) || out.Stdout != "ok" {
		t.Fatalf("params %+v out %+v", p, out)
	}
	// An explicit context is resolved like the other tools and named in the consent text.
	code, msg = errorOf(t, callJSON(t, h, "run_action", `{"context":"work","fleet":"prod","runner":"r","action":"a"}`))
	if code != "bad_request" || !strings.Contains(msg, "fleet `prod` in context `work` (argument) is guarded") {
		t.Fatalf("%s %q", code, msg)
	}
	if rc := fb.calls("Resolve"); rc[len(rc)-1] != [2]string{"work", "prod"} {
		t.Fatalf("resolve %v", rc)
	}
	n := len(fb.Calls)
	code, _ = errorOf(t, callJSON(t, h, "run_action", `{"context":"Bad Ctx","fleet":"prod","runner":"r","action":"a"}`))
	if code != "bad_request" || len(fb.Calls) != n {
		t.Fatalf("bad context: %s", code)
	}
}

func TestToolArgumentValidation(t *testing.T) {
	fb := &fakeBackend{}
	h := startTools(t, ListTools(fb))
	cases := []struct{ tool, args string }{
		{"list_fleets", `{"bogus":1}`},
		{"list_runners", `{"fleet":"dev","extra":true}`},
		{"workspace_list", `{"fleet":5}`},
		{"run_action", `{"runner":"r","action":"a","nope":1}`},
		{"run_action", `{"runner":"r"}`},
		{"run_action", `{"runner":"r","action":"a","timeout_s":-1}`},
		{"run_action", `{"runner":"r","action":"a","fleet":"Bad Name"}`},
		{"run_action", `{"runner":"r","action":"a","params":{"k":1}}`},
		{"list_runners", `{"fleet":"a/b"}`},
		{"list_fleets", `[]`},
		{"list_fleets", `"x"`},
	}
	for _, c := range cases {
		code, _ := errorOf(t, callJSON(t, h, c.tool, c.args))
		if code != "bad_request" {
			t.Errorf("%s %s: code %s", c.tool, c.args, code)
		}
	}
	if len(fb.Calls) != 0 {
		t.Errorf("backend reached on bad args: %v", fb.Calls)
	}
}

func TestNotSetUpReachesClient(t *testing.T) {
	fb := &fakeBackend{}
	fb.ContextNamesFn = func() ([]string, error) { return nil, fmt.Errorf("wrapped: %w", orch.ErrNotSetUp) }
	fb.ResolveFn = func(context.Context, string, string) (orch.Target, error) { return orch.Target{}, orch.ErrNotSetUp }
	h := startTools(t, ListTools(fb))
	for _, c := range []struct{ tool, args string }{
		{"list_fleets", `{}`}, {"list_runners", `{}`}, {"run_action", `{"runner":"r","action":"a"}`}, {"workspace_list", `{}`},
	} {
		code, msg := errorOf(t, callJSON(t, h, c.tool, c.args))
		if code != api.CodeUnavailable || msg != "not set up: run `mirafleet mcp login …`" || strings.Contains(msg, "/") {
			t.Errorf("%s: %s %q", c.tool, code, msg)
		}
	}
}

func TestToolErrorMapping(t *testing.T) {
	var e *Error
	if err := toolError(context.Canceled); !asErr(err, &e) || e.Code != "canceled" {
		t.Errorf("canceled: %v", err)
	}
	if err := toolError(context.DeadlineExceeded); !asErr(err, &e) || e.Code != api.CodeTimeout {
		t.Errorf("timeout: %v", err)
	}
	if toolError(nil) != nil {
		t.Error("nil")
	}
	h := startTools(t, []Tool{errTool("boom", func(context.Context, Request) (any, error) {
		return nil, toolError(fmt.Errorf("open /home/x/.ssh/id: denied"))
	})})
	code, msg := errorOf(t, callJSON(t, h, "boom", `{}`))
	if code != "internal" || strings.Contains(msg, "/home") {
		t.Errorf("%s %q", code, msg)
	}
}

func asErr(err error, target **Error) bool {
	e, ok := err.(*Error)
	if ok {
		*target = e
	}
	return ok
}

func TestListsCapAt200(t *testing.T) {
	fb := &fakeBackend{Contexts: []string{"main"}}
	fb.FleetsListFn = func(context.Context, string) (*api.FleetsListResult, error) {
		r := &api.FleetsListResult{}
		for i := 0; i < 250; i++ {
			r.Fleets = append(r.Fleets, api.FleetInfo{Name: fmt.Sprintf("f%d", i), Mode: "actions"})
		}
		return r, nil
	}
	fb.ResolveFn = func(_ context.Context, c, f string) (orch.Target, error) {
		return orch.Target{Context: "main", ContextSource: orch.SrcOnly, Fleet: "dev", FleetSource: orch.SrcArgument}, nil
	}
	fb.RunnersListFn = func(context.Context, string, string) (*api.RunnersListResult, error) {
		r := &api.RunnersListResult{}
		for i := 0; i < 250; i++ {
			r.Runners = append(r.Runners, api.RunnerInfo{Name: fmt.Sprintf("r%d", i), Status: "online"})
		}
		return r, nil
	}
	fb.WorkspaceListFn = func(context.Context, string, string) (*api.WorkspaceListResult, error) {
		r := &api.WorkspaceListResult{}
		for i := 0; i < 250; i++ {
			r.Workspaces = append(r.Workspaces, api.WorkspaceInfo{ID: fmt.Sprintf("w%d", i), Fleet: "dev", State: "open"})
		}
		return r, nil
	}
	h := startTools(t, ListTools(fb))
	var f fleetsOut
	okJSON(t, callJSON(t, h, "list_fleets", `{}`), &f)
	var r runnersOut
	okJSON(t, callJSON(t, h, "list_runners", `{}`), &r)
	var w workspacesOut
	okJSON(t, callJSON(t, h, "workspace_list", `{}`), &w)
	if len(f.Fleets) != 200 || !f.Truncated || len(r.Runners) != 200 || !r.Truncated || len(w.Workspaces) != 200 || !w.Truncated {
		t.Fatalf("caps: %d %v / %d %v / %d %v", len(f.Fleets), f.Truncated, len(r.Runners), r.Truncated, len(w.Workspaces), w.Truncated)
	}
	if w.Where.Fleet != "dev" || w.Where.ContextSource != orch.SrcOnly {
		t.Fatalf("where %+v", w.Where)
	}
}
