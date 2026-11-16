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

// pluginDir is the plugin root: the folder Claude Code (and the plugin directory validator) sees.
// The Go hook package lives next to it, outside the root.
const pluginDir = "claude-code/plugin"

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
	if !ok || len(c.MCPServers) != 1 || s.Command != "node" {
		t.Fatalf("mcp servers: %+v", c.MCPServers)
	}
	// The server's cwd is the project, which `mirafleet mcp` defaults -project-dir to, so no flag.
	if strings.Join(s.Args, " ") != launcher+" mcp" {
		t.Fatalf("args %q", s.Args)
	}
}

const launcher = "${CLAUDE_PLUGIN_ROOT}/bin/mirafleet.js"

// The plugin root holds only plugin files, so the plugin directory validator sees no Go source and
// no program other than the Node launcher (docs/M6.md section 2.18).
func TestPluginRootFileTypes(t *testing.T) {
	err := filepath.WalkDir(pluginDir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(pluginDir, p)
		rel = filepath.ToSlash(rel)
		ext := filepath.Ext(rel)
		ok := false
		switch {
		case rel == "bin/mirafleet.js":
			ok = true
		case ext == ".json" || ext == ".md" || ext == ".png":
			ok = !strings.HasPrefix(rel, "bin/")
		}
		if strings.HasPrefix(rel, "skills/") && ext != ".md" {
			ok = false
		}
		if !ok {
			t.Errorf("%s: not an allowed plugin file", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"bin/mirafleet.js", ".mcp.json", "hooks/hooks.json", ".claude-plugin/plugin.json", ".claude-plugin/icon.png", "README.md"} {
		if _, err := os.Stat(filepath.Join(pluginDir, filepath.FromSlash(f))); err != nil {
			t.Error(err)
		}
	}
}

// The launcher reads only PATH, runs without a shell, and fetches nothing.
func TestLauncherSource(t *testing.T) {
	src := string(read(t, "bin", "mirafleet.js"))
	for _, bad := range []string{"eval(", "new Function", "npx", "http.get", "https.get", "fetch(", "exec(", "execSync", "shell: true", "CLAUDE_PROJECT_DIR"} {
		if strings.Contains(src, bad) {
			t.Errorf("launcher contains %q", bad)
		}
	}
	// Named reads: PATH only. The whole environment is only copied to the child (spread), with the
	// harness PID added.
	for _, e := range regexp.MustCompile(`process\.env(\.\w+|\[[^\]]*\])`).FindAllString(src, -1) {
		if e != "process.env.PATH" {
			t.Errorf("launcher reads %s, only PATH is allowed", e)
		}
	}
	if !strings.Contains(src, "{ ...process.env, MIRAFLEET_HARNESS_PID: String(process.ppid) }") {
		t.Error("launcher must pass the harness PID to the binary")
	}
	if !strings.Contains(src, "shell: false") {
		t.Error("launcher must spawn with shell: false")
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
			// Exec form: command is the bare executable (node) and args is set, so no shell is
			// involved (works on Windows without sh or PowerShell). The only file run is the
			// plugin's own launcher, which starts the installed mirafleet binary.
			if hk.Type != "command" || hk.Command != "node" {
				t.Fatalf("%s: %+v", ev, hk)
			}
			if strings.Join(hk.Args, " ") != launcher+" mcp hook claude-code "+sub {
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
		if filepath.Ext(p) == ".png" { // binary (the plugin icon): CR bytes are data
			return nil
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
	for _, f := range []string{"opencode/README.md", "generic/README.md", "README.md", "claude-code/plugin/README.md"} {
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
