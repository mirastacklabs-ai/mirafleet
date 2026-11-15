package gui

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

type testEnv struct {
	t    *testing.T
	s    *Server
	dir  string
	logM sync.Mutex
	logs []string

	ipM sync.Mutex
	ips []netip.Addr
}

func (e *testEnv) log() string {
	e.logM.Lock()
	defer e.logM.Unlock()
	return strings.Join(e.logs, "\n")
}

func (e *testEnv) setIPs(ips ...string) {
	e.ipM.Lock()
	defer e.ipM.Unlock()
	e.ips = nil
	for _, s := range ips {
		e.ips = append(e.ips, netip.MustParseAddr(s))
	}
}

// newEnv builds a server with an injected host name and interface list; mod adjusts the Config.
func newEnv(t *testing.T, mod func(*Config)) *testEnv {
	t.Helper()
	e := &testEnv{t: t, dir: t.TempDir()}
	e.setIPs("192.0.2.10", "2001:db8::5", "fe80::1")
	cfg := Config{
		Version:  "test",
		Listen:   "127.0.0.1:0",
		DataDir:  e.dir,
		Hostname: func() (string, error) { return "central-01.example.org", nil },
		Interfaces: func() ([]netip.Addr, error) {
			e.ipM.Lock()
			defer e.ipM.Unlock()
			return append([]netip.Addr(nil), e.ips...), nil
		},
		Logf: func(f string, a ...any) {
			e.logM.Lock()
			e.logs = append(e.logs, fmt.Sprintf(f, a...))
			e.logM.Unlock()
		},
	}
	if mod != nil {
		mod(&cfg)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	e.s = s
	return e
}

func (e *testEnv) start() string {
	e.t.Helper()
	if err := e.s.Start(); err != nil {
		e.t.Fatalf("Start: %v", err)
	}
	e.t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = e.s.Shutdown(ctx)
	})
	return e.s.Addr()
}

func (e *testEnv) do(method, host, target string, mod func(*http.Request)) *httptest.ResponseRecorder {
	e.t.Helper()
	r := httptest.NewRequest(method, target, nil)
	r.Host = host
	r.RemoteAddr = "192.0.2.77:40000"
	if mod != nil {
		mod(r)
	}
	rec := httptest.NewRecorder()
	e.s.Handler().ServeHTTP(rec, r)
	return rec
}

func TestListenerOnAllInterfacesSelfSigned(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.Listen = "0.0.0.0:0" })
	addr := e.start()
	host, port, _ := net.SplitHostPort(addr)
	if host != "0.0.0.0" {
		t.Fatalf("bound to %s, want the wildcard 0.0.0.0", addr)
	}
	crt, err := os.ReadFile(filepath.Join(e.dir, "gui", "tls.crt"))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(crt) {
		t.Fatal("cannot load the generated certificate")
	}
	dialer := &net.Dialer{Timeout: 3 * time.Second}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "localhost"},
		DialContext: func(ctx context.Context, n, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, n, "127.0.0.1:"+port)
		},
	}}
	resp, err := client.Get("https://localhost:" + port + "/nothing")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.TLS.Version != tls.VersionTLS13 {
		t.Errorf("TLS version %x, want 1.3", resp.TLS.Version)
	}
	if resp.StatusCode != 404 || resp.Header.Get("Content-Security-Policy") == "" {
		t.Errorf("status %d, CSP %q", resp.StatusCode, resp.Header.Get("Content-Security-Policy"))
	}
	if resp.Header.Get("Strict-Transport-Security") != "" {
		t.Error("HSTS must not be sent with the self-signed certificate")
	}
	// A TLS 1.2 client is refused.
	c12, err := tls.DialWithDialer(dialer, "tcp", "127.0.0.1:"+port,
		&tls.Config{RootCAs: pool, ServerName: "localhost", MaxVersion: tls.VersionTLS12})
	if err == nil {
		c12.Close()
		t.Error("TLS 1.2 handshake succeeded")
	}
}

