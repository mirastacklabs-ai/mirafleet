package gui

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Config configures the GUI server. Everything but Backend and DataDir (or CertFile+KeyFile)
// has a default.
type Config struct {
	Backend Backend
	Version string
	Logf    func(format string, args ...any) // default: discard

	Listen   string   // default DefaultListen; "off" is the caller's business
	DataDir  string   // central's data dir: the self-signed certificate lives in <DataDir>/gui
	CertFile string   // -gui-cert and -gui-key: both or neither
	KeyFile  string   //
	GUIHosts []string // -gui-host names that the Host allowlist and the certificate accept

	// Shown in the Account screen's About section when non-empty.
	HostKeyFingerprint string // central's SSH host-key fingerprint
	SSHAdvertise       string // the SSH advertise address

	Now         func() time.Time       // default time.Now
	Hostname    func() (string, error) // default os.Hostname
	Interfaces  InterfaceLister        // default SystemInterfaces
	HostRefresh time.Duration          // default DefaultHostRefresh
	MaxInFlight int                    // default DefaultMaxInFlight
	Timeout     time.Duration          // default DefaultRequestTimeout
}

// Server is the admin GUI: one TLS listener, the middleware chain and the screens' routes.
type Server struct {
	cfg    Config
	logf   func(string, ...any)
	now    func() time.Time
	assets *Assets
	tmpl   *renderer
	mux    *http.ServeMux
	guard  *hostGuard
	cert   *certInfo
	hsts   bool

	sem         chan struct{}
	reqTimeout  time.Duration
	timeouts    map[string]time.Duration
	handler     http.Handler
	flashSecret []byte

	// PageDefaults, when set, fills the session-dependent Page fields (Authed, CSRF, Nav for error
	// pages) for every page including error pages. It is not called for 421 and 503.
	PageDefaults func(r *http.Request, pg *Page)

	auth   *authState // login limiter, hash semaphore, CSRF checker, setup state (handlers_auth.go)
	nonces nonceSet   // used join-token form nonces (handlers_tokens.go)

	mu     sync.Mutex
	srv    *http.Server
	ln     net.Listener
	stop   chan struct{}
	notice string

	// baseCancel cancels the BaseContext of every request; Shutdown and Close call it so backend
	// calls abort before the caller closes the store. closing keeps the shut-down http.Server
	// so Close can still force it after a timed-out Shutdown.
	baseCancel  context.CancelFunc
	closing     *http.Server
	closingStop context.CancelFunc
}

var guiHostRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)

// CheckHostName validates a -gui-host value: a DNS-style name, not an IP address. The command
// line calls it at parse time.
func CheckHostName(h string) error {
	h = normHost(h)
	if _, err := netip.ParseAddr(h); err == nil {
		return fmt.Errorf("gui: -gui-host %q is an IP address; list names (this machine's own addresses are always accepted)", h)
	}
	if !guiHostRE.MatchString(h) {
		return fmt.Errorf("gui: -gui-host %q is not a valid host name", h)
	}
	return nil
}

