package gui

// The shared test harness of the screen tests: one GUI server over a real service on a temp
// SQLite file, a movable clock, a spy backend, a live-session fake, and two ways to sign in
// (loginDirect makes a session through the service and keeps its cookie value; loginForTest runs
// the real setup/login forms and returns a cookie-jar client).

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
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

	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/central/registry"
	"github.com/mirastacklabs-ai/mirafleet/internal/policy"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/store/sqlite"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// Credentials of the admin that loginDirect and loginForTest create.
const (
	testUser = "root-admin"
	testPW   = "correct horse battery staple"
	testHost = "192.0.2.10:8443"
)

// TestMain gives every test cheap Argon2id parameters (G-11): hashing at production cost under
// -race made a single login take seconds.
func TestMain(m *testing.M) {
	restore := auth.SetParamsForTest(auth.TestParams)
	code := m.Run()
	restore()
	os.Exit(code)
}

// testClock is real time plus an offset that advance moves, so rows stamped with time.Now()
// stay consistent with the service clock.
type testClock struct {
	mu  sync.Mutex
	off time.Duration
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.off)
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.off += d
	c.mu.Unlock()
}

// fakeLive stands in for the registry's live sessions. Workspace answers with res and err (an
// OK result by default), which is what the destroy call of a workspace kill sees.
type fakeLive struct {
	res *transport.WorkspaceResult
	err error
}

func (*fakeLive) Exec(context.Context, int64, transport.ExecRequest) (*transport.ExecResult, error) {
	return nil, nil
}
func (l *fakeLive) Workspace(context.Context, int64, transport.WorkspaceRequest, io.Reader, io.Writer) (*transport.WorkspaceResult, error) {
	return l.res, l.err
}
func (*fakeLive) Kick(int64, string)                     {}
func (*fakeLive) Tombstone(int64)                        {}
func (*fakeLive) ClearTombstone(int64)                   {}
func (*fakeLive) SetStatus(int64, registry.Status) error { return nil }
func (*fakeLive) Snapshot() []registry.SessionInfo       { return nil }

type fakeConns struct{}

func (fakeConns) KickOperator(int64, string) {}

// authSpy records the passive flag of every GUIAuthenticate call.
type authSpy struct {
	Backend
	mu      sync.Mutex
	passive []bool
}

func (b *authSpy) GUIAuthenticate(ctx context.Context, remote, tok string, passive bool) (auth.Principal, int64, error) {
	b.mu.Lock()
	b.passive = append(b.passive, passive)
	b.mu.Unlock()
	return b.Backend.GUIAuthenticate(ctx, remote, tok, passive)
}

// guiHarness is one test GUI. Build it with newGUIHarness; sign in with loginDirect (a session
// cookie value kept in tok, used by do/get/post) or loginForTest (a cookie-jar client).
type guiHarness struct {
	t    *testing.T
	srv  *Server
	svc  *service.Service
	db   *sqlite.DB
	dir  string
	clk  *testClock
	spy  *authSpy
	live *fakeLive
	adm  auth.Principal // the admin, once signed in
	tok  string         // session cookie value of loginDirect ("" = signed out)
	c    *testClient    // the signed-in client of harnesses that use one
	n    int            // counter for seeded rows

	mu      sync.Mutex
	logs    []string
	seen    []string // every response (headers and body) a testClient received
	clients int
}

// authEnv is the former name of guiHarness, kept for files that still use it.
type authEnv = guiHarness

type harnessOpts struct {
	cfg  func(*Config)
	wrap func(Backend) Backend
	reg  bool
}

type harnessOpt func(*harnessOpts)

// withConfig adjusts the server Config.
func withConfig(f func(*Config)) harnessOpt { return func(o *harnessOpts) { o.cfg = f } }

// withBackend wraps the (spy) backend the server calls, for tests that inject data.
func withBackend(f func(Backend) Backend) harnessOpt { return func(o *harnessOpts) { o.wrap = f } }

// withRegistrationSecret initialises the operator registration secret (needs an admin; the
// harness signs in directly for it).
func withRegistrationSecret() harnessOpt { return func(o *harnessOpts) { o.reg = true } }

