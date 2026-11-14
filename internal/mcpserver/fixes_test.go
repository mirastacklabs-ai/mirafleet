package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/orch"
)

func TestDecodeArgsExactKeys(t *testing.T) {
	type inner struct {
		A int `json:"a"`
	}
	type args struct {
		Confirm string            `json:"confirm"`
		Fleet   string            `json:"fleet"`
		Env     map[string]string `json:"env"`
		In      inner             `json:"in"`
		List    []inner           `json:"list"`
	}
	for _, c := range []struct {
		in   string
		want string // "" = accepted
	}{
		{`{"confirm":"prod","fleet":"x"}`, ""},
		{`{"Confirm":"prod"}`, `mis-cased argument "Confirm"`},
		{`{"CONFIRM":"prod"}`, `mis-cased argument "CONFIRM"`},
		{`{"Fleet":"prod"}`, `mis-cased argument "Fleet"`},
		{`{"confirm":"a","confirm":"b"}`, `duplicate argument "confirm"`},
		{`{"confirm":"","Confirm":"x"}`, `mis-cased argument "Confirm"`},
		{`{"env":{"Foo":"a","FOO":"b","confirm":"c"}}`, ""},
		{`{"in":{"A":1}}`, `mis-cased argument "A"`},
		{`{"list":[{"a":1},{"A":2}]}`, `mis-cased argument "A"`},
		{`{"nope":1}`, `argument "nope"`},
	} {
		var v args
		err := DecodeArgs(Request{Args: json.RawMessage(c.in)}, &v)
		var e *Error
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", c.in, err)
		case c.want != "" && (!errors.As(err, &e) || e.Code != "bad_request" || !strings.Contains(e.Message, c.want) || len(e.Message) > 80):
			t.Errorf("%s: %v", c.in, err)
		}
	}
	long := `{"` + strings.Repeat("Z", 500) + `":1}`
	var v args
	var e *Error
	if err := DecodeArgs(Request{Args: json.RawMessage(long)}, &v); !errors.As(err, &e) || len(e.Message) > 80 {
		t.Errorf("long key not clipped: %v", err)
	}
}

func TestEveryToolRejectsMiscasedAndDuplicateKeys(t *testing.T) {
	fb := &fakeBackend{Contexts: []string{"main"}}
	h := startTools(t, append(ListTools(fb), WorkspaceTools(fb)...))
	cases := []struct{ tool, args string }{
		{"list_fleets", `{"Context":"main"}`},
		{"list_runners", `{"Fleet":"dev"}`},
		{"list_runners", `{"fleet":"dev","fleet":"prod"}`},
		{"run_action", `{"runner":"r","action":"a","fleet":"prod","Confirm":"prod"}`},
		{"run_action", `{"runner":"r","action":"a","fleet":"prod","confirm":"","Confirm":"prod"}`},
		{"workspace_list", `{"FLEET":"prod"}`},
		{"workspace_open", `{"fleet":"prod","Confirm":"prod"}`},
		{"workspace_open", `{"fleet":"prod","confirm":"","Confirm":"x"}`},
		{"workspace_open", `{"fleet":"prod","confirm":"prod","confirm":"prod"}`},
		{"exec", `{"command":"id","fleet":"prod","CONFIRM":"prod"}`},
		{"exec", `{"command":"id","Fleet":"prod"}`},
		{"read_file", `{"path":"p","Fleet":"prod"}`},
		{"write_file", `{"path":"p","content":"x","fleet":"prod","Confirm":"prod"}`},
		{"workspace_close", `{"Workspace":"ws-1"}`},
	}
	for _, c := range cases {
		code, msg := errorOf(t, callJSON(t, h, c.tool, c.args))
		if code != "bad_request" || !(strings.Contains(msg, "mis-cased") || strings.Contains(msg, "duplicate")) {
			t.Errorf("%s %s: %s %q", c.tool, c.args, code, msg)
		}
	}
	if len(fb.Calls) != 0 {
		t.Fatalf("backend reached: %v", fb.Calls)
	}
}

func TestMapKeysStayData(t *testing.T) {
	r := newWSRig(t, "dev")
	r.mustCall(t, "exec", `{"command":"x","env":{"FOO":"a","BAR":"b"}}`)
	if p := r.f.calls("WorkspaceExec")[0].(api.WorkspaceExecParams); p.Env["FOO"] != "a" {
		t.Fatalf("%+v", p.Env)
	}
	r.mustCall(t, "workspace_open", `{"labels":{"Gpu":"yes","gpu":"no"}}`)
	if p := r.f.calls("WorkspaceOpen")[1].(api.WorkspaceOpenParams); p.Labels["Gpu"] != "yes" {
		t.Fatalf("%+v", p.Labels)
	}
}

