package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The real repository: licenses/extra agrees with dev.sh --ignore and ASSETS.md.
func TestRepoExtrasTied(t *testing.T) {
	if err := checkExtras("../.."); err != nil {
		t.Fatal(err)
	}
}

func extraFixture(t *testing.T) string {
	root := t.TempDir()
	write(t, filepath.Join(root, "go.mod"), "module example.com/self\n\ngo 1.27\n")
	write(t, filepath.Join(root, "licenses/extra/README.md"),
		"| File | Component | Licence | Why |\n|---|---|---|---|\n"+
			"| `b.txt` | github.com/x/b | MIT-0 | no classifier |\n"+
			"| `A-OFL.txt` | A font | OFL-1.1 | asset |\n")
	write(t, filepath.Join(root, "licenses/extra/A-OFL.txt"), "text A\n")
	write(t, filepath.Join(root, "licenses/extra/b.txt"), "text B\n")
	write(t, filepath.Join(root, "scripts/dev.sh"), "x --ignore github.com/x/b \\\n")
	write(t, filepath.Join(root, "scripts/release.sh"), "x --ignore github.com/x/b \\\n y --ignore github.com/x/b \\\n")
	write(t, filepath.Join(root, "internal/gui/static/ASSETS.md"), "| `LICENSES/A-OFL.txt` |\n")
	write(t, filepath.Join(root, "internal/gui/static/LICENSES/A-OFL.txt"), "text A\n")
	return root
}

