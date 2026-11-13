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
	"encoding/base64"
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

	// EnrollRatePerMin limits enroll connections per source IP (token bucket, burst = rate).
	// Zero means DefaultEnrollRatePerMin.
	EnrollRatePerMin int
	// EnrollDeadline is the whole-connection lifetime of an enroll connection. Zero means
	// DefaultEnrollDeadline.
	EnrollDeadline time.Duration
	// TouchInterval is the minimum gap between heartbeat-driven last_seen/info writes of a
	// runner. Zero means DefaultTouchInterval.
	TouchInterval time.Duration
	// Now is the clock for last_seen writes and the rate limiter. Zero means time.Now.
	Now func() time.Time
}

// Defaults for the optional Config fields.
const (
	DefaultEnrollRatePerMin = 10
	DefaultEnrollDeadline   = 30 * time.Second
	DefaultTouchInterval    = 5 * time.Minute
)

// extPubKey carries the base64 SSH wire form of the presented key for enroll connections (the
// enrol service needs the key itself, not only its fingerprint). Set in the auth callback, i.e.
// only for the key whose signature the handshake verified.
const extPubKey = "mirafleet-pubkey"

type Server struct {
	cfg     Config
	ssh     *ssh.ServerConfig
	limiter *ipLimiter

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
	if cfg.EnrollRatePerMin <= 0 {
		cfg.EnrollRatePerMin = DefaultEnrollRatePerMin
	}
	if cfg.EnrollDeadline <= 0 {
		cfg.EnrollDeadline = DefaultEnrollDeadline
	}
	if cfg.TouchInterval <= 0 {
		cfg.TouchInterval = DefaultTouchInterval
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	ka := &auth.KeyAuth{Store: cfg.Store, Logf: cfg.Logf}
	base := auth.PublicKeyCallback(ka)
	pkcb := func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		perms, err := base(meta, key)
		if err == nil && meta.User() == string(auth.KindEnroll) {
			perms.Extensions[extPubKey] = base64.StdEncoding.EncodeToString(key.Marshal())
		}
		return perms, err
	}
	sc := &ssh.ServerConfig{
		PublicKeyCallback:       pkcb,
		PublicKeyAuthAlgorithms: []string{ssh.KeyAlgoED25519},
		MaxAuthTries:            3,
	}
	sc.AddHostKey(cfg.HostKey)
	return &Server{cfg: cfg, ssh: sc, limiter: newIPLimiter(cfg.EnrollRatePerMin, cfg.Now),
		conns: map[*ssh.ServerConn]struct{}{}}, nil
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
	accepted := time.Now()
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
	s.dispatch(sconn, p, accepted, chans, reqs)
}