// New validates cfg, loads or creates the certificate, parses the templates and builds the
// handler. It does not listen; see Start.
func New(cfg Config) (*Server, error) {
	if cfg.Listen == "" {
		cfg.Listen = DefaultListen
	}
	if _, _, err := net.SplitHostPort(cfg.Listen); err != nil {
		return nil, fmt.Errorf("gui: listen address %q: %w", cfg.Listen, err)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Hostname == nil {
		cfg.Hostname = os.Hostname
	}
	if cfg.Interfaces == nil {
		cfg.Interfaces = SystemInterfaces
	}
	if cfg.HostRefresh <= 0 {
		cfg.HostRefresh = DefaultHostRefresh
	}
	if cfg.MaxInFlight <= 0 {
		cfg.MaxInFlight = DefaultMaxInFlight
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultRequestTimeout
	}
	s := &Server{cfg: cfg, logf: cfg.Logf, now: cfg.Now, sem: make(chan struct{}, cfg.MaxInFlight),
		reqTimeout: cfg.Timeout, timeouts: map[string]time.Duration{}, mux: http.NewServeMux()}
	if s.logf == nil {
		s.logf = func(string, ...any) {}
	}
	s.flashSecret = make([]byte, 32)
	if _, err := rand.Read(s.flashSecret); err != nil {
		return nil, err
	}

	guiHosts := make([]string, 0, len(cfg.GUIHosts))
	for _, h := range cfg.GUIHosts {
		h = normHost(h)
		if h == "" {
			continue
		}
		if err := CheckHostName(h); err != nil {
			return nil, err
		}
		guiHosts = append(guiHosts, h)
	}
	hostname, _ := cfg.Hostname()

	assets, err := NewAssets(StaticFS())
	if err != nil {
		return nil, err
	}
	s.assets = assets
	if s.tmpl, err = newRenderer(assets.TemplateFuncs()); err != nil {
		return nil, err
	}

	if (cfg.CertFile == "") != (cfg.KeyFile == "") {
		return nil, errors.New("gui: -gui-cert and -gui-key must be given together")
	}
	dns := selfSignedNames(hostname, guiHosts)
	if cfg.CertFile != "" {
		if s.cert, err = loadOperatorCert(cfg.CertFile, cfg.KeyFile); err != nil {
			return nil, err
		}
		s.hsts = true
		for _, n := range s.cert.DNSNames {
			if !strings.HasPrefix(n, "*") { // a wildcard SAN is not a Host name to allow
				dns = append(dns, n)
			}
		}
	} else {
		if cfg.DataDir == "" {
			return nil, errors.New("gui: a data directory is required for the self-signed certificate")
		}
		ips, _ := ownIPs(cfg.Interfaces)
		var regen bool
		if s.cert, regen, err = ensureSelfSigned(cfg.DataDir, dns, ips, s.now()); err != nil {
			return nil, err
		}
		if regen {
			s.logf("gui: generated a self-signed certificate (valid until %s)", s.cert.NotAfter.UTC().Format("2006-01-02"))
		}
	}
	s.guard = newHostGuard(dns, cfg.Interfaces, s.logf)

	s.routes()
	s.handler = s.buildHandler()
	return s, nil
}

// Handler is the full handler (middleware chain included), for tests and for Start.
func (s *Server) Handler() http.Handler { return s.handler }

// TLSConfig is the listener's TLS configuration: TLS 1.3 minimum, the GUI certificate.
func (s *Server) TLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{s.cert.cert}}
}

// Assets returns the embedded static files (the template function "asset" comes from it).
func (s *Server) Assets() *Assets { return s.assets }

// Fingerprint is the SHA-256 fingerprint of the served certificate.
func (s *Server) Fingerprint() string { return s.cert.Fingerprint }

// CertExpiry is the served certificate's NotAfter.
func (s *Server) CertExpiry() time.Time { return s.cert.NotAfter }

// SelfSigned reports whether the served certificate was generated by central (no HSTS).
func (s *Server) SelfSigned() bool { return !s.cert.Operator }

// Addr is the bound address after Start ("" before).
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Notice is the all-interfaces notice for the bound address ("" for a loopback bind or before
// Start), shown in the log and on the Account screen.
func (s *Server) Notice() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.notice
}