func TestStartupNotice(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.Listen = "0.0.0.0:0" })
	e.start()
	out := e.log()
	_, port, _ := net.SplitHostPort(e.s.Addr())
	for _, want := range []string{
		"GUI is reachable on all interfaces (0.0.0.0:" + port + ")",
		"To restrict it: -gui-listen 127.0.0.1:" + port + " (then use ssh -L), -gui-listen off, or a firewall rule for port " + port + ".",
		"Check that your browser's certificate fingerprint matches " + e.s.Fingerprint() + " before you type the setup token or your password.",
		"SHA-256 fingerprint: " + e.s.Fingerprint(),
		"https://localhost:" + port + "/",
		"https://192.0.2.10:" + port + "/",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "mfs1.") || strings.Contains(strings.ToLower(out), "setup token:") {
		t.Errorf("log shows a token:\n%s", out)
	}
	if e.s.Notice() == "" {
		t.Error("Notice() empty for a wildcard bind")
	}

	lo := newEnv(t, nil)
	lo.start()
	if strings.Contains(lo.log(), "reachable on") || lo.s.Notice() != "" {
		t.Errorf("loopback bind produced a notice:\n%s", lo.log())
	}
}

func TestSelfSignedSANs(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.GUIHosts = []string{"Central.Example.com."} })
	crt, err := os.ReadFile(filepath.Join(e.dir, "gui", "tls.crt"))
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(crt)
	leaf, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	have := strings.Join(leaf.DNSNames, ",")
	for _, n := range []string{"localhost", "central-01", "central-01.example.org", "central.example.com"} {
		if !strings.Contains(","+have+",", ","+n+",") {
			t.Errorf("DNS SANs %q lack %s", have, n)
		}
	}
	ips := map[string]bool{}
	for _, ip := range leaf.IPAddresses {
		ips[ip.String()] = true
	}
	for _, ip := range []string{"127.0.0.1", "::1", "192.0.2.10", "2001:db8::5"} {
		if !ips[ip] {
			t.Errorf("IP SANs %v lack %s", ips, ip)
		}
	}
	if ips["fe80::1"] {
		t.Error("link-local address in the SANs")
	}
	if leaf.NotAfter.Sub(leaf.NotBefore) < 729*24*time.Hour {
		t.Errorf("validity %s, want about 2 years", leaf.NotAfter.Sub(leaf.NotBefore))
	}
	if leaf.PublicKeyAlgorithm != x509.ECDSA {
		t.Errorf("key algorithm %v, want ECDSA", leaf.PublicKeyAlgorithm)
	}
	ds, _ := os.Stat(filepath.Join(e.dir, "gui"))
	ks, _ := os.Stat(filepath.Join(e.dir, "gui", "tls.key"))
	if ds.Mode().Perm() != 0o700 || ks.Mode().Perm() != 0o600 {
		t.Errorf("modes dir %o key %o, want 700 and 600", ds.Mode().Perm(), ks.Mode().Perm())
	}
	if e.s.Fingerprint() != Fingerprint(blk.Bytes) {
		t.Error("Fingerprint() does not match the certificate file")
	}
}

