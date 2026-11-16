package broker

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

const decoy = "DECOY-TOKEN-s3cr3t-0123456789"
const jwt = "eyJhbGciOi.DECOY-JWT.sig"

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

type rig struct {
	b    *Broker
	logs *syncBuf
	addr string
	prov *MapProvider
}

func certPEM(c *x509.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})
}

// newRig starts a broker whose dialer sends "example.com:443" to credAddr.
func newRig(t *testing.T, up *httptest.Server, cred Credential) *rig {
	t.Helper()
	logs := &syncBuf{}
	prov := NewMapProvider(cred)
	b, err := New(Config{
		Provider:    prov,
		CorporateCA: certPEM(up.Certificate()),
		Logger:      slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Dial: func(_ contextT, network, addr string) (net.Conn, error) {
			if addr == "example.com:443" {
				addr = up.Listener.Addr().String()
			}
			return net.Dial(network, addr)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go b.Serve(l)
	t.Cleanup(func() { b.Close() })
	return &rig{b: b, logs: logs, addr: l.Addr().String(), prov: prov}
}

func (r *rig) client(proxyURL string, trust []byte) *http.Client {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(trust)
	pu, _ := url.Parse(proxyURL)
	return &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu),
		TLSClientConfig: &tls.Config{RootCAs: pool}, DisableKeepAlives: true}}
}

func upstream(t *testing.T, h http.HandlerFunc) *httptest.Server {
	s := httptest.NewTLSServer(h)
	t.Cleanup(s.Close)
	return s
}

func TestInjectKinds(t *testing.T) {
	cases := []struct {
		cred Credential
		hdr  string
		want string
	}{
		{Credential{Name: "a", Kind: Basic, User: "oauth2", Hosts: []string{"example.com"}, Secret: decoy},
			"Authorization", "Basic " + base64.StdEncoding.EncodeToString([]byte("oauth2:"+decoy))},
		{Credential{Name: "b", Kind: Bearer, Hosts: []string{"example.com"}, Secret: decoy}, "Authorization", "Bearer " + decoy},
		{Credential{Name: "c", Kind: Header, HeaderName: "PRIVATE-TOKEN", Hosts: []string{"example.com"}, Secret: decoy}, "Private-Token", decoy},
	}
	for _, c := range cases {
		t.Run(c.cred.Kind, func(t *testing.T) {
			if err := c.cred.Validate(); err != nil {
				t.Fatal(err)
			}
			var got, hostSeen string
			var echoed atomic.Bool
			up := upstream(t, func(w http.ResponseWriter, r *http.Request) {
				got, hostSeen = r.Header.Get(c.hdr), r.Host
				if r.Header.Get("X-Agent") != "1" {
					t.Error("agent header lost")
				}
				// hostile upstream: echo the secret in headers
				w.Header().Set("X-Echo", decoy)
				w.Header().Set("Authorization", "Bearer "+decoy)
				w.Header().Set("Private-Token", "x")
				io.WriteString(w, "ok")
				echoed.Store(true)
			})
			r := newRig(t, up, c.cred)
			cl := r.client(r.b.ProxyURL(r.addr), r.b.CAPEM())
			req, _ := http.NewRequest("GET", "https://example.com/repo.git/info/refs?x=1", nil)
			req.Header.Set("X-Agent", "1")
			req.Header.Set(c.hdr, "agent-chosen")
			req.Host = "evil.example"
			resp, err := cl.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if got != c.want {
				t.Fatalf("upstream saw %q want %q", got, c.want)
			}
			if hostSeen != "example.com:443" {
				t.Fatalf("host %q", hostSeen)
			}
			if string(body) != "ok" || resp.StatusCode != 200 {
				t.Fatalf("bad response %d %q", resp.StatusCode, body)
			}
			dump := fmt.Sprint(resp.Header, string(body), r.logs.String(), c.cred, fmt.Sprintf("%#v %+v", c.cred, c.cred))
			if strings.Contains(dump, decoy) {
				t.Fatalf("decoy leaked: %s", dump)
			}
			if resp.Header.Get("Authorization") != "" || resp.Header.Get("Private-Token") != "" || resp.Header.Get("X-Echo") != "" {
				t.Fatalf("secret headers survived: %v", resp.Header)
			}
		})
	}
}

