package handoff

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var k1 = Key{PID: 42, Project: "/work/proj"}

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, ok, err := Read(dir, k1); ok || err != nil {
		t.Fatalf("empty read: %v %v", ok, err)
	}
	if err := Write(dir, k1, "cc:abc-123", time.Now()); err != nil {
		t.Fatal(err)
	}
	s, ok, err := Read(dir, k1)
	if err != nil || !ok || s != "cc:abc-123" {
		t.Fatalf("read: %q %v %v", s, ok, err)
	}
	// Unclean project path maps to the same key.
	if s, ok, _ := Read(dir, Key{PID: 42, Project: "/work/proj/"}); !ok || s != "cc:abc-123" {
		t.Fatal("clean path differs")
	}
	if err := Write(dir, k1, "cc:new", time.Now()); err != nil {
		t.Fatal(err)
	}
	if s, _, _ := Read(dir, k1); s != "cc:new" {
		t.Fatalf("overwrite: %q", s)
	}
	if err := Remove(dir, k1); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := Read(dir, k1); ok {
		t.Fatal("still there")
	}
	if err := Remove(dir, k1); err != nil {
		t.Fatalf("remove missing: %v", err)
	}
	fi, err := os.Stat(filepath.Join(dir, "sessions"))
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v %v", fi, err)
	}
}

func TestAtomicNoTempFilesAndMode(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 5; i++ {
		if err := Write(dir, k1, "s", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	ents, _ := os.ReadDir(filepath.Join(dir, "sessions"))
	if len(ents) != 1 || strings.HasPrefix(ents[0].Name(), ".tmp") {
		t.Fatalf("entries: %v", ents)
	}
	fi, _ := ents[0].Info()
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v", fi.Mode())
	}
}

func TestSymlinkDirRefused(t *testing.T) {
	dir, other := t.TempDir(), t.TempDir()
	if err := os.Symlink(other, filepath.Join(dir, "sessions")); err != nil {
		t.Skip(err)
	}
	if err := Write(dir, k1, "s", time.Now()); err == nil {
		t.Fatal("write through symlink")
	}
	if _, _, err := Read(dir, k1); err == nil {
		t.Fatal("read through symlink")
	}
	if err := Remove(dir, k1); err == nil {
		t.Fatal("remove through symlink")
	}
	if ents, _ := os.ReadDir(other); len(ents) != 0 {
		t.Fatalf("symlink target written: %v", ents)
	}
}

func TestBadSessionRefused(t *testing.T) {
	dir := t.TempDir()
	for _, s := range []string{"", "a b", "a/b", strings.Repeat("x", 129), "x\n"} {
		if err := Write(dir, k1, s, time.Now()); err == nil {
			t.Errorf("accepted %q", s)
		}
	}
	// A tampered file with a bad session reads as not found.
	if err := Write(dir, k1, "ok", time.Now()); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "sessions", k1.name())
	b, _ := json.Marshal(file{V: 1, Session: "bad session", Project: k1.clean(), PID: k1.PID})
	os.WriteFile(p, b, 0o600)
	if _, ok, err := Read(dir, k1); ok || err != nil {
		t.Fatalf("bad stored session: %v %v", ok, err)
	}
}

func TestMismatchIsNotFound(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sessions")
	os.MkdirAll(p, 0o700)
	for _, h := range []file{
		{V: 1, Session: "s", Project: k1.clean(), PID: 43},
		{V: 1, Session: "s", Project: "/other", PID: k1.PID},
		{V: 2, Session: "s", Project: k1.clean(), PID: k1.PID},
	} {
		b, _ := json.Marshal(h)
		os.WriteFile(filepath.Join(p, k1.name()), b, 0o600)
		if s, ok, err := Read(dir, k1); ok || err != nil {
			t.Errorf("%+v read as %q %v %v", h, s, ok, err)
		}
	}
}

func TestPruneOld(t *testing.T) {
	dir := t.TempDir()
	old := Key{PID: 1, Project: "/old"}
	if err := Write(dir, old, "s", time.Now()); err != nil {
		t.Fatal(err)
	}
	// Six days later nothing is pruned; eight days later the old file goes.
	if err := Write(dir, k1, "s", time.Now().Add(6*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := Read(dir, old); !ok {
		t.Fatal("pruned too early")
	}
	if err := Write(dir, k1, "s", time.Now().Add(8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := Read(dir, old); ok {
		t.Fatal("old file kept")
	}
	if _, ok, _ := Read(dir, k1); !ok {
		t.Fatal("fresh file pruned")
	}
}

func TestOversizeRefused(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sessions")
	os.MkdirAll(p, 0o700)
	big := `{"v":1,"session":"s","project":"` + strings.Repeat("a", maxFile) + `"}`
	os.WriteFile(filepath.Join(p, k1.name()), []byte(big), 0o600)
	if _, ok, err := Read(dir, k1); ok || err == nil {
		t.Fatalf("oversize accepted: %v %v", ok, err)
	}
}

func TestOnlyDocumentedFields(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, k1, "cc:x", time.Now()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "sessions", k1.name()))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if len(m) != 5 {
		t.Fatalf("fields: %v", m)
	}
	for _, f := range []string{"v", "session", "project", "pid", "written"} {
		if _, ok := m[f]; !ok {
			t.Errorf("missing %s", f)
		}
	}
}
