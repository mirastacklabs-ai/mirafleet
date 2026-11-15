package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type tent struct {
	name string
	body string
	typ  byte // 0 = regular
}

func mkTarGz(t *testing.T, ents []tent) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range ents {
		h := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Typeflag: tar.TypeReg}
		if e.typ != 0 {
			h.Typeflag, h.Size, h.Linkname = e.typ, 0, "/etc/passwd"
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typ == 0 {
			tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func mkZip(t *testing.T, ents []tent) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range ents {
		w, err := zw.Create(e.name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(e.body))
	}
	zw.Close()
	return buf.Bytes()
}

// npmFixture builds a repo root (real npm templates, fake server.json/LICENSE/NOTICE) and a dist
// directory with six archives plus checksums.txt. mutate may change the entries of one archive.
func npmFixture(t *testing.T, version string, mutate func(p npmPlatform, ents []tent) []tent) (root, dist string) {
	t.Helper()
	root = t.TempDir()
	repo := filepath.Join("..", "..")
	for _, rel := range []string{"npm/mirafleet/package.json", "npm/mirafleet/README.md", "npm/mirafleet/bin/mirafleet.js", "npm/platform/package.json", "npm/platform/README.md", "server.json"} {
		b, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(dst), 0o755)
		os.WriteFile(dst, b, 0o644)
	}
	os.WriteFile(filepath.Join(root, "LICENSE"), []byte("ROOT-LICENSE"), 0o644)
	os.WriteFile(filepath.Join(root, "NOTICE"), []byte("ROOT-NOTICE"), 0o644)
	dist = t.TempDir()
	var sums strings.Builder
	for _, p := range npmPlatforms {
		ents := []tent{
			{name: p.Binary(), body: "BIN-" + p.GoOS + "-" + p.GoArch},
			{name: "LICENSE", body: "LIC"},
			{name: "NOTICE", body: "NOT"},
			{name: "THIRD_PARTY_LICENSES.txt", body: "TP"},
			{name: "README.md", body: "readme"},
		}
		if mutate != nil {
			ents = mutate(p, ents)
		}
		var data []byte
		if p.GoOS == "windows" {
			data = mkZip(t, ents)
		} else {
			data = mkTarGz(t, ents)
		}
		name := p.Archive(version)
		os.WriteFile(filepath.Join(dist, name), data, 0o644)
		s := sha256.Sum256(data)
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(s[:]), name)
	}
	os.WriteFile(filepath.Join(dist, "checksums.txt"), []byte(sums.String()), 0o644)
	return root, dist
}

func TestNPMGenerate(t *testing.T) {
	root, dist := npmFixture(t, "0.6.0", nil)
	out := filepath.Join(t.TempDir(), "npm")
	c, stdout, errs := do("-root", root, "npm", "-dist", dist, "-out", out, "v0.6.0")
	if c != 0 {
		t.Fatalf("code %d: %s", c, errs)
	}
	if n := len(strings.Fields(stdout)); n != 7 {
		t.Fatalf("want 7 dirs, got %d: %q", n, stdout)
	}
	var w npmPackage
	b, _ := os.ReadFile(filepath.Join(out, "mirafleet", "package.json"))
	if err := json.Unmarshal(b, &w); err != nil {
		t.Fatal(err)
	}
	if w.Version != "0.6.0" || w.MCPName != "io.github.mirastacklabs-ai/mirafleet" || w.Bin["mirafleet"] != "bin/mirafleet.js" {
		t.Errorf("wrapper: %+v", w)
	}
	if len(w.OptionalDependencies) != 6 || len(w.Scripts) != 0 || len(w.Dependencies) != 0 {
		t.Errorf("deps: %+v", w)
	}
	names := npmPackageNames()
	if len(names) != 7 {
		t.Fatal(names)
	}
	for _, p := range npmPlatforms {
		if w.OptionalDependencies[p.Name()] != "0.6.0" {
			t.Errorf("optionalDependencies[%s] = %q", p.Name(), w.OptionalDependencies[p.Name()])
		}
		d := filepath.Join(out, "mirafleet-"+p.OS+"-"+p.CPU)
		var pk npmPackage
		pb, _ := os.ReadFile(filepath.Join(d, "package.json"))
		if err := json.Unmarshal(pb, &pk); err != nil {
			t.Fatal(err)
		}
		if pk.Name != p.Name() || pk.Version != "0.6.0" || pk.OS[0] != p.OS || pk.CPU[0] != p.CPU || len(pk.Scripts) != 0 {
			t.Errorf("%s: %+v", p.Name(), pk)
		}
		bin := filepath.Join(d, "bin", p.Binary())
		st, err := os.Stat(bin)
		if err != nil || st.Mode().Perm() != 0o755 {
			t.Errorf("%s binary: %v %v", p.Name(), st, err)
		}
		if got, _ := os.ReadFile(bin); string(got) != "BIN-"+p.GoOS+"-"+p.GoArch {
			t.Errorf("%s binary content %q", p.Name(), got)
		}
		if got, _ := os.ReadFile(filepath.Join(d, "THIRD_PARTY_LICENSES.txt")); string(got) != "TP" {
			t.Errorf("%s third party %q", p.Name(), got)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(out, "mirafleet", "LICENSE")); string(got) != "ROOT-LICENSE" {
		t.Errorf("wrapper LICENSE %q", got)
	}
	if st, err := os.Stat(filepath.Join(out, "mirafleet", "bin", "mirafleet.js")); err != nil || st.Mode().Perm() != 0o755 {
		t.Errorf("launcher mode: %v %v", st, err)
	}
}

func TestNPMTamperedArchiveRefused(t *testing.T) {
	root, dist := npmFixture(t, "0.6.0", nil)
	f := filepath.Join(dist, npmPlatforms[2].Archive("0.6.0"))
	b, _ := os.ReadFile(f)
	b[len(b)/2] ^= 0xff
	os.WriteFile(f, b, 0o644)
	out := filepath.Join(t.TempDir(), "npm")
	c, _, errs := do("-root", root, "npm", "-dist", dist, "-out", out, "0.6.0")
	if c != 1 || !strings.Contains(errs, "checksum mismatch") {
		t.Fatalf("%d %q", c, errs)
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("output written despite failure")
	}
}

func TestNPMUnlistedArchiveRefused(t *testing.T) {
	root, dist := npmFixture(t, "0.6.0", nil)
	b, _ := os.ReadFile(filepath.Join(dist, "checksums.txt"))
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	os.WriteFile(filepath.Join(dist, "checksums.txt"), []byte(strings.Join(lines[1:], "\n")+"\n"), 0o644)
	c, _, errs := do("-root", root, "npm", "-dist", dist, "-out", filepath.Join(t.TempDir(), "o"), "0.6.0")
	if c != 1 || !strings.Contains(errs, "not listed") {
		t.Fatalf("%d %q", c, errs)
	}
}

func TestNPMUnsafeEntriesRefused(t *testing.T) {
	cases := map[string]tent{
		"dotdot":  {name: "../evil", body: "x"},
		"abs":     {name: "/etc/evil", body: "x"},
		"symlink": {name: "link", typ: tar.TypeSymlink},
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			root, dist := npmFixture(t, "0.6.0", func(p npmPlatform, ents []tent) []tent {
				if p.GoOS == "linux" && p.GoArch == "amd64" {
					return append(ents, bad)
				}
				return ents
			})
			c, _, errs := do("-root", root, "npm", "-dist", dist, "-out", filepath.Join(t.TempDir(), "o"), "0.6.0")
			if c != 1 || !strings.Contains(errs, "mirafleet_0.6.0_linux_amd64.tar.gz") {
				t.Fatalf("%d %q", c, errs)
			}
		})
	}
}

