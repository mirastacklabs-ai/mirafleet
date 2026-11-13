package osb

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var ctx = context.Background()

func mustEP(t *testing.T, c *Client) *Endpoint {
	t.Helper()
	ep, err := c.GetEndpoint(ctx, "sb1")
	if err != nil {
		t.Fatal(err)
	}
	return ep
}

func TestLifecycle(t *testing.T) {
	f := newFake(t, newPKI(t), fakeOpts{})
	c := f.client(t)

	sb, err := c.CreateSandbox(ctx, CreateRequest{
		Image: Image{URI: "img"}, Entrypoint: []string{"sleep", "infinity"},
		ResourceLimits: map[string]string{"cpu": "1"}, Timeout: 120,
		NetworkPolicy: &NetworkPolicy{DefaultAction: "deny"},
	})
	if err != nil || sb.ID != "sb1" {
		t.Fatalf("create: %v %+v", err, sb)
	}
	if !strings.Contains(string(f.created), `"defaultAction":"deny"`) {
		t.Fatalf("body: %s", f.created)
	}
	if got, err := c.GetSandbox(ctx, "sb1"); err != nil || got.Status.State != "Running" {
		t.Fatalf("get: %v %+v", err, got)
	}
	if _, err := c.GetSandbox(ctx, "missing"); !IsNotFound(err) {
		t.Fatalf("want not found, got %v", err)
	}
	if err := c.DeleteSandbox(ctx, "sb1"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteSandbox(ctx, "gone"); err != nil {
		t.Fatalf("404 delete must be ok: %v", err)
	}
	if _, err := c.GetSandbox(ctx, "../x"); err == nil {
		t.Fatal("bad id accepted")
	}
	// list pages, fake ignores the metadata filter; the client re-filters.
	l, err := c.ListSandboxes(ctx, ListOptions{Metadata: map[string]string{"mirafleet.runner": "r1"}, PageSize: 2})
	if err != nil || len(l) != 2 || l[0].ID != "a1" || l[1].ID != "c1" {
		t.Fatalf("list: %v %+v", err, l)
	}
}

func TestExecd(t *testing.T) {
	f := newFake(t, newPKI(t), fakeOpts{})
	c := f.client(t)
	ep := mustEP(t, c)
	if ep.Path != "/sandboxes/sb1/proxy/44772" {
		t.Fatalf("path %q", ep.Path)
	}
	uid := 1000
	st, err := c.RunCommand(ctx, ep, CommandRequest{Argv: []string{"echo", "hi"}, Timeout: 5000, UID: &uid, GID: &uid})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var types []string
	var id string
	for {
		ev, err := st.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		types = append(types, ev.Type)
		if ev.Type == EventInit {
			id = ev.SessionID()
		}
		if ev.Type == EventComplete && (ev.ExitCode == nil || *ev.ExitCode != 0) {
			t.Fatal("exit code")
		}
	}
	if id != "cmd-1" || strings.Join(types, ",") != "init,stdout,execution_complete" {
		t.Fatalf("%q %v", id, types)
	}
	if f.cmdBody.Argv[0] != "echo" || f.cmdBody.Timeout != 5000 || *f.cmdBody.UID != 1000 {
		t.Fatalf("cmd body %+v", f.cmdBody)
	}
	if err := c.CancelCommand(ctx, ep, "cmd-1"); err != nil || f.cancelledID != "cmd-1" {
		t.Fatalf("cancel %v %q", err, f.cancelledID)
	}

	fi, err := c.StatFile(ctx, ep, 1000, 1000, "/workspace/a")
	if err != nil || fi.Size != 11 || fi.Type != "file" {
		t.Fatalf("stat %v %+v", err, fi)
	}
	if _, err := c.StatFile(ctx, ep, 1000, 1000, "/nope"); !IsNotFound(err) {
		t.Fatalf("want 404: %v", err)
	}

	fb, err := c.DownloadFile(ctx, ep, 1000, 1000, "/workspace/a", 6, 10)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(fb)
	fb.Close()
	if string(b) != "world" || fb.Status != 206 || fb.ContentRange != "bytes 6-10/11" {
		t.Fatalf("range: %q %d %q", b, fb.Status, fb.ContentRange)
	}
	fb, err = c.DownloadFile(ctx, ep, 1000, 1000, "/workspace/a", 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(fb)
	fb.Close()
	if string(b) != "hello world" {
		t.Fatalf("full: %q", b)
	}

	err = c.UploadFile(ctx, ep, 1000, 1000, UploadMeta{Path: "/workspace/up", Owner: "1000", Group: "1000", Mode: 644}, strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	if f.uploadData != "payload" || !strings.Contains(f.uploadMeta, `"path":"/workspace/up"`) || !strings.Contains(f.uploadMeta, `"mode":644`) {
		t.Fatalf("upload %q %q", f.uploadMeta, f.uploadData)
	}
	// headers: both secrets are sent on execd, X-Evil from the server is not echoed back.
	last := f.sawHeaders[len(f.sawHeaders)-1]
	if last.Get(apiKeyHeader) != testAPIKey || last.Get(execdTokHeader) != testExecdTok || last.Get("X-Evil") != "" {
		t.Fatalf("headers %v", last)
	}
}

func TestEndpointValidation(t *testing.T) {
	f := newFake(t, newPKI(t), fakeOpts{})
	c := f.client(t)
	const good = "/sandboxes/sb1/proxy/44772"
	for _, ok := range []string{
		"https://evil.example:9/sandboxes/sb1/proxy/44772", // foreign host: ignored, path used
		"http://evil.example/sandboxes/sb1/proxy/44772",    // plain http scheme: ignored
		"evil.example:80/sandboxes/sb1/proxy/44772",
		good, "/v1" + good,
	} {
		f.endpoint = ok
		ep, err := c.GetEndpoint(ctx, "sb1")
		if err != nil {
			t.Fatalf("%q: %v", ok, err)
		}
		if !strings.HasSuffix(ep.Path, good) || strings.Contains(ep.Path, "evil") {
			t.Fatalf("%q -> %q", ok, ep.Path)
		}
		// requests go to the configured host regardless; the fake only serves the bare path
		if ok != "/v1"+good {
			if _, err := c.StatFile(ctx, ep, 1000, 1000, "/x"); err != nil {
				t.Fatalf("%q: stat: %v", ok, err)
			}
		}
	}
	for _, bad := range []string{
		"/", "https://evil.example", "https://evil.example/", "/sandboxes/other/proxy/44772",
		"/sandboxes/sb1/proxy/8080", "/sandboxes/sb1/proxy/44772/../x", "/sandboxes/sb1/proxy/44772?x=1",
		"/sandboxes/sb1/proxy/44772#f", "/sandboxes/sb1/proxy/%2e%2e", "/v1/sandboxes/sb1", "evil.example",
	} {
		f.endpoint = bad
		if _, err := c.GetEndpoint(ctx, "sb1"); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	// a hand-built Endpoint with a foreign shape is refused at call time too
	if _, err := c.RunCommand(ctx, &Endpoint{Path: "https://evil/x"}, CommandRequest{Argv: []string{"x"}}); err == nil {
		t.Fatal("bad endpoint path accepted")
	}
}

func TestSecretsNeverLeak(t *testing.T) {
	f := newFake(t, newPKI(t), fakeOpts{})
	c := f.client(t)
	ep := mustEP(t, c)
	_, err := c.GetSandbox(ctx, "leak")
	if err == nil {
		t.Fatal("want error")
	}
	var errs []string
	errs = append(errs, err.Error())
	// bad key on the wire -> 401 text must not echo anything secret
	bad := f.cfg(t)
	os.WriteFile(bad.APIKeyFile, []byte("wrong-key-zzz"), 0o600)
	c2, _ := NewClient(bad)
	_, err = c2.GetSandbox(ctx, "sb1")
	errs = append(errs, err.Error())
	_, err = c.RunCommand(ctx, &Endpoint{Path: ep.Path, token: &secret{"wrong"}}, CommandRequest{Argv: []string{"x"}})
	errs = append(errs, err.Error())

	outs := append(errs,
		fmt.Sprintf("%v %+v %#v %s", c, c, c, c),
		fmt.Sprintf("%v %+v %#v %s", ep, ep, ep, ep),
		fmt.Sprintf("%v %+v", *ep, struct{ E *Endpoint }{ep}),
		fmt.Sprintf("%+v", struct{ C *Client }{c}),
	)
	for _, s := range outs {
		for _, sec := range []string{testAPIKey, testExecdTok, "wrong-key-zzz"} {
			if strings.Contains(s, sec) {
				t.Fatalf("secret %q in %q", sec, s)
			}
		}
	}
	if !strings.Contains(errs[0], "[redacted]") {
		t.Fatalf("expected scrubbing marker: %q", errs[0])
	}
}

func TestRedirectRefused(t *testing.T) {
	f := newFake(t, newPKI(t), fakeOpts{})
	c := f.client(t)
	_, err := c.GetSandbox(ctx, "redir")
	if !errors.Is(err, ErrRedirect) {
		t.Fatalf("want ErrRedirect, got %v", err)
	}
}

func TestPlainHTTPBaseRefused(t *testing.T) {
	f := newFake(t, newPKI(t), fakeOpts{})
	cfg := f.cfg(t)
	for _, b := range []string{"http://127.0.0.1:1", "ftp://x", "127.0.0.1:8443", "https://", "https://h/v1", "https://u:p@h", "https://h?x=1"} {
		cfg.BaseURL = b
		if _, err := NewClient(cfg); err == nil {
			t.Fatalf("accepted base %q", b)
		}
	}
}

func TestConfigRefusals(t *testing.T) {
	f := newFake(t, newPKI(t), fakeOpts{})
	base := f.cfg(t)
	cases := map[string]func(*Config){
		"no ca":         func(c *Config) { c.CAFile = "" },
		"no cert":       func(c *Config) { c.CertFile = "" },
		"no key":        func(c *Config) { c.KeyFile = "" },
		"no api key":    func(c *Config) { c.APIKeyFile = "" },
		"bad pin":       func(c *Config) { c.PinSHA256 = "AAAA" },
		"empty ca pem":  func(c *Config) { c.CAFile = c.APIKeyFile },
		"missing ca":    func(c *Config) { c.CAFile = filepath.Join(t.TempDir(), "nope") },
		"loose key mod": func(c *Config) { os.Chmod(c.APIKeyFile, 0o644) },
	}
	for name, mut := range cases {
		cfg := base
		os.Chmod(cfg.APIKeyFile, 0o600)
		mut(&cfg)
		if _, err := NewClient(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPin(t *testing.T) {
	p := newPKI(t)
	f := newFake(t, p, fakeOpts{})
	cfg := f.cfg(t)

	cfg.PinSHA256 = SPKIPin(p.serverLeaf)
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetSandbox(ctx, "sb1"); err != nil {
		t.Fatalf("right pin: %v", err)
	}

	other := newPKI(t)
	cfg.PinSHA256 = SPKIPin(other.serverLeaf)
	c, _ = NewClient(cfg)
	_, err = c.GetSandbox(ctx, "sb1")
	if err == nil || !strings.Contains(err.Error(), "pin") {
		t.Fatalf("wrong pin must be refused, got %v", err)
	}
}

func TestWrongCARefused(t *testing.T) {
	p := newPKI(t)
	f := newFake(t, p, fakeOpts{})
	cfg := f.cfg(t)
	cfg.CAFile = newPKI(t).caFile // a different CA
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.GetSandbox(ctx, "sb1")
	var ua x509.UnknownAuthorityError
	if err == nil || !errors.As(err, &ua) {
		t.Fatalf("want unknown authority, got %v", err)
	}
}

func TestTLS12ServerRefused(t *testing.T) {
	f := newFake(t, newPKI(t), fakeOpts{maxTLS: tls.VersionTLS12})
	c := f.client(t)
	if _, err := c.GetSandbox(ctx, "sb1"); err == nil {
		t.Fatal("TLS 1.2 server accepted")
	}
}

func TestMissingClientCertRejectedByServer(t *testing.T) {
	p := newPKI(t)
	f := newFake(t, p, fakeOpts{})
	// The client side refuses to even be built without a certificate...
	cfg := f.cfg(t)
	cfg.CertFile, cfg.KeyFile = "", ""
	if _, err := NewClient(cfg); err == nil {
		t.Fatal("client built without client cert")
	}
	// ...and the fake (mTLS) rejects a client that connects without one.
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: func() *tls.Config {
		pool := x509.NewCertPool()
		pool.AddCert(p.caCert)
		return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS13}
	}()}}
	resp, err := hc.Get(f.srv.URL + "/v1/sandboxes/sb1")
	if err == nil {
		resp.Body.Close()
		t.Fatal("server accepted a client without certificate")
	}
}

func TestSystemRootsNotUsed(t *testing.T) {
	// Server presents a cert from a CA that is not the configured one; SSL_CERT_FILE must not help.
	p := newPKI(t)
	f := newFake(t, p, fakeOpts{})
	t.Setenv("SSL_CERT_FILE", p.caFile)
	cfg := f.cfg(t)
	cfg.CAFile = newPKI(t).caFile
	c, _ := NewClient(cfg)
	if _, err := c.GetSandbox(ctx, "sb1"); err == nil {
		t.Fatal("system roots consulted")
	}
}

func TestNoProxyFromEnv(t *testing.T) {
	f := newFake(t, newPKI(t), fakeOpts{})
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("https_proxy", "http://127.0.0.1:1")
	c := f.client(t)
	if _, err := c.GetSandbox(ctx, "sb1"); err != nil {
		t.Fatalf("env proxy used: %v", err)
	}
}

func TestCommandNonStreamAnswer(t *testing.T) {
	f := newFake(t, newPKI(t), fakeOpts{})
	c := f.client(t)
	ep := mustEP(t, c)
	if _, err := c.RunCommand(ctx, ep, CommandRequest{}); err == nil {
		t.Fatal("empty argv accepted")
	}
}
