package integrations_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func stripFrontmatter(t *testing.T, s string) string {
	t.Helper()
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if !strings.HasPrefix(s, "---\n") {
		t.Fatal("missing frontmatter")
	}
	rest := s[4:]
	i := strings.Index(rest, "\n---\n")
	if i < 0 {
		t.Fatal("unterminated frontmatter")
	}
	return strings.TrimSpace(rest[i+5:])
}

func TestOpencodeCommandBodiesMatchShared(t *testing.T) {
	shared, err := os.ReadFile("commands/mirafleet.md")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.TrimSpace(strings.ReplaceAll(string(shared), "\r\n", "\n"))
	for _, n := range []string{"mirafleet", "openfleet"} {
		b, err := os.ReadFile(filepath.Join("opencode", "commands", n+".md"))
		if err != nil {
			t.Fatal(err)
		}
		if got := stripFrontmatter(t, string(b)); got != want {
			t.Errorf("%s.md body differs from integrations/commands/mirafleet.md", n)
		}
		if !strings.Contains(string(b), "\ndescription: ") {
			t.Errorf("%s.md lacks a description", n)
		}
	}
}

func TestOpencodeExampleJSON(t *testing.T) {
	b, err := os.ReadFile("opencode/opencode.example.json")
	if err != nil {
		t.Fatal(err)
	}
	checkOpencodeJSON(t, b)

	// The JSON snippet in the README must be the example file (same decoded value).
	rb, err := os.ReadFile("opencode/README.md")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile("(?s)```json\n(.*?)```").FindSubmatch(rb)
	if m == nil {
		t.Fatal("opencode/README.md has no ```json block")
	}
	checkOpencodeJSON(t, m[1])
	var a, c any
	if err := json.Unmarshal(b, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(m[1], &c); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, c) {
		t.Errorf("README JSON snippet differs from opencode.example.json:\n%v\n%v", c, a)
	}
}

func checkOpencodeJSON(t *testing.T, b []byte) {
	t.Helper()
	var cfg struct {
		MCP struct {
			Servers map[string]map[string]any `json:"servers"`
		} `json:"mcp"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	s, ok := cfg.MCP.Servers["mirafleet"]
	if !ok || len(cfg.MCP.Servers) != 1 {
		t.Fatalf("want exactly the mirafleet server: %v", cfg.MCP.Servers)
	}
	// No environment key: secrets must not ride in a config file.
	if _, has := s["environment"]; has || len(s) != 2 || s["type"] != "local" {
		t.Errorf("bad server entry: %v", s)
	}
	cmd, _ := s["command"].([]any)
	want := []any{"mirafleet", "mcp", "-project-dir", "."}
	if !reflect.DeepEqual(cmd, want) {
		t.Errorf("command argv %v, want %v", cmd, want)
	}
}

var mdLink = regexp.MustCompile(`\]\(([^)\s]+)\)`)

func TestREADMERelativeLinks(t *testing.T) {
	files := []string{"README.md", "opencode/README.md", "generic/README.md"}
	if m, _ := filepath.Glob("claude-code/README.md"); len(m) > 0 {
		files = append(files, m...)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range mdLink.FindAllStringSubmatch(string(b), -1) {
			l := m[1]
			if strings.Contains(l, "://") || strings.HasPrefix(l, "#") || strings.HasPrefix(l, "mailto:") {
				continue
			}
			l, _, _ = strings.Cut(l, "#")
			if _, err := os.Stat(filepath.Join(filepath.Dir(f), l)); err != nil {
				t.Errorf("%s: broken link %q", f, m[1])
			}
		}
	}
}