func TestSelfSignedRenew(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	dns := []string{"central", "localhost"}
	ips := []netip.Addr{netip.MustParseAddr("192.0.2.10")}

	first, regen, err := ensureSelfSigned(dir, dns, ips, now)
	if err != nil || !regen {
		t.Fatalf("first: regen=%v err=%v", regen, err)
	}
	if got := first.NotAfter.Sub(now); got < 729*24*time.Hour || got > 731*24*time.Hour {
		t.Errorf("NotAfter is %s after now, want 2 years", got)
	}
	again, regen, err := ensureSelfSigned(dir, dns, ips, now.Add(24*time.Hour))
	if err != nil || regen || again.Fingerprint != first.Fingerprint {
		t.Fatalf("reuse: regen=%v err=%v same=%v", regen, err, again != nil && again.Fingerprint == first.Fingerprint)
	}
	// An IP change alone does not regenerate; the Host allowlist decides access.
	if _, regen, _ = ensureSelfSigned(dir, dns, []netip.Addr{netip.MustParseAddr("192.0.2.99")}, now); regen {
		t.Error("IP change regenerated the certificate")
	}
	// A changed -gui-host set does.
	if _, regen, _ = ensureSelfSigned(dir, append([]string{"extra.example.com"}, dns...), ips, now); !regen {
		t.Error("changed DNS names did not regenerate")
	}
	cur, _, _ := ensureSelfSigned(dir, append([]string{"extra.example.com"}, dns...), ips, now)
	// Under 30 days left: renew. Just over: keep.
	if _, regen, _ = ensureSelfSigned(dir, append([]string{"extra.example.com"}, dns...), ips, cur.NotAfter.Add(-31*24*time.Hour)); regen {
		t.Error("renewed with 31 days left")
	}
	renewed, regen, _ := ensureSelfSigned(dir, append([]string{"extra.example.com"}, dns...), ips, cur.NotAfter.Add(-29*24*time.Hour))
	if !regen || renewed.Fingerprint == cur.Fingerprint {
		t.Error("did not renew with 29 days left")
	}
	// A key file that group or others can read is not trusted: replaced.
	if err := os.Chmod(filepath.Join(dir, "gui", "tls.key"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, regen, _ = ensureSelfSigned(dir, append([]string{"extra.example.com"}, dns...), ips, now); !regen {
		t.Error("loose key permissions did not regenerate")
	}
}

// writeOperatorCert writes a cert and key pair to dir and returns their paths.
func writeOperatorCert(t *testing.T, dir string, keyMode os.FileMode, dns ...string) (certFile, keyFile string) {
	t.Helper()
	crt, key, err := generateSelfSigned(dns, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile = filepath.Join(dir, "op.crt"), filepath.Join(dir, "op.key")
	if err := os.WriteFile(certFile, crt, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, key, keyMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(keyFile, keyMode); err != nil { // defeat the umask
		t.Fatal(err)
	}
	return certFile, keyFile
}

func TestOperatorCertKeyPermsAndHSTS(t *testing.T) {
	d := t.TempDir()
	cf, kf := writeOperatorCert(t, d, 0o600, "gui.corp.example")
	e := newEnv(t, func(c *Config) { c.CertFile, c.KeyFile = cf, kf })
	if e.s.SelfSigned() {
		t.Error("operator certificate reported as self-signed")
	}
	rec := e.do("GET", "localhost", "/nothing", nil)
	if got := rec.Header().Get("Strict-Transport-Security"); !strings.HasPrefix(got, "max-age=") {
		t.Errorf("HSTS = %q with an operator certificate", got)
	}
	// The certificate's DNS SAN is on the Host allowlist.
	if rec := e.do("GET", "gui.corp.example:8443", "/nothing", nil); rec.Code == http.StatusMisdirectedRequest {
		t.Error("operator certificate SAN not accepted as Host")
	}
	if _, err := os.Stat(filepath.Join(e.dir, "gui")); err == nil {
		t.Error("self-signed files created next to an operator certificate")
	}

	self := newEnv(t, nil)
	if got := self.do("GET", "localhost", "/nothing", nil).Header().Get("Strict-Transport-Security"); got != "" {
		t.Errorf("HSTS = %q with the self-signed certificate", got)
	}

	for _, mode := range []os.FileMode{0o640, 0o644, 0o604} {
		cf, kf := writeOperatorCert(t, t.TempDir(), mode, "x.example")
		if _, err := New(Config{DataDir: t.TempDir(), CertFile: cf, KeyFile: kf}); err == nil || !strings.Contains(err.Error(), "chmod 600") {
			t.Errorf("key mode %o accepted: %v", mode, err)
		}
	}
	cf, kf = writeOperatorCert(t, t.TempDir(), 0o400, "x.example")
	if _, err := New(Config{CertFile: cf, KeyFile: kf}); err != nil {
		t.Errorf("key mode 0400 refused: %v", err)
	}
	if _, err := New(Config{DataDir: t.TempDir(), CertFile: cf}); err == nil {
		t.Error("-gui-cert without -gui-key accepted")
	}
}

func TestGUIHostValidation(t *testing.T) {
	for _, h := range []string{"192.0.2.1", "*.example.com", "a b", "-x.example"} {
		if _, err := New(Config{DataDir: t.TempDir(), GUIHosts: []string{h}}); err == nil {
			t.Errorf("-gui-host %q accepted", h)
		}
	}
}

func TestHostAllowlist(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.GUIHosts = []string{"central.example.com"} })
	ok := []string{
		"127.0.0.1", "127.0.0.1:8443", "[::1]", "[::1]:8443", "localhost", "LOCALHOST:9", "localhost.",
		"192.0.2.10", "192.0.2.10:1234", "[2001:db8::5]:8443",
		"central-01", "central-01.example.org", "Central-01.Example.org:8443", "central-01.example.org.",
		"central.example.com", "central.example.com:443",
	}
	bad := []string{
		"", "rebind.evil.example", "rebind.evil.example:8443", "192-168-1-5.nip.io", "0.0.0.0", "0.0.0.0:8443",
		"[::]", "198.51.100.7", "198.51.100.7:8443", "[2001:db8::99]", "2001:db8::5", "::1",
		"[fe80::1]", "localhost.evil.example", "evil.localhost.example", "example.org", "xcentral-01",
		"central-01.example.org.evil.example", "127.0.0.1:99999", "127.0.0.1:", "localhost:abc", "[::1", "127.1",
		"2130706433", "central.example.com:80:80", "[::ffff:192.0.2.10]",
	}
	for _, h := range ok {
		if rec := e.do("GET", h, "/nothing", nil); rec.Code != http.StatusNotFound {
			t.Errorf("Host %q: status %d, want it accepted (404 from the router)", h, rec.Code)
		}
	}
	for _, h := range bad {
		rec := e.do("GET", h, "/nothing", nil)
		if rec.Code != http.StatusMisdirectedRequest {
			t.Errorf("Host %q: status %d, want 421", h, rec.Code)
		}
		if rec.Header().Get("Content-Security-Policy") == "" {
			t.Errorf("Host %q: refusal lacks the security headers", h)
		}
	}
}

func TestHostAllowlistRefresh(t *testing.T) {
	e := newEnv(t, nil)
	if rec := e.do("GET", "203.0.113.9:8443", "/x", nil); rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("address not on an interface yet: %d", rec.Code)
	}
	e.setIPs("192.0.2.10", "203.0.113.9", "0.0.0.0", "fe80::2", "224.0.0.1")
	// Not yet: the set is read at start and refreshed on a timer.
	if rec := e.do("GET", "203.0.113.9", "/x", nil); rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("new address accepted before the refresh: %d", rec.Code)
	}
	e.s.guard.refresh()
	if rec := e.do("GET", "203.0.113.9", "/x", nil); rec.Code != http.StatusNotFound {
		t.Errorf("new address after refresh: %d, want accepted", rec.Code)
	}
	for _, h := range []string{"0.0.0.0", "[fe80::2]", "224.0.0.1"} {
		if rec := e.do("GET", h, "/x", nil); rec.Code != http.StatusMisdirectedRequest {
			t.Errorf("Host %q accepted after refresh: %d", h, rec.Code)
		}
	}
	// An address that goes away is dropped.
	e.setIPs("192.0.2.10")
	e.s.guard.refresh()
	if rec := e.do("GET", "203.0.113.9", "/x", nil); rec.Code != http.StatusMisdirectedRequest {
		t.Errorf("removed address still accepted: %d", rec.Code)
	}

	// The timer drives the refresh in a started server.
	f := newEnv(t, func(c *Config) { c.HostRefresh = 5 * time.Millisecond })
	f.start()
	f.setIPs("192.0.2.10", "203.0.113.50")
	deadline := time.Now().Add(5 * time.Second)
	for f.do("GET", "203.0.113.50", "/x", nil).Code == http.StatusMisdirectedRequest {
		if time.Now().After(deadline) {
			t.Fatal("timer never picked up the new address")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

var wantHeaders = []string{"Content-Security-Policy", "X-Content-Type-Options", "Referrer-Policy", "X-Frame-Options",
	"Cross-Origin-Opener-Policy", "Cross-Origin-Resource-Policy", "Permissions-Policy", "Cache-Control", "X-Request-Id"}

func TestHeadersOnEveryResponse(t *testing.T) {
	e := newEnv(t, nil)
	e.s.mux.HandleFunc("GET /ok", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	app, err := e.s.Assets().URL("app.css")
	if err != nil {
		t.Fatal(err)
	}
	post := func(r *http.Request) {
		r.Header.Set("Content-Type", "text/plain")
		r.Body = http.NoBody
		r.ContentLength = 3
	}
	cases := []struct {
		name, host, method, path string
		mod                      func(*http.Request)
		code                     int
	}{
		{"ok", "localhost", "GET", "/ok", nil, 200},
		{"404", "localhost", "GET", "/missing", nil, 404},
		{"405", "localhost", "POST", "/ok", nil, 405},
		{"static", "localhost", "GET", app, nil, 200},
		{"static 404", "localhost", "GET", "/static/deadbeef/app.css", nil, 404},
		{"421", "evil.example", "GET", "/ok", nil, 421},
		{"415", "localhost", "POST", "/ok", post, 415},
	}
	for _, c := range cases {
		rec := e.do(c.method, c.host, c.path, c.mod)
		if rec.Code != c.code {
			t.Errorf("%s: status %d, want %d", c.name, rec.Code, c.code)
		}
		for _, h := range wantHeaders {
			if rec.Header().Get(h) == "" {
				t.Errorf("%s: missing header %s", c.name, h)
			}
		}
		if got := rec.Header().Get("Content-Security-Policy"); !strings.Contains(got, "default-src 'none'") ||
			strings.Contains(got, "unsafe-") {
			t.Errorf("%s: CSP %q", c.name, got)
		}
	}
	if rec := e.do("POST", "localhost", "/ok", nil); rec.Header().Get("Allow") == "" {
		t.Error("405 lacks Allow")
	}
}

func TestStaticCaching(t *testing.T) {
	e := newEnv(t, nil)
	for _, name := range []string{"app.css", "app.js", "vendor/htmx.min.js", "fonts/Oswald-latin-var.woff2", "favicon.svg"} {
		u, err := e.s.Assets().URL(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.HasPrefix(u, "/static/"+e.s.Assets().Hash()+"/") {
			t.Errorf("%s: URL %s is not content-hashed", name, u)
		}
		rec := e.do("GET", "localhost", u, nil)
		if rec.Code != 200 || rec.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
			t.Errorf("%s: %d, Cache-Control %q (middleware must keep immutable on 200)", name, rec.Code, rec.Header().Get("Cache-Control"))
		}
	}
	for _, p := range []string{"/static/deadbeefdeadbeef/app.css", "/static/" + e.s.Assets().Hash() + "/nope.css", "/static/", "/static/x"} {
		rec := e.do("GET", "localhost", p, nil)
		if rec.Code != 404 || rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: %d, Cache-Control %q, want 404 no-store (G-01)", p, rec.Code, rec.Header().Get("Cache-Control"))
		}
	}
	if rec := e.do("GET", "localhost", "/missing", nil); rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("HTML 404 Cache-Control %q", rec.Header().Get("Cache-Control"))
	}
	// The layout references the favicon and stylesheet by hashed URL.
	rec := e.do("GET", "localhost", "/missing", nil)
	if !strings.Contains(rec.Body.String(), `href="/static/`+e.s.Assets().Hash()+`/favicon.svg"`) {
		t.Errorf("error page lacks the hashed favicon link:\n%s", rec.Body.String())
	}
}

func TestBodyCapContentTypeAndPRG(t *testing.T) {
	e := newEnv(t, nil)
	e.s.mux.HandleFunc("POST /form", func(w http.ResponseWriter, r *http.Request) {
		if _, code := ParseForm(w, r); code != 0 {
			e.s.ErrorPage(w, r, code)
			return
		}
		e.s.Redirect(w, r, "/done", "", "")
	})
	form := func(body string, length int64) func(*http.Request) {
		return func(r *http.Request) {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.Body = http.NoBody
			if body != "" {
				r.Body = nopCloser{strings.NewReader(body)}
			}
			r.ContentLength = length
		}
	}
	big := "a=" + strings.Repeat("x", MaxFormBytes+10)
	if rec := e.do("POST", "localhost", "/form", form(big, int64(len(big)))); rec.Code != 413 {
		t.Errorf("declared oversize body: %d, want 413", rec.Code)
	}
	if rec := e.do("POST", "localhost", "/form", form(big, -1)); rec.Code != 413 {
		t.Errorf("chunked oversize body: %d, want 413", rec.Code)
	}
	if rec := e.do("POST", "localhost", "/form", form("a=b", 3)); rec.Code != 303 || rec.Header().Get("Location") != "/done" {
		t.Errorf("small form: %d %q, want 303 /done", rec.Code, rec.Header().Get("Location"))
	}
	ct := func(v string) func(*http.Request) {
		return func(r *http.Request) {
			r.Header.Set("Content-Type", v)
			r.Body = nopCloser{strings.NewReader("{}")}
			r.ContentLength = 2
		}
	}
	for _, v := range []string{"application/json", "multipart/form-data; boundary=x", "text/plain", ""} {
		if rec := e.do("POST", "localhost", "/form", ct(v)); rec.Code != 415 {
			t.Errorf("Content-Type %q: %d, want 415", v, rec.Code)
		}
	}
	// A bodyless POST (a plain button form) is fine.
	if rec := e.do("POST", "localhost", "/form", func(r *http.Request) { r.Header.Set("Content-Type", "application/x-www-form-urlencoded") }); rec.Code != 413 && rec.Code != 303 {
		t.Errorf("empty urlencoded POST: %d", rec.Code)
	}
	// htmx POSTs get HX-Redirect with 200 instead of a 303.
	rec := e.do("POST", "localhost", "/form", func(r *http.Request) { form("a=b", 3)(r); r.Header.Set("HX-Request", "true") })
	if rec.Code != 200 || rec.Header().Get("HX-Redirect") != "/done" || rec.Header().Get("Location") != "" {
		t.Errorf("htmx redirect: %d HX-Redirect %q Location %q", rec.Code, rec.Header().Get("HX-Redirect"), rec.Header().Get("Location"))
	}
}

type nopCloser struct{ *strings.Reader }

func (nopCloser) Close() error { return nil }

func TestRedirectOnlyLocal(t *testing.T) {
	e := newEnv(t, nil)
	for _, target := range []string{"//evil.example", "https://evil.example/", "/\\evil.example", "evil", "/a\r\nSet-Cookie: x=1",
		"/\x09/evil.example", "/\n/evil", "/\r/evil", "//evil", "/\\evil", "https://evil", "javascript:alert(1)", "/a\x7f", "/a\x00"} {
		for _, hx := range []bool{false, true} {
			rec := httptest.NewRecorder()
			r := httptest.NewRequest("POST", "/x", nil)
			loc := func() string { return rec.Header().Get("Location") }
			if hx {
				r.Header.Set("HX-Request", "true")
				loc = func() string { return rec.Header().Get("HX-Redirect") }
			}
			e.s.Redirect(rec, r, target, "", "")
			if got := loc(); got != "/" {
				t.Errorf("Redirect(%q, htmx=%v) -> %q, want /", target, hx, got)
			}
		}
	}
	// A path that merely looks odd but stays on this site is kept.
	for _, target := range []string{"/ /evil", "/%09/evil", "/fleets/a?x=1#y"} {
		rec := httptest.NewRecorder()
		e.s.Redirect(rec, httptest.NewRequest("POST", "/x", nil), target, "", "")
		if loc := rec.Header().Get("Location"); loc != target && loc != strings.ReplaceAll(target, " ", "%20") {
			t.Errorf("Redirect(%q) -> %q, want it kept", target, loc)
		}
	}
}

func TestPanicResetsHeadersAndAborts(t *testing.T) {
	e := newEnv(t, nil)
	e.s.mux.HandleFunc("GET /staged", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "stale", Value: "1"})
		w.Header().Set("Location", "https://evil.example/")
		w.Header().Set("Cache-Control", "public, max-age=999")
		panic("late")
	})
	rec := e.do("GET", "localhost", "/staged", nil)
	h := rec.Header()
	if rec.Code != 500 || h.Get("Set-Cookie") != "" || h.Get("Location") != "" || h.Get("Cache-Control") != "no-store" ||
		h.Get("Content-Security-Policy") == "" || h.Get("X-Request-Id") == "" || !strings.Contains(rec.Body.String(), h.Get("X-Request-Id")) {
		t.Errorf("staged headers survived or were lost: %d %v", rec.Code, h)
	}
	e.s.mux.HandleFunc("GET /torn", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("partial"))
		panic("after write")
	})
	defer func() {
		if rec := recover(); rec != http.ErrAbortHandler {
			t.Errorf("recover = %v, want http.ErrAbortHandler", rec)
		}
	}()
	e.do("GET", "localhost", "/torn", nil)
	t.Error("no abort after a panic past WriteHeader")
}