func newGUIHarness(t *testing.T, opts ...harnessOpt) *guiHarness {
	t.Helper()
	var o harnessOpts
	for _, f := range opts {
		f(&o)
	}
	ctx := context.Background()
	h := &guiHarness{t: t, dir: t.TempDir(), clk: &testClock{}, live: &fakeLive{res: &transport.WorkspaceResult{Code: transport.CodeOK}}}
	db, err := sqlite.Open(ctx, filepath.Join(h.dir, "mirafleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	h.db = db
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	host, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	h.svc, err = service.New(service.Config{Store: db, Policy: policy.TablePolicy{}, Live: h.live, HostKey: host,
		OperatorConns: fakeConns{}, Now: h.clk.now, Logf: h.logf})
	if err != nil {
		t.Fatal(err)
	}
	h.spy = &authSpy{Backend: h.svc}
	var be Backend = h.spy
	if o.wrap != nil {
		be = o.wrap(be)
	}
	h.srv = h.newServer(be, h.dir, o.cfg)
	if o.reg {
		h.loginDirect()
		if err := h.svc.InitRegistrationSecret(ctx, h.adm, service.SourceGUI); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

// newAdminHarness is newGUIHarness already signed in with loginDirect.
func newAdminHarness(t *testing.T, opts ...harnessOpt) *guiHarness {
	t.Helper()
	h := newGUIHarness(t, opts...)
	if h.tok == "" {
		h.loginDirect()
	}
	return h
}

// newAuthEnv is the former constructor, kept for files that still call it.
func newAuthEnv(t *testing.T) *authEnv { return newGUIHarness(t) }

// newServer builds a GUI server over be (data directory dir) with the harness's clock, log and
// fixed host data; mod adjusts the Config. A test that needs a second server over the same
// service uses it too.
func (h *guiHarness) newServer(be Backend, dir string, mod func(*Config)) *Server {
	h.t.Helper()
	cfg := Config{
		Backend: be, Version: "test-1.2.3", Listen: "127.0.0.1:0", DataDir: dir, Logf: h.logf, Now: h.clk.now,
		Hostname:           func() (string, error) { return "central-01", nil },
		Interfaces:         func() ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("192.0.2.10")}, nil },
		HostKeyFingerprint: "SHA256:hostkeyfp", SSHAdvertise: "central.example.org:2222",
	}
	if mod != nil {
		mod(&cfg)
	}
	s, err := New(cfg)
	if err != nil {
		h.t.Fatal(err)
	}
	return s
}

func (h *guiHarness) logf(f string, a ...any) {
	h.mu.Lock()
	h.logs = append(h.logs, fmt.Sprintf(f, a...))
	h.mu.Unlock()
}

func (h *guiHarness) logText() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.logs, "\n")
}

func (h *guiHarness) seenText() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.seen, "\n")
}

// auditCount counts audit rows with the given action.
func (h *guiHarness) auditCount(action string) int {
	h.t.Helper()
	evs, err := h.db.ListAudit(context.Background(), 0, 1000)
	if err != nil {
		h.t.Fatal(err)
	}
	n := 0
	for _, ev := range evs {
		if ev.Action == action {
			n++
		}
	}
	return n
}

// audit lists the audit rows of one action through the service.
func (h *guiHarness) audit(action string) []store.AuditEvent {
	h.t.Helper()
	evs, err := h.svc.ListAudit(context.Background(), h.adm, service.SourceGUI, store.AuditFilter{Action: action})
	if err != nil {
		h.t.Fatal(err)
	}
	return evs
}

// ---- direct session (do/get/post/follow) ----

// loginDirect creates the first admin and a session through the service and keeps the cookie.
func (h *guiHarness) loginDirect() {
	h.t.Helper()
	ctx := context.Background()
	st, _, err := h.svc.EnsureGUISetup(ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	h.tok, h.adm, err = h.svc.CompleteGUISetup(ctx, "192.0.2.77:40000", "test", st, testUser, testPW)
	if err != nil {
		h.t.Fatal(err)
	}
}

func (h *guiHarness) csrf() string { return FormToken(CSRFKey(h.tok)) }

func (h *guiHarness) do(method, target string, form url.Values, mod func(*http.Request)) *httptest.ResponseRecorder {
	h.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, target, body)
	r.Host, r.RemoteAddr = testHost, "192.0.2.77:40000"
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if h.tok != "" {
		r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: h.tok})
	}
	if mod != nil {
		mod(r)
	}
	rec := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(rec, r)
	return rec
}

