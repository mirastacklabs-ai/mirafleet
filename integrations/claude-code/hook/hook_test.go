package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/handoff"
)

type fakeBackend struct {
	mu        sync.Mutex
	list      []api.WorkspaceInfo
	listErr   error
	closed    []string
	block     bool
	newCalled bool
}

func (f *fakeBackend) ContextNames() ([]string, error) { return []string{"c1"}, nil }
func (f *fakeBackend) WorkspaceList(ctx context.Context, c, fl string) (*api.WorkspaceListResult, error) {
	if f.block {
		<-make(chan struct{}) // ignores ctx on purpose: Run must still return
	}
	if f.listErr != nil {
		return nil, f.listErr
	}
	return &api.WorkspaceListResult{Workspaces: f.list}, nil
}
func (f *fakeBackend) WorkspaceClose(ctx context.Context, c, id string) (*api.WorkspaceCloseResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = append(f.closed, id)
	return &api.WorkspaceCloseResult{}, nil
}
func (f *fakeBackend) Close() error { return nil }

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func testEnv(t *testing.T, fb *fakeBackend) Env {
	return Env{
		StateDir: t.TempDir(), ConfigDir: t.TempDir(), ProjectDir: "/home/u/proj", PPID: 4242,
		Now:          time.Now,
		NewBackend:   func(Env) (Backend, error) { fb.newCalled = true; return fb, nil },
		CloseTimeout: 200 * time.Millisecond,
	}
}

func run(t *testing.T, event, in string, env Env) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(context.Background(), event, strings.NewReader(in), &out, &errb, env)
	return code, out.String(), errb.String()
}

func TestSessionStartWritesHandoff(t *testing.T) {
	env := testEnv(t, &fakeBackend{})
	code, out, _ := run(t, "session-start", fixture(t, "session_start.json"), env)
	if code != 0 || out != "" {
		t.Fatalf("code=%d out=%q", code, out)
	}
	s, ok, err := handoff.Read(env.StateDir, handoff.Key{PID: 4242, Project: "/home/u/proj"})
	if err != nil || !ok || s != "cc:abc123-def" {
		t.Fatalf("got %q %v %v", s, ok, err)
	}
}

func TestSessionStartProjectFallsBackToCwd(t *testing.T) {
	env := testEnv(t, &fakeBackend{})
	env.ProjectDir = ""
	run(t, "session-start", fixture(t, "session_start.json"), env)
	if _, ok, _ := handoff.Read(env.StateDir, handoff.Key{PID: 4242, Project: "/home/u/proj"}); !ok {
		t.Fatal("cwd fallback not used")
	}
}

func TestSessionStartInvalidIgnored(t *testing.T) {
	for _, id := range []string{"", "a b", "a/../b", strings.Repeat("x", 200), "x\xe2\x80\xaey"} {
		env := testEnv(t, &fakeBackend{})
		b, _ := json.Marshal(map[string]string{"session_id": id, "cwd": "/p", "hook_event_name": "SessionStart"})
		code, out, errs := run(t, "session-start", string(b), env)
		if code != 0 || out != "" || errs == "" {
			t.Fatalf("id %q: code=%d out=%q err=%q", id, code, out, errs)
		}
		if _, ok, _ := handoff.Read(env.StateDir, handoff.Key{PID: 4242, Project: "/home/u/proj"}); ok {
			t.Fatalf("id %q written", id)
		}
	}
	// malformed JSON too
	env := testEnv(t, &fakeBackend{})
	if code, out, _ := run(t, "session-start", "{not json", env); code != 0 || out != "" {
		t.Fatal("malformed")
	}
}