func TestEmptyHandlerGetsNoStore(t *testing.T) {
	e := newEnv(t, nil)
	e.s.mux.HandleFunc("POST /empty", func(w http.ResponseWriter, r *http.Request) {})
	rec := e.do("POST", "localhost", "/empty", nil)
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("empty response: %d %v", rec.Code, rec.Header())
	}
}

func TestSelfSignedRefusesLooseDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "gui"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "gui"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, _, err := ensureSelfSigned(dir, []string{"localhost"}, nil, time.Now())
	if err == nil || !strings.Contains(err.Error(), "0700") {
		t.Fatalf("loose gui dir accepted: %v", err)
	}
}

func TestGUIURLsIPv4OnlyForIPv4Bind(t *testing.T) {
	own := []netip.Addr{netip.MustParseAddr("192.0.2.10"), netip.MustParseAddr("2001:db8::5")}
	v4 := strings.Join(guiURLs("0.0.0.0:8443", own, nil), " ")
	if !strings.Contains(v4, "192.0.2.10") || strings.Contains(v4, "2001:db8") {
		t.Errorf("ipv4 bind: %s", v4)
	}
	v6 := strings.Join(guiURLs("[::]:8443", own, nil), " ")
	if !strings.Contains(v6, "192.0.2.10") || !strings.Contains(v6, "2001:db8") {
		t.Errorf("ipv6 bind: %s", v6)
	}
}

