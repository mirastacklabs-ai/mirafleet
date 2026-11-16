package integrations_test

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// cursorPluginDir is the Cursor plugin (Cursor Marketplace). The repo-root .cursor-plugin/marketplace.json
// points at it.
const cursorPluginDir = "cursor/plugin"

var kebabRE = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func readJSONFile(t *testing.T, p string, v any) {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", p, err)
	}
}

// relPathOK reports whether p is a clean relative path with no "..".
func relPathOK(p string) bool {
	return p != "" && !filepath.IsAbs(p) && !strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "~") &&
		!strings.Contains(p, "\\") && filepath.ToSlash(filepath.Clean(p)) == strings.TrimPrefix(p, "./") &&
		!strings.HasPrefix(filepath.Clean(p), "..")
}

func TestCursorPluginManifest(t *testing.T) {
	var m struct {
		Name        string `json:"name"`
		Version     string `json:"version"`
		Description string `json:"description"`
		Author      struct {
			Name string `json:"name"`
		} `json:"author"`
		Homepage   string   `json:"homepage"`
		Repository string   `json:"repository"`
		License    string   `json:"license"`
		Keywords   []string `json:"keywords"`
		Logo       string   `json:"logo"`
	}
	readJSONFile(t, filepath.Join(cursorPluginDir, ".cursor-plugin/plugin.json"), &m)
	if m.Name != "mirafleet" || !kebabRE.MatchString(m.Name) {
		t.Errorf("name = %q, want kebab-case mirafleet", m.Name)
	}
	if !regexp.MustCompile(`^\d+\.\d+\.\d+(-rc\.\d+)?$`).MatchString(m.Version) {
		t.Errorf("version = %q", m.Version)
	}
	if len(m.Description) < 40 || !strings.Contains(m.Description, "mirafleet mcp login") {
		t.Errorf("description = %q", m.Description)
	}
	if m.Author.Name != "mirastacklabs-ai" || m.License != "Apache-2.0" {
		t.Errorf("author/license = %q / %q", m.Author.Name, m.License)
	}
	const repo = "https://github.com/mirastacklabs-ai/mirafleet"
	if m.Homepage != repo || m.Repository != repo {
		t.Errorf("homepage/repository = %q / %q", m.Homepage, m.Repository)
	}
	if len(m.Keywords) == 0 {
		t.Error("no keywords")
	}
	if !relPathOK(m.Logo) {
		t.Fatalf("logo %q must be a relative path", m.Logo)
	}
	logo, err := os.ReadFile(filepath.Join(cursorPluginDir, filepath.FromSlash(m.Logo)))
	if err != nil {
		t.Fatalf("logo: %v", err)
	}
	claude, err := os.ReadFile("claude-code/plugin/.claude-plugin/icon.png")
	if err != nil {
		t.Fatal(err)
	}
	if string(logo) != string(claude) {
		t.Error("logo differs from the Claude Code plugin icon (one brand icon)")
	}
	if _, err := os.Stat(filepath.Join(cursorPluginDir, "README.md")); err != nil {
		t.Error("plugin README.md missing")
	}
}

func TestCursorPluginMCP(t *testing.T) {
	var pm struct {
		Version string `json:"version"`
	}
	readJSONFile(t, filepath.Join(cursorPluginDir, ".cursor-plugin/plugin.json"), &pm)
	var m struct {
		MCPServers map[string]struct {
			Type    string   `json:"type"`
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	readJSONFile(t, filepath.Join(cursorPluginDir, "mcp.json"), &m)
	s, ok := m.MCPServers["mirafleet"]
	if !ok || len(m.MCPServers) != 1 {
		t.Fatalf("mcpServers = %v, want only mirafleet", m.MCPServers)
	}
	want := []string{"-y", "@mirastacklabs/mirafleet@" + pm.Version, "mcp", "-project-dir", "${workspaceFolder}"}
	if s.Type != "stdio" || s.Command != "npx" || strings.Join(s.Args, " ") != strings.Join(want, " ") {
		t.Errorf("server = %+v, want npx %v pinned to the plugin version", s, want)
	}
}

func TestCursorPluginCommandIsShared(t *testing.T) {
	if got := m6Read(t, filepath.Join(cursorPluginDir, "commands/mirafleet.md")); got != m6Shared(t) {
		t.Error("cursor/plugin/commands/mirafleet.md differs from commands/mirafleet.md")
	}
	if m6Read(t, "cursor/commands/mirafleet.md") != m6Shared(t) {
		t.Error("cursor/commands/mirafleet.md differs from commands/mirafleet.md")
	}
}

func TestCursorMarketplace(t *testing.T) {
	var m struct {
		Name  string `json:"name"`
		Owner struct {
			Name string `json:"name"`
		} `json:"owner"`
		Metadata struct {
			Description string `json:"description"`
		} `json:"metadata"`
		Plugins []struct {
			Name        string `json:"name"`
			Source      string `json:"source"`
			Description string `json:"description"`
		} `json:"plugins"`
	}
	readJSONFile(t, "../.cursor-plugin/marketplace.json", &m)
	if m.Name != "mirastacklabs" || !kebabRE.MatchString(m.Name) || m.Owner.Name != "mirastacklabs-ai" || m.Metadata.Description == "" {
		t.Errorf("marketplace header = %q / %q / %q", m.Name, m.Owner.Name, m.Metadata.Description)
	}
	if len(m.Plugins) != 1 {
		t.Fatalf("want 1 plugin entry, got %d", len(m.Plugins))
	}
	p := m.Plugins[0]
	if p.Name != "mirafleet" || p.Description == "" {
		t.Errorf("entry = %+v", p)
	}
	if !relPathOK(p.Source) || p.Source != "integrations/"+cursorPluginDir {
		t.Fatalf("source = %q, want integrations/%s (relative, no ..)", p.Source, cursorPluginDir)
	}
	var pm struct {
		Name string `json:"name"`
	}
	readJSONFile(t, filepath.Join("..", filepath.FromSlash(p.Source), ".cursor-plugin/plugin.json"), &pm)
	if pm.Name != p.Name {
		t.Errorf("entry name %q != plugin.json name %q", p.Name, pm.Name)
	}
}

// The "Add to Cursor" button in cursor/README.md carries the server config as base64 JSON.
func TestCursorInstallButton(t *testing.T) {
	readme := m6Read(t, "cursor/README.md")
	m := regexp.MustCompile(`https://cursor\.com/en/install-mcp\?name=mirafleet&config=([A-Za-z0-9+/=]+)\)`).FindStringSubmatch(readme)
	if m == nil {
		t.Fatal("no install-mcp link in cursor/README.md")
	}
	if strings.Contains(m[1], "+") {
		t.Error("button config contains '+', which is unsafe in a query string")
	}
	b, err := base64.StdEncoding.DecodeString(m[1])
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Command != "npx" || strings.Join(cfg.Args, " ") != "-y @mirastacklabs/mirafleet mcp -project-dir ${workspaceFolder}" {
		t.Errorf("button config = %s", b)
	}
}