func bigRunners(n, actions int) *api.RunnersListResult {
	res := &api.RunnersListResult{}
	for i := 0; i < n; i++ {
		ri := api.RunnerInfo{Name: fmt.Sprintf("runner-%03d", i), Status: "online", Online: true, FreeSlots: 1}
		for j := 0; j < actions; j++ {
			ri.Actions = append(ri.Actions, api.ActionInfo{
				Name:   fmt.Sprintf("action-%03d", j),
				Params: []api.ActionParamInfo{{Name: "target", Pattern: "^[a-z0-9._-]{1,64}$"}, {Name: "ref", Pattern: "^[a-f0-9]{7,40}$"}},
			})
		}
		res.Runners = append(res.Runners, ri)
	}
	return res
}

func TestListRunnersByteBudget(t *testing.T) {
	fb := &fakeBackend{Cap: 8 << 10}
	fb.RunnersListFn = func(context.Context, string, string) (*api.RunnersListResult, error) { return bigRunners(30, 40), nil }
	fb.ResolveFn = func(context.Context, string, string) (orch.Target, error) {
		return orch.Target{Context: "main", ContextSource: orch.SrcOnly, Fleet: "dev", FleetSource: orch.SrcArgument}, nil
	}
	h := startTools(t, ListTools(fb))
	var out runnersOut
	text := okJSON(t, callJSON(t, h, "list_runners", `{}`), &out)
	if len(text) > fb.Cap {
		t.Fatalf("encoded result %d > cap %d", len(text), fb.Cap)
	}
	if len(out.Runners) != 30 || len(out.Runners[0].Actions) == 0 || out.Runners[0].ActionsOmitted {
		t.Fatalf("first runner %+v (%d runners)", out.Runners[0], len(out.Runners))
	}
	last := out.Runners[len(out.Runners)-1]
	if len(last.Actions) != 0 || !last.ActionsOmitted {
		t.Fatalf("last runner kept actions: %+v", last)
	}
	// One runner on request: its actions come back (as many as the budget allows).
	text = okJSON(t, callJSON(t, h, "list_runners", `{"runner":"runner-029"}`), &out)
	if len(out.Runners) != 1 || out.Runners[0].Name != "runner-029" || len(out.Runners[0].Actions) == 0 || len(text) > fb.Cap {
		t.Fatalf("single runner: %d runners, %d bytes", len(out.Runners), len(text))
	}
	if code, _ := errorOf(t, callJSON(t, h, "list_runners", `{"runner":"nope"}`)); code != api.CodeNotFound {
		t.Fatalf("code %s", code)
	}
	// Even the bare entries do not fit: truncated, still within the cap.
	fb.Cap = 900
	text = okJSON(t, callJSON(t, h, "list_runners", `{}`), &out)
	if !out.Truncated || len(out.Runners) == 0 || len(out.Runners) >= 30 || len(text) > fb.Cap {
		t.Fatalf("truncated=%v runners=%d bytes=%d", out.Truncated, len(out.Runners), len(text))
	}
}

func TestListFleetsExplicitContextFailureIsToolError(t *testing.T) {
	fb := &fakeBackend{Contexts: []string{"a", "b"}}
	fb.FleetsListFn = func(_ context.Context, c string) (*api.FleetsListResult, error) {
		if c == "b" {
			return nil, &orch.Error{Code: api.CodeUnavailable, Message: "cannot reach central"}
		}
		if c == "c" {
			return nil, errors.New("dial tcp 10.0.0.9: secret detail")
		}
		return &api.FleetsListResult{}, nil
	}
	h := startTools(t, ListTools(fb))
	code, msg := errorOf(t, callJSON(t, h, "list_fleets", `{"context":"b"}`))
	if code != api.CodeUnavailable || msg != "cannot reach central" {
		t.Fatalf("%s %q", code, msg)
	}
	// In the all-contexts form an internal error is reported generically and logged.
	fb.Contexts = []string{"a", "c"}
	var out fleetsOut
	text := okJSON(t, callJSON(t, h, "list_fleets", `{}`), &out)
	if len(out.Errors) != 1 || out.Errors[0].Code != "internal" || strings.Contains(text, "secret") {
		t.Fatalf("%s", text)
	}
	if !strings.Contains(h.logs.String(), "secret detail") {
		t.Fatalf("not logged: %q", h.logs.String())
	}
}

