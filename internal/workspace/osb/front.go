package osb

// The built-in node-local TLS front. OpenSandbox's lifecycle server
// and execd speak plain HTTP; the runner refuses anything but https with TLS 1.3, so this small
// stdlib reverse proxy sits next to OpenSandbox (ideally in its network namespace) and is the
// only thing that listens on a TLS port. Rules:
//
//   - TLS 1.3 only; a client certificate is REQUIRED and verified against the node CA (mTLS);
//   - the listen address and the upstream must both be loopback; the upstream is plain http;
//   - only the lifecycle API (/v1/sandboxes...) and the execd data path through the server proxy
//     (/sandboxes/{id}/(proxy|port)/44772/..., optionally /v1-prefixed) are routed; everything
//     else is 404 and never reaches the upstream;
//   - the path forwarded upstream is the validated, decoded one (re-encoded), so the front and
//     the upstream cannot disagree about what was asked;
//   - hop-by-hop and forwarding headers are dropped; API keys and execd tokens pass through
//     untouched and are never logged (the front logs no headers, bodies or query strings).

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// FrontConfig configures the TLS front.
type FrontConfig struct {
	Listen   string // loopback host:port, e.g. 127.0.0.1:8443 (port 0 picks one)
	Upstream string // loopback host:port of OpenSandbox, plain http, e.g. 127.0.0.1:8080
	CertFile string // the front's server certificate (PEM)
	KeyFile  string // its private key (PEM, mode 0600 or 0400, not a symlink)
	CAFile   string // the node CA (PEM): the only trust anchor for client certificates
	Logf     func(format string, args ...any)
}

// Front is a TLS-terminating reverse proxy for OpenSandbox.
type Front struct {
	cfg      FrontConfig
	upstream *url.URL
	tlsCfg   *tls.Config
	handler  http.Handler

	handshakeTimeout time.Duration // TLS handshake and request headers (default 10s)
	bodyTimeout      time.Duration // inactivity limit while the client sends a body (default 60s)

	mu  sync.Mutex
	srv *http.Server
	ln  net.Listener
}

var (
	frontSandboxID = `[A-Za-z0-9][A-Za-z0-9._-]{0,127}`
	frontProxyR    = regexp.MustCompile(`^(?:/v1)?/sandboxes/` + frontSandboxID + `/(?:proxy|port)/` + strconv.Itoa(ExecdPort) + `(?:/.*)?$`)
)

// frontRouteAllowed reports whether the decoded request path may be forwarded.
func frontRouteAllowed(p string) bool {
	if p == "" || p[0] != '/' || strings.ContainsAny(p, "\\") {
		return false
	}
	for i := 0; i < len(p); i++ {
		if p[i] < 0x20 || p[i] == 0x7f { // NUL, CR, LF, tabs and other control bytes
			return false
		}
	}
	// Reject anything that is not already in canonical form: dot segments, doubled slashes.
	// (A trailing slash is canonical for our purposes.)
	if c := path.Clean(p); c != p && c+"/" != p {
		return false
	}
	if p == "/v1/sandboxes" {
		return true
	}
	if rest, ok := strings.CutPrefix(p, "/v1/sandboxes/"); ok {
		// The lifecycle API. OpenSandbox's server proxy (/proxy/{port}, /port/{port}) can reach any
		// port of a sandbox, so under this prefix it is allowed for execd's port only.
		segs := strings.Split(rest, "/")
		for _, s := range segs[1:] {
			if s == "proxy" || s == "port" {
				return frontProxyR.MatchString(p)
			}
		}
		return true
	}
	return frontProxyR.MatchString(p)
}

