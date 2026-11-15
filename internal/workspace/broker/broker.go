package broker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// DialFunc dials the upstream; tests inject.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Use is the secret-free record of one credentialed request, for the runner's audit log.
type Use struct {
	Credential string
	Host       string
	Status     int
	Time       time.Time
}

// Config configures a Broker. Provider is required.
type Config struct {
	Provider    Provider
	CorporateCA []byte // optional PEM added to the system roots for upstream TLS
	CAValidity  time.Duration
	Dial        DialFunc
	Logger      *slog.Logger
	OnUse       func(Use)

	IdleTimeout time.Duration // default 60s
	DialTimeout time.Duration // default 10s
}

// Broker is one session's proxy.
type Broker struct {
	cfg      Config
	ca       *sessionCA
	password string
	tr       *http.Transport
	tokens   tokenCache
	constr   map[string]bool // permitted credential hosts (no port)

	mu     sync.Mutex
	srv    *http.Server
	conns  map[net.Conn]struct{}
	closed bool
}

// New creates the per-session CA (constrained to Provider.Hosts()) and proxy password.
func New(cfg Config) (*Broker, error) {
	if cfg.Provider == nil {
		return nil, errors.New("broker: nil provider")
	}
	if cfg.CAValidity <= 0 {
		cfg.CAValidity = 12 * time.Hour
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 60 * time.Second
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 10 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Dial == nil {
		d := &net.Dialer{Timeout: cfg.DialTimeout}
		cfg.Dial = d.DialContext
	}
	hosts := cfg.Provider.Hosts()
	ca, err := newCA(hosts, cfg.CAValidity)
	if err != nil {
		return nil, err
	}
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	if len(cfg.CorporateCA) > 0 && !roots.AppendCertsFromPEM(cfg.CorporateCA) {
		return nil, errors.New("broker: corporate CA is not valid PEM")
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	b := &Broker{
		cfg: cfg, ca: ca, password: hex.EncodeToString(raw),
		constr: map[string]bool{}, conns: map[net.Conn]struct{}{},
	}
	for _, h := range hosts {
		if hh, _, err := normHost(h); err == nil {
			b.constr[hh] = true
		}
	}
	b.tr = &http.Transport{
		DialContext:           cfg.Dial,
		TLSClientConfig:       &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{}, // HTTP/1.1 only
		TLSHandshakeTimeout:   cfg.DialTimeout,
		ResponseHeaderTimeout: 5 * time.Minute,
		IdleConnTimeout:       cfg.IdleTimeout,
		Proxy:                 nil,
	}
	return b, nil
}

// CAPEM is the session CA certificate (public part only).
func (b *Broker) CAPEM() []byte { return append([]byte(nil), b.ca.pem...) }

// Bundle returns systemBundle followed by the session CA: the combined bundle the runner
// writes to <sess>/ca.pem for SSL_CERT_FILE and friends.
func (b *Broker) Bundle(systemBundle []byte) []byte {
	out := append([]byte(nil), systemBundle...)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	return append(out, b.ca.pem...)
}

// SystemBundle reads the VM's system CA bundle (SSL_CERT_FILE or a well-known path).
func SystemBundle() ([]byte, error) {
	paths := []string{os.Getenv("SSL_CERT_FILE"), "/etc/ssl/certs/ca-certificates.crt",
		"/etc/pki/tls/certs/ca-bundle.crt", "/etc/ssl/cert.pem"}
	for _, p := range paths {
		if p == "" {
			continue
		}
		if data, err := os.ReadFile(p); err == nil && len(data) > 0 {
			return data, nil
		}
	}
	return nil, errors.New("broker: no system CA bundle found")
}

// Password is the per-session proxy password (user name is ignored).
func (b *Broker) Password() string { return b.password }

// ProxyURL returns the http://user:password@addr URL for HTTPS_PROXY. Secret: do not log.
func (b *Broker) ProxyURL(addr string) string {
	return (&url.URL{Scheme: "http", User: url.UserPassword("mirafleet", b.password), Host: addr}).String()
}

// Serve accepts proxy connections on l until Close.
func (b *Broker) Serve(l net.Listener) error {
	srv := &http.Server{Handler: http.HandlerFunc(b.handle), ReadHeaderTimeout: 10 * time.Second,
		TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){}}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		l.Close()
		return http.ErrServerClosed
	}
	b.srv = srv
	b.mu.Unlock()
	return srv.Serve(l)
}

// Close stops the proxy and drops every open tunnel.
func (b *Broker) Close() error {
	b.mu.Lock()
	b.closed = true
	srv := b.srv
	for c := range b.conns {
		c.Close()
	}
	b.mu.Unlock()
	b.tr.CloseIdleConnections()
	if srv != nil {
		return srv.Close()
	}
	return nil
}

func (b *Broker) track(c net.Conn) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return false
	}
	b.conns[c] = struct{}{}
	return true
}

func (b *Broker) untrack(c net.Conn) {
	b.mu.Lock()
	delete(b.conns, c)
	b.mu.Unlock()
}

