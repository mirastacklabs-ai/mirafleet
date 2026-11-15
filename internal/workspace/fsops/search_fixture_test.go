package fsops

import (
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

var updateSearch = flag.Bool("update-search", false, "rewrite the grep and glob golden files")

// searchFixture copies testdata/search into a temp dir, restoring the dot names that cannot live
// in the repository as such (_gitignore, _hidden, _git), and adds a binary file and symlinks.
func searchFixture(t *testing.T) (*os.Root, string) {
	t.Helper()
	dir := t.TempDir()
	rename := map[string]string{"_gitignore": ".gitignore", "_hidden": ".hidden", "_git": ".git"}
	src := filepath.Join("testdata", "search")
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if rel == "." {
			return nil
		}
		parts := strings.Split(rel, string(filepath.Separator))
		for i, s := range parts {
			if n, ok := rename[s]; ok {
				parts[i] = n
			}
		}
		dst := filepath.Join(append([]string{dir}, parts...)...)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	must(t, os.WriteFile(filepath.Join(outside, "leak.txt"), []byte("TODO outside\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, "bin.dat"), []byte("TODO\x00binary"), 0o644))
	must(t, os.Symlink(outside, filepath.Join(dir, "escape")))
	must(t, os.Symlink(filepath.Join(outside, "leak.txt"), filepath.Join(dir, "leak-link.txt")))
	must(t, os.Symlink("src", filepath.Join(dir, "linkdir")))
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	must(t, filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			return os.Chtimes(p, base.Add(-time.Hour), base.Add(-time.Hour))
		}
		return err
	}))
	for rel, plus := range map[string]int{"main.go": 3, "src/sub/deep.go": 2, "src/app.go": 1} {
		must(t, os.Chtimes(filepath.Join(dir, rel), base.Add(time.Duration(plus)*time.Hour), base.Add(time.Duration(plus)*time.Hour)))
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return root, dir
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func renderGrep(r GrepResult) string {
	var b strings.Builder
	for _, l := range r.Lines {
		b.WriteString(l + "\n")
	}
	for _, f := range r.Files {
		b.WriteString(f + "\n")
	}
	for _, c := range r.Counts {
		b.WriteString(c.Path + ":" + strconv.Itoa(c.Count) + "\n")
	}
	if r.Truncated {
		b.WriteString("[truncated: " + r.Reason + "]\n")
	}
	return b.String()
}

func searchGolden(t *testing.T, name, got string) {
	t.Helper()
	p := filepath.Join("testdata", "search-golden", name+".golden")
	if *updateSearch {
		must(t, os.MkdirAll(filepath.Dir(p), 0o755))
		must(t, os.WriteFile(p, []byte(got), 0o644))
		return
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%v (run with -update-search)", err)
	}
	if string(want) != got {
		t.Errorf("%s mismatch\n--- want\n%s--- got\n%s", name, want, got)
	}
}
