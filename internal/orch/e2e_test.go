package orch

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/apiclient"
	"github.com/mirastacklabs-ai/mirafleet/internal/opconf"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// sshServer is a minimal api@mirafleet server on 127.0.0.1:0 (hello, fleets.list).
type sshServer struct {
	addr    string
	hostKey ssh.PublicKey
	mu      sync.Mutex
	conns   []*ssh.ServerConn
	users   []string
	hellos  atomic.Int32
}

func startSSH(t *testing.T) *sshServer {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	hs, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	s := &sshServer{hostKey: hs.PublicKey()}
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(c ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
		s.mu.Lock()
		s.users = append(s.users, c.User())
		s.mu.Unlock()
		return &ssh.Permissions{}, nil
	}}
	cfg.AddHostKey(hs)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close(); s.killAll() })
	s.addr = ln.Addr().String()
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				sc, chans, reqs, err := ssh.NewServerConn(nc, cfg)
				if err != nil {
					nc.Close()
					return
				}
				s.mu.Lock()
				s.conns = append(s.conns, sc)
				s.mu.Unlock()
				go ssh.DiscardRequests(reqs)
				for nch := range chans {
					if nch.ChannelType() != transport.ChanAPI {
						nch.Reject(ssh.UnknownChannelType, "no")
						continue
					}
					ch, creqs, err := nch.Accept()
					if err != nil {
						continue
					}
					go ssh.DiscardRequests(creqs)
					go func() {
						defer ch.Close()
						raw, err := api.ReadFrame(ch, api.MaxRequestBytes)
						if err != nil {
							return
						}
						var req api.Request
						if json.Unmarshal(raw, &req) != nil {
							return
						}
						var res any
						switch req.Method {
						case api.MethodHello:
							s.hellos.Add(1)
							res = api.HelloResult{Operator: "op", Versions: []int{api.Version}}
						case api.MethodFleetsList:
							res = api.FleetsListResult{Fleets: []api.FleetInfo{{Name: "dev"}}}
						default:
							_ = api.WriteResponse(ch, api.NewError(req.ID, api.Errorf(api.CodeNotFound, "no such thing")))
							return
						}
						resp, _ := api.NewResult(req.ID, res)
						_ = api.WriteResponse(ch, resp)
					}()
				}
			}()
		}
	}()
	return s
}

func (s *sshServer) killAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conns {
		c.Close()
	}
	s.conns = nil
}

func (s *sshServer) numConns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

func pin(t *testing.T, dir, addr string, key ssh.PublicKey) {
	t.Helper()
	line := apiclient.KnownHostsLine(addr, key) + "\n"
	if err := os.WriteFile(opconf.KnownHostsPath(dir), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestEndToEndPinningAndRedial(t *testing.T) {
	srv := startSSH(t)
	dir := setup(t, opconf.Context{Name: "a", Addr: srv.addr})
	pin(t, dir, srv.addr, srv.hostKey)
	// Default dial: real apiclient, real SSH, host key pinned from known_hosts.
	o, err := New(Config{ConfigDir: dir, StateDir: t.TempDir(), ProjectDir: t.TempDir(), Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()

	r, err := o.FleetsList(context.Background(), "a")
	if err != nil || len(r.Fleets) != 1 || r.Fleets[0].Name != "dev" {
		t.Fatalf("got %+v %v", r, err)
	}
	if srv.hellos.Load() != 1 || srv.users[0] != transport.UserOperator {
		t.Fatalf("hellos=%d users=%v", srv.hellos.Load(), srv.users)
	}
	if _, err := o.FleetsList(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if srv.numConns() != 1 {
		t.Fatalf("conns = %d, want one shared connection", srv.numConns())
	}

	// Kill the connection: an idempotent call redials and succeeds.
	srv.killAll()
	if _, err := o.FleetsList(context.Background(), "a"); err != nil {
		t.Fatalf("redial failed: %v", err)
	}
	if srv.numConns() != 1 || srv.hellos.Load() != 2 {
		t.Fatalf("conns=%d hellos=%d", srv.numConns(), srv.hellos.Load())
	}

	// A central error keeps its code.
	_, err = o.WorkspaceList(context.Background(), "a", "")
	var e *Error
	if !errors.As(err, &e) || e.Code != api.CodeNotFound {
		t.Fatalf("err = %v", err)
	}

	// Close ends the connection.
	o.Close()
	srv.mu.Lock()
	cs := append([]*ssh.ServerConn(nil), srv.conns...)
	srv.mu.Unlock()
	for _, c := range cs {
		done := make(chan struct{})
		go func() { c.Wait(); close(done) }() // returns once the client has gone
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("server still sees a connection after Close")
		}
	}
}

func TestEndToEndHostKeyMismatchRefused(t *testing.T) {
	srv := startSSH(t)
	dir := setup(t, opconf.Context{Name: "a", Addr: srv.addr})
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	os2, _ := ssh.NewSignerFromKey(other)
	pin(t, dir, srv.addr, os2.PublicKey()) // pinned a different key
	o, _ := New(Config{ConfigDir: dir, StateDir: t.TempDir(), ProjectDir: t.TempDir(), Logf: t.Logf})
	defer o.Close()
	_, err := o.FleetsList(context.Background(), "a")
	var e *Error
	if !errors.As(err, &e) || e.Code != api.CodeUnavailable || strings.Contains(err.Error(), "SHA256") {
		t.Fatalf("err = %v", err)
	}
	if srv.hellos.Load() != 0 {
		t.Fatal("no request may be sent to an unpinned host")
	}
	// An empty known_hosts is also a refusal: there is no trust on first use.
	if err := os.WriteFile(opconf.KnownHostsPath(dir), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := o.FleetsList(context.Background(), "a"); err == nil || srv.hellos.Load() != 0 {
		t.Fatalf("err = %v", err)
	}
}
