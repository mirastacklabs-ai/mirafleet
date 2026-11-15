package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/integrations/claude-code/hook"
	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/orch"
)

type fakeHookBackend struct{ closed []string }

func (f *fakeHookBackend) ContextNames() ([]string, error) { return []string{"c1"}, nil }
func (f *fakeHookBackend) WorkspaceList(context.Context, string, string) (*api.WorkspaceListResult, error) {
	return &api.WorkspaceListResult{Workspaces: []api.WorkspaceInfo{
		{ID: "ws-mine", Session: "cc:abc-123"},
		{ID: "ws-other", Session: "cc:zzz"},
	}}, nil
}
func (f *fakeHookBackend) WorkspaceClose(_ context.Context, _, id string) (*api.WorkspaceCloseResult, error) {
	f.closed = append(f.closed, id)
	return &api.WorkspaceCloseResult{}, nil
}
func (f *fakeHookBackend) Close() error { return nil }

func runHook(t *testing.T, env hook.Env, event, in string) (stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code, err := mcpHook([]string{"claude-code", event}, strings.NewReader(in), &out, &errb, env)
	if err != nil || code != 0 {
		t.Fatalf("mcpHook %s: code %d err %v", event, code, err)
	}
	return out.String(), errb.String()
}

func TestMCPHookRejectsOtherHarnessAndBadArgs(t *testing.T) {
	var o, e bytes.Buffer
	for _, args := range [][]string{{"opencode", "session-start"}, {"claude-code"}, {}, {"claude-code", "x", "y"}} {
		if _, err := mcpHook(args, strings.NewReader("{}"), &o, &e, hook.Env{}); err == nil {
			t.Errorf("args %v: want error", args)
		}
	}
	if err := mcpMain([]string{"hook", "cursor", "session-start"}, strings.NewReader("{}"), &o); err == nil ||
		!strings.Contains(err.Error(), "unknown hook harness") {
		t.Errorf("routed unknown harness: want an \"unknown hook harness\" error, got %v", err)
	}
}

// Hook lifecycle through mcpHook with an injected PPID, state dir and a fake backend (no real
// process, central or Claude Code): session-start handoff -> orch.SessionFor reads cc:<id> ->
// PreToolUse asks only with confirm -> SessionEnd(clear) asks the fake backend to close the
// caller's workspace and removes the handoff. Real stdio and PPID wiring are not covered here.
func TestMCPHookEndToEnd(t *testing.T) {
	fb := &fakeHookBackend{}
	env := hook.Env{
		StateDir: t.TempDir(), ConfigDir: t.TempDir(), ProjectDir: t.TempDir(), PPID: 31337,
		Now:          time.Now,
		NewBackend:   func(hook.Env) (hook.Backend, error) { return fb, nil },
		CloseTimeout: 2 * time.Second,
	}
	out, _ := runHook(t, env, "session-start", `{"session_id":"abc-123","cwd":"/elsewhere","hook_event_name":"SessionStart","source":"startup"}`)
	if out != "" {
		t.Fatalf("session-start wrote stdout: %q", out)
	}

	o, err := orch.New(orch.Config{ConfigDir: env.ConfigDir, StateDir: env.StateDir, ProjectDir: env.ProjectDir, PPID: env.PPID})
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	if s, src := o.SessionFor(orch.Target{}); s != "cc:abc-123" || src != "handoff" {
		t.Fatalf("SessionFor = %q (%s), want cc:abc-123 from handoff", s, src)
	}

	const tool = "mcp__plugin_mirafleet_mirafleet__run_action"
	out, _ = runHook(t, env, "pre-tool-use", `{"hook_event_name":"PreToolUse","tool_name":"`+tool+`","tool_input":{"fleet":"prod","action":"tail-logs","runner":"db-1"}}`)
	if out != "" {
		t.Fatalf("pre-tool-use without confirm wrote %q", out)
	}
	out, _ = runHook(t, env, "pre-tool-use", `{"hook_event_name":"PreToolUse","tool_name":"`+tool+`","tool_input":{"fleet":"prod","action":"tail-logs","runner":"db-1","confirm":"prod"}}`)
	var po struct {
		H struct {
			Decision string `json:"permissionDecision"`
			Reason   string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &po); err != nil || po.H.Decision != "ask" || !strings.Contains(po.H.Reason, "prod") {
		t.Fatalf("pre-tool-use with confirm: %q (%v)", out, err)
	}

	runHook(t, env, "session-end", `{"session_id":"abc-123","hook_event_name":"SessionEnd","reason":"clear"}`)
	if len(fb.closed) != 1 || fb.closed[0] != "ws-mine" {
		t.Fatalf("closed %v, want [ws-mine]", fb.closed)
	}
	if s, _ := o.SessionFor(orch.Target{}); s == "cc:abc-123" {
		t.Fatal("handoff survived session-end")
	}
}

// The plugin manifest version must equal pluginVersion (the binary's own `version` is "dev" until
// the release build links it in).
func TestPluginVersionLockstep(t *testing.T) {
	b, err := os.ReadFile("../../integrations/claude-code/.claude-plugin/plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m.Version != pluginVersion {
		t.Fatalf("plugin.json version %q != pluginVersion %q: bump both", m.Version, pluginVersion)
	}
}

// A local tool in a remote project is denied through the real `mcp hook` entry point.
func TestMCPHookDeniesLocalToolInRemoteProject(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/.mirafleet.yaml", []byte("remote: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := hook.Env{ProjectDir: dir, Now: time.Now}
	out, _ := runHook(t, env, "pre-tool-use", `{"hook_event_name":"PreToolUse","tool_name":"Edit","tool_input":{"file_path":"/x"}}`)
	if !strings.Contains(out, `"permissionDecision":"deny"`) || !strings.Contains(out, "multi_edit") && !strings.Contains(out, "`edit`") {
		t.Fatalf("remote project, Edit: %q", out)
	}
	env.ProjectDir = t.TempDir()
	if out, _ := runHook(t, env, "pre-tool-use", `{"tool_name":"Edit","tool_input":{}}`); out != "" {
		t.Fatalf("no project file, Edit: %q", out)
	}
}
