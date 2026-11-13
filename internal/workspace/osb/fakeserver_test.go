package osb

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testAPIKey   = "sk-test-API-KEY-9f3a"
	testExecdTok = "tok-test-EXECD-77b1"
)

// pki is a throwaway CA with a server and a client leaf, written as PEM files.
type pki struct {
	dir                       string
	caFile                    string
	caCert                    *x509.Certificate
	serverCert                tls.Certificate
	serverLeaf                *x509.Certificate
	clientCertFile, clientKey string
}

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func writePEM(t *testing.T, path, typ string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newCA(t *testing.T, name string) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	k := newKey(t)
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return c, k
}

func issue(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, cn string, server bool) (*x509.Certificate, []byte, *ecdsa.PrivateKey) {
	t.Helper()
	k := newKey(t)
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature,
	}
	if server {
		tpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	} else {
		tpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca, &k.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	return leaf, der, k
}

func newPKI(t *testing.T) *pki {
	t.Helper()
	dir := t.TempDir()
	ca, caKey := newCA(t, "test-ca")
	p := &pki{dir: dir, caCert: ca, caFile: filepath.Join(dir, "ca.pem")}
	writePEM(t, p.caFile, "CERTIFICATE", ca.Raw)

	leaf, der, k := issue(t, ca, caKey, "front", true)
	p.serverLeaf = leaf
	p.serverCert = tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k, Leaf: leaf}

	_, cder, ck := issue(t, ca, caKey, "runner", false)
	p.clientCertFile = filepath.Join(dir, "client.pem")
	p.clientKey = filepath.Join(dir, "client.key")
	writePEM(t, p.clientCertFile, "CERTIFICATE", cder)
	kb, _ := x509.MarshalPKCS8PrivateKey(ck)
	writePEM(t, p.clientKey, "PRIVATE KEY", kb)
	return p
}

// fake is an in-process OpenSandbox lifecycle + execd (via proxy path) over mTLS.
type fake struct {
	srv *httptest.Server
	pki *pki

	mu          sync.Mutex
	endpoint    string // value returned for the endpoint; "" = proper path
	created     []byte
	uploadMeta  string
	uploadData  string
	sawHeaders  []http.Header
	cmdBody     CommandRequest
	cancelledID string
	sse         string // SSE payload for /command
}

type fakeOpts struct {
	maxTLS uint16 // 0 = 1.3
	noMTLS bool
}

func newFake(t *testing.T, p *pki, o fakeOpts) *fake {
	t.Helper()
	f := &fake{pki: p}
	f.sse = "data: {\"type\":\"init\",\"text\":\"cmd-1\"}\n\ndata: {\"type\":\"stdout\",\"text\":\"hi\\n\"}\n\n" +
		"data: {\"type\":\"execution_complete\",\"exit_code\":0}\n\n"
	srv := httptest.NewUnstartedServer(http.HandlerFunc(f.handle))
	pool := x509.NewCertPool()
	pool.AddCert(p.caCert)
	tc := &tls.Config{Certificates: []tls.Certificate{p.serverCert}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS12}
	if o.noMTLS {
		tc.ClientAuth = tls.NoClientCert
	}
	if o.maxTLS != 0 {
		tc.MaxVersion = o.maxTLS
	}
	srv.TLS = tc
	srv.Config.ErrorLog = nil
	srv.StartTLS()
	t.Cleanup(srv.Close)
	f.srv = srv
	return f
}