func TestNPMMissingBinaryRefused(t *testing.T) {
	root, dist := npmFixture(t, "0.6.0", func(p npmPlatform, ents []tent) []tent {
		if p.GoOS == "windows" && p.GoArch == "arm64" {
			return ents[1:]
		}
		return ents
	})
	c, _, errs := do("-root", root, "npm", "-dist", dist, "-out", filepath.Join(t.TempDir(), "o"), "0.6.0")
	if c != 1 || !strings.Contains(errs, "no mirafleet.exe") {
		t.Fatalf("%d %q", c, errs)
	}
}

func TestNPMRefusals(t *testing.T) {
	root, dist := npmFixture(t, "0.6.0", nil)
	nonEmpty := t.TempDir()
	os.WriteFile(filepath.Join(nonEmpty, "x"), nil, 0o644)
	if c, _, errs := do("-root", root, "npm", "-dist", dist, "-out", nonEmpty, "0.6.0"); c != 1 || !strings.Contains(errs, "not empty") {
		t.Errorf("non-empty out: %d %q", c, errs)
	}
	if c, _, errs := do("-root", root, "npm", "-dist", dist, "-out", filepath.Join(t.TempDir(), "o"), "0.6"); c != 1 || !strings.Contains(errs, "not a version") {
		t.Errorf("bad version: %d %q", c, errs)
	}
	// A template with a lifecycle script is refused.
	p := filepath.Join(root, "npm", "platform", "package.json")
	b, _ := os.ReadFile(p)
	os.WriteFile(p, []byte(strings.Replace(string(b), `"license"`, `"scripts": {"postinstall": "x"}, "license"`, 1)), 0o644)
	if c, _, errs := do("-root", root, "npm", "-dist", dist, "-out", filepath.Join(t.TempDir(), "o"), "0.6.0"); c != 1 || !strings.Contains(errs, "lifecycle") {
		t.Errorf("scripts: %d %q", c, errs)
	}
}

func TestNPMServerJSONMismatch(t *testing.T) {
	root, dist := npmFixture(t, "0.6.0", nil)
	p := filepath.Join(root, "server.json")
	b, _ := os.ReadFile(p)
	os.WriteFile(p, []byte(strings.Replace(string(b), "io.github.mirastacklabs-ai/mirafleet", "io.github.other/x", 1)), 0o644)
	if c, _, errs := do("-root", root, "npm", "-dist", dist, "-out", filepath.Join(t.TempDir(), "o"), "0.6.0"); c != 1 || !strings.Contains(errs, "mcpName") {
		t.Errorf("%d %q", c, errs)
	}
}

func TestNPMFixtureRoundTrip(t *testing.T) {
	root, _ := npmFixture(t, "0.6.0", nil) // gives templates, server.json, LICENSE, NOTICE
	bin := filepath.Join(t.TempDir(), "bin")
	os.WriteFile(bin, []byte("ELF"), 0o755)
	dist := filepath.Join(t.TempDir(), "d")
	if c, _, errs := do("-root", root, "npmfixture", "-bin", bin, "-dist", dist, "0.0.1-rc.1"); c != 0 {
		t.Fatalf("%d %q", c, errs)
	}
	out := filepath.Join(t.TempDir(), "o")
	if c, _, errs := do("-root", root, "npm", "-dist", dist, "-out", out, "0.0.1-rc.1"); c != 0 {
		t.Fatalf("%d %q", c, errs)
	}
	if got, _ := os.ReadFile(filepath.Join(out, "mirafleet-win32-arm64", "bin", "mirafleet.exe")); string(got) != "ELF" {
		t.Errorf("%q", got)
	}
}
