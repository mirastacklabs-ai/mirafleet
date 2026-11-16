package keys

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// WriteAtomic0600 replaces the target, leaves no temp file and (unix) creates it 0600.
func TestWriteAtomic0600(t *testing.T) {
	dir := t.TempDir()
	for _, body := range []string{"one", "two-longer"} {
		if err := WriteAtomic0600(dir, "known_hosts", []byte(body)); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(dir, "known_hosts"))
		if err != nil || string(got) != body {
			t.Fatalf("%q %v", got, err)
		}
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Fatalf("leftovers: %v", ents)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(filepath.Join(dir, "known_hosts")); st.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v", st.Mode())
		}
	}
	if err := WriteAtomic0600(filepath.Join(dir, "missing"), "x", nil); err == nil {
		t.Fatal("missing dir accepted")
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 || strings.HasPrefix(ents[0].Name(), ".tmp") {
		t.Fatalf("leftovers after failure: %v", ents)
	}
}

// WriteNew removes the file it created when the write fails (a read-only handle makes Write fail).
func TestWriteNewRemovesFileOnWriteError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("createExcl is the Windows implementation there")
	}
	old := createExcl
	defer func() { createExcl = old }()
	createExcl = func(path string, mode os.FileMode) (*os.File, error) {
		return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDONLY, mode)
	}
	dir := t.TempDir()
	if err := WriteNew(filepath.Join(dir, "f"), []byte("data"), 0o600); err == nil {
		t.Fatal("write error not reported")
	}
	if err := WriteAtomic0600(dir, "g", []byte("data")); err == nil {
		t.Fatal("write error not reported")
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("leftovers: %v", ents)
	}
}
