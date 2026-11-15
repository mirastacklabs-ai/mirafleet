package fsops

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestGrepGolden(t *testing.T) {
	root, _ := searchFixture(t)
	cases := []struct {
		name string
		o    GrepOptions
	}{
		{"files", GrepOptions{Pattern: "TODO"}},
		{"files_ignorecase", GrepOptions{Pattern: "todo", IgnoreCase: true}},
		{"files_casesensitive", GrepOptions{Pattern: "todo"}},
		{"content_n", GrepOptions{Pattern: "TODO", Mode: "content", LineNumbers: true}},
		{"content_nonum", GrepOptions{Pattern: "TODO", Mode: "content", Path: "src"}},
		{"count", GrepOptions{Pattern: "(?i)todo", Mode: "count"}},
		{"context_c1", GrepOptions{Pattern: "TODO", Mode: "content", LineNumbers: true, Context: 1, Path: "."}},
		{"context_a_b", GrepOptions{Pattern: "Hello", Mode: "content", LineNumbers: true, After: 2, Before: 1, Glob: "*.md"}},
		{"multiline", GrepOptions{Pattern: `func main\(\) \{.*?\}`, Mode: "content", LineNumbers: true, Multiline: true}},
		{"multiline_anchor", GrepOptions{Pattern: `^TODO.*\n.*$`, Mode: "content", LineNumbers: true, Multiline: true, Glob: "README.md"}},
		{"glob_braces", GrepOptions{Pattern: "TODO", Glob: "*.{go,py}"}},
		{"glob_negate", GrepOptions{Pattern: "TODO", Glob: "*.go !src/**"}},
		{"glob_anchored", GrepOptions{Pattern: "TODO", Glob: "src/*.go"}},
		{"type_py", GrepOptions{Pattern: "TODO", Type: "py"}},
		{"type_go", GrepOptions{Pattern: "TODO", Type: "go", Mode: "count"}},
		{"hidden", GrepOptions{Pattern: "TODO", Hidden: true}},
		{"noignore", GrepOptions{Pattern: "TODO", NoIgnore: true}},
		{"single_file", GrepOptions{Pattern: "TODO", Path: "root-only.txt", Mode: "content", LineNumbers: true}},
		{"subdir_ignore_inherited", GrepOptions{Pattern: "TODO", Path: "src/sub"}},
		{"head_limit", GrepOptions{Pattern: "TODO", Mode: "content", HeadLimit: 3}},
		{"offset_limit", GrepOptions{Pattern: "TODO", Mode: "content", Offset: 2, HeadLimit: 2}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, err := Grep(context.Background(), root, c.o)
			if err != nil {
				t.Fatal(err)
			}
			searchGolden(t, "grep_"+c.name, renderGrep(r))
		})
	}
}

func TestGrepNeverLeaks(t *testing.T) {
	root, _ := searchFixture(t)
	r, err := Grep(context.Background(), root, GrepOptions{Pattern: "TODO", Mode: "content", Hidden: true, NoIgnore: true})
	if err != nil {
		t.Fatal(err)
	}
	out := renderGrep(r)
	for _, bad := range []string{"leak", "escape", "linkdir", "bin.dat", ".git/", "outside"} {
		if strings.Contains(out, bad) {
			t.Errorf("output mentions %q:\n%s", bad, out)
		}
	}
}

func TestGrepErrors(t *testing.T) {
	root, _ := searchFixture(t)
	for _, o := range []GrepOptions{
		{Pattern: ""},
		{Pattern: "("},
		{Pattern: "x", Mode: "bogus"},
		{Pattern: "x", Type: "nosuchtype"},
		{Pattern: "x", Path: "../etc"},
		{Pattern: "x", Path: "/etc"},
		{Pattern: "x", After: -1},
	} {
		if _, err := Grep(context.Background(), root, o); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: err = %v, want ErrInvalid", o, err)
		}
	}
	if _, err := Grep(context.Background(), root, GrepOptions{Pattern: "x", Path: "missing"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing path: %v", err)
	}
}

func TestGrepLimits(t *testing.T) {
	root, _ := searchFixture(t)
	r, err := Grep(context.Background(), root, GrepOptions{Pattern: "TODO", Limits: SearchLimits{MaxFiles: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Truncated || r.Reason != ReasonMaxFiles || len(r.Files) > 2 {
		t.Errorf("max files: %+v", r)
	}
	r, err = Grep(context.Background(), root, GrepOptions{Pattern: "TODO", Limits: SearchLimits{MaxBytes: 30}})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Truncated || r.Reason != ReasonMaxBytes {
		t.Errorf("max bytes: %+v", r)
	}
	r, err = Grep(context.Background(), root, GrepOptions{Pattern: "TODO", HeadLimit: 1})
	if err != nil || !r.Truncated || r.Reason != ReasonHeadLimit || len(r.Files) != 1 {
		t.Errorf("head limit: %+v %v", r, err)
	}
	r, _ = Grep(context.Background(), root, GrepOptions{Pattern: "TODO", Offset: 100})
	if len(r.Files) != 0 || r.Truncated {
		t.Errorf("offset past end: %+v", r)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Grep(ctx, root, GrepOptions{Pattern: "TODO"}); err == nil {
		t.Error("canceled context should fail")
	}
}

func TestGrepLongLineClipped(t *testing.T) {
	root, dir := searchFixture(t)
	must(t, writeFile(dir, "long.txt", "needle "+strings.Repeat("é", 5000)+"\n"))
	r, err := Grep(context.Background(), root, GrepOptions{Pattern: "needle", Mode: "content", Path: "long.txt"})
	if err != nil || len(r.Lines) != 1 {
		t.Fatalf("%v %v", r, err)
	}
	if len(r.Lines[0]) > maxGrepLineBytes+20 || !strings.HasSuffix(r.Lines[0], "…") {
		t.Errorf("line not clipped: %d bytes", len(r.Lines[0]))
	}
}

func TestRgArgs(t *testing.T) {
	got, err := RgArgs(GrepOptions{Pattern: "-x", Mode: "content", IgnoreCase: true, Context: 2, After: 1, Multiline: true, Glob: "*.{go,ts} !vendor/**", Type: "go", Path: "src"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--no-config", "--no-require-git", "--sort", "path", "--json", "-B", "2", "-A", "1", "-i", "-U", "--multiline-dotall",
		"--glob", "*.{go,ts}", "--glob", "!vendor/**", "--type", "go", "-e", "-x", "--", "src"}
	if !slices.Equal(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	got, _ = RgArgs(GrepOptions{Pattern: "a", Mode: "count"})
	if !slices.Contains(got, "-c") || !slices.Contains(got, "--with-filename") || got[len(got)-1] != "." {
		t.Errorf("count args %q", got)
	}
	got, _ = RgArgs(GrepOptions{Pattern: "a"})
	if !slices.Contains(got, "-l") {
		t.Errorf("files args %q", got)
	}
	if _, err := RgArgs(GrepOptions{Pattern: "(?<=a)b"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("lookbehind must be rejected, got %v", err)
	}
}