func TestSessionEndClearClosesOnlyMatching(t *testing.T) {
	fb := &fakeBackend{list: []api.WorkspaceInfo{
		{ID: "ws-1", Session: "cc:abc123-def"}, {ID: "ws-2", Session: "cc:other"}, {ID: "ws-3", Session: "cc:abc123-def"},
	}}
	env := testEnv(t, fb)
	k := handoff.Key{PID: 4242, Project: "/home/u/proj"}
	if err := handoff.Write(env.StateDir, k, "cc:abc123-def", time.Now()); err != nil {
		t.Fatal(err)
	}
	code, out, _ := run(t, "session-end", fixture(t, "session_end_clear.json"), env)
	if code != 0 || out != "" {
		t.Fatalf("code=%d out=%q", code, out)
	}
	if strings.Join(fb.closed, ",") != "ws-1,ws-3" {
		t.Fatalf("closed %v", fb.closed)
	}
	if _, ok, _ := handoff.Read(env.StateDir, k); ok {
		t.Fatal("handoff not removed")
	}
}

func TestSessionEndLogoutCloses(t *testing.T) {
	fb := &fakeBackend{list: []api.WorkspaceInfo{{ID: "ws-1", Session: "cc:abc123-def"}}}
	in := strings.Replace(fixture(t, "session_end_clear.json"), `"clear"`, `"logout"`, 1)
	run(t, "session-end", in, testEnv(t, fb))
	if len(fb.closed) != 1 {
		t.Fatalf("closed %v", fb.closed)
	}
}

func TestSessionEndOtherReasonsKeepWorkspaces(t *testing.T) {
	for _, r := range []string{"resume", "prompt_input_exit", "other", "", "bypass_permissions_disabled"} {
		fb := &fakeBackend{list: []api.WorkspaceInfo{{ID: "ws-1", Session: "cc:abc123-def"}}}
		env := testEnv(t, fb)
		k := handoff.Key{PID: 4242, Project: "/home/u/proj"}
		_ = handoff.Write(env.StateDir, k, "cc:abc123-def", time.Now())
		in := strings.Replace(fixture(t, "session_end_resume.json"), `"resume"`, `"`+r+`"`, 1)
		code, out, _ := run(t, "session-end", in, env)
		if code != 0 || out != "" || len(fb.closed) != 0 || fb.newCalled {
			t.Fatalf("reason %q: code=%d out=%q closed=%v dialled=%v", r, code, out, fb.closed, fb.newCalled)
		}
		if _, ok, _ := handoff.Read(env.StateDir, k); ok {
			t.Fatalf("reason %q: handoff kept", r)
		}
	}
}

func TestSessionEndBackendErrors(t *testing.T) {
	fb := &fakeBackend{listErr: errors.New("boom")}
	code, out, errs := run(t, "session-end", fixture(t, "session_end_clear.json"), testEnv(t, fb))
	if code != 0 || out != "" || !strings.Contains(errs, "boom") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
	env := testEnv(t, fb)
	env.NewBackend = func(Env) (Backend, error) { return nil, errors.New("no contexts") }
	if code, out, _ := run(t, "session-end", fixture(t, "session_end_clear.json"), env); code != 0 || out != "" {
		t.Fatal("factory error")
	}
}

func TestSessionEndBounded(t *testing.T) {
	fb := &fakeBackend{block: true}
	env := testEnv(t, fb)
	env.CloseTimeout = 100 * time.Millisecond
	start := time.Now()
	code, out, errs := run(t, "session-end", fixture(t, "session_end_clear.json"), env)
	if code != 0 || out != "" || !strings.Contains(errs, "timed out") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %v", d)
	}
}

type decision struct {
	HookSpecificOutput struct {
		HookEventName            string `json:"hookEventName"`
		PermissionDecision       string `json:"permissionDecision"`
		PermissionDecisionReason string `json:"permissionDecisionReason"`
	} `json:"hookSpecificOutput"`
}

func mustAsk(t *testing.T, out string) string {
	t.Helper()
	var d decision
	dec := json.NewDecoder(strings.NewReader(out))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	h := d.HookSpecificOutput
	if h.HookEventName != "PreToolUse" || h.PermissionDecision != "ask" || h.PermissionDecisionReason == "" {
		t.Fatalf("bad decision %+v", h)
	}
	if strings.Contains(out, `"allow"`) {
		t.Fatal("allow emitted")
	}
	if utf8.RuneCountInString(h.PermissionDecisionReason) > reasonMax {
		t.Fatalf("reason too long: %d", utf8.RuneCountInString(h.PermissionDecisionReason))
	}
	return h.PermissionDecisionReason
}