func TestErrorPageSkipsDefaultsFor421And503(t *testing.T) {
	e := newEnv(t, nil)
	calls := 0
	e.s.PageDefaults = func(r *http.Request, pg *Page) { calls++ }
	if rec := e.do("GET", "evil.example", "/", nil); rec.Code != 421 || calls != 0 {
		t.Errorf("421: code %d defaults calls %d", rec.Code, calls)
	}
	if rec := e.do("GET", "localhost", "/missing", nil); rec.Code != 404 || calls != 1 {
		t.Errorf("404: code %d defaults calls %d", rec.Code, calls)
	}
	rec := httptest.NewRecorder()
	e.s.renderError(rec, httptest.NewRequest("GET", "/", nil), 503)
	if calls != 1 {
		t.Errorf("503 ran PageDefaults")
	}
}

func TestFlashSubjectCutOnRuneBoundary(t *testing.T) {
	e := newEnv(t, nil)
	RegisterFlash("test.rune", "ok", "Saved %s")
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/x", nil)
	e.s.setFlash(rec, r, "test.rune", strings.Repeat("a", flashMaxSubject-1)+"éé")
	c := rec.Result().Cookies()[0]
	msg, ok := OpenFlash(e.s.flashSecret, c.Value, e.s.now())
	if !ok {
		t.Fatal("flash does not verify with the process key")
	}
	_, subj, _ := strings.Cut(msg, "\x00")
	if !utf8.ValidString(subj) || len(subj) > flashMaxSubject {
		t.Errorf("subject cut badly: %d bytes valid=%v", len(subj), utf8.ValidString(subj))
	}
}

