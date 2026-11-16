package main

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func writeGeminiArchive(t *testing.T, path, version string, files []string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := gzip.NewWriter(f)
	tw := tar.NewWriter(zw)
	for _, n := range files {
		body := "x"
		if n == "gemini-extension.json" {
			body = `{"name":"mirafleet","version":"` + version + `"}`
		}
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestGeminiCheck(t *testing.T) {
	dir := t.TempDir()
	for _, n := range geminiArchives {
		writeGeminiArchive(t, filepath.Join(dir, n), "0.6.1", geminiWant)
	}
	if c, _, errs := do("gemini-check", "v0.6.1", dir); c != 0 {
		t.Fatalf("code %d %q", c, errs)
	}
	if c, _, _ := do("gemini-check", "v0.6.2", dir); c != 1 {
		t.Fatal("passed with a version mismatch")
	}
	writeGeminiArchive(t, filepath.Join(dir, "linux.mirafleet.tar.gz"), "0.6.1", geminiWant[:3])
	if c, _, _ := do("gemini-check", "v0.6.1", dir); c != 1 {
		t.Fatal("passed with a missing file")
	}
	os.Remove(filepath.Join(dir, "win32.mirafleet.tar.gz"))
	if c, _, _ := do("gemini-check", "v0.6.1", dir); c != 1 {
		t.Fatal("passed with a missing archive")
	}
}
