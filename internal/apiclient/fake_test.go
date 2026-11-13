package apiclient

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
)

func newSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// fakeServer is an in-test SSH server on 127.0.0.1:0. handler runs per accepted channel.
type fakeServer struct {
	addr     string
	hostKey  ssh.PublicKey
	authed   atomic.Int32
	channels atomic.Int32
	mu       sync.Mutex
	users    []string
	ln       net.Listener
}

func (f *fakeServer) lastUser() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.users) == 0 {
		return ""
	}
	return f.users[len(f.users)-1]
}

func startFake(t *testing.T, handler func(chanType string, ch ssh.Channel, req *api.Request)) *fakeServer {
	t.Helper()
	hs := newSigner(t)
	f := &fakeServer{hostKey: hs.PublicKey()}
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(c ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			f.authed.Add(1)
			f.mu.Lock()
			f.users = append(f.users, c.User())
			f.mu.Unlock()
			return &ssh.Permissions{}, nil
		},
	}
	cfg.AddHostKey(hs)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.ln = ln
	f.addr = ln.Addr().String()
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer nc.Close()
				sc, chans, reqs, err := ssh.NewServerConn(nc, cfg)
				if err != nil {
					return
				}
				defer sc.Close()
				go ssh.DiscardRequests(reqs)
				for nch := range chans {
					f.channels.Add(1)
					ch, creqs, err := nch.Accept()
					if err != nil {
						continue
					}
					go ssh.DiscardRequests(creqs)
					go func(typ string) {
						defer ch.Close()
						var req *api.Request
						raw, err := api.ReadFrame(ch, api.MaxRequestBytes)
						if err == nil {
							var r api.Request
							if json.Unmarshal(raw, &r) == nil {
								req = &r
							}
						}
						handler(typ, ch, req)
					}(nch.ChannelType())
				}
			}()
		}
	}()
	return f
}

func reply(t *testing.T, ch ssh.Channel, req *api.Request, result any) {
	t.Helper()
	resp, err := api.NewResult(req.ID, result)
	if err != nil {
		t.Error(err)
		return
	}
	if err := api.WriteResponse(ch, resp); err != nil {
		t.Error(err)
	}
}
