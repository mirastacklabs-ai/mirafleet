package handoff

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A symlink planted inside sessions/ that points outside is never followed.
func TestSymlinkEntryOutsideRootNotFollowed(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	if err := Write(dir, k1, "cc:real", time.Now()); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(outside, "evil.json")
	body := fmt.Sprintf(`{"v":1,"session":"cc:evil","project":%q,"pid":%d}`, k1.clean(), k1.PID)
	if err := os.WriteFile(target, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "sessions", k1.name())
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if s, ok, err := Read(dir, k1); ok || err == nil {
		t.Fatalf("read followed an escaping symlink: %q %v %v", s, ok, err)
	}
	if removed, err := RemoveIf(dir, k1, "cc:evil"); removed || err == nil {
		t.Fatalf("RemoveIf followed an escaping symlink: %v %v", removed, err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("outside file touched: %v", err)
	}
}

// Prune never follows or deletes through a symlink, even to an old file outside.
func TestPruneDoesNotFollowSymlinks(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	if err := Write(dir, k1, "s", time.Now()); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(outside, "old.json")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "sessions", "link.json")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if err := Write(dir, Key{PID: 2, Project: "/p"}, "s", time.Now().Add(30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("symlink target removed: %v", err)
	}
}

// The state dir itself is Lstat-checked.
func TestSymlinkStateDirRefused(t *testing.T) {
	base, real := t.TempDir(), t.TempDir()
	link := filepath.Join(base, "state")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if err := Write(link, k1, "s", time.Now()); err == nil {
		t.Fatal("write through a symlinked state dir")
	}
	if _, _, err := Read(link, k1); err == nil {
		t.Fatal("read through a symlinked state dir")
	}
	if ents, _ := os.ReadDir(real); len(ents) != 0 {
		t.Fatalf("symlink target written: %v", ents)
	}
}

// Prune reads at most maxScanned entries, so a huge directory is not slurped whole.
func TestPruneBounded(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, k1, "s", time.Now()); err != nil {
		t.Fatal(err)
	}
	sd := filepath.Join(dir, "sessions")
	oldT := time.Now().Add(-30 * 24 * time.Hour)
	const extra = maxScanned + 100
	for i := 0; i < extra; i++ {
		p := filepath.Join(sd, fmt.Sprintf("old-%04d.json", i))
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, oldT, oldT); err != nil {
			t.Fatal(err)
		}
	}
	if err := Write(dir, k1, "s", time.Now()); err != nil {
		t.Fatal(err)
	}
	left := 0
	ents, _ := os.ReadDir(sd)
	for _, e := range ents {
		if e.Name() != k1.name() {
			left++
		}
	}
	if left < 100 || left >= extra {
		t.Fatalf("expected a bounded prune (100..%d old files left), got %d", extra-1, left)
	}
}