func TestNonCredentialHostTunnelled(t *testing.T) {
	var sawAuth atomic.Value
	other := upstream(t, func(w http.ResponseWriter, r *http.Request) {
		sawAuth.Store(r.Header.Get("Authorization"))
		io.WriteString(w, "plain")
	})
	cred := upstream(t, func(w http.ResponseWriter, r *http.Request) {})
	r := newRig(t, cred, Credential{Name: "a", Kind: Bearer, Hosts: []string{"example.com"}, Secret: decoy})
	cl := r.client(r.b.ProxyURL(r.addr), certPEM(other.Certificate())) // trusts only the real cert
	resp, err := cl.Get(other.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if issuer := resp.TLS.PeerCertificates[0].Issuer.CommonName; strings.Contains(issuer, "mirafleet") {
		t.Fatal("non-credential host was intercepted")
	}
	if v, _ := sawAuth.Load().(string); v != "" {
		t.Fatalf("auth injected on non-credential host: %q", v)
	}
	if strings.Contains(r.logs.String(), decoy) {
		t.Fatal("log leak")
	}
}

func TestProxyAuth(t *testing.T) {
	up := upstream(t, func(w http.ResponseWriter, r *http.Request) { t.Error("must not reach upstream") })
	r := newRig(t, up, Credential{Name: "a", Kind: Bearer, Hosts: []string{"example.com"}, Secret: decoy})
	for _, auth := range []string{"", "Basic " + base64.StdEncoding.EncodeToString([]byte("u:wrong"))} {
		c, err := net.Dial("tcp", r.addr)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(c, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n")
		if auth != "" {
			fmt.Fprintf(c, "Proxy-Authorization: %s\r\n", auth)
		}
		fmt.Fprintf(c, "\r\n")
		resp, err := http.ReadResponse(bufioReader(c), nil)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 407 {
			t.Fatalf("status %d", resp.StatusCode)
		}
		c.Close()
	}
	// wrong password via a real client fails too
	pu := "http://u:wrong@" + r.addr
	cl := r.client(pu, r.b.CAPEM())
	if _, err := cl.Get("https://example.com/"); err == nil {
		t.Fatal("expected failure")
	}
}

func TestRegistryExchange(t *testing.T) {
	var tokenCalls, goodCalls atomic.Int32
	var up *httptest.Server
	up = upstream(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			u, p, ok := r.BasicAuth()
			if !ok || u != "robot" || p != decoy || r.URL.Query().Get("scope") != "repository:foo/bar:pull" {
				w.WriteHeader(401)
				return
			}
			tokenCalls.Add(1)
			fmt.Fprintf(w, `{"token":%q,"expires_in":300}`, jwt)
		case r.Header.Get("Authorization") == "Bearer "+jwt:
			goodCalls.Add(1)
			io.WriteString(w, "manifest")
		default:
			w.Header().Set("WWW-Authenticate", `Bearer realm="https://example.com/token",service="reg",scope="repository:foo/bar:pull"`)
			w.WriteHeader(401)
		}
	})
	r := newRig(t, up, Credential{Name: "reg", Kind: Registry, User: "robot", Hosts: []string{"example.com"}, Secret: decoy})
	cl := r.client(r.b.ProxyURL(r.addr), r.b.CAPEM())
	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest("GET", "https://example.com/v2/foo/bar/manifests/latest", nil)
		req.Header.Set("Authorization", "Bearer agent-own")
		resp, err := cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || string(body) != "manifest" {
			t.Fatalf("status %d %q", resp.StatusCode, body)
		}
		dump := fmt.Sprint(resp.Header, string(body), r.logs.String())
		if strings.Contains(dump, jwt) || strings.Contains(dump, decoy) || resp.Header.Get("Www-Authenticate") != "" {
			t.Fatalf("leak: %s", dump)
		}
	}
	if tokenCalls.Load() != 1 || goodCalls.Load() != 2 {
		t.Fatalf("token calls %d good calls %d (token should be cached)", tokenCalls.Load(), goodCalls.Load())
	}
}