func TestFlashRoundTrip(t *testing.T) {
	e := newEnv(t, nil)
	RegisterFlash("test.saved", "ok", "Saved %s")
	e.s.mux.HandleFunc("POST /set", func(w http.ResponseWriter, r *http.Request) {
		e.s.Redirect(w, r, "/show", "test.saved", "<b>fleet-1</b>")
	})
	e.s.mux.HandleFunc("GET /show", func(w http.ResponseWriter, r *http.Request) {
		pg := e.s.Page(w, r, "Show", "")
		pg.Data = ErrorData{Status: 200, Heading: "Shown"}
		e.s.Render(w, r, 200, "errors", "errors/panel", pg)
	})
	rec := e.do("POST", "localhost", "/set", nil)
	if rec.Code != 303 {
		t.Fatalf("set: %d", rec.Code)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != FlashCookieName || !cookies[0].Secure || !cookies[0].HttpOnly ||
		cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("flash cookie: %+v", cookies)
	}
	get := func(c *http.Cookie, hx bool) *httptest.ResponseRecorder {
		return e.do("GET", "localhost", "/show", func(r *http.Request) {
			if c != nil {
				r.AddCookie(c)
			}
			if hx {
				r.Header.Set("HX-Request", "true")
			}
		})
	}
	got := get(cookies[0], false)
	body := got.Body.String()
	if !strings.Contains(body, "Saved &lt;b&gt;fleet-1&lt;/b&gt;") || !strings.Contains(body, `class="flash ok"`) {
		t.Errorf("flash not shown, escaped, as ok:\n%s", body)
	}
	if cs := got.Result().Cookies(); len(cs) != 1 || cs[0].MaxAge >= 0 {
		t.Errorf("flash cookie not expired after display: %+v", cs)
	}
	// An htmx poll neither shows nor consumes it; the fragment has no document.
	frag := get(cookies[0], true)
	if strings.Contains(frag.Body.String(), "Saved") || strings.Contains(frag.Body.String(), "<html") || len(frag.Result().Cookies()) != 0 {
		t.Errorf("fragment consumed or showed the flash:\n%s", frag.Body.String())
	}
	// Forged and tampered cookies show nothing.
	forged := &http.Cookie{Name: FlashCookieName, Value: cookies[0].Value[:len(cookies[0].Value)-2] + "AA"}
	if strings.Contains(get(forged, false).Body.String(), "Saved") {
		t.Error("tampered flash cookie shown")
	}
	if strings.Contains(get(&http.Cookie{Name: FlashCookieName, Value: "x.y.z"}, false).Body.String(), "flash ok") {
		t.Error("garbage flash cookie shown")
	}
}

