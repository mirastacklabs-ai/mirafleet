package osb

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// frontEnv is a front over a recording plain-http upstream, with PKI minted by InitPKI.
type frontEnv struct {
	pki    *PKI
	addr   string
	logs   *syncBuf
	hits   chan *http.Request
	client *http.Client // the runner-side hardened client
	front  *Front
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func newFrontEnv(t *testing.T) *frontEnv {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "pki")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p, err := InitPKI(dir, 30, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	e := &frontEnv{pki: p, logs: &syncBuf{}, hits: make(chan *http.Request, 16)}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.hits <- r.Clone(r.Context())
		if strings.HasSuffix(r.URL.Path, "/stream") {
			fl := w.(http.Flusher)
			io.WriteString(w, "data: one\n\n")
			fl.Flush()
			time.Sleep(50 * time.Millisecond)
			io.WriteString(w, "data: two\n\n")
			return
		}
		w.Header().Set("X-Keep", "k")
		io.WriteString(w, "upstream:"+r.Method+":"+r.URL.RequestURI())
	}))
	t.Cleanup(up.Close)
	logf := func(f string, a ...any) { io.WriteString(e.logs, f+"\n") }
	f, err := NewFront(FrontConfig{Listen: "127.0.0.1:0", Upstream: up.Listener.Addr().String(),
		CertFile: p.FrontCert, KeyFile: p.FrontKey, CAFile: p.CAFile, Logf: logf})
	if err != nil {
		t.Fatal(err)
	}
	if e.addr, err = f.Start(); err != nil {
		t.Fatal(err)
	}
	e.front = f
	t.Cleanup(func() { f.Close() })
	e.client, err = NewHTTPClient(Config{BaseURL: "https://" + e.addr, CAFile: p.CAFile,
		PinSHA256: p.FrontPinSHA, CertFile: p.ClientCert, KeyFile: p.ClientKey})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *frontEnv) do(t *testing.T, method, path string, hdr map[string]string) (int, string, http.Header) {
	t.Helper()
	req, _ := http.NewRequest(method, "https://"+e.addr+path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func TestFrontRoutesAndHeaders(t *testing.T) {
	e := newFrontEnv(t)
	const key = "OPEN-SANDBOX-API-KEY-super-secret-value"
	const tok = "execd-token-super-secret-value"
	for _, p := range []string{
		"/v1/sandboxes", "/v1/sandboxes?metadata=a%3Db", "/v1/sandboxes/abc", "/v1/sandboxes/abc/endpoints/44772?use_server_proxy=true",
		"/sandboxes/abc/proxy/44772/command", "/v1/sandboxes/abc/proxy/44772/v1/filesystem/1000/1000/files/info?path=%2Fworkspace%2Fa",
		"/sandboxes/abc/port/44772/command", "/v1/sandboxes/abc/port/44772", "/sandboxes/abc.d-1_2/proxy/44772/",
	} {
		code, body, hdr := e.do(t, "GET", p, map[string]string{"OPEN-SANDBOX-API-KEY": key, "X-EXECD-ACCESS-TOKEN": tok,
			"Connection": "keep-alive, X-Strip", "X-Strip": "s", "X-Forwarded-For": "6.6.6.6", "Forwarded": "for=6.6.6.6", "Te": "trailers"})
		if code != 200 || !strings.HasPrefix(body, "upstream:GET:") {
			t.Errorf("%s: %d %q", p, code, body)
			continue
		}
		up := <-e.hits
		if up.Header.Get("OPEN-SANDBOX-API-KEY") != key || up.Header.Get("X-EXECD-ACCESS-TOKEN") != tok {
			t.Errorf("%s: credentials not forwarded", p)
		}
		for _, h := range []string{"X-Strip", "X-Forwarded-For", "Forwarded", "Te"} {
			if up.Header.Get(h) != "" {
				t.Errorf("%s: header %s reached the upstream", p, h)
			}
		}
		if hdr.Get("X-Keep") != "k" {
			t.Errorf("%s: response hop-by-hop handling: %v", p, hdr)
		}
	}
	if code, body, _ := e.do(t, "POST", "/v1/sandboxes", nil); code != 200 || body != "upstream:POST:/v1/sandboxes" {
		t.Errorf("POST: %d %q", code, body)
	}
	// The query string reaches the upstream intact and the path is re-encoded from the decoded form.
	if _, body, _ := e.do(t, "GET", "/v1/sandboxes?metadata=mirafleet.runner%3Df%2Fr", nil); body != "upstream:GET:/v1/sandboxes?metadata=mirafleet.runner%3Df%2Fr" {
		t.Errorf("query: %q", body)
	}
	if strings.Contains(e.logs.String(), key) || strings.Contains(e.logs.String(), tok) {
		t.Error("a credential was logged")
	}
}

func TestFrontRefusesEverythingElse(t *testing.T) {
	e := newFrontEnv(t)
	for _, p := range []string{
		"/", "/health", "/metrics", "/docs", "/openapi.json", "/v1", "/v1/", "/v1/sandboxesx", "/v2/sandboxes",
		"/sandboxes", "/sandboxes/abc", "/sandboxes/abc/proxy/8080/x", "/sandboxes/abc/proxy/44773", "/sandboxes/abc/proxy/44772x",
		"/sandboxes/abc/other/44772", "/sandboxes//proxy/44772", "/sandboxes/-bad/proxy/44772", "/sandboxes/a/b/proxy/44772",
		"/v1/sandboxes/../admin", "/v1/sandboxes/%2e%2e/admin", "/v1/sandboxes/a/../../x", "/v1/sandboxes//x",
		"/sandboxes/abc/proxy/44772/../../etc", "/sandboxes/abc/proxy/44772/%2e%2e/x", "/v1/sandboxes/a%5Cb",
		"/v1/filesystem/1000/1000/files/info", "/admin", "/etc/passwd",
	} {
		code, _, _ := e.do(t, "GET", p, nil)
		if code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, code)
		}
	}
	if code, _, _ := e.do(t, "TRACE", "/v1/sandboxes", nil); code != http.StatusMethodNotAllowed {
		t.Errorf("TRACE = %d", code)
	}
	if code, _, _ := e.do(t, "CONNECT", "/v1/sandboxes", nil); code == 200 {
		t.Errorf("CONNECT = %d", code)
	}
	select {
	case r := <-e.hits:
		t.Fatalf("a refused request reached the upstream: %s %s", r.Method, r.URL)
	default:
	}
}