func TestRegistryTokenRefusedNoLeak(t *testing.T) {
	up := upstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			w.WriteHeader(401)
			io.WriteString(w, decoy)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="https://example.com/token",service="reg"`)
		w.WriteHeader(401)
	})
	r := newRig(t, up, Credential{Name: "reg", Kind: Registry, User: "robot", Hosts: []string{"example.com"}, Secret: decoy})
	cl := r.client(r.b.ProxyURL(r.addr), r.b.CAPEM())
	resp, err := cl.Get("https://example.com/v2/x/manifests/1")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 502 || strings.Contains(fmt.Sprint(resp.Header, string(body), r.logs.String()), decoy) {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}
}

func TestRegistryRealmMustBeCredentialHost(t *testing.T) {
	var hit atomic.Bool
	up := upstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			hit.Store(true)
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="https://evil.test/token"`)
		w.WriteHeader(401)
	})
	r := newRig(t, up, Credential{Name: "reg", Kind: Registry, User: "robot", Hosts: []string{"example.com"}, Secret: decoy})
	cl := r.client(r.b.ProxyURL(r.addr), r.b.CAPEM())
	resp, err := cl.Get("https://example.com/v2/x/manifests/1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 502 || hit.Load() {
		t.Fatalf("realm outside credential hosts was used: %d", resp.StatusCode)
	}
}

func TestUpstreamErrorHasNoSecret(t *testing.T) {
	up := upstream(t, func(w http.ResponseWriter, r *http.Request) {})
	logs := &syncBuf{}
	b, _ := New(Config{
		Provider: NewMapProvider(Credential{Name: "a", Kind: Bearer, Hosts: []string{"example.com"}, Secret: decoy}),
		Logger:   slog.New(slog.NewTextHandler(logs, nil)),
		Dial:     func(_ contextT, _, _ string) (net.Conn, error) { return nil, fmt.Errorf("dial failed with %s", decoy) },
	})
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	go b.Serve(l)
	defer b.Close()
	_ = up
	r := &rig{b: b}
	cl := r.client(b.ProxyURL(l.Addr().String()), b.CAPEM())
	resp, err := cl.Get("https://example.com/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 502 || strings.Contains(fmt.Sprint(string(body), logs.String(), resp.Header), decoy) {
		t.Fatalf("status %d leak", resp.StatusCode)
	}
}