func (h *guiHarness) get(target string) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.do("GET", target, nil, nil)
}

// post sends a form with a valid CSRF token added.
func (h *guiHarness) post(target string, form url.Values) *httptest.ResponseRecorder {
	h.t.Helper()
	form.Set("_csrf", h.csrf())
	return h.do("POST", target, form, nil)
}

// follow GETs the redirect target of rec, carrying the flash cookie.
func (h *guiHarness) follow(rec *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	h.t.Helper()
	if rec.Code != http.StatusSeeOther {
		h.t.Fatalf("status %d, want 303; body: %.300s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	return h.do("GET", loc, nil, func(r *http.Request) {
		for _, c := range rec.Result().Cookies() {
			if c.Name == FlashCookieName && c.MaxAge >= 0 {
				r.AddCookie(c)
			}
		}
	})
}

// wantBody checks the status and that the body contains every sub; it returns the body.
func wantBody(t *testing.T, rec *httptest.ResponseRecorder, status int, subs ...string) string {
	t.Helper()
	b := rec.Body.String()
	if rec.Code != status {
		t.Fatalf("status %d, want %d; body: %.400s", rec.Code, status, b)
	}
	for _, s := range subs {
		if !strings.Contains(b, s) {
			t.Errorf("body lacks %q", s)
		}
	}
	return b
}

// ---- cookie-jar client (setup and login through the real forms) ----

// testClient is a browser stand-in: a cookie jar, a fixed remote address, plain form posts and no
// JavaScript.
type testClient struct {
	e      *guiHarness
	remote string
	jar    map[string]string
}

func (h *guiHarness) newClient() *testClient {
	h.mu.Lock()
	h.clients++
	n := h.clients
	h.mu.Unlock()
	return &testClient{e: h, remote: fmt.Sprintf("198.51.100.%d:5000", n), jar: map[string]string{}}
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
func (h *guiHarness) completeSetup(c *testClient) {
	h.t.Helper()
	path, created, err := h.srv.PrepareSetup(context.Background())
	if err != nil || !created {
		h.t.Fatalf("PrepareSetup = %q %v %v", path, created, err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		h.t.Fatal(err)
	}
	rec := c.post("/setup", "/setup", url.Values{"token": {strings.TrimSpace(string(b))}, "username": {testUser},
		"password": {testPW}, "password2": {testPW}})
	if rec.Code != http.StatusSeeOther {
		h.t.Fatalf("setup: status %d:\n%s", rec.Code, rec.Body.String())
	}
	h.adm = auth.Principal{Kind: auth.KindAdmin, Name: testUser, Remote: "test"}
}

// loginForTest returns a client signed in as the admin. On a fresh harness it runs the setup flow
// first; otherwise it signs in with the standard credentials. Each call gives a new session
// from its own remote address.
func loginForTest(t *testing.T, h *guiHarness) *testClient {
	t.Helper()
	c := h.newClient()
	if open, err := h.svc.GUISetupOpen(context.Background()); err != nil {
		t.Fatal(err)
	} else if open {
		h.completeSetup(c)
		return c
	}
	if rec := c.login(testUser, testPW); rec.Code != http.StatusSeeOther {
		t.Fatalf("login: status %d:\n%s", rec.Code, rec.Body.String())
	}
	if h.adm.Name == "" {
		h.adm = auth.Principal{Kind: auth.KindAdmin, Name: testUser, Remote: "test"}
	}
	return c
}

// fleet reads one fleet through the service.
func (h *guiHarness) fleet(name string) (store.Fleet, error) {
	return h.svc.GetFleet(context.Background(), h.adm, service.SourceGUI, name)
}

// fleets lists the fleets through the service.
func (h *guiHarness) fleets() []store.Fleet {
	h.t.Helper()
	fl, err := h.svc.ListFleets(context.Background(), h.adm, service.SourceGUI)
	if err != nil {
		h.t.Fatal(err)
	}
	return fl
}