func TestFrontStreamsResponses(t *testing.T) {
	e := newFrontEnv(t)
	req, _ := http.NewRequest("POST", "https://"+e.addr+"/sandboxes/abc/proxy/44772/stream", nil)
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 64)
	start := time.Now()
	n, _ := resp.Body.Read(buf)
	if !strings.Contains(string(buf[:n]), "one") || time.Since(start) > 40*time.Millisecond {
		t.Errorf("first event was not flushed promptly: %q after %s", buf[:n], time.Since(start))
	}
	rest, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(rest), "two") {
		t.Errorf("rest: %q", rest)
	}
}

func rawTLS(t *testing.T, addr string, cfg *tls.Config) error {
	t.Helper()
	d := &tls.Dialer{Config: cfg}
	c, err := d.Dial("tcp", addr)
	if err != nil {
		return err
	}
	defer c.Close()
	// TLS 1.3 reports a missing or bad client certificate on the first read.
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write([]byte("GET /v1/sandboxes HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		return err
	}
	_, err = c.Read(make([]byte, 1))
	return err
}

func TestFrontTLSRules(t *testing.T) {
	e := newFrontEnv(t)
	caPEM, _ := os.ReadFile(e.pki.CAFile)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	good, err := tls.LoadX509KeyPair(e.pki.ClientCert, e.pki.ClientKey)
	if err != nil {
		t.Fatal(err)
	}
	base := func() *tls.Config {
		return &tls.Config{RootCAs: pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{good}}
	}
	if err := rawTLS(t, e.addr, base()); err != nil {
		t.Fatalf("valid client refused: %v", err)
	}
	// No client certificate.
	c := base()
	c.Certificates = nil
	if err := rawTLS(t, e.addr, c); err == nil {
		t.Error("connection without a client certificate was served")
	}
	// A client certificate from another CA.
	other := newFrontEnv(t)
	oc, err := tls.LoadX509KeyPair(other.pki.ClientCert, other.pki.ClientKey)
	if err != nil {
		t.Fatal(err)
	}
	c = base()
	c.Certificates = []tls.Certificate{oc}
	if err := rawTLS(t, e.addr, c); err == nil {
		t.Error("client certificate from a foreign CA was served")
	}
	// The server certificate is the front's own (ServerAuth) and the client cert is not usable as one.
	c = base()
	c.MaxVersion = tls.VersionTLS12
	c.MinVersion = tls.VersionTLS12
	if err := rawTLS(t, e.addr, c); err == nil {
		t.Error("TLS 1.2 accepted")
	}
	// The SPKI pin is stable and matches the served certificate.
	cs := tls.Config{RootCAs: pool, ServerName: "localhost", Certificates: []tls.Certificate{good}, MinVersion: tls.VersionTLS13}
	conn, err := tls.Dial("tcp", e.addr, &cs)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if got := SPKIPin(conn.ConnectionState().PeerCertificates[0]); got != e.pki.FrontPinSHA {
		t.Errorf("pin %s != reported %s", got, e.pki.FrontPinSHA)
	}
}

func TestFrontConfigRules(t *testing.T) {
	e := newFrontEnv(t)
	p := e.pki
	ok := FrontConfig{Listen: "127.0.0.1:0", Upstream: "127.0.0.1:8080", CertFile: p.FrontCert, KeyFile: p.FrontKey, CAFile: p.CAFile}
	if _, err := NewFront(ok); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	withUp := ok
	withUp.Upstream = "http://127.0.0.1:8080"
	if _, err := NewFront(withUp); err != nil {
		t.Errorf("http:// upstream refused: %v", err)
	}
	loose := filepath.Join(t.TempDir(), "k.pem")
	b, _ := os.ReadFile(p.FrontKey)
	os.WriteFile(loose, b, 0o644)
	mod := func(f func(*FrontConfig)) FrontConfig { c := ok; f(&c); return c }
	for name, c := range map[string]FrontConfig{
		"public listen":   mod(func(c *FrontConfig) { c.Listen = "0.0.0.0:8443" }),
		"lan listen":      mod(func(c *FrontConfig) { c.Listen = "192.168.1.5:8443" }),
		"name listen":     mod(func(c *FrontConfig) { c.Listen = "localhost:8443" }),
		"no port listen":  mod(func(c *FrontConfig) { c.Listen = "127.0.0.1" }),
		"remote upstream": mod(func(c *FrontConfig) { c.Upstream = "10.0.0.5:8080" }),
		"name upstream":   mod(func(c *FrontConfig) { c.Upstream = "opensandbox:8080" }),
		"https upstream":  mod(func(c *FrontConfig) { c.Upstream = "https://127.0.0.1:8080" }),
		"upstream path":   mod(func(c *FrontConfig) { c.Upstream = "http://127.0.0.1:8080/x" }),
		"zero upstream":   mod(func(c *FrontConfig) { c.Upstream = "127.0.0.1:0" }),
		"no CA":           mod(func(c *FrontConfig) { c.CAFile = "" }),
		"open key mode":   mod(func(c *FrontConfig) { c.KeyFile = loose }),
		"cert as CA":      mod(func(c *FrontConfig) { c.CAFile = p.ClientKey }),
	} {
		if _, err := NewFront(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := NewFront(mod(func(c *FrontConfig) { c.Listen = "[::1]:0" })); err != nil {
		t.Errorf("::1 listen refused: %v", err)
	}
}

func TestInitPKI(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "p")
	os.Mkdir(dir, 0o700)
	p, err := InitPKI(dir, 10, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{p.CAFile, p.FrontCert, p.FrontKey, p.ClientCert, p.ClientKey} {
		fi, err := os.Stat(f)
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v", f, fi, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "ca-key.pem")); err == nil || len(p.FrontPinSHA) != 64 {
		t.Error("a CA key was written or the pin is malformed")
	}
	if _, err := InitPKI(dir, 10, time.Now()); err == nil {
		t.Error("overwrote an existing PKI")
	}
	open := filepath.Join(t.TempDir(), "o")
	os.Mkdir(open, 0o755)
	if _, err := InitPKI(open, 10, time.Now()); err == nil {
		t.Error("used a world-readable directory")
	}
	if _, err := InitPKI(dir, 0, time.Now()); err == nil {
		t.Error("days 0 accepted")
	}
}
