// Package hook implements the Claude Code hook handlers for the mirafleet plugin
// (`mirafleet mcp hook claude-code <session-start|session-end|pre-tool-use>`, wired in S12).
//
// Hook contract, from the official docs (https://docs.claude.com/en/docs/claude-code/hooks, which
// 301-redirects to https://code.claude.com/docs/en/hooks), as read on the day S11 was written:
//
//   - Input is one JSON object on stdin. Common fields: session_id, transcript_path, cwd,
//     hook_event_name (also permission_mode, prompt_id, agent_id, ... which we ignore: unknown
//     fields are accepted because Claude Code adds them).
//   - SessionStart adds source (startup|resume|clear|compact|fork). Not used here.
//   - SessionEnd documents reason values clear, resume, logout, prompt_input_exit, other.
//   - PreToolUse adds tool_name, tool_input (tool specific object) and tool_use_id. Output is
//     {"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow|deny|ask|defer",
//     "permissionDecisionReason":"..."}}. We emit "ask" (guarded confirm) and "deny" (local tools
//     in a remote project, remote.go); never "allow".
//   - Exit 0 = success (stdout parsed as JSON); exit 2 = blocking error; other = non-blocking.
//     We exit 0 always, so a failing hook can never block a session.
//   - CLAUDE_PROJECT_DIR is exported to the hook process and is the project root.
//
// Not confirmed by the pages we could read (the fetch was truncated before the SessionEnd and
// PreToolUse input-schema sections): that reason sits at the top level of the SessionEnd input
// (assumed, matching the other fields), and the exact field names of the mirafleet tools'
// tool_input (we use fleet, action, runner, workspace; confirm per docs/M5.md §1.7).
//
// Assumption Q2 (docs/M5.md §1.4): the hook process and the MCP server are both direct children of
// Claude Code, so os.Getppid() is the same PID in both and keys the handoff file. If false, the
// MCP server falls back to a per-process session and only --resume reattach is lost.
//
// This package never dials central at SessionStart, never prints on stdout except the
// PreToolUse "ask" JSON, and imports only harness-neutral internals (handoff, orch, api, opconf).
package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/handoff"
	"github.com/mirastacklabs-ai/mirafleet/internal/opconf"
	"github.com/mirastacklabs-ai/mirafleet/internal/orch"
)

const (
	maxInput        = 1 << 20
	sessionPrefix   = "cc:"
	defaultCloseTTL = 10 * time.Second
	fieldMax        = 64  // runes per reason field
	reasonMax       = 200 // runes in the whole reason
	genericReason   = "mirafleet: could not read this tool call; confirm before running it"
	// ambiguousReason must never echo argument values: they cannot be trusted to mean one thing.
	ambiguousReason = "mirafleet call with ambiguous arguments"
	toolPrefix      = "mcp__plugin_mirafleet_mirafleet__"
)

// knownFields are the tool_input keys this hook reads (and the server decodes exact-case).
var knownFields = []string{"confirm", "fleet", "runner", "action", "workspace", "context"}

// knownTools hard-codes the mirafleet MCP tool names (a test checks it against internal/mcpserver).
var knownTools = map[string]bool{
	"list_fleets": true, "list_runners": true, "run_action": true, "workspace_open": true, "exec": true,
	"read_file": true, "write_file": true, "workspace_close": true, "workspace_list": true,
	// M7 remote tools (docs/M7.md §3.7).
	"bash": true, "bash_output": true, "kill_shell": true, "job_list": true, "read_output": true,
	"read": true, "write": true, "edit": true, "multi_edit": true, "grep": true, "glob": true,
	"ls": true, "notebook_edit": true, "upload": true, "download": true,
}

// Backend is the slice of *orch.Orch that SessionEnd needs.
type Backend interface {
	ContextNames() ([]string, error)
	WorkspaceList(ctx context.Context, cname, fleet string) (*api.WorkspaceListResult, error)
	WorkspaceClose(ctx context.Context, cname, id string) (*api.WorkspaceCloseResult, error)
	Close() error
}

// Env carries everything the handlers take from the process, so tests can inject it.
type Env struct {
	StateDir   string // handoff files
	ConfigDir  string // keys, known_hosts, contexts.yaml
	ProjectDir string // CLAUDE_PROJECT_DIR; empty: the hook input's cwd
	Home       string // home directory, bounds the .mirafleet.yaml walk-up; empty: project dir only
	PPID       int    // the harness PID
	Now        func() time.Time

	// NewBackend builds the SessionEnd backend; nil uses orch.New with this Env.
	NewBackend func(Env) (Backend, error)
	// CloseTimeout bounds the SessionEnd workspace close; zero means 10 s.
	CloseTimeout time.Duration
}

