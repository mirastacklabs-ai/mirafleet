package integrations_test

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// N2.7: Codex CLI, Gemini CLI, Cursor, VS Code, Cline and Kilo Code. Every snippet must parse and
// name the right command; every command copy must equal the shared body.

var m6Dirs = []string{"codex", "gemini", "cursor", "vscode", "cline", "kilocode"}

type m6Server struct {
	command string
	args    []string
}

func m6Read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func m6Shared(t *testing.T) string {
	return m6Read(t, "commands/mirafleet.md")
}

// m6Frontmatter splits "---\nyaml\n---\nbody" and parses the YAML into a map.
func m6Frontmatter(t *testing.T, s string) (map[string]any, string) {
	t.Helper()
	if !strings.HasPrefix(s, "---\n") {
		t.Fatal("missing frontmatter")
	}
	rest := s[4:]
	i := strings.Index(rest, "\n---\n")
	if i < 0 {
		t.Fatal("unterminated frontmatter")
	}
	fm := map[string]any{}
	if err := yaml.Unmarshal([]byte(rest[:i]), &fm); err != nil {
		t.Fatalf("frontmatter is not valid YAML: %v", err)
	}
	return fm, rest[i+5:]
}

// m6JSONServer finds the mirafleet entry in a harness JSON config. servers is the path to the map
// holding it ("mcpServers", "servers" or "mcp"). A command given as an array (Kilo) is split.
func m6JSONServer(t *testing.T, b []byte, key string) (m6Server, map[string]any) {
	t.Helper()
	var cfg map[string]map[string]map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	e, ok := cfg[key]["mirafleet"]
	if !ok {
		t.Fatalf("no %s.mirafleet entry", key)
	}
	var s m6Server
	switch c := e["command"].(type) {
	case string:
		s.command = c
		for _, a := range e["args"].([]any) {
			s.args = append(s.args, a.(string))
		}
	case []any:
		for i, a := range c {
			if i == 0 {
				s.command = a.(string)
			} else {
				s.args = append(s.args, a.(string))
			}
		}
	default:
		t.Fatalf("bad command %v", e["command"])
	}
	return s, e
}

// m6TOMLServer reads the small subset used by Codex: one [mcp_servers.mirafleet] table with
// string/array-of-string values. No TOML library is allowed, so anything else fails the test.
func m6TOMLServer(t *testing.T, s string) m6Server {
	t.Helper()
	var out m6Server
	in := false
	for _, ln := range strings.Split(strings.TrimSpace(s), "\n") {
		ln = strings.TrimSpace(ln)
		switch {
		case ln == "" || strings.HasPrefix(ln, "#"):
		case strings.HasPrefix(ln, "["):
			if in {
				t.Fatalf("unexpected extra table %q", ln)
			}
			if ln != "[mcp_servers.mirafleet]" {
				t.Fatalf("unexpected table %q", ln)
			}
			in = true
		case in && strings.HasPrefix(ln, "command = "):
			if err := json.Unmarshal([]byte(strings.TrimPrefix(ln, "command = ")), &out.command); err != nil {
				t.Fatalf("bad command line %q: %v", ln, err)
			}
		case in && strings.HasPrefix(ln, "args = "):
			if err := json.Unmarshal([]byte(strings.TrimPrefix(ln, "args = ")), &out.args); err != nil {
				t.Fatalf("bad args line %q: %v", ln, err)
			}
		default:
			t.Fatalf("unsupported TOML line %q", ln)
		}
	}
	if !in || out.command == "" || out.args == nil {
		t.Fatal("TOML lacks [mcp_servers.mirafleet] command/args")
	}
	return out
}

func m6Blocks(t *testing.T, readme, lang string) []string {
	t.Helper()
	re := regexp.MustCompile("(?s)```" + lang + "\n(.*?)```")
	var out []string
	for _, m := range re.FindAllStringSubmatch(m6Read(t, readme), -1) {
		out = append(out, m[1])
	}
	return out
}

func m6CheckServer(t *testing.T, s m6Server, npx bool) {
	t.Helper()
	var want []string
	if npx {
		want = []string{"-y", "@mirastacklabs/mirafleet", "mcp"}
		if s.command != "npx" {
			t.Errorf("command = %q, want npx", s.command)
		}
	} else {
		want = []string{"mcp"}
		if s.command != "mirafleet" {
			t.Errorf("command = %q, want mirafleet", s.command)
		}
	}
	if len(s.args) < len(want) || !reflect.DeepEqual(s.args[:len(want)], want) {
		t.Errorf("args = %v, want prefix %v", s.args, want)
	}
	if len(s.args) < 2 || s.args[len(s.args)-2] != "-project-dir" {
		t.Errorf("args = %v, want to end with -project-dir <dir>", s.args)
	}
}

