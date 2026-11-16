package integrations

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readFile(p string) ([]byte, error) { return os.ReadFile(p) }

func readRootJSON(t *testing.T, v any, rel string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: invalid JSON: %v", rel, err)
	}
}

type pluginManifest struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

func TestMarketplaceJSON(t *testing.T) {
	var m struct {
		Name  string `json:"name"`
		Owner struct {
			Name string `json:"name"`
		} `json:"owner"`
		Plugins []struct {
			Name   string `json:"name"`
			Source struct {
				Source string `json:"source"`
				URL    string `json:"url"`
				Path   string `json:"path"`
				Ref    string `json:"ref"`
			} `json:"source"`
		} `json:"plugins"`
	}
	readRootJSON(t, &m, ".claude-plugin/marketplace.json")
	var pm pluginManifest
	readJSON(t, &pm, ".claude-plugin", "plugin.json")

	if m.Name != "mirastacklabs" {
		t.Errorf("marketplace name = %q", m.Name)
	}
	if m.Owner.Name == "" {
		t.Error("owner.name is required")
	}
	if len(m.Plugins) != 1 {
		t.Fatalf("want 1 plugin entry, got %d", len(m.Plugins))
	}
	p := m.Plugins[0]
	if p.Name != pm.Name {
		t.Errorf("entry name %q != manifest name %q", p.Name, pm.Name)
	}
	// No "version" on the entry: Claude Code takes the version from plugin.json, and a second copy
	// could only drift. The release tag is pinned through source.ref instead.
	// path and ref move together (relgen bump writes both). The plugin root is
	// integrations/claude-code/plugin from the first release after v0.6.0; the v0.6.0 tag has the
	// plugin at integrations/claude-code, so that pair must stay valid for existing installs until
	// the next bump.
	wantPath := "integrations/claude-code/plugin"
	if p.Source.Ref == "v0.6.0" {
		wantPath = "integrations/claude-code"
	}
	if p.Source.Source != "git-subdir" || p.Source.Path != wantPath {
		t.Fatalf("source = %+v, want git-subdir %s", p.Source, wantPath)
	}
	if !strings.HasPrefix(p.Source.URL, "https://github.com/mirastacklabs-ai/mirafleet") {
		t.Errorf("source url = %q", p.Source.URL)
	}
	if p.Source.Ref != "v"+pm.Version {
		t.Errorf("ref %q must pin the release tag v%s", p.Source.Ref, pm.Version)
	}
	if _, err := os.Stat(filepath.FromSlash(p.Source.Path[len("integrations/"):])); err != nil {
		t.Errorf("source path does not exist: %v", err)
	}
}

func TestServerJSON(t *testing.T) {
	var s struct {
		Schema      string `json:"$schema"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Version     string `json:"version"`
		Repository  struct {
			URL    string `json:"url"`
			Source string `json:"source"`
		} `json:"repository"`
		Packages []struct {
			RegistryType string `json:"registryType"`
			Identifier   string `json:"identifier"`
			Version      string `json:"version"`
			Transport    struct {
				Type string `json:"type"`
			} `json:"transport"`
			PackageArguments []struct {
				Type  string `json:"type"`
				Value string `json:"value"`
			} `json:"packageArguments"`
		} `json:"packages"`
	}
	readRootJSON(t, &s, "server.json")
	var pm pluginManifest
	readJSON(t, &pm, ".claude-plugin", "plugin.json")

	if s.Name != "io.github.mirastacklabs-ai/mirafleet" {
		t.Errorf("name = %q", s.Name)
	}
	// Verified 2026-11-14 against the MCP Registry docs (docs/M6.md section 2.18).
	if s.Schema != "https://static.modelcontextprotocol.io/schemas/2025-12-11/server.schema.json" {
		t.Errorf("$schema = %q", s.Schema)
	}
	if s.Description == "" || len(s.Description) > 100 {
		t.Errorf("a description of at most 100 chars is required (len %d)", len(s.Description))
	}
	if s.Repository.URL == "" || s.Repository.Source != "github" {
		t.Errorf("repository = %+v", s.Repository)
	}
	if s.Version != pm.Version {
		t.Errorf("version %q != plugin manifest %q", s.Version, pm.Version)
	}
	if len(s.Packages) != 1 {
		t.Fatalf("server.json lists npm only: want 1 package, got %d", len(s.Packages))
	}
	p := s.Packages[0]
	if p.RegistryType != "npm" || p.Identifier != "@mirastacklabs/mirafleet" {
		t.Errorf("package = %s %s", p.RegistryType, p.Identifier)
	}
	if p.Version != s.Version {
		t.Errorf("package version %q != %q", p.Version, s.Version)
	}
	if p.Transport.Type != "stdio" {
		t.Errorf("transport = %q, want stdio", p.Transport.Type)
	}
	if len(p.PackageArguments) != 1 || p.PackageArguments[0].Type != "positional" || p.PackageArguments[0].Value != "mcp" {
		t.Errorf("packageArguments = %+v, want one positional `mcp`", p.PackageArguments)
	}
}