func TestNameConstraints(t *testing.T) {
	ca, err := newCA([]string{"gitlab.example", "registry.example:5000", "10.1.2.3"}, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	if ca.cert.MaxPathLen != 0 || !ca.cert.PermittedDNSDomainsCritical {
		t.Fatal("CA not constrained")
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	verify := func(host string) error {
		l, err := ca.leaf(host)
		if err != nil {
			t.Fatal(err)
		}
		c, _ := x509.ParseCertificate(l.Certificate[0])
		_, err = c.Verify(x509.VerifyOptions{Roots: pool, DNSName: host})
		return err
	}
	for _, ok := range []string{"gitlab.example", "registry.example", "10.1.2.3"} {
		if err := verify(ok); err != nil {
			t.Errorf("%s should verify: %v", ok, err)
		}
	}
	for _, bad := range []string{"evil.example", "github.com", "10.9.9.9"} {
		if err := verify(bad); err == nil {
			t.Errorf("%s must not verify under the session CA", bad)
		}
	}
}

func TestBundleAndValidate(t *testing.T) {
	b, err := New(Config{Provider: NewMapProvider()})
	if err != nil {
		t.Fatal(err)
	}
	if got := b.Bundle([]byte("SYSTEM")); !bytes.HasPrefix(got, []byte("SYSTEM\n")) || !bytes.Contains(got, b.CAPEM()) {
		t.Fatal("bundle")
	}
	if (Credential{Name: "x", Kind: Header, HeaderName: "bad name", Hosts: []string{"h"}, Secret: "s"}).Validate() == nil {
		t.Fatal("bad header accepted")
	}
}

// A CA whose hosts are all DNS names must not vouch for an IP address, and one whose hosts
// are all IPs must not vouch for a DNS name.
func TestNameConstraintsCoverTheOtherFamily(t *testing.T) {
	for _, tc := range []struct {
		hosts  []string
		ok     string
		badSet []string
	}{
		{[]string{"gitlab.example"}, "gitlab.example", []string{"10.9.9.9", "::1", "evil.example"}},
		{[]string{"10.1.2.3"}, "10.1.2.3", []string{"gitlab.example", "10.9.9.9"}},
	} {
		ca, err := newCA(tc.hosts, 1<<40)
		if err != nil {
			t.Fatal(err)
		}
		pool := x509.NewCertPool()
		pool.AddCert(ca.cert)
		verify := func(host string) error {
			l, err := ca.leaf(host)
			if err != nil {
				t.Fatal(err)
			}
			c, _ := x509.ParseCertificate(l.Certificate[0])
			_, err = c.Verify(x509.VerifyOptions{Roots: pool, DNSName: host})
			return err
		}
		if err := verify(tc.ok); err != nil {
			t.Errorf("%v: %s should verify: %v", tc.hosts, tc.ok, err)
		}
		for _, bad := range tc.badSet {
			if verify(bad) == nil {
				t.Errorf("%v: %s must not verify", tc.hosts, bad)
			}
		}
	}
}

// A secret echoed in the body (also split across reads) is redacted, the registry bearer is
// scrubbed like the secret, and an empty secret does not drop every header.
func TestBodyEchoIsRedacted(t *testing.T) {
	up := upstream(t, func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		io.WriteString(w, "a "+decoy[:10])
		fl.Flush()
		io.WriteString(w, decoy[10:]+" b "+decoy)
	})
	r := newRig(t, up, Credential{Name: "b", Kind: Bearer, Hosts: []string{"example.com"}, Secret: decoy})
	cl := r.client(r.b.ProxyURL(r.addr), r.b.CAPEM())
	resp, err := cl.Get("https://example.com/x")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if want := "a " + redacted + " b " + redacted; string(body) != want {
		t.Fatalf("body %q, want %q", body, want)
	}
}

func TestRegistryBearerEchoIsScrubbed(t *testing.T) {
	up := upstream(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			fmt.Fprintf(w, `{"token":%q}`, jwt)
		case r.Header.Get("Authorization") == "Bearer "+jwt:
			w.Header().Set("X-Echo", "Bearer "+jwt)
			io.WriteString(w, "t="+jwt)
		default:
			w.Header().Set("WWW-Authenticate", `Bearer realm="https://example.com/token",service="reg"`)
			w.WriteHeader(401)
		}
	})
	r := newRig(t, up, Credential{Name: "reg", Kind: Registry, User: "robot", Hosts: []string{"example.com"}, Secret: decoy})
	cl := r.client(r.b.ProxyURL(r.addr), r.b.CAPEM())
	resp, err := cl.Get("https://example.com/v2/x/manifests/1")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || strings.Contains(fmt.Sprint(resp.Header, string(body)), jwt) {
		t.Fatalf("bearer leaked: %d %v %q", resp.StatusCode, resp.Header, body)
	}
}

func TestScrubEmptySecretKeepsHeaders(t *testing.T) {
	resp := &http.Response{StatusCode: 200, Header: http.Header{"X-Keep": {"v"}}, Body: io.NopCloser(strings.NewReader("hi"))}
	scrub(resp, Credential{}, []string{""})
	if resp.Header.Get("X-Keep") != "v" {
		t.Fatal("empty secret dropped a header")
	}
	if b, _ := io.ReadAll(resp.Body); string(b) != "hi" {
		t.Fatalf("body %q", b)
	}
}
