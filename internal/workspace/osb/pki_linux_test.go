package osb

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// F-19: InitPKIAt writes through the verified fd, so replacing the path (here: renaming the
// directory and putting another in its place) cannot redirect the writes.
func TestInitPKIAtWritesThroughFD(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "p")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	moved := filepath.Join(base, "moved")
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := InitPKIAt(f, dir, 10, time.Now()); err != nil {
		t.Fatal(err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("wrote into the replacement path: %v", ents)
	}
	if ents, _ := os.ReadDir(moved); len(ents) != 5 {
		t.Errorf("verified directory has %d files, want 5", len(ents))
	}
	if _, err := InitPKIAt(f, dir, 10, time.Now()); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("second run: %v", err)
	}

	// A symlink planted at a target name is refused (O_EXCL|O_NOFOLLOW); nothing is written
	// through it and files already written are rolled back.
	d2 := filepath.Join(base, "d2")
	if err := os.Mkdir(d2, 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(base, "secret")
	if err := os.Symlink(secret, filepath.Join(d2, PKIFrontKeyFile)); err != nil {
		t.Fatal(err)
	}
	f2, err := os.Open(d2)
	if err != nil {
		t.Fatal(err)
	}
	defer f2.Close()
	if _, err := InitPKIAt(f2, d2, 10, time.Now()); err == nil {
		t.Error("followed a planted symlink")
	}
	if _, err := os.Lstat(secret); err == nil {
		t.Error("created the symlink target")
	}
	if ents, _ := os.ReadDir(d2); len(ents) != 1 {
		t.Errorf("rollback left files: %v", ents)
	}

	// The path form refuses a symlinked directory.
	ln := filepath.Join(base, "ln")
	if err := os.Symlink(moved, ln); err != nil {
		t.Fatal(err)
	}
	if _, err := InitPKI(ln, 10, time.Now()); err == nil {
		t.Error("InitPKI followed a symlinked dir")
	}
}

// InitPKIAt refuses a directory not owned by the effective uid. Needs root to chown.
func TestInitPKIAtRefusesForeignOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to chown the directory")
	}
	dir := filepath.Join(t.TempDir(), "p")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(dir, 65534, -1); err != nil {
		t.Skipf("chown: %v", err)
	}
	f, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := InitPKIAt(f, dir, 10, time.Now()); err == nil || !strings.Contains(err.Error(), "owned by uid") {
		t.Fatalf("InitPKIAt on foreign-owned dir: %v", err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("wrote into a foreign-owned dir: %v", ents)
	}
}
