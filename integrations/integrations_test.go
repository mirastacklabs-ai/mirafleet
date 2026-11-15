package integrations

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/mirastacklabs-ai/mirafleet/internal/mcpserver"
)

const pluginDir = "claude-code"

func read(t *testing.T, rel ...string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{pluginDir}, rel...)...))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func readJSON(t *testing.T, v any, rel ...string) {
	t.Helper()
	if err := json.Unmarshal(read(t, rel...), v); err != nil {
		t.Fatalf("%s: invalid JSON: %v", filepath.Join(rel...), err)
	}
}

// frontmatter splits a SKILL.md into its frontmatter lines and body (everything after the closing ---).
func frontmatter(t *testing.T, b []byte) (fm map[string]string, body string) {
	t.Helper()
	s := string(b)
	if !strings.HasPrefix(s, "---\n") {
		t.Fatal("no frontmatter")
	}
	end := strings.Index(s[4:], "\n---\n")
	if end < 0 {
		t.Fatal("unterminated frontmatter")
	}
	fm = map[string]string{}
	for _, l := range strings.Split(s[4:4+end], "\n") {
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			t.Fatalf("bad frontmatter line %q", l)
		}
		fm[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	return fm, s[4+end+len("\n---\n"):]
}

type manifest struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
	License     string `json:"license"`
	Repository  string `json:"repository"`
	Homepage    string `json:"homepage"`
	Author      struct {
		Name string `json:"name"`
	} `json:"author"`
}

func keySet(m map[string]any) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func TestPluginManifest(t *testing.T) {
	var raw map[string]any
	readJSON(t, &raw, ".claude-plugin", "plugin.json")
	// No hooks, mcpServers or commands keys: components are found at the plugin root by convention.
	wantKeys := "author description displayName homepage keywords license name repository version"
	if got := strings.Join(keySet(raw), " "); got != wantKeys {
		t.Fatalf("plugin.json keys %q, want %q", got, wantKeys)
	}
	var m manifest
	readJSON(t, &m, ".claude-plugin", "plugin.json")
	if m.Name != "mirafleet" || m.Description == "" || m.Author.Name == "" || m.License != "Apache-2.0" {
		t.Fatalf("manifest fields: %+v", m)
	}
	const gh = "https://github.com/mirastacklabs-ai/mirafleet"
	if m.Repository != gh || m.Homepage != gh {
		t.Fatalf("repository/homepage: %q %q", m.Repository, m.Homepage)
	}
	// Lockstep with the binary (pluginVersion in cmd/mirafleet) is checked in cmd/mirafleet,
	// because package main cannot be imported here.
	// Same shape relgen bump writes (tools/relgen/check.go semverRE): X.Y.Z or X.Y.Z-rc.N.
	if !regexp.MustCompile(`^\d+\.\d+\.\d+(-rc\.\d+)?$`).MatchString(m.Version) {
		t.Fatalf("version %q is not semver", m.Version)
	}
}

func TestMCPJSON(t *testing.T) {
	var raw map[string]map[string]map[string]any
	readJSON(t, &raw, ".mcp.json")
	if len(raw) != 1 || len(raw["mcpServers"]) != 1 {
		t.Fatalf(".mcp.json shape: %v", raw)
	}
	// Exact key set: no env (secrets and tokens must never ride in the plugin config).
	if got := strings.Join(keySet(raw["mcpServers"]["mirafleet"]), " "); got != "args command" {
		t.Fatalf("mirafleet server keys %q, want \"args command\"", got)
	}
	var c struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	readJSON(t, &c, ".mcp.json")
	s, ok := c.MCPServers["mirafleet"]
	if !ok || len(c.MCPServers) != 1 || s.Command != "mirafleet" {
		t.Fatalf("mcp servers: %+v", c.MCPServers)
	}
	if strings.Join(s.Args, " ") != "mcp -project-dir ${CLAUDE_PROJECT_DIR}" {
		t.Fatalf("args %q", s.Args)
	}
}

type hooksFile struct {
	Hooks map[string][]struct {
		Matcher string `json:"matcher"`
		Hooks   []struct {
			Type    string   `json:"type"`
			Command string   `json:"command"`
			Args    []string `json:"args"`
			Timeout int      `json:"timeout"`
		} `json:"hooks"`
	} `json:"hooks"`
}

func TestHooksExecForm(t *testing.T) {
	var h hooksFile
	readJSON(t, &h, "hooks", "hooks.json")
	want := map[string]string{"SessionStart": "session-start", "SessionEnd": "session-end", "PreToolUse": "pre-tool-use"}
	if len(h.Hooks) != len(want) {
		t.Fatalf("events: %v", h.Hooks)
	}
	for ev, sub := range want {
		groups := h.Hooks[ev]
		wantGroups := 1
		if ev == "PreToolUse" {
			wantGroups = 2 // mirafleet tools (guarded confirm) and local tools (remote deny)
		}
		if len(groups) != wantGroups {
			t.Fatalf("%s: want %d groups: %+v", ev, wantGroups, groups)
		}
		for _, g := range groups {
			if len(g.Hooks) != 1 {
				t.Fatalf("%s: want one hook per group: %+v", ev, g)
			}
			hk := g.Hooks[0]
			// Exec form: command is the bare executable and args is set, so no shell is involved
			// (works on Windows without sh or PowerShell).
			if hk.Type != "command" || hk.Command != "mirafleet" {
				t.Fatalf("%s: %+v", ev, hk)
			}
			if strings.Join(hk.Args, " ") != "mcp hook claude-code "+sub {
				t.Fatalf("%s args: %q", ev, hk.Args)
			}
			if strings.ContainsAny(hk.Command, " \t") {
				t.Fatalf("%s: command has whitespace", ev)
			}
			if ev == "SessionEnd" && hk.Timeout != 15 {
				t.Fatalf("SessionEnd timeout %d", hk.Timeout)
			}
		}
	}
}