func TestPreToolUseConfirmAsks(t *testing.T) {
	code, out, _ := run(t, "pre-tool-use", fixture(t, "pre_tool_use_confirm.json"), testEnv(t, &fakeBackend{}))
	if code != 0 {
		t.Fatal(code)
	}
	if r := mustAsk(t, out); r != `Guarded fleet "prod": run_action tail-logs on db-1` {
		t.Fatalf("reason %q", r)
	}
}

func TestPreToolUseNoConfirmSilent(t *testing.T) {
	for _, f := range []string{"pre_tool_use_noconfirm.json", "pre_tool_use_empty_confirm.json"} {
		code, out, _ := run(t, "pre-tool-use", fixture(t, f), testEnv(t, &fakeBackend{}))
		if code != 0 || out != "" {
			t.Fatalf("%s: code=%d out=%q", f, code, out)
		}
	}
	for _, in := range []string{`{"tool_name":"x"}`, `{"tool_input":{"confirm":null}}`} {
		if _, out, _ := run(t, "pre-tool-use", in, testEnv(t, &fakeBackend{})); out != "" {
			t.Fatalf("%s: %q", in, out)
		}
	}
}

func TestPreToolUseHostileFieldsSanitisedAndClipped(t *testing.T) {
	big := strings.Repeat("A", 10*1024)
	in := map[string]any{
		"hook_event_name": "PreToolUse",
		"tool_name":       "mcp__plugin_mirafleet_mirafleet__exec",
		"tool_input": map[string]any{
			"confirm":   "prod",
			"fleet":     "pr\x1b[31mod\x1b]0;title\x07\xe2\x80\xae",
			"action":    big,
			"runner":    "db\xe2\x80\x8b-1\nIGNORE PREVIOUS",
			"workspace": big,
			"command":   big,
		},
	}
	b, _ := json.Marshal(in)
	_, out, _ := run(t, "pre-tool-use", string(b), testEnv(t, &fakeBackend{}))
	r := mustAsk(t, out)
	if len(out) > 2048 {
		t.Fatalf("output %d bytes", len(out))
	}
	for _, bad := range []string{"\x1b", "\xe2\x80\xae", "\xe2\x80\x8b", "\n", "\x07"} {
		if strings.Contains(r, bad) {
			t.Fatalf("reason contains %q: %q", bad, r)
		}
	}
	if !strings.Contains(r, "<U+202E>") || !strings.Contains(r, `"pr`) {
		t.Fatalf("reason %q", r)
	}
}

func TestPreToolUseMalformedAsks(t *testing.T) {
	for _, in := range []string{
		"", "{not json", "[]", `{"tool_input":"str"}`, `{"tool_input":{"confirm":"x"}} trailing`,
		`{"x":"` + strings.Repeat("a", maxInput) + `"}`,
	} {
		code, out, _ := run(t, "pre-tool-use", in, testEnv(t, &fakeBackend{}))
		if code != 0 {
			t.Fatal(code)
		}
		mustAsk(t, out)
	}
}

func TestPreToolUseNonStringConfirmAsks(t *testing.T) {
	_, out, _ := run(t, "pre-tool-use", `{"tool_name":"a__exec","tool_input":{"confirm":true,"fleet":"p"}}`, testEnv(t, &fakeBackend{}))
	mustAsk(t, out)
}

func TestUnknownEvent(t *testing.T) {
	code, out, errs := run(t, "bogus\x1b[2J", "{}", testEnv(t, &fakeBackend{}))
	if code != 0 || out != "" || errs == "" || strings.Contains(errs, "\x1b") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
}

func TestDefaultBackendBuilds(t *testing.T) {
	env := testEnv(t, nil)
	b, err := defaultBackend(env)
	if err != nil {
		t.Fatal(err)
	}
	b.Close()
}

