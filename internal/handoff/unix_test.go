//go:build unix

package handoff

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A FIFO planted at the handoff name must not hang Read.
func TestReadFIFONotHung(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, k1, "s", time.Now()); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "sessions", k1.name())
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Skip("mkfifo unavailable:", err)
	}
	done := make(chan error, 1)
	go func() {
		_, ok, err := Read(dir, k1)
		if ok {
			t.Error("fifo read as found")
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("fifo accepted")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Read hung on a FIFO")
	}
}

// A sessions dir owned by another uid is refused (needs root to create one).
func TestForeignOwnedSessionsRefused(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs root to chown")
	}
	dir := t.TempDir()
	if err := Write(dir, k1, "s", time.Now()); err != nil {
		t.Fatal(err)
	}
	sd := filepath.Join(dir, "sessions")
	if err := os.Chown(sd, 12345, 12345); err != nil {
		t.Skip("chown unavailable:", err)
	}
	if err := os.Chmod(sd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Write(dir, k1, "s", time.Now()); err == nil {
		t.Fatal("foreign-owned sessions dir accepted")
	}
	if fi, _ := os.Stat(sd); fi.Mode().Perm() != 0o755 {
		t.Fatalf("foreign dir was chmod-ed to %v", fi.Mode().Perm())
	}
}

func TestOwnedByUs(t *testing.T) {
	fi, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !ownedByUs(fi) {
		t.Fatal("own temp dir reported foreign")
	}
}

// A loose-mode sessions dir we own is tightened.
func TestLooseOwnedSessionsTightened(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, k1, "s", time.Now()); err != nil {
		t.Fatal(err)
	}
	sd := filepath.Join(dir, "sessions")
	if err := os.Chmod(sd, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Read(dir, k1); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(sd); fi.Mode().Perm() != 0o700 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
}
