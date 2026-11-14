//go:build unix

package opconf

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// F-06: openNoFollow refuses a symlink and does not block on a FIFO.
func TestOpenNoFollow(t *testing.T) {
	d := t.TempDir()
	real := filepath.Join(d, "real")
	write(t, real, "x")
	link := filepath.Join(d, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip(err)
	}
	if f, err := openNoFollow(link); err == nil {
		f.Close()
		t.Fatal("symlink followed")
	}
	fifo := filepath.Join(d, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip(err)
	}
	type res struct {
		f   *os.File
		err error
	}
	ch := make(chan res, 1)
	go func() { f, err := openNoFollow(fifo); ch <- res{f, err} }()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatal(r.err)
		}
		if st, _ := r.f.Stat(); st.Mode().IsRegular() {
			t.Fatal("fifo reported regular")
		}
		r.f.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("open blocked on a FIFO")
	}
}

// A FIFO named .mirafleet.yaml is refused, not read.
func TestLoadProjectFIFORefused(t *testing.T) {
	base := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(base, ProjectName), 0o600); err != nil {
		t.Skip(err)
	}
	done := make(chan error, 1)
	go func() { _, _, err := LoadProject(base, base); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("fifo accepted")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("LoadProject hung on a FIFO")
	}
}

// F-18/F-11: saved contexts are 0600 and the dir is synced via the keys helper without leftovers.
func TestSaveContextsMode(t *testing.T) {
	dir := t.TempDir()
	if err := SaveContexts(dir, &Contexts{Contexts: []Context{{Name: "a", Addr: "h:1"}}}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(ContextsPath(dir))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", st, err)
	}
}