// Start listens on cfg.Listen, logs the URLs, the certificate fingerprint and the bind notice
// and serves in the background. It never logs the setup token.
func (s *Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv != nil {
		return errors.New("gui: already started")
	}
	// An IPv4 literal binds IPv4 only (so 0.0.0.0 is not silently dual-stack); "[::]" or a bare
	// ":port" is dual-stack.
	network := "tcp"
	if h, _, _ := net.SplitHostPort(s.cfg.Listen); h != "" {
		if a, err := netip.ParseAddr(h); err == nil && a.Is4() {
			network = "tcp4"
		}
	}
	ln, err := net.Listen(network, s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("gui: listen %s: %w", s.cfg.Listen, err)
	}
	s.ln = ln
	baseCtx, baseCancel := context.WithCancel(context.Background())
	s.baseCancel = baseCancel
	s.srv = &http.Server{
		Handler:           s.handler,
		BaseContext:       func(net.Listener) context.Context { return baseCtx },
		TLSConfig:         s.TLSConfig(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          log.New(logWriter{s.logf}, "", 0),
	}
	s.stop = make(chan struct{})

	addr := ln.Addr().String()
	own, _ := ownIPs(s.cfg.Interfaces)
	hostname, _ := s.cfg.Hostname()
	var names []string
	if h := normHost(hostname); h != "" {
		names = append(names, h)
	}
	names = append(names, s.guard.hostNames()...)
	urls := guiURLs(addr, own, dedupe(names))
	s.logf("GUI listening on %s", strings.Join(urls, " "))
	kind := "operator certificate"
	if s.SelfSigned() {
		kind = "self-signed"
	}
	s.logf("GUI certificate (%s) SHA-256 fingerprint: %s, valid until %s", kind, s.cert.Fingerprint, s.cert.NotAfter.UTC().Format("2006-01-02"))
	if s.notice = bindNotice(addr); s.notice != "" {
		s.logf("%s", s.notice)
	}
	if s.SelfSigned() && s.notice != "" {
		s.logf("%s", fingerprintNotice(s.cert.Fingerprint))
	}

	srv, stop := s.srv, s.stop
	go func() {
		if err := srv.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logf("gui: serve: %v", err)
		}
	}()
	// Housekeeping: purge expired GUI sessions now and every PurgeInterval until Shutdown/Close.
	if s.cfg.Backend != nil {
		StartPurge(baseCtx, s.now, func(ctx context.Context, _ time.Time) error {
			_, err := s.cfg.Backend.PurgeGUISessions(ctx)
			return err
		}, func(err error) { s.logf("gui: purge sessions: %v", err) })
	}
	go func() {
		t := time.NewTicker(s.cfg.HostRefresh)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				s.guard.refresh()
			case <-stop:
				return
			}
		}
	}()
	// Certificate expiry: warn at start and then once a day while running.
	s.warnCertExpiry()
	go func() {
		t := time.NewTicker(CertCheckInterval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				s.warnCertExpiry()
			case <-stop:
				return
			}
		}
	}()
	return nil
}

// Certificate expiry warning window and check interval.
const (
	CertWarnWithin    = 30 * 24 * time.Hour
	CertCheckInterval = 24 * time.Hour
)

// warnCertExpiry logs a warning when the served certificate expires within CertWarnWithin (or
// already has). A self-signed certificate is renewed at the next start of central.
func (s *Server) warnCertExpiry() {
	left := s.cert.NotAfter.Sub(s.now())
	switch {
	case left <= 0:
		s.logf("gui: WARNING the GUI certificate expired on %s; browsers will refuse it", s.cert.NotAfter.UTC().Format("2006-01-02"))
	case left < CertWarnWithin:
		s.logf("gui: WARNING the GUI certificate expires on %s (%d days); renew it", s.cert.NotAfter.UTC().Format("2006-01-02"), int(left/(24*time.Hour)))
	}
}

// dedupe returns names without duplicates and without the empty string, keeping the first
// spelling; only names that look like the machine's own (not -gui-host aliases of other
// machines) are offered as URLs, so the full host name is listed first.
func dedupe(names []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range names {
		if n == "" || n == "localhost" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// Shutdown stops accepting, drains requests until ctx ends, and stops the address refresh.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	srv, stop, cancel := s.srv, s.stop, s.baseCancel
	s.closing, s.closingStop = srv, cancel
	s.srv, s.stop, s.baseCancel = nil, nil, nil
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	close(stop)
	err := srv.Shutdown(ctx)
	if err == nil {
		cancel() // all requests are done; release the base context
	}
	return err
}

// Close stops the server at once: it cancels the context of every in-flight request (so backend
// calls abort) and closes all connections. Call it when Shutdown timed out. Safe after Shutdown
// and when not started. It does not wait for handlers to return: a handler still unwinding
// may find the store closed, which only turns its answer into an error.
func (s *Server) Close() error {
	s.mu.Lock()
	srv, stop, cancel := s.srv, s.stop, s.baseCancel
	if srv == nil {
		srv, cancel = s.closing, s.closingStop
	}
	s.srv, s.stop, s.baseCancel, s.closing, s.closingStop = nil, nil, nil, nil, nil
	s.mu.Unlock()
	if stop != nil {
		close(stop)
	}
	if cancel != nil {
		cancel()
	}
	if srv == nil {
		return nil
	}
	return srv.Close()
}
