//go:build unix

package central

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// D-20: closing an older server never removes the socket of a newer one on the same path.
func TestAdminSocketNotRemovedByOlderServer(t *testing.T) {
	e1 := newEnv(t)
	e2 := newEnv(t)
	dir, err := os.MkdirTemp("", "cf-two")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "admin.sock")

	ctx1, cancel1 := context.WithCancel(context.Background())
	t.Cleanup(cancel1)
	must(t, e1.srv.ServeAdmin(ctx1, sock))
	must(t, os.Remove(sock)) // e1's file goes away (e.g. a successor cleaning a "stale" path)...
	ctx2, cancel2 := context.WithCancel(context.Background())
	t.Cleanup(cancel2)
	must(t, e2.srv.ServeAdmin(ctx2, sock)) // ...and the second server binds the same path

	cancel1()
	e1.srv.Close() // waits for e1's accept loop and its cleanup
	if _, err := os.Lstat(sock); err != nil {
		t.Fatalf("older server removed the newer server's socket: %v", err)
	}
	var fl map[string]any
	must(t, AdminCall(sock, "fleets.list", nil, &fl))

	// The owner still cleans up its own socket.
	cancel2()
	e2.srv.Close()
	if _, err := os.Lstat(sock); !os.IsNotExist(err) {
		t.Fatalf("socket not removed by its owner: %v", err)
	}
	if c, err := net.DialTimeout("unix", sock, time.Second); err == nil {
		c.Close()
		t.Fatal("socket still accepting")
	}
}
