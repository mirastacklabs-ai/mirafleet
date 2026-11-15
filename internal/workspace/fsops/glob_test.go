package fsops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(dir, name, content string) error {
	return os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)
}

func TestGlobGolden(t *testing.T) {
	root, _ := searchFixture(t)
	cases := []struct {
		name string
		o    GlobOptions
	}{
		{"go_mtime", GlobOptions{Pattern: "**/*.go"}},
		{"star_root_only", GlobOptions{Pattern: "*.go"}},
		{"src_braces", GlobOptions{Pattern: "src/**/*.{go,py}"}},
		{"in_path", GlobOptions{Pattern: "*.go", Path: "src"}},
		{"deep_in_path", GlobOptions{Pattern: "**/*.go", Path: "src"}},
		{"all_md", GlobOptions{Pattern: "**/*.md"}},
		{"hidden", GlobOptions{Pattern: "**/*.txt", Hidden: true}},
		{"noignore", GlobOptions{Pattern: "**/*.log", NoIgnore: true}},
		{"ignored_dirs_skipped", GlobOptions{Pattern: "**/*"}},
		{"limit", GlobOptions{Pattern: "**/*.go", Limit: 2}},
		{"question_class", GlobOptions{Pattern: "src/[a-b]??.go"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, err := Glob(context.Background(), root, c.o)
			if err != nil {
				t.Fatal(err)
			}
			out := strings.Join(r.Paths, "\n") + "\n"
			if r.Truncated {
				out += "[truncated: " + r.Reason + "]\n"
			}
			searchGolden(t, "glob_"+c.name, out)
		})
	}
}

func TestGlobErrors(t *testing.T) {
	root, _ := searchFixture(t)
	for _, o := range []GlobOptions{{Pattern: ""}, {Pattern: "/etc/*"}, {Pattern: "*", Path: "../x"}} {
		if _, err := Glob(context.Background(), root, o); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: %v", o, err)
		}
	}
	if _, err := Glob(context.Background(), root, GlobOptions{Pattern: "*", Path: "nope"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing dir: %v", err)
	}
}

func TestGlobSymlinksNotFollowed(t *testing.T) {
	root, _ := searchFixture(t)
	r, err := Glob(context.Background(), root, GlobOptions{Pattern: "**/*", Hidden: true, NoIgnore: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range r.Paths {
		if strings.Contains(p, "leak") || strings.HasPrefix(p, "escape") || strings.HasPrefix(p, "linkdir") || strings.HasPrefix(p, ".git/") {
			t.Errorf("unexpected %s", p)
		}
	}
}

func TestGlobMaxFiles(t *testing.T) {
	root, _ := searchFixture(t)
	r, err := Glob(context.Background(), root, GlobOptions{Pattern: "**/*", Limits: SearchLimits{MaxFiles: 3}})
	if err != nil || !r.Truncated || r.Reason != ReasonMaxFiles || len(r.Paths) > 3 {
		t.Errorf("%+v %v", r, err)
	}
}