func TestWSMissingFileKeepsBindingWorkspaceGoneDropsIt(t *testing.T) {
	r := newWSRig(t, "dev")
	r.mustCall(t, "exec", `{"command":"a"}`) // binds ws-1
	r.f.WorkspaceReadFileFn = func(context.Context, string, api.WorkspaceReadFileParams) (*api.WorkspaceReadFileResult, []byte, error) {
		return nil, nil, &Error{Code: api.CodeNotFound, Message: "no such file"}
	}
	r.f.WorkspaceListFn = func(_ context.Context, c, f string) (*api.WorkspaceListResult, error) {
		if c != "main" || f != "dev" {
			t.Errorf("list(%s,%s)", c, f)
		}
		return &api.WorkspaceListResult{Workspaces: []api.WorkspaceInfo{{ID: "ws-1", Fleet: "dev", State: "open"}}}, nil
	}
	r.wantErr(t, "read_file", `{"path":"missing"}`, "not_found", "")
	r.mustCall(t, "exec", `{"command":"b"}`)
	if n := len(r.f.calls("WorkspaceOpen")); n != 1 {
		t.Fatalf("%d opens: a missing file unbound a healthy workspace", n)
	}
	// Listed but closing: not live, so the binding goes.
	r.f.WorkspaceListFn = func(context.Context, string, string) (*api.WorkspaceListResult, error) {
		return &api.WorkspaceListResult{Workspaces: []api.WorkspaceInfo{{ID: "ws-1", Fleet: "dev", State: "closing"}}}, nil
	}
	r.wantErr(t, "read_file", `{"path":"missing"}`, "not_found", "")
	r.mustCall(t, "exec", `{"command":"c"}`)
	if n := len(r.f.calls("WorkspaceOpen")); n != 2 {
		t.Fatalf("%d opens, want a fresh one after the workspace went away", n)
	}
	// The list itself failing keeps the binding.
	r.f.WorkspaceListFn = func(context.Context, string, string) (*api.WorkspaceListResult, error) {
		return nil, &Error{Code: api.CodeUnavailable, Message: "down"}
	}
	r.wantErr(t, "read_file", `{"path":"missing"}`, "not_found", "")
	r.mustCall(t, "exec", `{"command":"d"}`)
	if n := len(r.f.calls("WorkspaceOpen")); n != 2 {
		t.Fatalf("%d opens", n)
	}
}

func TestWSExplicitWorkspaceReportsItsRealFleet(t *testing.T) {
	r := newWSRig(t, "dev")
	r.f.WorkspaceListFn = func(_ context.Context, _, f string) (*api.WorkspaceListResult, error) {
		if f != "" {
			t.Errorf("explicit lookup filtered by fleet %q", f)
		}
		return &api.WorkspaceListResult{Workspaces: []api.WorkspaceInfo{{ID: "ws-p", Fleet: "prod", State: "open"}}}, nil
	}
	r.f.WorkspaceExecFn = func(_ context.Context, _ string, p api.WorkspaceExecParams, _ []byte) (*api.WorkspaceExecResult, error) {
		if p.Confirm != "prod" {
			return nil, &Error{Code: "bad_request", Message: "fleet requires confirm to equal the fleet name"}
		}
		return &api.WorkspaceExecResult{}, nil
	}
	v, code, msg := r.call(t, "exec", `{"command":"id","workspace":"ws-p"}`)
	if v != nil || code != "bad_request" || !strings.Contains(msg, "fleet `prod` in context `main`") {
		t.Fatalf("%v %s %q", v, code, msg)
	}
	v = r.mustCall(t, "exec", `{"command":"id","workspace":"ws-p","confirm":"prod"}`)
	w := v["where"].(map[string]any)
	if w["fleet"] != "prod" || w["fleet_source"] != srcWorkspace {
		t.Fatalf("where %v", w)
	}
	if n := len(r.f.calls("WorkspaceList")); n != 1 {
		t.Fatalf("%d list calls, want the fleet cached per id", n)
	}
	r.wantErr(t, "exec", `{"command":"id","workspace":"ws-unknown"}`, "not_found", "")
	if n := len(r.f.calls("WorkspaceExec")); n != 2 {
		t.Fatalf("%d execs reached the backend", n)
	}
}

