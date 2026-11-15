//go:build linux

package sshagent

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh/agent"
)

func TestServeSocket(t *testing.T) {
	k := newSigner(t)
	dir := filepath.Join(t.TempDir(), "sess")
	s, err := Listen(dir, staticProvider{{Signer: k, User: "git"}}, os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	di, _ := os.Stat(dir)
	si, _ := os.Stat(s.Path())
	if di.Mode().Perm() != 0o700 || si.Mode().Perm() != 0o600 {
		t.Fatalf("modes dir=%v sock=%v", di.Mode().Perm(), si.Mode().Perm())
	}
	c, err := net.Dial("unix", s.Path())
	if err != nil {
		t.Fatal(err)
	}
	ac := agent.NewClient(c)
	keys, err := ac.List()
	if err != nil || len(keys) != 1 {
		t.Fatalf("keys=%v err=%v", keys, err)
	}
	if err := ac.RemoveAll(); err == nil {
		t.Fatal("removeall allowed over socket")
	}
	c.Close()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.Path()); !os.IsNotExist(err) {
		t.Fatal("socket not removed")
	}
}

func TestPeerUIDMismatchRefused(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sess")
	s, err := Listen(dir, staticProvider{{Signer: newSigner(t), User: "git"}}, os.Getuid()+1)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := net.Dial("unix", s.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := agent.NewClient(c).List(); err == nil {
		t.Fatal("foreign uid served")
	}
}

func TestListenRejectsLooseDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(dir, staticProvider{}, os.Getuid()); err == nil {
		t.Fatal("loose dir accepted")
	}
}
