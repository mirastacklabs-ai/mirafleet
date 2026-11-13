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
	go func() {
		if c, err := net.Dial("unix", ln.Addr().String()); err == nil {
			defer c.Close()
			<-make(chan struct{})
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