func (b *Broker) authOK(r *http.Request) bool {
	h := r.Header.Get("Proxy-Authorization")
	if len(h) < 6 || !strings.EqualFold(h[:6], "basic ") {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(h[6:]))
	if err != nil {
		return false
	}
	_, pw, ok := strings.Cut(string(raw), ":")
	if !ok {
		return false
	}
	a, c := sha256.Sum256([]byte(pw)), sha256.Sum256([]byte(b.password))
	return subtle.ConstantTimeCompare(a[:], c[:]) == 1
}

func (b *Broker) handle(w http.ResponseWriter, r *http.Request) {
	if !b.authOK(r) {
		w.Header().Set("Proxy-Authenticate", `Basic realm="mirafleet"`)
		w.Header().Set("Connection", "close")
		http.Error(w, "proxy authentication required", http.StatusProxyAuthRequired)
		return
	}
	if r.Method == http.MethodConnect {
		b.connect(w, r)
		return
	}
	b.plain(w, r)
}

// plain forwards a non-CONNECT (plain http://) request. Credential hosts are refused: a secret
// is never injected over cleartext.
func (b *Broker) plain(w http.ResponseWriter, r *http.Request) {
	if r.URL.Host == "" || r.URL.Scheme != "http" {
		http.Error(w, "unsupported proxy request", http.StatusBadRequest)
		return
	}
	if b.constr[strings.ToLower(r.URL.Hostname())] {
		http.Error(w, "credential hosts require https", http.StatusForbidden)
		return
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	stripHop(out.Header)
	out.Header.Del("Proxy-Authorization")
	resp, err := b.tr.RoundTrip(out)
	if err != nil {
		http.Error(w, "upstream unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	stripHop(resp.Header)
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func (b *Broker) connect(w http.ResponseWriter, r *http.Request) {
	host, port, err := normHost(r.Host)
	if err != nil {
		http.Error(w, "bad target", http.StatusBadRequest)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	intercept := false
	if b.constr[host] {
		_, intercept = b.cfg.Provider.Lookup(host, port)
	}
	ctx := r.Context()
	var up net.Conn
	if !intercept {
		up, err = b.cfg.Dial(ctx, "tcp", net.JoinHostPort(host, port))
		if err != nil {
			http.Error(w, "upstream unreachable", http.StatusBadGateway)
			return
		}
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		if up != nil {
			up.Close()
		}
		return
	}
	if !b.track(conn) {
		conn.Close()
		if up != nil {
			up.Close()
		}
		return
	}
	defer b.untrack(conn)
	defer conn.Close()
	if _, err := conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
		if up != nil {
			up.Close()
		}
		return
	}
	if up != nil {
		b.tunnel(conn, brw, up)
		return
	}
	b.serveTLS(conn, host, port)
}

// tunnel splices bytes both ways without looking at them.
func (b *Broker) tunnel(c net.Conn, brw *bufio.ReadWriter, up net.Conn) {
	defer up.Close()
	if !b.track(up) {
		return
	}
	defer b.untrack(up)
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(up, io.MultiReader(brw.Reader, c))
		if tc, ok := up.(interface{ CloseWrite() error }); ok {
			tc.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		io.Copy(c, up)
		if tc, ok := c.(interface{ CloseWrite() error }); ok {
			tc.CloseWrite()
		}
		done <- struct{}{}
	}()
	<-done
	<-done
}

func writeErr(c net.Conn, status int, msg string) {
	fmt.Fprintf(c, "HTTP/1.1 %d %s\r\nContent-Type: text/plain\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		status, http.StatusText(status), len(msg), msg)
}

// serveTLS terminates TLS for a credential host and serves HTTP/1.1 requests on it.
func (b *Broker) serveTLS(conn net.Conn, host, port string) {
	leaf, err := b.ca.leaf(host)
	if err != nil {
		return
	}
	tc := tls.Server(conn, &tls.Config{
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"http/1.1"},
		Certificates: []tls.Certificate{*leaf},
	})
	conn.SetDeadline(time.Now().Add(b.cfg.DialTimeout))
	if err := tc.Handshake(); err != nil {
		return
	}
	hostport := net.JoinHostPort(host, port)
	br := bufio.NewReader(tc)
	for {
		tc.SetReadDeadline(time.Now().Add(b.cfg.IdleTimeout))
		tc.SetWriteDeadline(time.Time{})
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		tc.SetReadDeadline(time.Time{})
		keep := b.exchange(tc, req, host, port, hostport)
		if !keep || req.Close {
			return
		}
	}
}

// secretHeaders are response headers that must never reach the agent.
var secretHeaders = []string{"Authorization", "Proxy-Authorization", "Proxy-Authenticate",
	"Private-Token", "X-Auth-Token", "X-Api-Key"}

func stripHop(h http.Header) {
	for _, k := range strings.Split(h.Get("Connection"), ",") {
		if k = strings.TrimSpace(k); k != "" {
			h.Del(k)
		}
	}
	for _, k := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Te", "Trailer", "Upgrade"} {
		h.Del(k)
	}
}

// exchange handles one intercepted request; it reports whether the connection can be reused.
func (b *Broker) exchange(tc net.Conn, req *http.Request, host, port, hostport string) bool {
	log := b.cfg.Logger
	cred, ok := b.cfg.Provider.Lookup(host, port)
	if !ok {
		writeErr(tc, http.StatusForbidden, "credential no longer available")
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	out := req.Clone(ctx)
	out.RequestURI = ""
	out.URL = &url.URL{Scheme: "https", Host: hostport, Path: req.URL.Path, RawPath: req.URL.RawPath, RawQuery: req.URL.RawQuery}
	out.Host = hostport // never the client-chosen Host: the credential goes only to this host
	stripHop(out.Header)
	for _, k := range append([]string{"Proxy-Authorization"}, secretHeaders...) {
		out.Header.Del(k)
	}
	if cred.HeaderName != "" {
		out.Header.Del(cred.HeaderName)
	}
	var resp *http.Response
	var err error
	switch cred.Kind {
	case Basic:
		out.SetBasicAuth(cred.User, cred.Secret)
	case Bearer:
		out.Header.Set("Authorization", "Bearer "+cred.Secret)
	case Header:
		out.Header.Set(cred.HeaderName, cred.Secret)
	}
	out.Header.Del("Accept-Encoding") // the transport then asks for gzip itself and decodes it, so the body scrub sees plain text
	secrets := []string{cred.Secret}
	if cred.Kind == Registry {
		var tok string
		resp, tok, err = b.registryDo(ctx, cred, hostport, out)
		secrets = append(secrets, tok)
	} else {
		resp, err = b.tr.RoundTrip(out)
	}
	if err != nil {
		log.Warn("broker upstream error", "host", hostport, "credential", cred.Name)
		writeErr(tc, http.StatusBadGateway, "upstream request failed")
		return false
	}
	defer resp.Body.Close()
	stripHop(resp.Header)
	scrub(resp, cred, secrets)
	if cred.Kind == Registry {
		resp.Header.Del("Www-Authenticate")
	}
	log.Info("broker request", "host", hostport, "credential", cred.Name, "method", req.Method, "status", resp.StatusCode)
	if b.cfg.OnUse != nil {
		b.cfg.OnUse(Use{Credential: cred.Name, Host: hostport, Status: resp.StatusCode, Time: time.Now()})
	}
	if err := resp.Write(tc); err != nil {
		return false
	}
	return true
}

// scrub drops credential-looking response headers and any header that echoes a secret (the
// credential's own, and for a registry the exchanged bearer token), and replaces an echoed
// secret in the body. Empty secrets are ignored (they would match everything).
func scrub(resp *http.Response, cred Credential, secrets []string) {
	h := resp.Header
	for _, k := range secretHeaders {
		h.Del(k)
	}
	if cred.HeaderName != "" {
		h.Del(cred.HeaderName)
	}
	var live []string
	for _, s := range secrets {
		if s != "" {
			live = append(live, s)
		}
	}
	for k, vs := range h {
	hdr:
		for _, v := range vs {
			for _, s := range live {
				if strings.Contains(v, s) {
					delete(h, k)
					break hdr
				}
			}
		}
	}
	noBody := resp.StatusCode < 200 || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotModified ||
		(resp.Request != nil && resp.Request.Method == http.MethodHead)
	if len(live) > 0 && resp.Body != nil && resp.Body != http.NoBody && !noBody {
		resp.Body = &scrubBody{r: resp.Body, secrets: live}
		// The length may change, so the body is sent chunked (or until close, for HTTP/1.0).
		resp.ContentLength = -1
		resp.TransferEncoding = []string{"chunked"}
		h.Del("Content-Length")
	}
}

const redacted = "[redacted]"

// scrubBody replaces every occurrence of a secret in the stream, including one split across
// reads, by holding back the last len(longest secret)-1 bytes until more data (or EOF) arrives.
type scrubBody struct {
	r       io.ReadCloser
	secrets []string
	out     []byte // ready to hand out
	tail    []byte // held back, may start a secret
	err     error  // sticky read error, returned once out is drained
	tmp     [32 << 10]byte
}

func (s *scrubBody) Read(p []byte) (int, error) {
	for len(s.out) == 0 && s.err == nil {
		n, err := s.r.Read(s.tmp[:])
		data := append(s.tail, s.tmp[:n]...)
		hold := 0
		for _, sec := range s.secrets {
			data = bytes.ReplaceAll(data, []byte(sec), []byte(redacted))
			hold = max(hold, len(sec)-1)
		}
		if err != nil {
			s.err, s.tail, s.out = err, nil, data
		} else {
			hold = min(hold, len(data))
			s.out, s.tail = data[:len(data)-hold:len(data)-hold], append([]byte(nil), data[len(data)-hold:]...)
		}
	}
	if len(s.out) > 0 {
		n := copy(p, s.out)
		s.out = s.out[n:]
		return n, nil
	}
	return 0, s.err
}

func (s *scrubBody) Close() error { return s.r.Close() }