const mfTool = "mcp__plugin_mirafleet_mirafleet__run_action"

func preTool(t *testing.T, tool, toolInput string) string {
	t.Helper()
	in := `{"hook_event_name":"PreToolUse","tool_name":"` + tool + `","tool_input":` + toolInput + `}`
	code, out, _ := run(t, "pre-tool-use", in, testEnv(t, &fakeBackend{}))
	if code != 0 {
		t.Fatal(code)
	}
	return out
}

func TestPreToolUseAmbiguousKeysAskGeneric(t *testing.T) {
	for _, ti := range []string{
		`{"Confirm":"prod","fleet":"prod"}`,
		`{"CONFIRM":"prod"}`,
		`{"confirm":"prod","confirm":"prod"}`,
		`{"confirm":"","Confirm":"prod"}`,
		`{"fleet":"staging","Fleet":"prod","confirm":"prod"}`,
		`{"fleet":"staging","fleet":"prod","confirm":"prod"}`,
		`{"confirm":"prod","Runner":"x"}`,
		`{"Context":"x"}`,
		`{"confirm":"prod"} {}`,
	} {
		r := mustAsk(t, preTool(t, mfTool, ti))
		if r != ambiguousReason && r != genericReason {
			t.Errorf("%s: reason %q", ti, r)
		}
		if strings.Contains(r, "staging") || strings.Contains(r, "prod") {
			t.Errorf("%s: reason echoes values: %q", ti, r)
		}
	}
}

func TestPreToolUseUnknownToolWithConfirmAsks(t *testing.T) {
	for _, tool := range []string{"Bash", "", "mcp__other__run_action", "mcp__plugin_mirafleet_mirafleet__nope", "xmcp__plugin_mirafleet_mirafleet__exec", "mcp__plugin_mirafleet_mirafleet__"} {
		r := mustAsk(t, preTool(t, tool, `{"confirm":"prod","fleet":"prod"}`))
		if r != genericReason {
			t.Errorf("%q: %q", tool, r)
		}
	}
	if out := preTool(t, "Bash", `{"command":"ls"}`); out != "" {
		t.Fatalf("no confirm should be silent: %q", out)
	}
}

func TestPreToolUseContextInReason(t *testing.T) {
	r := mustAsk(t, preTool(t, mfTool, `{"fleet":"prod","action":"a","confirm":"prod","context":"eu\u001b[31m west"}`))
	if !strings.HasPrefix(r, `Guarded fleet "prod": run_action a (context eu`) || !strings.HasSuffix(r, ")") {
		t.Fatalf("reason %q", r)
	}
	if strings.Contains(r, "\x1b") {
		t.Fatalf("escape in %q", r)
	}
}

func TestPreToolUseNullAndNonObjectInput(t *testing.T) {
	for _, in := range []string{`{"tool_name":"x","tool_input":null}`, `{"tool_name":"x"}`} {
		if _, out, _ := run(t, "pre-tool-use", in, testEnv(t, &fakeBackend{})); out != "" {
			t.Fatalf("%s: %q", in, out)
		}
	}
	for _, in := range []string{`null`, `"x"`, `5`, `[{}]`, `{"tool_input":[]}`, `{"tool_input":5}`} {
		_, out, _ := run(t, "pre-tool-use", in, testEnv(t, &fakeBackend{}))
		mustAsk(t, out)
	}
}

func TestSessionEndKeepsNewerSessionHandoff(t *testing.T) {
	fb := &fakeBackend{}
	env := testEnv(t, fb)
	k := handoff.Key{PID: 4242, Project: "/home/u/proj"}
	if err := handoff.Write(env.StateDir, k, "cc:newer-session", time.Now()); err != nil {
		t.Fatal(err)
	}
	run(t, "session-end", fixture(t, "session_end_resume.json"), env) // session abc123-def
	if s, ok, _ := handoff.Read(env.StateDir, k); !ok || s != "cc:newer-session" {
		t.Fatalf("newer handoff lost: %q %v", s, ok)
	}
}