// DefaultEnv fills an Env from the process: opconf directories, CLAUDE_PROJECT_DIR, os.Getppid.
// Directory lookup errors leave the field empty; session-start then reports and exits 0.
func DefaultEnv() Env {
	e := Env{PPID: os.Getppid(), ProjectDir: os.Getenv("CLAUDE_PROJECT_DIR"), Now: time.Now}
	e.Home, _ = os.UserHomeDir()
	e.StateDir, _ = opconf.StateDir()
	e.ConfigDir, _ = opconf.Dir()
	return e
}

func defaultBackend(e Env) (Backend, error) {
	return orch.New(orch.Config{ConfigDir: e.ConfigDir, StateDir: e.StateDir, ProjectDir: e.ProjectDir, PPID: e.PPID, Now: e.Now})
}

type input struct {
	SessionID string          `json:"session_id"`
	Cwd       string          `json:"cwd"`
	Event     string          `json:"hook_event_name"`
	Reason    string          `json:"reason"`
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
}

func readInput(r io.Reader) (*input, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxInput+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxInput {
		return nil, fmt.Errorf("input larger than %d bytes", maxInput)
	}
	if t := bytes.TrimSpace(b); len(t) == 0 || t[0] != '{' {
		return nil, fmt.Errorf("input is not a JSON object") // also rejects a top-level null
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	var in input
	if err := dec.Decode(&in); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing data after JSON object")
	}
	return &in, nil
}

// Run handles one hook event and returns the process exit code (always 0).
func Run(ctx context.Context, event string, stdin io.Reader, stdout, stderr io.Writer, env Env) int {
	if env.Now == nil {
		env.Now = time.Now
	}
	switch event {
	case "session-start":
		sessionStart(stdin, stderr, env)
	case "session-end":
		sessionEnd(ctx, stdin, stderr, env)
	case "pre-tool-use":
		preToolUse(stdin, stdout, stderr, env)
	default:
		fmt.Fprintf(stderr, "mirafleet hook: unknown event %q\n", clipRunes(orch.Sanitize([]byte(event)), fieldMax))
	}
	return 0
}

func projectOf(env Env, in *input) string {
	if env.ProjectDir != "" {
		return env.ProjectDir
	}
	return in.Cwd
}

func sessionStart(stdin io.Reader, stderr io.Writer, env Env) {
	in, err := readInput(stdin)
	if err != nil {
		fmt.Fprintf(stderr, "mirafleet hook: session-start: bad input: %v\n", err)
		return
	}
	session := sessionPrefix + in.SessionID
	if e := api.ValidateSession(session); e != nil || in.SessionID == "" {
		fmt.Fprintln(stderr, "mirafleet hook: session-start: ignoring invalid session_id")
		return
	}
	proj := projectOf(env, in)
	if env.StateDir == "" || proj == "" {
		fmt.Fprintln(stderr, "mirafleet hook: session-start: no state or project directory")
		return
	}
	if err := handoff.Write(env.StateDir, handoff.Key{PID: env.PPID, Project: proj}, session, env.Now()); err != nil {
		fmt.Fprintf(stderr, "mirafleet hook: session-start: handoff: %v\n", err)
	}
}

func sessionEnd(ctx context.Context, stdin io.Reader, stderr io.Writer, env Env) {
	in, err := readInput(stdin)
	if err != nil {
		fmt.Fprintf(stderr, "mirafleet hook: session-end: bad input: %v\n", err)
		return
	}
	if env.StateDir != "" {
		if proj := projectOf(env, in); proj != "" {
			// Only remove the handoff if it still names this session: after a /clear or a
			// resume, a newer session may already have rewritten it.
			if _, err := handoff.RemoveIf(env.StateDir, handoff.Key{PID: env.PPID, Project: proj}, sessionPrefix+in.SessionID); err != nil {
				fmt.Fprintf(stderr, "mirafleet hook: session-end: handoff: %v\n", err)
			}
		}
	}
	if in.Reason != "clear" && in.Reason != "logout" {
		return // the idle TTL reaps the workspaces
	}
	session := sessionPrefix + in.SessionID
	if in.SessionID == "" || api.ValidateSession(session) != nil {
		return
	}
	d := env.CloseTimeout
	if d <= 0 {
		d = defaultCloseTTL
	}
	cctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	newB := env.NewBackend
	if newB == nil {
		newB = defaultBackend
	}
	done := make(chan error, 1)
	go func() { done <- closeMatching(cctx, newB, env, session) }()
	select {
	case err := <-done:
		if err != nil {
			fmt.Fprintf(stderr, "mirafleet hook: session-end: %v\n", err)
		}
	case <-cctx.Done():
		fmt.Fprintln(stderr, "mirafleet hook: session-end: timed out closing workspaces")
	}
}