func TestErrorPages(t *testing.T) {
	e := newEnv(t, nil)
	e.s.mux.HandleFunc("GET /err/{code}", func(w http.ResponseWriter, r *http.Request) {
		var code int
		fmt.Sscan(r.PathValue("code"), &code)
		e.s.ErrorPage(w, r, code)
	})
	for _, code := range []int{403, 404, 405, 413, 415, 421, 429, 500, 503, 418} {
		rec := e.do("GET", "localhost", fmt.Sprintf("/err/%d", code), nil)
		body := rec.Body.String()
		id := rec.Header().Get("X-Request-Id")
		if rec.Code != code || !strings.Contains(body, fmt.Sprintf("Error %d", code)) || id == "" || !strings.Contains(body, id) {
			t.Errorf("%d: status %d, request id %q in body: %v", code, rec.Code, id, strings.Contains(body, id))
		}
		if !strings.Contains(body, "<!doctype html>") || !strings.Contains(body, "MIRAFLEET") {
			t.Errorf("%d: not a branded full page", code)
		}
		frag := e.do("GET", "localhost", fmt.Sprintf("/err/%d", code), func(r *http.Request) { r.Header.Set("HX-Request", "true") })
		if strings.Contains(frag.Body.String(), "<html") || !strings.Contains(frag.Body.String(), "error-panel") {
			t.Errorf("%d: htmx request did not get the fragment", code)
		}
	}
}