func (f *fake) cfg(t *testing.T) Config {
	t.Helper()
	kf := filepath.Join(f.pki.dir, "api.key")
	if err := os.WriteFile(kf, []byte(testAPIKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return Config{
		BaseURL: "https://" + f.srv.Listener.Addr().String(),
		CAFile:  f.pki.caFile, CertFile: f.pki.clientCertFile, KeyFile: f.pki.clientKey,
		APIKeyFile: kf, CallTimeout: 5 * time.Second,
	}
}

func (f *fake) client(t *testing.T) *Client {
	t.Helper()
	c, err := NewClient(f.cfg(t))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func jsonOut(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fake) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.sawHeaders = append(f.sawHeaders, r.Header.Clone())
	f.mu.Unlock()
	if r.Header.Get(apiKeyHeader) != testAPIKey {
		http.Error(w, "bad key", 401)
		return
	}
	p := r.URL.Path
	const px = "/sandboxes/sb1/proxy/44772"
	if strings.HasPrefix(p, px+"/") {
		if r.Header.Get(execdTokHeader) != testExecdTok {
			http.Error(w, "bad token", 401)
			return
		}
		f.execd(w, r, strings.TrimPrefix(p, px))
		return
	}
	switch {
	case p == "/v1/sandboxes" && r.Method == "POST":
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.created = b
		f.mu.Unlock()
		jsonOut(w, 202, Sandbox{ID: "sb1", Status: Status{State: "Running"}})
	case p == "/v1/sandboxes" && r.Method == "GET":
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		mk := func(id, runner string) Sandbox {
			return Sandbox{ID: id, Metadata: map[string]string{"mirafleet.runner": runner}}
		}
		items := []Sandbox{mk("a1", "r1"), mk("b1", "other")}
		if page == 2 {
			items = []Sandbox{mk("c1", "r1")}
		}
		jsonOut(w, 200, map[string]any{"items": items, "pagination": map[string]any{"page": page, "hasNextPage": page < 2}})
	case p == "/v1/sandboxes/sb1" && r.Method == "GET":
		jsonOut(w, 200, Sandbox{ID: "sb1", Status: Status{State: "Running"}})
	case p == "/v1/sandboxes/sb1" && r.Method == "DELETE":
		w.WriteHeader(204)
	case p == "/v1/sandboxes/leak":
		http.Error(w, "boom key="+testAPIKey+" tok="+testExecdTok, 500)
	case p == "/v1/sandboxes/redir":
		http.Redirect(w, r, "/v1/sandboxes/sb1", http.StatusFound)
	case p == "/v1/sandboxes/sb1/endpoints/44772":
		if r.URL.Query().Get("use_server_proxy") != "true" {
			http.Error(w, "need proxy", 400)
			return
		}
		f.mu.Lock()
		ep := f.endpoint
		f.mu.Unlock()
		if ep == "" {
			ep = "ignored-host:1" + px
		}
		jsonOut(w, 200, map[string]any{"endpoint": ep, "headers": map[string]string{execdTokHeader: testExecdTok, "X-Evil": "1"}})
	default:
		http.NotFound(w, r)
	}
}

func (f *fake) execd(w http.ResponseWriter, r *http.Request, p string) {
	switch {
	case p == "/command" && r.Method == "POST":
		var cr CommandRequest
		_ = json.NewDecoder(r.Body).Decode(&cr)
		f.mu.Lock()
		f.cmdBody = cr
		s := f.sse
		f.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, s)
	case p == "/command" && r.Method == "DELETE":
		f.mu.Lock()
		f.cancelledID = r.URL.Query().Get("id")
		f.mu.Unlock()
		w.WriteHeader(200)
	case p == "/v1/filesystem/1000/1000/files/info":
		q := r.URL.Query().Get("path")
		if q == "/nope" {
			http.Error(w, "no such file", 404)
			return
		}
		jsonOut(w, 200, map[string]FileInfo{q: {Path: q, Type: "file", Size: 11, Owner: "u", Group: "g", Mode: 644}})
	case p == "/v1/filesystem/1000/1000/files/download":
		data := "hello world"
		if rg := r.Header.Get("Range"); rg != "" {
			var a, b int
			if _, err := fmt.Sscanf(rg, "bytes=%d-%d", &a, &b); err == nil {
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/11", a, b))
				w.WriteHeader(206)
				_, _ = io.WriteString(w, data[a:b+1])
				return
			}
		}
		_, _ = io.WriteString(w, data)
	case p == "/v1/filesystem/1000/1000/files/upload":
		mr, err := r.MultipartReader()
		if err != nil {
			http.Error(w, "not multipart", 400)
			return
		}
		var names []string
		for {
			part, err := mr.NextPart()
			if err != nil {
				break
			}
			b, _ := io.ReadAll(part)
			names = append(names, part.FormName())
			f.mu.Lock()
			if part.FormName() == "metadata" {
				f.uploadMeta = string(b)
			} else {
				f.uploadData = string(b)
			}
			f.mu.Unlock()
		}
		if strings.Join(names, ",") != "metadata,file" {
			http.Error(w, "part order", 400)
			return
		}
		jsonOut(w, 200, map[string]any{})
	default:
		http.NotFound(w, r)
	}
}
