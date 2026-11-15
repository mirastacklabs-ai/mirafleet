package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestWikiGolden(t *testing.T) {
	out := filepath.Join(t.TempDir(), "wiki")
	c, _, errs := do("-root", "testdata/wiki/in", "wiki", "-out", out, "-commit", "abc1234")
	if c != 0 {
		t.Fatalf("code %d %q", c, errs)
	}
	var got []string
	filepath.Walk(out, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			rel, _ := filepath.Rel(out, p)
			got = append(got, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(got)
	want := "01-First-Chapter.md Glossary.md Guide.md Home.md Quickstart.md _Sidebar.md diagrams/flow.svg"
	if strings.Join(got, " ") != want {
		t.Fatalf("files: %v", got)
	}
	for _, f := range got {
		b, _ := os.ReadFile(filepath.Join(out, filepath.FromSlash(f)))
		goldenN26(t, "wiki/golden/"+f, string(b))
	}
}

func TestWikiLinks(t *testing.T) {
	pages := map[string]string{"docs/guide/README.md": "Guide", "docs/guide/glossary.md": "Glossary"}
	dia := map[string]bool{"docs/guide/diagrams/a.svg": true}
	body := "[g](glossary.md#t) ![a](diagrams/a.svg) [arch](../ARCHITECTURE.md) [x](https://e.com/a.md) [h](#h)"
	got, err := rewriteLinks("docs/guide/README.md", body, pages, dia)
	if err != nil {
		t.Fatal(err)
	}
	want := "[g](Glossary#t) ![a](diagrams/a.svg) [arch](" + wikiBlobBase + "docs/ARCHITECTURE.md) [x](https://e.com/a.md) [h](#h)"
	if got != want {
		t.Errorf("got %q", got)
	}
	// A link that climbs out of the repository is refused, never rewritten to a path.
	if _, err := rewriteLinks("docs/guide/README.md", "[e](../../../etc/passwd)", pages, dia); err == nil {
		t.Error("escaping link accepted")
	}
}

func TestWikiOutputGuards(t *testing.T) {
	out := t.TempDir()
	os.WriteFile(filepath.Join(out, "keep"), []byte("x"), 0o644)
	if c, _, errs := do("-root", "testdata/wiki/in", "wiki", "-out", out, "-commit", "abc"); c != 1 || !strings.Contains(errs, "not empty") {
		t.Errorf("non-empty out: %d %q", c, errs)
	}
	if c, _, _ := do("-root", "testdata/wiki/in", "wiki", "-out", t.TempDir(), "-commit", "a b"); c != 1 {
		t.Error("bad commit accepted")
	}
}
