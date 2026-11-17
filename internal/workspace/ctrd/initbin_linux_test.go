//go:build linux

package ctrd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	if fi, _ := os.Stat(filepath.Dir(got)); fi.Mode().Perm() != 0o700 {
		t.Errorf(".init mode = %04o, want 0700", fi.Mode().Perm())
	}

	// Identical content: nothing is rewritten (same inode, same mtime).
	before, _ := os.Stat(got)
	time.Sleep(10 * time.Millisecond)
	if _, err := stageInitBinary(self, root); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(got)
	if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Error("an identical binary was copied again")
	}

	// A changed binary replaces it; a stale temp file from a crashed start is removed.
	stale := filepath.Join(root, ".init", "mirafleet.deadbeef.tmp")
	if err := os.WriteFile(stale, []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
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
		t.Errorf(".init holds %d entries, want only mirafleet", len(ents))
	}

	// A wrong mode on .init is refused, not repaired.
	if err := os.Chmod(filepath.Join(root, ".init"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := stageInitBinary(self, root); err == nil || !strings.Contains(err.Error(), "0700") {
		t.Errorf("a 0755 .init was accepted: %v", err)
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

func TestAppArmorPlan(t *testing.T) {
	dir := t.TempDir()
	write := func(name, s string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	yes, no, absent := write("y", "Y\n"), write("n", "N\n"), filepath.Join(dir, "absent")
	for _, c := range []struct {
		name, mode, file string
		supports, load   bool
		errHas           string
	}{
		{"off ignores everything", "off", yes, false, false, ""},
		{"required, supported", "required", yes, true, true, ""},
		{"required, unsupported", "required", yes, false, false, "required but AppArmor is not available"},
		{"auto, supported", "auto", yes, true, true, ""},
		{"auto, enabled kernel, no parser", "auto", yes, false, false, "cannot load its profile (apparmor_parser not found)"},
		{"auto, kernel without AppArmor", "auto", absent, false, false, ""},
		{"auto, AppArmor off in kernel", "auto", no, false, false, ""},
		{"auto, unreadable state", "auto", dir, false, false, "cannot tell"},
		{"unknown mode", "maybe", yes, true, false, "unknown mode"},
	} {
		load, err := appArmorPlan(c.mode, c.supports, c.file)
		if load != c.load || (err == nil) != (c.errHas == "") || (err != nil && !strings.Contains(err.Error(), c.errHas)) {
			t.Errorf("%s: load=%v err=%v, want load=%v err~%q", c.name, load, err, c.load, c.errHas)
		}
	}
}
