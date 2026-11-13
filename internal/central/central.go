// Package central is the embedded SSH server. Peers authenticate with an Ed25519 key looked up
// in the store (auth.KeyAuth); the SSH user binds the role (runner, operator, enroll). Identity
// travels only in ssh.Permissions.Extensions (auth.Encode/Decode). After the handshake the
// connection is dispatched by principal kind (roles.go).
//
// Live runner sessions live in internal/central/registry, keyed by the runner's store id. The
// registry is built by the caller and shared with the service, which uses it for exec, kick and
// tombstones. Central depends on the store.Store interface and *service.Service only, never on
// a concrete store.
package central

import (
	"errors"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/central/registry"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
)

// Config configures a Server. Every field except Logf is required.
type Config struct {
	Listen   string // host:port; ":0"-style ports are fine
	HostKey  ssh.Signer
	Store    store.Store
	Service  *service.Service
	Registry *registry.Registry
	// Operators tracks live operator connections so a revoke can drop them. The same value must
	// be the service's OperatorConns.
	Operators *OperatorConns
	Logf      func(format string, args ...any)
}

type Server struct {
	cfg Config
	ssh *ssh.ServerConfig

	mu     sync.Mutex
	ln     net.Listener
	conns  map[*ssh.ServerConn]struct{}
	wg     sync.WaitGroup
	closed bool
}

// New validates cfg and returns a Server that has not started listening.
func New(cfg Config) (*Server, error) {
	switch {
	case cfg.Listen == "":
		return nil, errors.New("central: Listen required")
	case cfg.HostKey == nil:
		return nil, errors.New("central: HostKey required")
	case cfg.Store == nil:
		return nil, errors.New("central: Store required")
	case cfg.Service == nil:
		return nil, errors.New("central: Service required")
	case cfg.Registry == nil:
		return nil, errors.New("central: Registry required")
	case cfg.Operators == nil:
		return nil, errors.New("central: Operators required")
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	ka := &auth.KeyAuth{Store: cfg.Store, Logf: cfg.Logf}
	sc := &ssh.ServerConfig{
		PublicKeyCallback:       auth.PublicKeyCallback(ka),
		PublicKeyAuthAlgorithms: []string{ssh.KeyAlgoED25519},
		MaxAuthTries:            3,
	}
	sc.AddHostKey(cfg.HostKey)
	return &Server{cfg: cfg, ssh: sc, conns: map[*ssh.ServerConn]struct{}{}}, nil
}

// Start binds the listener and serves in the background. It returns the bound address.
func (s *Server) Start() (string, error) {
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			s.wg.Add(1)
			go func() { defer s.wg.Done(); s.handle(nc) }()
		}
	}()
	return ln.Addr().String(), nil
}

// Close stops listening, drops all connections and waits for handlers.
func (s *Server) Close() {
	s.mu.Lock()
	s.closed = true
	if s.ln != nil {
		s.ln.Close()
	}
	for c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

const handshakeTimeout = 30 * time.Second

func (s *Server) handle(nc net.Conn) {
	nc.SetDeadline(time.Now().Add(handshakeTimeout))
	sconn, chans, reqs, err := ssh.NewServerConn(nc, s.ssh)
	if err != nil {
		nc.Close()
		s.cfg.Logf("handshake from %s failed: %v", nc.RemoteAddr(), err)
		return
	}
	nc.SetDeadline(time.Time{})
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		sconn.Close()
		return
	}
	s.conns[sconn] = struct{}{}
	s.mu.Unlock()
	defer func() {
		sconn.Close()
		s.mu.Lock()
		delete(s.conns, sconn)
		s.mu.Unlock()
	}()

	// Identity comes only from the verified permissions of this connection.
	p, err := auth.Decode(sconn.Permissions)
	if err != nil {
		s.cfg.Logf("handshake from %s: missing or invalid identity; closing", nc.RemoteAddr())
		return
	}
	s.dispatch(sconn, p, chans, reqs)
}
