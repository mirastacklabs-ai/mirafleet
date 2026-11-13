//go:build linux

package egress

import (
	"os"
	"testing"
)

func TestListenInNetnsOwnNamespace(t *testing.T) {
	// Entering our own namespace needs no privilege and exercises the full switch-and-return path.
	l, err := ListenInNetns("/proc/self/ns/net", "127.0.0.1:0")
	if err != nil {
		t.Skipf("setns unavailable here: %v", err)
	}
	defer l.Close()
	if l.Addr().String() == "" {
		t.Fatal("no address")
	}
}

func TestListenInNetnsBadPath(t *testing.T) {
	if _, err := ListenInNetns("/nonexistent/ns/net", "127.0.0.1:0"); err == nil {
		t.Fatal("expected error")
	}
	f, _ := os.CreateTemp(t.TempDir(), "x")
	defer f.Close()
	if _, err := ListenInNetns(f.Name(), "127.0.0.1:0"); err == nil {
		t.Fatal("expected error for a non-namespace file")
	}
}
