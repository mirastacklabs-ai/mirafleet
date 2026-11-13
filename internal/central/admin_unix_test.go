//go:build unix

package central

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestCheckPeerAcceptsSameUID(t *testing.T) {
	dir, err := os.MkdirTemp("", "cf-peer")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	ln, err := net.Listen("unix", filepath.Join(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan struct{}) // releases the dialing goroutine when the test ends
	finished := make(chan struct{})
	defer func() { close(done); <-finished }()
	go func() {
		defer close(finished)
		if c, err := net.Dial("unix", ln.Addr().String()); err == nil {
			defer c.Close()
			<-done
		}
	}()
	c, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	uid, err := peerUID(c.(*net.UnixConn))
	if err != nil || int(uid) != os.Getuid() {
		t.Fatalf("uid=%d err=%v", uid, err)
	}
	if err := checkPeer(c); err != nil {
		t.Fatal(err)
	}
}
