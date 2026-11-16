package integrations

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// directoryDir is the slim setup plugin submitted to the Anthropic plugin directory. The directory
// refuses command hooks, local MCP servers, bin/ and any file that is not text, JSON, YAML or an
// image, for organizations that are not Verified (docs/M6.md section 2.18). It holds only
// Markdown, JSON and the icon.
const directoryDir = "claude-code/directory"

func TestDirectoryPluginFiles(t *testing.T) {
	want := map[string]bool{
		".claude-plugin/plugin.json":      false,
		".claude-plugin/icon.png":         false,
		"README.md":                       false,
		"skills/mirafleet-setup/SKILL.md": false,
	}
	err := filepath.WalkDir(directoryDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(directoryDir, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == "bin" || strings.HasPrefix(rel, "bin/") || rel == "hooks" || rel == "scripts" {
				t.Errorf("%s: directories bin, hooks and scripts are not allowed", rel)
			}
			return nil
		}
		if _, ok := want[rel]; ok {
			want[rel] = true
		}
		base := filepath.Base(rel)
		if base == "hooks.json" || base == ".mcp.json" || base == "mcp.json" {
			t.Errorf("%s: not allowed in a directory plugin", rel)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		switch ext := strings.ToLower(filepath.Ext(rel)); ext {
		case ".md", ".txt", ".json", ".yaml", ".yml":
			if !utf8.Valid(b) {
				t.Errorf("%s: not valid UTF-8", rel)
			}
			if bytes.Contains(b, []byte{0}) {
				t.Errorf("%s: contains a NUL byte", rel)
			}
			if ext == ".json" && !json.Valid(b) {
				t.Errorf("%s: invalid JSON", rel)
			}
		case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".ico":
		default:
			t.Errorf("%s: file type not allowed in a directory plugin", rel)
		}
		if bytes.HasPrefix(b, []byte("#!")) {
			t.Errorf("%s: starts with #!", rel)
		}
		// Any #! at the start of a line in text is also a red flag.
		if ext := filepath.Ext(rel); ext != ".png" && bytes.Contains(b, []byte("\n#!")) {
			t.Errorf("%s: has a #! line", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for f, seen := range want {
		if !seen {
			t.Errorf("missing %s", f)
		}
	}
}

func TestDirectoryPluginManifest(t *testing.T) {
	var m struct {
		Name       string `json:"name"`
		Display    string `json:"displayName"`
		Version    string `json:"version"`
		License    string `json:"license"`
		Docs       string `json:"documentationUrl"`
		Support    string `json:"supportUrl"`
		Privacy    string `json:"privacyPolicyUrl"`
		Terms      string `json:"termsOfServiceUrl"`
		Desc       string `json:"description"`
		Hooks      any    `json:"hooks"`
		McpServers any    `json:"mcpServers"`
	}
	b, err := os.ReadFile(filepath.Join(directoryDir, ".claude-plugin", "plugin.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m.Name != "mirafleet-setup" || m.Display != "mirafleet" || m.License != "Apache-2.0" || m.Desc == "" {
		t.Fatalf("manifest fields: %+v", m)
	}
	if m.Hooks != nil || m.McpServers != nil {
		t.Error("a directory plugin declares no hooks or MCP servers")
	}
	const gh = "https://github.com/mirastacklabs-ai/mirafleet/blob/main/"
	for name, c := range map[string]struct{ got, want string }{
		"documentationUrl":  {m.Docs, gh + "integrations/claude-code/directory/README.md"},
		"supportUrl":        {m.Support, gh + "SUPPORT.md"},
		"privacyPolicyUrl":  {m.Privacy, gh + "PRIVACY.md"},
		"termsOfServiceUrl": {m.Terms, gh + "LICENSE"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", name, c.got, c.want)
		}
	}
	// The setup plugin must not clash with the full plugin of the same marketplace.
	var full pluginManifest
	readJSON(t, &full, ".claude-plugin", "plugin.json")
	if m.Name == full.Name {
		t.Errorf("setup plugin and full plugin share the name %q", m.Name)
	}
	if m.Version != full.Version {
		t.Errorf("version %s, full plugin %s", m.Version, full.Version)
	}
}

func TestDirectoryPluginIconIdentical(t *testing.T) {
	a, err := os.ReadFile(filepath.Join(directoryDir, ".claude-plugin", "icon.png"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, read(t, ".claude-plugin", "icon.png")) {
		t.Error("directory plugin icon differs from the full plugin icon")
	}
}

func TestDirectoryPluginSkill(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(directoryDir, "skills", "mirafleet-setup", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	fm, body := frontmatter(t, b)
	if fm["name"] != "mirafleet-setup" || fm["description"] == "" {
		t.Errorf("frontmatter: %v", fm)
	}
	for _, s := range []string{
		"claude plugin marketplace add mirastacklabs-ai/mirafleet",
		"claude plugin install mirafleet@mirastacklabs",
		"mirafleet mcp login HOST:PORT -name NAME -code -",
		"list_fleets",
		"--verify-signature",
	} {
		if !strings.Contains(body, s) {
			t.Errorf("SKILL.md lacks %q", s)
		}
	}
}
