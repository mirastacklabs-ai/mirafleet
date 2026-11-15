package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// copyTree copies testdata/tree into a fresh temp dir.
func copyTree(t *testing.T) string {
	t.Helper()
	src := filepath.Join("testdata", "tree")
	dst := t.TempDir()
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		return os.WriteFile(out, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func do(args ...string) (int, string, string) {
	var o, e bytes.Buffer
	c := run(args, &o, &e)
	return c, o.String(), e.String()
}

func read(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCheckFixtureAtOldVersion(t *testing.T) {
	root := copyTree(t)
	// marketplace carries the tag form v0.5.0, the others 0.5.0: all match.
	if c, out, errs := do("-root", root, "check", "v0.5.0"); c != 0 {
		t.Fatalf("unexpected %d %q %q", c, out, errs)
	}
}

func TestCheckMismatchListsEveryFile(t *testing.T) {
	root := copyTree(t)
	c, _, errs := do("-root", root, "check", "v0.6.0")
	if c != 1 {
		t.Fatalf("code %d", c)
	}
	for _, p := range []string{"plugin.json", "mcp_hook.go", "marketplace.json", "server.json", "protover.go"} {
		if !strings.Contains(errs, p) {
			t.Errorf("missing %s in %q", p, errs)
		}
	}
	if strings.Count(errs, "server.json") != 2 {
		t.Errorf("server.json should report top-level and package: %q", errs)
	}
}

func TestBumpThenCheck(t *testing.T) {
	root := copyTree(t)
	if c, out, errs := do("-root", root, "bump", "0.6.0"); c != 0 {
		t.Fatalf("bump %d %q %q", c, out, errs)
	}
	if c, out, errs := do("-root", root, "check", "v0.6.0"); c != 0 {
		t.Fatalf("check %d %q %q", c, out, errs)
	}
	// Only the version strings changed; formatting is preserved byte for byte.
	for _, rel := range []string{
		"integrations/claude-code/.claude-plugin/plugin.json",
		"cmd/mirafleet/mcp_hook.go",
		".claude-plugin/marketplace.json",
		"server.json",
		"deploy/compose/central/compose.yaml",
		"deploy/compose/runner/compose.yaml",
		"deploy/compose/lab/compose.yaml",
	} {
		want := strings.ReplaceAll(read(t, filepath.Join("testdata", "tree"), rel), "0.5.0", "0.6.0")
		if got := read(t, root, rel); got != want {
			t.Errorf("%s:\n%s\nwant:\n%s", rel, got, want)
		}
	}
	// The protocol level follows the minor in 0.x; the rest of the file is untouched.
	wantProto := strings.Replace(read(t, filepath.Join("testdata", "tree"), "internal/protover/protover.go"),
		"const Current = 5", "const Current = 6", 1)
	if got := read(t, root, "internal/protover/protover.go"); got != wantProto {
		t.Errorf("protover.go:\n%s\nwant:\n%s", got, wantProto)
	}
	// Idempotent.
	_, out, _ := do("-root", root, "bump", "0.6.0")
	if !strings.Contains(out, "(0 changed)") {
		t.Errorf("second bump: %q", out)
	}
	// Pre-release.
	if c, _, errs := do("-root", root, "bump", "0.6.0-rc.1"); c != 0 {
		t.Fatal(errs)
	}
	if c, _, errs := do("-root", root, "check", "v0.6.0-rc.1"); c != 0 {
		t.Fatal(errs)
	}
	if !strings.Contains(read(t, root, ".claude-plugin/marketplace.json"), `"ref": "v0.6.0-rc.1"`) {
		t.Error("marketplace ref not tag form")
	}
}

// After bump + check, every versioned file is consistent and the marketplace invariants that
// integrations/marketplace_test.go asserts hold: no entry "version" key, source.ref is the tag,
// plugin.json and server.json carry the bare version, protover.Current is the tag's level.
func TestBumpCheckInvariants(t *testing.T) {
	for _, c := range []struct{ ver, level string }{
		{"0.6.0", "6"}, {"0.7.2-rc.3", "7"}, {"1.0.0", "1"}, {"1.4.2", "1"}, {"2.0.0-rc.1", "2"},
	} {
		root := copyTree(t)
		if code, out, errs := do("-root", root, "bump", c.ver); code != 0 {
			t.Fatalf("bump %s: %d %q %q", c.ver, code, out, errs)
		}
		if code, out, errs := do("-root", root, "check", "v"+c.ver); code != 0 {
			t.Fatalf("check v%s: %d %q %q", c.ver, code, out, errs)
		}
		mp := read(t, root, ".claude-plugin/marketplace.json")
		var m struct {
			Plugins []map[string]any `json:"plugins"`
		}
		if err := json.Unmarshal([]byte(mp), &m); err != nil || len(m.Plugins) != 1 {
			t.Fatalf("marketplace: %v %s", err, mp)
		}
		if _, has := m.Plugins[0]["version"]; has {
			t.Errorf("%s: marketplace entry gained a version key", c.ver)
		}
		if src, _ := m.Plugins[0]["source"].(map[string]any); src["ref"] != "v"+c.ver {
			t.Errorf("%s: source.ref = %v", c.ver, src["ref"])
		}
		for _, rel := range []string{"integrations/claude-code/.claude-plugin/plugin.json", "server.json"} {
			if !strings.Contains(read(t, root, rel), `"version": "`+c.ver+`"`) {
				t.Errorf("%s: %s lacks version %s", c.ver, rel, c.ver)
			}
		}
		if !strings.Contains(read(t, root, "internal/protover/protover.go"), "const Current = "+c.level+"\n") {
			t.Errorf("%s: protover level, want %s", c.ver, c.level)
		}
		// A wrong level is caught by check even when every version string matches.
		p := filepath.Join(root, "internal/protover/protover.go")
		os.WriteFile(p, []byte("package protover\n\nconst Current = 99\n"), 0o644)
		if code, _, errs := do("-root", root, "check", "v"+c.ver); code != 1 || !strings.Contains(errs, "protover.go") {
			t.Errorf("%s: wrong level not reported: %d %q", c.ver, code, errs)
		}
	}
}

func TestBumpRefusesSymlinkTarget(t *testing.T) {
	root := copyTree(t)
	p := filepath.Join(root, "server.json")
	real := filepath.Join(root, "real-server.json")
	if err := os.Rename(p, real); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, p); err != nil {
		t.Skipf("no symlinks: %v", err)
	}
	c, _, errs := do("-root", root, "bump", "0.6.0")
	if c != 1 || !strings.Contains(errs, "not a regular file") {
		t.Errorf("symlink target: %d %q", c, errs)
	}
	if got := read(t, root, "cmd/mirafleet/mcp_hook.go"); !strings.Contains(got, `"0.5.0"`) {
		t.Errorf("partial write: %s", got)
	}
}

func TestBumpRenameFailureRestores(t *testing.T) {
	root := copyTree(t)
	orig := renameFn
	defer func() { renameFn = orig }()
	n := 0
	renameFn = func(from, to string) error {
		n++
		if n == 3 {
			return errors.New("injected")
		}
		return os.Rename(from, to)
	}
	c, _, errs := do("-root", root, "bump", "0.6.0")
	if c != 1 || !strings.Contains(errs, "injected") || !strings.Contains(errs, "all restored") ||
		!strings.Contains(errs, "plugin.json") {
		t.Fatalf("code %d %q", c, errs)
	}
	for _, rel := range []string{
		"integrations/claude-code/.claude-plugin/plugin.json", "cmd/mirafleet/mcp_hook.go",
		".claude-plugin/marketplace.json", "server.json", "internal/protover/protover.go",
	} {
		if got, want := read(t, root, rel), read(t, filepath.Join("testdata", "tree"), rel); got != want {
			t.Errorf("%s not restored:\n%s", rel, got)
		}
	}
	// No temp files left behind.
	filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && strings.HasPrefix(info.Name(), ".relgen-") {
			t.Errorf("temp file left: %s", p)
		}
		return nil
	})
}