func loopbackHostPort(what, hp string) (host, port string, err error) {
	host, port, err = net.SplitHostPort(hp)
	if err != nil {
		return "", "", fmt.Errorf("%s %q: want host:port: %w", what, hp, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return "", "", fmt.Errorf("%s %q: the host must be a loopback IP address (127.0.0.1 or ::1)", what, hp)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
		return "", "", fmt.Errorf("%s %q: invalid port", what, hp)
	}
	return host, port, nil
}

// NewFront validates cfg and loads the certificates; it does not listen yet.
func NewFront(cfg FrontConfig) (*Front, error) {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if _, _, err := loopbackHostPort("listen address", cfg.Listen); err != nil {
		return nil, err
	}
	up := cfg.Upstream
	if strings.Contains(up, "://") {
		u, err := url.Parse(up)
		if err != nil || u.Scheme != "http" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("upstream %q: want http://127.0.0.1:PORT (plain http to a loopback address; https is not used on this hop)", cfg.Upstream)
		}
		up = u.Host
	}
	uh, uport, err := loopbackHostPort("upstream", up)
	if err != nil {
		return nil, err
	}
	if uport == "0" {
		return nil, errors.New("upstream: port 0 is not a valid target")
	}
	if cfg.CertFile == "" || cfg.KeyFile == "" || cfg.CAFile == "" {
		return nil, errors.New("tls front: cert, key and CA files are required")
	}
	if err := checkPrivateFile(cfg.KeyFile); err != nil {
		return nil, fmt.Errorf("tls front key: %w", err)
	}
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("tls front: load server certificate: %w", err)
	}
	caPEM, err := os.ReadFile(cfg.CAFile)
	if err != nil {
		return nil, fmt.Errorf("tls front: read CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("tls front: the CA file contains no PEM certificate")
	}
	f := &Front{
		cfg:              cfg,
		handshakeTimeout: 10 * time.Second,
		bodyTimeout:      60 * time.Second,
		upstream:         &url.URL{Scheme: "http", Host: net.JoinHostPort(uh, uport)},
		tlsCfg: &tls.Config{
			MinVersion:   tls.VersionTLS13,
			MaxVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{cert},
			ClientAuth:   tls.RequireAndVerifyClientCert,
			ClientCAs:    pool,
		},
	}
	f.handler = f.newHandler()
	return f, nil
}

// checkPrivateFile requires a regular, non-symlink file that group and others cannot access.
func checkPrivateFile(p string) error {
	fi, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", p)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s: mode %04o is too open (want 0600)", p, fi.Mode().Perm())
	}
	return nil
}

// Handler is the routed reverse proxy (exported for tests; Start serves it behind TLS).
func (f *Front) Handler() http.Handler { return f.handler }

func (f *Front) newHandler() http.Handler {
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// pr.Out already lacks Forwarded and X-Forwarded-*; ReverseProxy removed hop-by-hop
			// headers. Rebuild the URL from the validated decoded path; the query passes through.
			pr.Out.URL.Scheme = f.upstream.Scheme
			pr.Out.URL.Host = f.upstream.Host
			pr.Out.URL.Path = pr.In.URL.Path
			pr.Out.URL.RawPath = ""
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			pr.Out.Host = f.upstream.Host
			pr.Out.Header.Del("Te") // ReverseProxy keeps "TE: trailers"; this hop does not need it
		},
		Transport: &http.Transport{
			Proxy:               nil, // never honour HTTP(S)_PROXY from the environment
			DialContext:         (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext,
			DisableCompression:  true,
			MaxIdleConnsPerHost: 8,
			IdleConnTimeout:     60 * time.Second,
			ForceAttemptHTTP2:   false,
		},
		FlushInterval: -1, // stream SSE (exec output) as it arrives
		ErrorLog:      log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// The error text can carry the upstream address only; never headers. Keep it short.
			f.cfg.Logf("tls-front: upstream error on %s %q: %s", r.Method, r.URL.Path, errClass(err))
			if db, ok := r.Body.(*deadlineBody); ok && db.timedOut.Load() {
				// The client stalled while sending its body. Answer before the connection
				// is closed so it sees 408 instead of a bare EOF.
				w.Header().Set("Connection", "close")
				http.Error(w, "request body timeout", http.StatusRequestTimeout)
				return
			}
			http.Error(w, "bad gateway", http.StatusBadGateway)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !frontRouteAllowed(r.URL.Path) {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodHead:
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.Body != nil && r.Body != http.NoBody {
			r.Body = &deadlineBody{rc: http.NewResponseController(w), src: r.Body, d: f.bodyTimeout, length: r.ContentLength}
		}
		rp.ServeHTTP(w, r)
	})
}