func TestLicensesAssembleDeterministic(t *testing.T) {
	root := extraFixture(t)
	s1, s2 := t.TempDir(), t.TempDir()
	for _, d := range []string{s1, s2} {
		write(t, filepath.Join(d, "github.com/z/zed/LICENSE"), "ZED\r\n")
		write(t, filepath.Join(d, "github.com/a/alpha/LICENSE"), "ALPHA\n")
		write(t, filepath.Join(d, "example.com/self/LICENSE"), "SELF\n")
	}
	write(t, filepath.Join(s2, "golang.org/x/sys/windows/LICENSE"), "WIN\n") // target-only module
	out := filepath.Join(t.TempDir(), "sub", "T.txt")
	first := ""
	for i := 0; i < 2; i++ { // swap the dir order: same bytes
		dirs := []string{s1, s2}
		if i == 1 {
			dirs = []string{s2, s1}
		}
		code, _, errOut := do(append([]string{"-root", root, "licenses", "-o", out}, dirs...)...)
		if code != 0 {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		raw, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		got := string(raw)
		order := []string{"github.com/a/alpha/LICENSE", "github.com/z/zed/LICENSE", "golang.org/x/sys/windows/LICENSE", "A font (OFL-1.1)", "github.com/x/b (MIT-0)"}
		last := -1
		for _, o := range order {
			idx := strings.Index(got, o)
			if idx <= last {
				t.Fatalf("section %q missing or out of order in:\n%s", o, got)
			}
			last = idx
		}
		if strings.Contains(got, "SELF") || strings.Contains(got, "\r") {
			t.Fatalf("own module or CR leaked:\n%s", got)
		}
		if i == 0 {
			first = got
		} else if first != got {
			t.Fatal("not deterministic")
		}
	}
}

func TestLicensesRejects(t *testing.T) {
	save := t.TempDir()
	write(t, filepath.Join(save, "github.com/a/alpha/LICENSE"), "ALPHA\n")
	cases := map[string]func(root string){
		"unlisted extra": func(r string) { write(t, filepath.Join(r, "licenses/extra/c.txt"), "C") },
		"missing listed": func(r string) { os.Remove(filepath.Join(r, "licenses/extra/A-OFL.txt")) },
		"empty listed":   func(r string) { write(t, filepath.Join(r, "licenses/extra/A-OFL.txt"), " \n") },
		"unreviewed ignore": func(r string) {
			both := "--ignore github.com/x/b \\\n--ignore github.com/new/mod \\\n"
			write(t, filepath.Join(r, "scripts/dev.sh"), both)
			write(t, filepath.Join(r, "scripts/release.sh"), both)
		},
		"ignore only in dev.sh": func(r string) {
			write(t, filepath.Join(r, "scripts/dev.sh"), "--ignore github.com/x/b \\\n--ignore github.com/x/c \\\n")
		},
		"ignore only in release.sh": func(r string) {
			write(t, filepath.Join(r, "scripts/release.sh"), "--ignore github.com/x/b \\\n--ignore github.com/x/c \\\n")
		},
		"licence not allowed": func(r string) {
			write(t, filepath.Join(r, "licenses/extra/README.md"),
				"| File | Component | Licence | Why |\n|---|---|---|---|\n"+
					"| `b.txt` | github.com/x/b | GPL-3.0 | bad |\n| `A-OFL.txt` | A font | OFL-1.1 | asset |\n")
		},
		"OFL on a non-font": func(r string) {
			write(t, filepath.Join(r, "licenses/extra/README.md"),
				"| File | Component | Licence | Why |\n|---|---|---|---|\n"+
					"| `b.txt` | github.com/x/b | OFL-1.1 | not a font |\n| `A-OFL.txt` | A font | OFL-1.1 | asset |\n")
		},
		"asset not copied": func(r string) {
			write(t, filepath.Join(r, "internal/gui/static/ASSETS.md"), "| `LICENSES/A-OFL.txt` | `LICENSES/new.txt` |\n")
		},
		"asset differs": func(r string) { write(t, filepath.Join(r, "internal/gui/static/LICENSES/A-OFL.txt"), "changed\n") },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			root := extraFixture(t)
			mutate(root)
			if code, _, _ := do("-root", root, "licenses", save); code != 1 {
				t.Fatalf("exit %d, want 1", code)
			}
		})
	}
	t.Run("conflicting save dirs", func(t *testing.T) {
		other := t.TempDir()
		write(t, filepath.Join(other, "github.com/a/alpha/LICENSE"), "DIFFERENT\n")
		if code, _, _ := do("-root", extraFixture(t), "licenses", save, other); code != 1 {
			t.Fatalf("exit %d, want 1", code)
		}
	})
	t.Run("source file in save dir", func(t *testing.T) {
		other := t.TempDir()
		write(t, filepath.Join(other, "github.com/a/alpha/LICENSE"), "ALPHA\n")
		write(t, filepath.Join(other, "github.com/a/alpha/main.go"), "package alpha\n")
		if code, _, _ := do("-root", extraFixture(t), "licenses", other); code != 1 {
			t.Fatalf("exit %d, want 1", code)
		}
	})
	t.Run("licence file names", func(t *testing.T) {
		for name, ok := range map[string]bool{
			"LICENSE": true, "LICENCE": true, "COPYING": true, "LICENSE.txt": true, "LICENSE.md": true,
			"license": true, "NOTICE": true, "NOTICE.txt": true, "NOTICE-3rd-party": true,
			"LICENSE-3RD-PARTY.md": true, "LICENSE.go": false, "NOTICE.s": false,
			"main.go": false, "README.md": false, "licensing.go": false,
		} {
			if licenceFileRE.MatchString(name) != ok {
				t.Errorf("licenceFileRE(%q) = %v, want %v", name, !ok, ok)
			}
		}
	})
	t.Run("empty save dir and no args", func(t *testing.T) {
		if code, _, _ := do("-root", extraFixture(t), "licenses", t.TempDir()); code != 1 {
			t.Fatalf("exit %d, want 1", code)
		}
		if code, _, _ := do("-root", extraFixture(t), "licenses"); code != 1 {
			t.Fatalf("exit %d, want 1", code)
		}
	})
}

var imageDefaultRE = regexp.MustCompile(`(?m)^([A-Z_]*IMAGE)="\$\{[A-Z_]+:-([^}]+)\}"`)

func imageDefaults(t *testing.T, script string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("../..", "scripts", script))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, m := range imageDefaultRE.FindAllStringSubmatch(string(b), -1) {
		out[m[1]] = m[2]
	}
	return out
}

// release.sh runs the same Go image as dev.sh, and every image it uses is pinned by digest.
func TestReleaseScriptImages(t *testing.T) {
	dev, rel := imageDefaults(t, "dev.sh"), imageDefaults(t, "release.sh")
	if rel["GO_IMAGE"] == "" || rel["GO_IMAGE"] != dev["GO_IMAGE"] {
		t.Fatalf("GO_IMAGE differs: dev.sh %q, release.sh %q", dev["GO_IMAGE"], rel["GO_IMAGE"])
	}
	for _, s := range []map[string]string{dev, rel} {
		for name, ref := range s {
			if !digestRE.MatchString(ref) {
				t.Errorf("%s=%q is not pinned by sha256 digest", name, ref)
			}
		}
	}
	for _, want := range []string{"SHELLCHECK_IMAGE", "ACTIONLINT_IMAGE"} {
		if rel[want] == "" {
			t.Errorf("release.sh lacks %s", want)
		}
	}
}

var digestRE = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)