func closeMatching(ctx context.Context, newB func(Env) (Backend, error), env Env, session string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("internal error")
		}
	}()
	b, err := newB(env)
	if err != nil {
		return err
	}
	defer b.Close()
	names, err := b.ContextNames()
	if err != nil {
		return err
	}
	var first error
	for _, c := range names {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		l, err := b.WorkspaceList(ctx, c, "")
		if err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		for _, w := range l.Workspaces {
			if w.Session != session {
				continue
			}
			if _, err := b.WorkspaceClose(ctx, c, w.ID); err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}

type hookOutput struct {
	HookSpecificOutput struct {
		HookEventName            string `json:"hookEventName"`
		PermissionDecision       string `json:"permissionDecision"`
		PermissionDecisionReason string `json:"permissionDecisionReason"`
	} `json:"hookSpecificOutput"`
}

func ask(stdout io.Writer, reason string) {
	var o hookOutput
	o.HookSpecificOutput.HookEventName = "PreToolUse"
	o.HookSpecificOutput.PermissionDecision = "ask" // never "allow"
	o.HookSpecificOutput.PermissionDecisionReason = reason
	b, _ := json.Marshal(o)
	stdout.Write(append(b, '\n'))
}

func preToolUse(stdin io.Reader, stdout, stderr io.Writer, env Env) {
	in, err := readInput(stdin)
	if err != nil {
		fmt.Fprintf(stderr, "mirafleet hook: pre-tool-use: bad input: %v\n", err)
		ask(stdout, genericReason)
		return
	}
	if denyRemote(stdout, env, in) {
		return // a local tool in a remote project (or with an unreadable project file): denied
	}
	ti := bytes.TrimSpace(in.ToolInput)
	if len(ti) == 0 || string(ti) == "null" {
		return // no tool_input: no confirm
	}
	args, ok := parseArgs(ti)
	if !ok {
		ask(stdout, ambiguousReason)
		return
	}
	raw, present := args["confirm"]
	if !present {
		return
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return
		}
	} else if string(bytes.TrimSpace(raw)) == "null" {
		return
	} // any other type: set, but not a string; ask (central refuses it anyway)

	suffix := strings.TrimPrefix(in.ToolName, toolPrefix)
	if suffix == in.ToolName || !knownTools[suffix] {
		ask(stdout, genericReason) // confirm on a tool we do not recognise: never allow
		return
	}
	tool := suffix
	field := func(k string) string { return clipRunes(oneLine(strOf(args[k])), fieldMax) }
	fleet, action, runner, ws, cx := field("fleet"), field("action"), field("runner"), field("workspace"), field("context")

	var sb strings.Builder
	sb.WriteString("Guarded fleet")
	if fleet != "" {
		fmt.Fprintf(&sb, " %q", fleet)
	}
	sb.WriteString(": " + tool)
	if action != "" {
		sb.WriteString(" " + action)
	}
	if runner != "" {
		sb.WriteString(" on " + runner)
	}
	if ws != "" {
		sb.WriteString(" in workspace " + ws)
	}
	if cx != "" {
		sb.WriteString(" (context " + cx + ")")
	}
	ask(stdout, clipRunes(sb.String(), reasonMax))
}

// parseArgs reads the tool_input object with a token stream. ok is false when it is not a
// well-formed object, when a key case-folds to one of the fields the server reads
// (knownFields) without matching exactly, or when such a field appears twice. The server
// must reject these too; refusing to guess here is defence in depth.
func parseArgs(b []byte) (args map[string]json.RawMessage, ok bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, false
	}
	args = map[string]json.RawMessage{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, isStr := t.(string)
		if !isStr {
			return nil, false
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, false
		}
		for _, f := range knownFields {
			if strings.EqualFold(key, f) && key != f {
				return nil, false
			}
		}
		if _, dup := args[key]; dup {
			for _, f := range knownFields {
				if key == f {
					return nil, false
				}
			}
		}
		args[key] = v
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	return args, true
}

func strOf(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// oneLine sanitises s (ANSI, controls, bidi and other format characters) and folds whitespace.
func oneLine(s string) string {
	return strings.Join(strings.Fields(orch.Sanitize([]byte(s))), " ")
}

func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "\xe2\x80\xa6"
}