// deadlineBody gives each read of a request body an inactivity deadline and clears it at the end
// of the body, so a slow-sending client times out (the handler then answers 408) while the
// response stream that follows is not subject to a read deadline.
//
// It also absorbs a read after Close once the body is known complete. The upstream transport
// re-reads the body after its last byte to confirm EOF, and closes it as soon as the response
// arrives; when the upstream answers first (an SSE stream), that late read returned
// http.ErrBodyReadAfterClose, the transport treated it as a failed request write and closed the
// upstream connection, cutting the response stream mid-flight.
type deadlineBody struct {
	rc       *http.ResponseController
	src      io.ReadCloser
	d        time.Duration
	length   int64 // declared Content-Length, -1 if unknown
	read     atomic.Int64
	eof      atomic.Bool
	timedOut atomic.Bool
}

func (b *deadlineBody) Read(p []byte) (int, error) {
	if b.eof.Load() {
		return 0, io.EOF
	}
	b.rc.SetReadDeadline(time.Now().Add(b.d)) // best effort: unsupported writers just skip it
	n, err := b.src.Read(p)
	got := b.read.Add(int64(n))
	if errors.Is(err, http.ErrBodyReadAfterClose) && b.length >= 0 && got == b.length {
		err = io.EOF // all bytes were delivered; only the final EOF probe came after Close
	}
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			b.timedOut.Store(true)
		}
		if err == io.EOF {
			b.eof.Store(true)
		}
		b.rc.SetReadDeadline(time.Time{})
	}
	return n, err
}

func (b *deadlineBody) Close() error { return b.src.Close() }

func errClass(err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, context.Canceled):
		return "client went away"
	case errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	default:
		return "unreachable or protocol error"
	}
}

// Start listens on cfg.Listen and serves in the background. It returns the actual address.
func (f *Front) Start() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.srv != nil {
		return "", errors.New("tls front: already started")
	}
	ln, err := net.Listen("tcp", f.cfg.Listen)
	if err != nil {
		return "", fmt.Errorf("tls front: listen: %w", err)
	}
	srv := &http.Server{
		Handler:   f.handler,
		TLSConfig: f.tlsCfg.Clone(),
		// net/http applies the smallest of Read/ReadHeader/WriteTimeout to the TLS handshake
		// too, so a silent or stalled client is dropped after this long (and the request headers
		// must also arrive within it). ReadTimeout is deliberately unset: exec streams are long.
		ReadHeaderTimeout: f.handshakeTimeout,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          log.New(logWriter(f.cfg.Logf), "", 0), // handshake failures: no headers, no secrets
	}
	f.srv, f.ln = srv, ln
	go func() {
		if err := srv.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			f.cfg.Logf("tls-front: serve: %v", err)
		}
	}()
	return ln.Addr().String(), nil
}

// Close stops the listener and closes open connections.
func (f *Front) Close() error {
	f.mu.Lock()
	srv := f.srv
	f.mu.Unlock()
	if srv == nil {
		return nil
	}
	return srv.Close()
}

// Run starts the front and blocks until ctx is done.
func (f *Front) Run(ctx context.Context) error {
	addr, err := f.Start()
	if err != nil {
		return err
	}
	f.cfg.Logf("tls-front: listening on %s (TLS 1.3, client certificate required), upstream %s", addr, f.upstream.Host)
	<-ctx.Done()
	return f.Close()
}

// logWriter adapts Logf to an io.Writer for http.Server.ErrorLog (handshake failures; these
// lines carry no headers or secrets).
type logWriter func(format string, args ...any)

func (w logWriter) Write(p []byte) (int, error) {
	w("tls-front: %s", strings.TrimSpace(string(p)))
	return len(p), nil
}
