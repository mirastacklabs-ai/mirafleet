package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// harnessRoot copies testdata/harness into a temp dir.
func harnessRoot(t *testing.T) string {
	t.Helper()
	src := filepath.Join("testdata", "harness")
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
		os.MkdirAll(filepath.Dir(out), 0o755)
		return os.WriteFile(out, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func TestHarnessInSync(t *testing.T) {
	c, out, errs := do("-root", harnessRoot(t), "harness")
	if c != 0 || !strings.Contains(out, "3 harness command copies") {
		t.Fatalf("code %d %q %q", c, out, errs)
	}
}

func TestHarnessDriftAndWrite(t *testing.T) {
	root := harnessRoot(t)
	shared := filepath.Join(root, "integrations/commands/mirafleet.md")
	os.WriteFile(shared, []byte("Changed body. Task: $ARGUMENTS\n"), 0o644)
	c, _, errs := do("-root", root, "harness")
	if c != 1 {
		t.Fatalf("drift not detected: %d", c)
	}
	for _, p := range []string{"opencode/commands/mirafleet.md", "gemini/commands/mirafleet.toml", "codex/prompts/mirafleet.md"} {
		if !strings.Contains(errs, p) {
			t.Errorf("drift list lacks %s: %q", p, errs)
		}
	}
	if strings.Contains(errs, "openfleet") {
		t.Errorf("unrelated command checked: %q", errs)
	}
	if c, _, errs := do("-root", root, "harness", "-write"); c != 0 {
		t.Fatalf("write: %d %q", c, errs)
	}
	goldenN26(t, "harness/opencode.golden", read(t, root, "integrations/opencode/commands/mirafleet.md"))
	goldenN26(t, "harness/gemini.golden", read(t, root, "integrations/gemini/commands/mirafleet.toml"))
	if c, _, errs := do("-root", root, "harness"); c != 0 {
		t.Fatalf("not in sync after write: %d %q", c, errs)
	}
}

func TestHarnessTomlEscapesRefused(t *testing.T) {
	root := harnessRoot(t)
	p := filepath.Join(root, "integrations/gemini/commands/mirafleet.toml")
	os.WriteFile(p, []byte("prompt = \"\"\"\nline\\n\n\"\"\"\n"), 0o644)
	if c, _, errs := do("-root", root, "harness"); c != 1 || !strings.Contains(errs, "backslash") {
		t.Fatalf("%d %q", c, errs)
	}
}
