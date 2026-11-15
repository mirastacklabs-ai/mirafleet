//go:build linux

package hostx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/fsops"
)

func TestRgParity(t *testing.T) {
	rg, err := exec.LookPath("rg")
	if err != nil {
		t.Skip("rg not installed")
	}
	dir := t.TempDir()
	files := map[string]string{
		"a.go":          "package a\n\nfunc Hello() {}\nfunc hello2() {}\n",
		"sub/b.go":      "package b\nvar x = 1\n// Hello again\n",
		"sub/c.txt":     "hello world\nsecond\nthird hello\n",
		"sub/deep/d.go": "package d\n",
		"ignored/e.go":  "func Hello() {}\n",
		".gitignore":    "ignored/\n",
		".hidden/h.go":  "func Hello() {}\n",
		"multi.txt":     "start\nmid\nend\n",
	}
	for p, c := range files {
		full := filepath.Join(dir, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(c), 0o644)
	}
	os.Symlink("/etc/hostname", filepath.Join(dir, "link.txt"))
	rgF, err := OpenFiles(dir, FilesOptions{Rg: rg})
	if err != nil {
		t.Fatal(err)
	}
	defer rgF.Close()
	goF, _ := OpenFiles(dir, FilesOptions{Rg: "-"})
	defer goF.Close()
	if !rgF.UsesRg() || goF.UsesRg() {
		t.Fatal("engine selection")
	}
	cases := []fsops.GrepOptions{
		{Pattern: "Hello"},
		{Pattern: "hello", IgnoreCase: true},
		{Pattern: "Hello", Mode: "count"},
		{Pattern: "hello", Mode: "content", LineNumbers: true},
		{Pattern: "hello", Mode: "content", LineNumbers: true, Context: 1},
		{Pattern: "Hello", Mode: "content", Glob: "*.go", Path: "sub"},
		{Pattern: "Hello", Type: "go", Mode: "count"},
		{Pattern: "start\nmid", Multiline: true, Mode: "content", LineNumbers: true},
		{Pattern: "hello", Mode: "content", HeadLimit: 2},
		{Pattern: "package", HeadLimit: 2},
		{Pattern: "package", Path: "link.txt"},
		{Pattern: "nomatch"},
	}
	for i, o := range cases {
		got, err1 := rgF.Grep(context.Background(), o)
		want, err2 := goF.Grep(context.Background(), o)
		if (err1 == nil) != (err2 == nil) {
			t.Fatalf("case %d errors: %v vs %v", i, err1, err2)
		}
		got.FilesSearched, want.FilesSearched = 0, 0
		if !reflect.DeepEqual(got, want) {
			t.Errorf("case %d %+v:\n rg: %+v\n go: %+v", i, o, got, want)
		}
	}
}