func TestWSBindingIsPerSession(t *testing.T) {
	r := newWSRig(t, "dev")
	r.f.Session = "cc:one"
	if v := r.mustCall(t, "exec", `{"command":"a"}`); v["workspace"] != "ws-1" {
		t.Fatalf("%v", v)
	}
	r.mustCall(t, "exec", `{"command":"a"}`)
	r.f.Session = "cc:two" // after /clear
	if v := r.mustCall(t, "exec", `{"command":"b"}`); v["workspace"] != "ws-2" {
		t.Fatalf("%v", v)
	}
	if n := len(r.f.calls("WorkspaceOpen")); n != 2 {
		t.Fatalf("%d opens, want 2", n)
	}
	ops := r.f.calls("WorkspaceOpen")
	if ops[1].(api.WorkspaceOpenParams).Session != "cc:two" {
		t.Fatalf("%v", ops)
	}
	r.f.Session = "cc:one" // back: the old binding is still there
	if v := r.mustCall(t, "exec", `{"command":"c"}`); v["workspace"] != "ws-1" {
		t.Fatalf("%v", v)
	}
	if n := len(r.f.calls("WorkspaceOpen")); n != 2 {
		t.Fatalf("%d opens", n)
	}
}

func TestWSImplicitOpenFailsClosedOnUnknownFleet(t *testing.T) {
	r := newWSRig(t, "ghost")
	r.wantErr(t, "exec", `{"command":"id"}`, "not_found", "fleet")
	if n := len(r.f.calls("WorkspaceOpen")); n != 0 {
		t.Fatalf("%d opens on an unknown fleet", n)
	}
}

func TestWSReadFileSanitizedFlagAndEmptyPage(t *testing.T) {
	r := newWSRig(t, "dev")
	body := []byte("plain\n")
	size := int64(len(body))
	eof := true
	r.f.WorkspaceReadFileFn = func(context.Context, string, api.WorkspaceReadFileParams) (*api.WorkspaceReadFileResult, []byte, error) {
		return &api.WorkspaceReadFileResult{Size: size, Length: len(body), EOF: eof}, body, nil
	}
	if v := r.mustCall(t, "read_file", `{"path":"f"}`); v["sanitized"] != nil {
		t.Fatalf("clean text flagged: %v", v)
	}
	body = []byte("a\x1b[31mred\x1b[0m\r\n")
	if v := r.mustCall(t, "read_file", `{"path":"f"}`); v["sanitized"] != true {
		t.Fatalf("%v", v)
	}
	if v := r.mustCall(t, "read_file", `{"path":"f","encoding":"base64"}`); v["sanitized"] != nil {
		t.Fatalf("%v", v)
	}
	// A page that is only the start of a rune, not at EOF: an error, never an empty page.
	body, size, eof = []byte{0xe2, 0x82}, 100, false
	r.wantErr(t, "read_file", `{"path":"f"}`, "bad_request", "not valid UTF-8")
	// A valid empty page before the end would also loop forever.
	body = nil
	r.wantErr(t, "read_file", `{"path":"f"}`, api.CodeUnavailable, "empty page")
	lt, _ := r.h.cs.ListTools(context.Background(), nil)
	for _, tl := range lt.Tools {
		if tl.Name == "read_file" && !strings.Contains(tl.Description, "SANITISED") {
			t.Errorf("description: %q", tl.Description)
		}
	}
}

func TestWSWriteFileOffsetOverflowAndWrappedFailures(t *testing.T) {
	r := newWSRig(t, "dev")
	r.wantErr(t, "write_file", `{"path":"x","content":"abc","offset":9223372036854775806}`, "bad_request", "offset")
	if n := len(r.f.calls("WorkspaceWriteFile")); n != 0 {
		t.Fatalf("%d writes", n)
	}
	r.f.WorkspaceWriteFileFn = func(context.Context, string, api.WorkspaceWriteFileParams, []byte) (*api.WorkspaceWriteFileResult, error) {
		return nil, errors.New("dial unix /run/secret.sock: refused")
	}
	v, code, msg := r.call(t, "write_file", `{"path":"x","content":"abc"}`)
	if v != nil || code != "internal" || !strings.Contains(msg, "(0 of 3 bytes were written)") || strings.Contains(msg, "secret") {
		t.Fatalf("%v %s %q", v, code, msg)
	}
	if !strings.Contains(r.h.logs.String(), "secret.sock") {
		t.Fatalf("not logged: %q", r.h.logs.String())
	}
}
