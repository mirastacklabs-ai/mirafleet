//go:build linux

package ctrd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestStageInitBinary(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "ws")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	self := filepath.Join(dir, "self")
	if err := os.WriteFile(self, []byte("v1"), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := stageInitBinary(self, root)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, ".init", "mirafleet"); got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
	if err := checkInitBinary(got); err != nil {
		t.Errorf("staged binary is not usable as init: %v", err)
	}
	if fi, _ := os.Stat(got); fi.Mode().Perm() != 0o555 {
		t.Errorf("mode = %04o, want 0555", fi.Mode().Perm())
	}

	// A second start replaces it and leaves no temporary file behind.
	if err := os.WriteFile(self, []byte("v2"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := stageInitBinary(self, root); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(got); !bytes.Equal(b, []byte("v2")) {
		t.Errorf("content = %q, want v2", b)
	}
	ents, _ := os.ReadDir(filepath.Join(root, ".init"))
	if len(ents) != 1 {
		t.Errorf(".init holds %d entries, want 1", len(ents))
	}

	// A symlinked .init is refused.
	root2 := filepath.Join(dir, "ws2")
	if err := os.Mkdir(root2, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dir, filepath.Join(root2, ".init")); err != nil {
		t.Fatal(err)
	}
	if _, err := stageInitBinary(self, root2); err == nil {
		t.Error("a symlinked .init was accepted")
	}
}