// The second PreToolUse group lists exactly the nine local tools as an exact-string matcher
// (letters and | only, so Claude Code does not treat it as a regexp).
func TestPreToolUseLocalToolsMatcher(t *testing.T) {
	var h hooksFile
	readJSON(t, &h, "hooks", "hooks.json")
	m := h.Hooks["PreToolUse"][1].Matcher
	if !regexp.MustCompile(`^[A-Za-z|]+$`).MatchString(m) {
		t.Fatalf("matcher %q is not a plain alternation", m)
	}
	got := strings.Split(m, "|")
	sort.Strings(got)
	want := []string{"Bash", "Edit", "Glob", "Grep", "LS", "MultiEdit", "NotebookEdit", "Read", "Write"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("local matcher %q, want %v", m, want)
	}
	// The anchored mirafleet matcher stays first and unchanged.
	if h.Hooks["PreToolUse"][0].Matcher != "^mcp__plugin_mirafleet_mirafleet__.*" {
		t.Fatalf("mirafleet matcher %q", h.Hooks["PreToolUse"][0].Matcher)
	}
}

// Every text file under integrations/ uses LF line endings.
func TestLFLineEndings(t *testing.T) {
	err := filepath.WalkDir(".", func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), "\r") {
			t.Errorf("%s contains CR", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The shared body, and so every skill and command copy, explains the remote workflow.
func TestSharedBodyExplainsRemoteWorkflow(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("commands", "mirafleet.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"remote: true", "workspace_open", "clone", "multi_edit", "run_in_background", "bash_output", "push", "CLAUDE.md", "never visible"} {
		if !strings.Contains(string(b), s) {
			t.Errorf("shared body lacks %q", s)
		}
	}
	for _, f := range []string{"opencode/README.md", "generic/README.md", "README.md", "claude-code/README.md"} {
		rb, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(rb), "remote: true") {
			t.Errorf("%s does not mention remote: true", f)
		}
	}
}

func TestPreToolUseMatcherCoversMirafleetToolsOnly(t *testing.T) {
	var h hooksFile
	readJSON(t, &h, "hooks", "hooks.json")
	m := h.Hooks["PreToolUse"][0].Matcher
	// Claude Code treats a matcher with characters beyond letters, digits, _ - space , | as an
	// unanchored JavaScript regexp; Go's RE2 agrees on this simple syntax.
	re, err := regexp.Compile(m)
	if err != nil {
		t.Fatal(err)
	}
	names := append(mcpserver.ToolNames(mcpserver.Config{Tools: mcpserver.ListTools(nil)}),
		mcpserver.ToolNames(mcpserver.Config{Tools: mcpserver.WorkspaceTools(nil)})...)
	if len(names) < 9 {
		t.Fatalf("expected the full tool set, got %v", names)
	}
	for _, n := range names {
		if !re.MatchString("mcp__plugin_mirafleet_mirafleet__" + n) {
			t.Errorf("matcher %q misses %s", m, n)
		}
	}
	for _, other := range []string{"Bash", "Write", "mcp__plugin_other_other__list_fleets", "mcp__mirafleet__list_fleets",
		"mcp__plugin_mirafleet_other__run_action", "mcp__plugin_design_figma__authenticate",
		"mcp__plugin_evil_x__mcp__plugin_mirafleet_mirafleet__exec"} {
		if re.MatchString(other) {
			t.Errorf("matcher %q matches non-mirafleet tool %s", m, other)
		}
	}
}

func TestSkills(t *testing.T) {
	cmd, err := os.ReadFile(filepath.Join("commands", "mirafleet.md"))
	if err != nil {
		t.Fatal(err)
	}
	fm, body := frontmatter(t, read(t, "skills", "mirafleet", "SKILL.md"))
	if fm["description"] == "" || fm["argument-hint"] == "" {
		t.Fatalf("main skill frontmatter: %v", fm)
	}
	if fm["disable-model-invocation"] != "" {
		t.Fatal("the main skill must stay model-invocable")
	}
	if body != string(cmd) {
		t.Fatal("skills/mirafleet/SKILL.md body differs from integrations/commands/mirafleet.md")
	}
	if !strings.Contains(body, "$ARGUMENTS") {
		t.Fatal("body lacks $ARGUMENTS")
	}
	for _, alias := range []string{"openfleet", "claudefleet"} {
		fm, body := frontmatter(t, read(t, "skills", alias, "SKILL.md"))
		if fm["disable-model-invocation"] != "true" || fm["description"] == "" {
			t.Errorf("%s frontmatter: %v", alias, fm)
		}
		if body != string(cmd) {
			t.Errorf("skills/%s/SKILL.md body differs from integrations/commands/mirafleet.md", alias)
		}
	}
}
