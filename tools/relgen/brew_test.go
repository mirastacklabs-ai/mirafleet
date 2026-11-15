package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var updateN26 = flag.Bool("update-n26", false, "rewrite the brew, serverjson, wiki and harness golden files")

// goldenN26 compares got with testdata/<rel>, or rewrites it with -update-n26.
func goldenN26(t *testing.T, rel, got string) {
	t.Helper()
	p := filepath.Join("testdata", filepath.FromSlash(rel))
	if *updateN26 {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != got {
		t.Errorf("%s differs from golden:\n%s", rel, got)
	}
}

func TestBrewGolden(t *testing.T) {
	c, out, errs := do("-root", t.TempDir(), "brew", "-checksums", "testdata/brew/checksums.txt", "v0.6.0")
	if c != 0 {
		t.Fatalf("code %d %q", c, errs)
	}
	goldenN26(t, "brew/mirafleet.rb.golden", out)
	for _, want := range []string{"bin.install \"mirafleet\"", "mirafleet --version", "on_macos do", "on_linux do", "on_arm do", "on_intel do"} {
		if !strings.Contains(out, want) {
			t.Errorf("formula lacks %q", want)
		}
	}
}

func TestBrewRefusals(t *testing.T) {
	sums := "testdata/brew/checksums.txt"
	if c, _, errs := do("brew", "-checksums", sums, "v0.6.0-rc.1"); c != 1 || !strings.Contains(errs, "pre-release") {
		t.Errorf("rc: %d %q", c, errs)
	}
	if c, _, errs := do("brew", "-checksums", sums, "v0.7.0"); c != 1 || !strings.Contains(errs, "no entry") {
		t.Errorf("missing entry: %d %q", c, errs)
	}
	if c, _, _ := do("brew", "-checksums", sums, "0.6.0"); c != 1 {
		t.Errorf("bare version accepted")
	}
	bad := filepath.Join(t.TempDir(), "sums")
	os.WriteFile(bad, []byte("zz  file\n"), 0o644)
	if c, _, errs := do("brew", "-checksums", bad, "v0.6.0"); c != 1 || !strings.Contains(errs, "line 1") {
		t.Errorf("bad line: %d %q", c, errs)
	}
}

func TestBrewVerifyDist(t *testing.T) {
	dist := t.TempDir()
	var lines []string
	for _, p := range brewPlatforms {
		name := brewArchive("0.6.0", p)
		data := []byte("archive " + name)
		os.WriteFile(filepath.Join(dist, name), data, 0o644)
		h := sha256.Sum256(data)
		lines = append(lines, hex.EncodeToString(h[:])+"  "+name)
	}
	sums := filepath.Join(dist, "checksums.txt")
	os.WriteFile(sums, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	if c, _, errs := do("brew", "-checksums", sums, "-dist", dist, "v0.6.0"); c != 0 {
		t.Fatalf("good dist: %d %q", c, errs)
	}
	os.WriteFile(filepath.Join(dist, brewArchive("0.6.0", brewPlatforms[0])), []byte("tampered"), 0o644)
	if c, _, errs := do("brew", "-checksums", sums, "-dist", dist, "v0.6.0"); c != 1 || !strings.Contains(errs, "does not match") {
		t.Fatalf("tampered: %d %q", c, errs)
	}
}
