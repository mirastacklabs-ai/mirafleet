package gui

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/central/registry"
	"github.com/mirastacklabs-ai/mirafleet/internal/policy"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/store/sqlite"
)

// Credentials of the admin that loginForTest creates.
const (
	testUser = "root-admin"
	testPW   = "correct horse battery staple"
	testHost = "192.0.2.10:8443"
)

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type liveStub struct{ registry.Live }

type connsStub struct{}

func (connsStub) KickOperator(int64, string) {}

// authEnv is a GUI server over a real service on a temp SQLite file, with a movable clock. Later
// screen tests build it with newAuthEnv and sign in with loginForTest.
type authEnv struct {
	t    *testing.T
	srv  *Server
	svc  *service.Service
	db   store.Store
	dir  string
	clk  *testClock
	mu   sync.Mutex
	logs []string
	seen []string // every response (headers and body) a testClient received
	n    int
}

func newAuthEnv(t *testing.T) *authEnv {
	t.Helper()
	ctx := context.Background()
	e := &authEnv{t: t, dir: t.TempDir(), clk: &testClock{t: time.Now()}}
	logf := func(f string, a ...any) {
		e.mu.Lock()
		e.logs = append(e.logs, fmt.Sprintf(f, a...))
		e.mu.Unlock()
	}
	db, err := sqlite.Open(ctx, filepath.Join(e.dir, "mirafleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	e.db = db
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	host, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	e.svc, err = service.New(service.Config{Store: db, Policy: policy.TablePolicy{}, Live: liveStub{}, HostKey: host,
		OperatorConns: connsStub{}, Now: e.clk.now, Logf: logf})
	if err != nil {
		t.Fatal(err)
	}
	e.srv, err = New(Config{
		Backend: e.svc, Version: "test-1.2.3", Listen: "127.0.0.1:0", DataDir: e.dir, Logf: logf, Now: e.clk.now,
		Hostname:           func() (string, error) { return "central-01", nil },
		Interfaces:         func() ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("192.0.2.10")}, nil },
		HostKeyFingerprint: "SHA256:hostkeyfp", SSHAdvertise: "central.example.org:2222",
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *authEnv) logText() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return strings.Join(e.logs, "\n")
}

func (e *authEnv) seenText() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return strings.Join(e.seen, "\n")
}

// auditCount counts audit rows with the given action.
func (e *authEnv) auditCount(action string) int {
	e.t.Helper()
	evs, err := e.db.ListAudit(context.Background(), 0, 1000)
	if err != nil {
		e.t.Fatal(err)
	}
	n := 0
	for _, ev := range evs {
		if ev.Action == action {
			n++
		}
	}
	return n
}

// testClient is a browser stand-in: a cookie jar, a fixed remote address, plain form posts and no
// JavaScript.
type testClient struct {
	e      *authEnv
	remote string
	jar    map[string]string
}

func (e *authEnv) newClient() *testClient {
	e.mu.Lock()
	e.n++
	n := e.n
	e.mu.Unlock()
	return &testClient{e: e, remote: fmt.Sprintf("198.51.100.%d:5000", n), jar: map[string]string{}}
}

// do sends a request through the full handler chain and applies the response cookies.
func (c *testClient) do(method, target string, form url.Values, mod func(*http.Request)) *httptest.ResponseRecorder {
	c.e.t.Helper()
	var r *http.Request
	if form != nil {
		r = httptest.NewRequest(method, target, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	r.Host, r.RemoteAddr = testHost, c.remote
	r.Header.Set("User-Agent", "TestBrowser/1.0")
	for k, v := range c.jar {
		r.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	if mod != nil {
		mod(r)
	}
	rec := httptest.NewRecorder()
	c.e.srv.Handler().ServeHTTP(rec, r)
	for _, ck := range rec.Result().Cookies() {
		if ck.MaxAge < 0 || ck.Value == "" {
			delete(c.jar, ck.Name)
		} else {
			c.jar[ck.Name] = ck.Value
		}
	}
	c.e.mu.Lock()
	c.e.seen = append(c.e.seen, fmt.Sprintf("%v\n%s", rec.Header(), rec.Body.String()))
	c.e.mu.Unlock()
	return rec
}

func (c *testClient) get(target string) *httptest.ResponseRecorder {
	c.e.t.Helper()
	return c.do("GET", target, nil, nil)
}

var csrfRE = regexp.MustCompile(`name="_csrf" value="([^"]+)"`)

// token returns the hidden _csrf value of the page at target.
func (c *testClient) token(target string) string {
	c.e.t.Helper()
	rec := c.get(target)
	m := csrfRE.FindStringSubmatch(rec.Body.String())
	if m == nil {
		c.e.t.Fatalf("GET %s: status %d, no _csrf field in:\n%s", target, rec.Code, rec.Body.String())
	}
	return m[1]
}

// post fetches the form at formPage for its token, then posts form (plus _csrf) to target.
func (c *testClient) post(formPage, target string, form url.Values) *httptest.ResponseRecorder {
	c.e.t.Helper()
	f := url.Values{}
	for k, v := range form {
		f[k] = v
	}
	f.Set("_csrf", c.token(formPage))
	return c.do("POST", target, f, nil)
}

func (c *testClient) login(user, pw string) *httptest.ResponseRecorder {
	c.e.t.Helper()
	return c.post("/login", "/login", url.Values{"username": {user}, "password": {pw}})
}

// completeSetup runs the first-run flow: the token file, then the setup form.
func (e *authEnv) completeSetup(c *testClient) {
	e.t.Helper()
	path, created, err := e.srv.PrepareSetup(context.Background())
	if err != nil || !created {
		e.t.Fatalf("PrepareSetup = %q %v %v", path, created, err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		e.t.Fatal(err)
	}
	rec := c.post("/setup", "/setup", url.Values{"token": {strings.TrimSpace(string(b))}, "username": {testUser},
		"password": {testPW}, "password2": {testPW}})
	if rec.Code != http.StatusSeeOther {
		e.t.Fatalf("setup: status %d:\n%s", rec.Code, rec.Body.String())
	}
}

// loginForTest returns a client signed in as the admin. On a fresh env it runs the setup flow
// first; otherwise it signs in with the standard credentials. Each call gives a new session
// from its own remote address.
func loginForTest(t *testing.T, e *authEnv) *testClient {
	t.Helper()
	c := e.newClient()
	if open, err := e.svc.GUISetupOpen(context.Background()); err != nil {
		t.Fatal(err)
	} else if open {
		e.completeSetup(c)
		return c
	}
	if rec := c.login(testUser, testPW); rec.Code != http.StatusSeeOther {
		t.Fatalf("login: status %d:\n%s", rec.Code, rec.Body.String())
	}
	return c
}