func TestM6HarnessSnippets(t *testing.T) {
	jsonKeys := map[string]string{"gemini": "mcpServers", "cursor": "mcpServers", "vscode": "servers", "cline": "mcpServers", "kilocode": "mcp"}
	examples := map[string]string{
		"gemini": "settings.example.json", "cursor": "mcp.example.json", "vscode": "mcp.example.json",
		"cline": "cline_mcp_settings.example.json", "kilocode": "kilo.example.json",
	}
	for dir, key := range jsonKeys {
		t.Run(dir, func(t *testing.T) {
			ex := []byte(m6Read(t, filepath.Join(dir, examples[dir])))
			exSrv, _ := m6JSONServer(t, ex, key)
			m6CheckServer(t, exSrv, false)

			blocks := m6Blocks(t, filepath.Join(dir, "README.md"), "json")
			if len(blocks) != 2 {
				t.Fatalf("README has %d json blocks, want 2 (installed, npx)", len(blocks))
			}
			// installed form equals the example file
			var a, b any
			if err := json.Unmarshal(ex, &a); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(blocks[0]), &b); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(a, b) {
				t.Errorf("README installed snippet differs from %s", examples[dir])
			}
			s0, _ := m6JSONServer(t, []byte(blocks[0]), key)
			m6CheckServer(t, s0, false)
			s1, e1 := m6JSONServer(t, []byte(blocks[1]), key)
			m6CheckServer(t, s1, true)
			if dir == "cline" {
				if aa, ok := e1["autoApprove"].([]any); !ok || len(aa) != 0 {
					t.Error("cline snippet must keep autoApprove empty")
				}
			}
		})
	}

	t.Run("codex", func(t *testing.T) {
		ex := m6TOMLServer(t, m6Read(t, "codex/config.example.toml"))
		m6CheckServer(t, ex, false)
		blocks := m6Blocks(t, "codex/README.md", "toml")
		if len(blocks) != 2 {
			t.Fatalf("README has %d toml blocks, want 2", len(blocks))
		}
		s0 := m6TOMLServer(t, blocks[0])
		if !reflect.DeepEqual(s0, ex) {
			t.Error("README installed snippet differs from config.example.toml")
		}
		m6CheckServer(t, m6TOMLServer(t, blocks[1]), true)
	})
}

func TestM6CommandCopies(t *testing.T) {
	shared := m6Shared(t)
	if !strings.Contains(shared, "$ARGUMENTS") {
		t.Fatal("shared body lost $ARGUMENTS")
	}
	trim := func(s string) string { return strings.TrimSpace(s) }

	// Verbatim copies.
	for _, p := range []string{"cursor/commands/mirafleet.md", "cline/workflows/mirafleet.md"} {
		if m6Read(t, p) != shared {
			t.Errorf("%s differs from integrations/commands/mirafleet.md", p)
		}
	}

	// Frontmatter plus verbatim body.
	for _, p := range []string{"codex/skills/mirafleet/SKILL.md", "kilocode/commands/mirafleet.md"} {
		fm, body := m6Frontmatter(t, m6Read(t, p))
		if body != shared {
			t.Errorf("%s body differs from the shared body", p)
		}
		if d, _ := fm["description"].(string); d == "" {
			t.Errorf("%s lacks a description", p)
		}
		if strings.HasPrefix(p, "codex") && fm["name"] != "mirafleet" {
			t.Errorf("%s name = %v, want mirafleet (must match the directory)", p, fm["name"])
		}
	}

	// VS Code: $ARGUMENTS becomes the prompt-file input variable.
	fm, body := m6Frontmatter(t, m6Read(t, "vscode/prompts/mirafleet.prompt.md"))
	want := strings.Replace(shared, "$ARGUMENTS", "${input:task:what to do on the fleets}", 1)
	if body != want {
		t.Error("vscode prompt body differs from the shared body (modulo ${input:task})")
	}
	if fm["agent"] != "agent" {
		t.Errorf("vscode agent = %v", fm["agent"])
	}
	if tools, _ := fm["tools"].([]any); len(tools) != 1 || tools[0] != "mirafleet/*" {
		t.Errorf("vscode tools = %v, want [mirafleet/*]", fm["tools"])
	}

	// Gemini: TOML with a multi-line basic string; $ARGUMENTS becomes {{args}}.
	g := m6Read(t, "gemini/commands/mirafleet.toml")
	if !regexp.MustCompile(`(?m)^description = "[^"\\]+"$`).MatchString(g) {
		t.Error("gemini toml lacks a simple description line")
	}
	const open, closing = "\nprompt = \"\"\"\n", "\"\"\"\n"
	i := strings.Index(g, open)
	if i < 0 || !strings.HasSuffix(g, closing) {
		t.Fatal("gemini toml: prompt = \"\"\" ... \"\"\" not found")
	}
	prompt := g[i+len(open) : len(g)-len(closing)]
	if strings.Contains(prompt, `"""`) || strings.Contains(prompt, `\`) {
		t.Error("gemini prompt has a triple quote or backslash; escape rules apply")
	}
	if trim(prompt) != trim(strings.Replace(shared, "$ARGUMENTS", "{{args}}", 1)) {
		t.Error("gemini prompt differs from the shared body (modulo {{args}})")
	}
}

func TestM6LFAndStructure(t *testing.T) {
	for _, d := range m6Dirs {
		if _, err := os.Stat(filepath.Join(d, "README.md")); err != nil {
			t.Errorf("%s: no README.md", d)
		}
		r := m6Read(t, filepath.Join(d, "README.md"))
		if !regexp.MustCompile(`checked 20\d\d-\d\d-\d\d`).MatchString(r) || !strings.Contains(r, "https://") {
			t.Errorf("%s/README.md lacks dated source URLs", d)
		}
		err := filepath.WalkDir(d, func(p string, de fs.DirEntry, err error) error {
			if err != nil || de.IsDir() || strings.EqualFold(filepath.Ext(p), ".png") {
				return err // the plugin logo is binary
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if bytes.Contains(b, []byte("\r")) {
				t.Errorf("%s contains CR; text files must be LF", p)
			}
			if len(b) > 0 && b[len(b)-1] != '\n' {
				t.Errorf("%s lacks a final newline", p)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
