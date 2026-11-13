//go:build unix

package central

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// A socket left behind by a dead central is removed and replaced.
func TestServeAdminReplacesStaleSocket(t *testing.T) {
	e := newEnv(t)
	dir, err := os.MkdirTemp("", "cf-stale")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "admin.sock")

	ln, err := net.Listen("unix", sock)
	must(t, err)
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close() // the file stays, nobody listens: a stale socket
	if _, err := os.Lstat(sock); err != nil {
		t.Fatal("setup: stale socket missing")
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	must(t, e.srv.ServeAdmin(ctx, sock))
	var fl map[string]any
	must(t, AdminCall(sock, "fleets.list", nil, &fl))
}

// A socket with a live listener must not be removed or replaced.
func TestServeAdminKeepsLiveSocket(t *testing.T) {
	e := newEnv(t)
	dir, err := os.MkdirTemp("", "cf-live")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "admin.sock")

	live, err := net.Listen("unix", sock)
	must(t, err)
	t.Cleanup(func() { live.Close() })
	go func() {
		for {
			c, err := live.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := e.srv.ServeAdmin(ctx, sock); err == nil {
		t.Fatal("ServeAdmin took over a live socket")
	}
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("live socket was removed: %v", err)
	}
	c.Close()
}

// A regular file at the socket path is never deleted.
func TestServeAdminKeepsNonSocketFile(t *testing.T) {
	e := newEnv(t)
	dir, err := os.MkdirTemp("", "cf-file")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "admin.sock")
	must(t, os.WriteFile(sock, []byte("x"), 0o600))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := e.srv.ServeAdmin(ctx, sock); err == nil {
		t.Fatal("ServeAdmin replaced a regular file")
	}
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("file removed: %v", err)
	}
}