func TestInFlightCap(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.MaxInFlight = 1 })
	entered, release := make(chan struct{}), make(chan struct{})
	e.s.mux.HandleFunc("GET /block", func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
	})
	done := make(chan int, 1)
	go func() { done <- e.do("GET", "localhost", "/block", nil).Code }()
	<-entered
	rec := e.do("GET", "localhost", "/other", nil)
	if rec.Code != 503 || rec.Header().Get("Retry-After") == "" || rec.Header().Get("Content-Security-Policy") == "" {
		t.Errorf("over the cap: %d Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	close(release)
	if code := <-done; code != 200 {
		t.Errorf("blocked request finished with %d", code)
	}
	if rec := e.do("GET", "localhost", "/other", nil); rec.Code != 404 {
		t.Errorf("slot not released: %d", rec.Code)
	}
}

func TestPanicBecomes500WithoutDetail(t *testing.T) {
	e := newEnv(t, nil)
	e.s.mux.HandleFunc("GET /boom", func(w http.ResponseWriter, r *http.Request) {
		panic("secret-detail-/etc/passwd")
	})
	rec := e.do("GET", "localhost", "/boom?token=hunter2", nil)
	body := rec.Body.String()
	id := rec.Header().Get("X-Request-Id")
	if rec.Code != 500 || strings.Contains(body, "secret-detail") || strings.Contains(body, "goroutine") || !strings.Contains(body, id) {
		t.Errorf("panic response: %d\n%s", rec.Code, body)
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Error("500 lacks headers")
	}
	out := e.log()
	if !strings.Contains(out, "secret-detail-/etc/passwd") || !strings.Contains(out, "req="+id) {
		t.Errorf("panic not logged with its stack and id:\n%s", out)
	}
	if strings.Contains(out, "hunter2") {
		t.Errorf("access log holds the query string:\n%s", out)
	}
	if !strings.Contains(out, "GET GET /boom 500") {
		t.Errorf("access log line lacks method, pattern and status:\n%s", out)
	}
	// The server keeps serving.
	if rec := e.do("GET", "localhost", "/missing", nil); rec.Code != 404 {
		t.Errorf("after panic: %d", rec.Code)
	}
}

func TestRouteTimeout(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.Timeout = time.Minute })
	var got time.Duration
	e.s.mux.HandleFunc("GET /slow", func(w http.ResponseWriter, r *http.Request) {
		dl, _ := r.Context().Deadline()
		got = time.Until(dl)
	})
	e.s.mux.HandleFunc("GET /export", func(w http.ResponseWriter, r *http.Request) {
		dl, _ := r.Context().Deadline()
		got = time.Until(dl)
	})
	e.s.RouteTimeout("GET /export", 5*time.Hour)
	e.do("GET", "localhost", "/slow", nil)
	if got < 50*time.Second || got > time.Minute {
		t.Errorf("default deadline %s", got)
	}
	e.do("GET", "localhost", "/export", nil)
	if got < 4*time.Hour {
		t.Errorf("route deadline %s", got)
	}
}