func TestBadVersionArguments(t *testing.T) {
	root := copyTree(t)
	for _, a := range [][]string{
		{"bump", "v0.6.0"}, {"bump", "0.6"}, {"bump", "01.2.3"}, {"bump"}, {"bump", "1.0.0", "x"},
		{"check", "0.6.0"}, {"check", "vx"}, {"check"},
	} {
		if c, _, _ := do(append([]string{"-root", root}, a...)...); c != 1 {
			t.Errorf("%v: code %d, want 1", a, c)
		}
	}
	if read(t, root, "server.json") != read(t, filepath.Join("testdata", "tree"), "server.json") {
		t.Error("rejected bump modified files")
	}
}

func TestBumpFailureLeavesTreeUntouched(t *testing.T) {
	root := copyTree(t)
	// Break the last target so earlier ones would otherwise already be written.
	if err := os.WriteFile(filepath.Join(root, "server.json"), []byte(`{"name":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if c, _, _ := do("-root", root, "bump", "0.6.0"); c != 1 {
		t.Fatal("want failure")
	}
	if got := read(t, root, "cmd/mirafleet/mcp_hook.go"); !strings.Contains(got, `"0.5.0"`) {
		t.Errorf("partial write: %s", got)
	}
}

func TestMissingFileAndAmbiguity(t *testing.T) {
	root := copyTree(t)
	os.Remove(filepath.Join(root, "server.json"))
	if c, _, errs := do("-root", root, "check", "v0.5.0"); c != 1 || !strings.Contains(errs, "server.json") {
		t.Errorf("missing file: %d %q", c, errs)
	}
	root = copyTree(t)
	p := filepath.Join(root, "cmd/mirafleet/mcp_hook.go")
	os.WriteFile(p, []byte("const pluginVersion = \"1\"\nconst pluginVersion = \"2\"\n"), 0o644)
	if c, _, _ := do("-root", root, "bump", "0.6.0"); c != 1 {
		t.Error("two constants must fail")
	}
}

func TestDispatch(t *testing.T) {
	if c, _, _ := do(); c != 2 {
		t.Errorf("no command: %d", c)
	}
	if c, _, e := do("nope"); c != 2 || !strings.Contains(e, "unknown command") {
		t.Errorf("unknown: %d %q", c, e)
	}
	_, _, e := do()
	if !strings.Contains(e, "bump") || !strings.Contains(e, "check") {
		t.Errorf("usage lacks commands: %q", e)
	}
}
