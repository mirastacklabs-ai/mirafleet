package central

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/apiclient"
	"github.com/mirastacklabs-ai/mirafleet/internal/gui"
	"github.com/mirastacklabs-ai/mirafleet/internal/keys"
	"github.com/mirastacklabs-ai/mirafleet/internal/policy"
	"github.com/mirastacklabs-ai/mirafleet/internal/runner"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/store/sqlite"
	"github.com/mirastacklabs-ai/mirafleet/internal/tokens"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/wsconfig"
)

// TestM4Done is the "M4 done when" scenario of docs/M4.md section 6.1: a real central (SQLite,
// registry, service, SSH), real runners (actions mode, and workspace mode over the fake
// executor), real operators over api@, and the admin GUI mounted over TLS. Everything the admin
// does goes through the GUI's HTTP interface, driven like a browser without JavaScript: form
// posts with CSRF tokens read from the pages, a cookie jar, and the self-signed certificate
// trusted by fingerprint alone. One offset clock is injected into the service and the GUI.
//
// Operator "opx" (step 7) and "bob" (step 9) are added through the store, since they only need
// to exist; "alice" and "dave" (step 8) register with codes shown by the GUI.

const (
	m4Pass1 = "m4-first-password-1"
	m4Pass2 = "m4-second-password-2"
	m4Pass3 = "m4-third-password-3"
)

type m4Fixture struct {
	rootT  *testing.T
	e      *env
	clk    *m4Clock
	dbPath string
	guiDir string
	w      *m4World
	srv    *gui.Server
	ts     *httptest.Server
	svc    *service.Service // the service the GUI talks to (replaced in step 12)
	pw     string

	a, b        *m4Browser
	setupTokens []string
	passwords   []string
	pwHashes    map[string]bool
	joinToken   string
	joinSeq     int
	secrets     [][]byte
	fakeWS      *m3Fake
	wsID        string
	crafted     bool
	did         map[string]int // GUI state changes made, by audit action
}

func (f *m4Fixture) startGUI(svc *service.Service) (*gui.Server, *httptest.Server) {
	t := f.rootT
	t.Helper()
	srv, err := gui.New(gui.Config{Backend: svc, Version: "m4-test", DataDir: f.guiDir, Logf: f.e.logf, Now: f.clk.now,
		Hostname:           func() (string, error) { return "central-m4", nil },
		Interfaces:         func() ([]netip.Addr, error) { return nil, nil },
		HostKeyFingerprint: ssh.FingerprintSHA256(f.e.hostSigner.PublicKey()), SSHAdvertise: f.e.addr})
	must(t, err)
	ts := httptest.NewUnstartedServer(srv.Handler())
	ts.TLS = srv.TLSConfig()
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return srv, ts
}

func (f *m4Fixture) login(t *testing.T) *m4Browser {
	t.Helper()
	b := f.w.newBrowser(t)
	p := b.post("/login", "/login", url.Values{"username": {"admin"}, "password": {f.pw}})
	if p.Status != http.StatusSeeOther || b.session() == "" {
		t.Fatalf("login: status %d, session %q", p.Status, b.session())
	}
	return b
}

func (f *m4Fixture) rows() []store.AuditEvent { return f.e.auditRows() }

func (f *m4Fixture) guiRows(action string) []store.AuditEvent {
	var out []store.AuditEvent
	for _, r := range f.rows() {
		if r.Source == "gui" && r.Action == action {
			out = append(out, r)
		}
	}
	return out
}

// wantIDs returns the ids of the audit rows matching pred, newest first, at most limit of them.
func (f *m4Fixture) wantIDs(pred func(store.AuditEvent) bool, limit int) []int64 {
	var ids []int64
	for _, r := range f.rows() {
		if pred(r) {
			ids = append(ids, r.ID)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] > ids[j] })
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	return ids
}

// captureHashes remembers the stored password hashes, for the hygiene step.
func (f *m4Fixture) captureHashes(t *testing.T) {
	t.Helper()
	for _, h := range m4PasswordHashes(t, f.dbPath) {
		f.pwHashes[h] = true
	}
}

// checkSessionTable asserts the gui_sessions rows are 32-byte hashes of cookies, never cookies.
func (f *m4Fixture) checkSessionTable(t *testing.T) {
	t.Helper()
	cookies := f.w.sessionCookies()
	for _, r := range m4Sessions(t, f.dbPath) {
		if len(r.Hash) != 32 {
			t.Errorf("gui_sessions token_hash has %d bytes, want 32", len(r.Hash))
		}
		for _, c := range cookies {
			if string(r.Hash) == c || bytes.Contains(r.Hash, []byte(c)) {
				t.Errorf("gui_sessions holds a cookie value")
			}
		}
	}
}

func TestM4Done(t *testing.T) {
	ctx := m3ctx(t)
	clk := &m4Clock{}
	e, dbPath := newM4Env(t, clk.now)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("central log:\n%s", e.logText())
		}
	})
	must(t, e.svc.InitRegistrationSecret(ctx, e.admin, service.SourceCLI))
	sec, err := e.db.GetSetting(ctx, store.SettingRegistrationSecret)
	must(t, err)

	f := &m4Fixture{rootT: t, e: e, clk: clk, dbPath: dbPath, guiDir: t.TempDir(), svc: e.svc, pw: m4Pass1,
		pwHashes: map[string]bool{}, did: map[string]int{}, secrets: [][]byte{sec}, passwords: []string{m4Pass1}}
	f.srv, f.ts = f.startGUI(e.svc)
	der := f.srv.TLSConfig().Certificates[0].Certificate[0]
	if gui.Fingerprint(der) != f.srv.Fingerprint() {
		t.Fatalf("fingerprint %s != %s", gui.Fingerprint(der), f.srv.Fingerprint())
	}
	f.w = newM4World(der)
	f.w.setBase(f.ts.URL)
	// What central does at GUI start: no admin yet, so a setup token file is written.
	path, created, err := f.srv.PrepareSetup(ctx)
	if err != nil || !created || path != f.srv.SetupTokenPath() {
		t.Fatalf("PrepareSetup = %q %v %v", path, created, err)
	}

	steps := []struct {
		name string
		fn   func(*testing.T, *m4Fixture)
	}{
		{"01_bootstrap", m4Bootstrap},
		{"02_cookies_headers", m4CookiesHeaders},
		{"03_csrf", m4CSRFStep},
		{"04_login_limits", m4LoginLimits},
		{"05_fleets", m4Fleets},
		{"06_join_token_runner", m4JoinTokenRunner},
		{"07_revoke_cascade", m4RevokeCascade},
		{"08_operators", m4Operators},
		{"09_workspaces", m4Workspaces},
		{"10_audit", m4Audit},
		{"11_sessions", m4Sessions11},
		{"12_restart_credential_binding", m4Restart},
		{"13_hygiene", m4Hygiene},
	}
	for _, s := range steps {
		if !t.Run(s.name, func(t *testing.T) { s.fn(t, f) }) {
			t.Logf("step %s failed; later steps depend on its state and are skipped", s.name)
			break
		}
	}
}

// ---- 1. Bootstrap ----

func m4Bootstrap(t *testing.T, f *m4Fixture) {
	b := f.w.newBrowser(t)
	for _, p := range []string{"/", "/fleets", "/fleets/new", "/runners", "/tokens", "/operators", "/workspaces", "/audit", "/account", "/login"} {
		if r := b.get(p); r.Status != http.StatusSeeOther || !strings.HasPrefix(r.loc(), "/setup") {
			t.Errorf("GET %s before setup: status %d, Location %q; want a redirect to /setup", p, r.Status, r.loc())
		}
	}
	tokenPath := f.srv.SetupTokenPath()
	st, err := os.Stat(tokenPath)
	must(t, err)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("setup token file mode %v, want 0600", st.Mode().Perm())
	}
	if want := filepath.Join(f.guiDir, "gui", "setup-token"); tokenPath != want {
		t.Fatalf("setup token path %s, want %s", tokenPath, want)
	}
	raw, err := os.ReadFile(tokenPath)
	must(t, err)
	tok := strings.TrimSpace(string(raw))
	if tok == "" {
		t.Fatal("empty setup token file")
	}
	f.setupTokens = append(f.setupTokens, tok)
	good := url.Values{"token": {tok}, "username": {"admin"}, "password": {f.pw}, "password2": {f.pw}}

	// /login refuses: it sends the browser to /setup and never signs anyone in.
	if r := b.post("/setup", "/login", url.Values{"username": {"admin"}, "password": {f.pw}}); r.Status != http.StatusSeeOther ||
		!strings.HasPrefix(r.loc(), "/setup") || b.session() != "" {
		t.Fatalf("POST /login before setup: status %d, Location %q, session %q", r.Status, r.loc(), b.session())
	}
	// A wrong token is refused and leaves everything as it was.
	bad := url.Values{"token": {"mfs_wrongwrongwrongwrongwrongwrongwrongwrongwro"}, "username": {"admin"}, "password": {f.pw}, "password2": {f.pw}}
	if r := b.post("/setup", "/setup", bad); r.Status != http.StatusUnprocessableEntity || b.session() != "" {
		t.Fatalf("setup with a wrong token: status %d, session %q", r.Status, b.session())
	}
	if _, err := os.Stat(tokenPath); err != nil {
		t.Fatalf("token file after a wrong token: %v", err)
	}
	// The right token creates the admin, removes the file and signs in.
	if r := b.post("/setup", "/setup", good); r.Status != http.StatusSeeOther || r.loc() != "/" || b.session() == "" {
		t.Fatalf("setup: status %d, Location %q, session %q", r.Status, r.loc(), b.session())
	}
	if _, err := os.Stat(tokenPath); !os.IsNotExist(err) {
		t.Fatalf("setup token file still there after setup: %v", err)
	}
	if r := b.get("/"); r.Status != http.StatusOK {
		t.Fatalf("GET / after setup: status %d", r.Status)
	}
	// /setup is gone, and the used token is useless.
	if r := b.get("/setup"); r.Status != http.StatusNotFound {
		t.Errorf("GET /setup after setup: status %d, want 404", r.Status)
	}
	other := f.w.newBrowser(t)
	if r := other.do("POST", "/setup", good, nil); r.Status != http.StatusNotFound || other.session() != "" {
		t.Errorf("second use of the setup token: status %d, session %q; want 404 and no session", r.Status, other.session())
	}
	if rows := f.guiRows("gui.setup"); len(rows) != 1 || rows[0].Actor != "admin" {
		t.Fatalf("gui.setup audit rows: %+v", rows)
	}
	f.did["gui.setup"]++
	f.did["gui.login"]++ // setup signs the admin in ("after setup")
	f.a = b
}

// ---- 2. Cookies and headers ----

const m4CSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; font-src 'self'; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"

func m4CookiesHeaders(t *testing.T, f *m4Fixture) {
	a := f.a.at(t)
	b := f.w.newBrowser(t)
	b.get("/login")
	pre := b.cookie(gui.PreCookieName)
	if pre == "" {
		t.Fatal("no pre-session cookie on /login")
	}
	p := b.post("/login", "/login", url.Values{"username": {"admin"}, "password": {f.pw}})
	if p.Status != http.StatusSeeOther {
		t.Fatalf("login: status %d", p.Status)
	}
	var ck *http.Cookie
	for _, c := range p.Cookies {
		if c.Name == gui.SessionCookieName {
			ck = c
		}
	}
	if ck == nil || ck.Name != "__Host-mf_session" {
		t.Fatalf("login set no __Host-mf_session cookie: %+v", p.Cookies)
	}
	if !ck.Secure || !ck.HttpOnly || ck.SameSite != http.SameSiteStrictMode || ck.Path != "/" || ck.Domain != "" {
		t.Errorf("session cookie attributes: %+v", ck)
	}
	// The token is fresh at every login: it differs from the pre-session cookie and from the
	// session the setup created.
	if ck.Value == pre || ck.Value == a.session() || ck.Value == "" {
		t.Error("the session token did not change at login")
	}
	f.did["gui.login"]++
	f.b = b

	// Headers on a page, on a login redirect, on a 404 and on the login form.
	for name, pg := range map[string]*m4Page{"GET /": a.get("/"), "POST /login": p, "404": a.get("/no-such-page"), "GET /login (anonymous)": f.w.newBrowser(t).get("/login")} {
		if got := pg.Header.Get("Content-Security-Policy"); got != m4CSP {
			t.Errorf("%s: CSP %q", name, got)
		}
		for k, v := range map[string]string{"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "Referrer-Policy": "no-referrer",
			"Cache-Control": "no-store"} {
			if got := pg.Header.Get(k); got != v {
				t.Errorf("%s: %s = %q, want %q", name, k, got, v)
			}
		}
		if pg.Header.Get("Strict-Transport-Security") != "" {
			t.Errorf("%s: HSTS with a self-signed certificate", name)
		}
	}
	// A foreign Host header is a rebinding attempt.
	r := a.do("GET", "/", nil, map[string]string{"Host": "evil.example"})
	if r.Status != http.StatusMisdirectedRequest || len(r.Cookies) != 0 {
		t.Errorf("Host evil.example: status %d (want 421), cookies %v", r.Status, r.Cookies)
	}
}

// ---- 3. CSRF ----

func m4CSRFStep(t *testing.T, f *m4Fixture) {
	a, b := f.a.at(t), f.b.at(t)
	ctx := m3ctx(t)
	tokA := a.token("/fleets/new")
	tokB := b.token("/fleets/new")
	if tokA == tokB {
		t.Fatal("two sessions share one CSRF token")
	}
	fleetsBefore, err := f.e.db.ListFleets(ctx)
	must(t, err)
	auditBefore := len(f.rows())
	mk := func(csrf string) url.Values {
		v := url.Values{"name": {"csrfx"}, "mode": {"actions"}}
		if csrf != "" {
			v.Set("_csrf", csrf)
		}
		return v
	}
	cases := []struct {
		name string
		form url.Values
		hdr  map[string]string
	}{
		{"no _csrf field", mk(""), nil},
		{"cross-site fetch metadata", mk(tokA), map[string]string{"Sec-Fetch-Site": "cross-site"}},
		{"foreign Origin", mk(tokA), map[string]string{"Sec-Fetch-Site": "", "Origin": "https://evil.example"}},
		{"token of another session", mk(tokB), nil},
		{"garbage token", mk("not-a-token"), nil},
	}
	for _, c := range cases {
		if r := a.do("POST", "/fleets", c.form, c.hdr); r.Status != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", c.name, r.Status)
		}
	}
	// Control: the right token works for the same request shape (on a harmless endpoint).
	if r := a.do("POST", "/account/theme", url.Values{"theme": {"dark"}, "_csrf": {tokA}}, nil); r.Status != http.StatusSeeOther {
		t.Errorf("theme with a valid token: status %d", r.Status)
	}
	fleetsAfter, err := f.e.db.ListFleets(ctx)
	must(t, err)
	if len(fleetsAfter) != len(fleetsBefore) {
		t.Errorf("fleets changed by refused requests: %d -> %d", len(fleetsBefore), len(fleetsAfter))
	}
	if n := len(f.rows()); n != auditBefore {
		t.Errorf("audit rows %d -> %d: a refused request left a row", auditBefore, n)
	}
}

// ---- 4. Login limits ----

var (
	m4Value = regexp.MustCompile(`value="[^"]*"`)
	m4ReqID = regexp.MustCompile(`request [0-9a-f]+`)
)

func m4LoginLimits(t *testing.T, f *m4Fixture) {
	b := f.w.newBrowser(t)
	wrong := func(user string) *m4Page {
		return b.post("/login", "/login", url.Values{"username": {user}, "password": {"definitely-not-the-password"}})
	}
	unknown, known := wrong("nobody"), wrong("admin")
	if unknown.Status != http.StatusUnauthorized || known.Status != http.StatusUnauthorized {
		t.Fatalf("wrong logins: status %d and %d, want 401", unknown.Status, known.Status)
	}
	norm := func(s string) string {
		return m4ReqID.ReplaceAllString(m4Value.ReplaceAllString(s, `value=""`), "request ")
	}
	if norm(unknown.Body) != norm(known.Body) {
		t.Error("unknown user and wrong password produce different pages")
	}
	// Ten consecutive failures lock the address out. The rate bucket (5 attempts, 12 s refill, the
	// setup login already took one) would answer 429 first, so a minute passes when it runs dry.
	for i := 3; i <= 10; i++ {
		if i == 5 || i == 10 {
			f.clk.advance(time.Minute)
		}
		if r := wrong("admin"); r.Status != http.StatusUnauthorized {
			t.Fatalf("wrong password %d: status %d, want 401", i, r.Status)
		}
	}
	good := url.Values{"username": {"admin"}, "password": {f.pw}}
	if r := wrong("admin"); r.Status != http.StatusTooManyRequests || r.Header.Get("Retry-After") == "" {
		t.Fatalf("11th attempt: status %d, Retry-After %q; want 429", r.Status, r.Header.Get("Retry-After"))
	}
	if r := b.post("/login", "/login", good); r.Status != http.StatusTooManyRequests || b.session() != "" {
		t.Fatalf("correct password while locked out: status %d, session %q; want 429", r.Status, b.session())
	}
	if n := len(f.guiRows("gui.lockout")); n != 1 {
		t.Errorf("gui.lockout rows: %d, want 1", n)
	}
	// After 15 minutes the lockout is over.
	f.clk.advance(16 * time.Minute)
	if r := b.post("/login", "/login", good); r.Status != http.StatusSeeOther || b.session() == "" {
		t.Fatalf("login after the lockout: status %d, session %q", r.Status, b.session())
	}
	f.did["gui.login"]++
	if r := b.post("/account", "/logout", nil); r.Status != http.StatusSeeOther || b.session() != "" {
		t.Errorf("logout: status %d, session %q", r.Status, b.session())
	}
	f.did["gui.logout"]++
	// The two other sessions are unaffected by the lockout and the time that passed (16 min idle).
	if !f.a.at(t).alive() || !f.b.at(t).alive() {
		t.Error("an existing session died during the lockout step")
	}
}

// ---- 5. Fleets ----

func m4Fleets(t *testing.T, f *m4Fixture) {
	a, ctx := f.a.at(t), m3ctx(t)
	if r := a.post("/fleets/new", "/fleets", url.Values{"name": {"prod"}, "mode": {"actions"}, "guarded": {"1"}}); r.Status != http.StatusSeeOther || r.loc() != "/fleets/prod" {
		t.Fatalf("create prod: status %d, Location %q", r.Status, r.loc())
	}
	f.did["fleet.create"]++
	dev := url.Values{"name": {"dev"}, "mode": {"workspace"}, "executor": {"containerd"}, "image": {m3Image}, "images": {"mirafleet-test/init:*"},
		"pull": {"never"}, "network": {"none"}, "cpus": {"1"}, "memory": {"256MiB"}, "pids": {"128"}, "tmpfs": {"16MiB"}, "disk": {"128MiB"},
		"idle_ttl": {"1h"}, "max_ttl": {"6h"}, "exec_default": {"30s"}, "exec_max": {"2m"}}
	if r := a.post("/fleets/new", "/fleets", dev); r.Status != http.StatusSeeOther || r.loc() != "/fleets/dev" {
		t.Fatalf("create dev: status %d, Location %q:\n%s", r.Status, r.loc(), r.Body)
	}
	f.did["fleet.create"]++
	prod, err := f.e.db.GetFleet(ctx, "prod")
	must(t, err)
	if prod.Mode != store.ModeActions || !prod.Confirm {
		t.Errorf("prod: %+v", prod)
	}
	d, err := f.e.db.GetFleet(ctx, "dev")
	must(t, err)
	if d.Mode != store.ModeWorkspace || d.Confirm || d.Workspace == nil || d.Workspace.DefaultImage != m3Image ||
		d.Workspace.MemoryMB != 256 || len(d.Workspace.Images) != 1 || d.Workspace.Network != store.NetworkNone {
		t.Errorf("dev: %+v ws %+v", d, d.Workspace)
	}
	if body := a.get("/fleets").Body; !strings.Contains(body, "prod") || !strings.Contains(body, "dev") {
		t.Error("the fleets page does not list prod and dev")
	}

	// An invalid name: 422 and nothing created (no row either).
	auditBefore := len(f.rows())
	if r := a.post("/fleets/new", "/fleets", url.Values{"name": {"Bad Name!"}, "mode": {"actions"}}); r.Status != http.StatusUnprocessableEntity {
		t.Errorf("invalid fleet name: status %d, want 422", r.Status)
	}
	if fl, err := f.e.db.ListFleets(ctx); err != nil || len(fl) != 2 {
		t.Errorf("fleets after an invalid name: %d %v", len(fl), err)
	}
	// The guard toggle on prod needs the typed name.
	guard := func(to, typed string) *m4Page {
		return a.post("/fleets/prod/guard", "/fleets/prod/guard", url.Values{"to": {to}, "confirm": {typed}})
	}
	for _, typed := range []string{"", "dev", "PROD"} {
		if r := guard("off", typed); r.Status != http.StatusUnprocessableEntity {
			t.Errorf("guard off with %q: status %d, want 422", typed, r.Status)
		}
	}
	if fl, _ := f.e.db.GetFleet(ctx, "prod"); !fl.Confirm {
		t.Fatal("the guard of prod was turned off without the typed name")
	}
	if n := len(f.rows()); n != auditBefore {
		t.Errorf("refused requests left %d audit rows", n-auditBefore)
	}
	// With the name it works, and turning it back on needs none.
	if r := guard("off", "prod"); r.Status != http.StatusSeeOther {
		t.Fatalf("guard off with the name: status %d", r.Status)
	}
	f.did["fleet.set_confirm"]++
	if fl, _ := f.e.db.GetFleet(ctx, "prod"); fl.Confirm {
		t.Fatal("guard still on after turning it off")
	}
	if r := guard("on", ""); r.Status != http.StatusSeeOther {
		t.Fatalf("guard on: status %d", r.Status)
	}
	f.did["fleet.set_confirm"]++
	if fl, _ := f.e.db.GetFleet(ctx, "prod"); !fl.Confirm {
		t.Fatal("guard off after turning it on")
	}
}

// ---- 6. Join token, runner, approve ----

var (
	m4NonceRE = regexp.MustCompile(`name="nonce" value="([^"]+)"`)
	m4TokenRE = regexp.MustCompile(`id="copy-token">([^<]+)</pre>`)
)

func m4JoinTokenRunner(t *testing.T, f *m4Fixture) {
	e, a, ctx := f.e, f.a.at(t), m3ctx(t)
	page := a.get("/tokens/new")
	csrf, nonce := m4CSRF.FindStringSubmatch(page.Body), m4NonceRE.FindStringSubmatch(page.Body)
	if csrf == nil || nonce == nil {
		t.Fatalf("token form lacks _csrf or nonce (status %d)", page.Status)
	}
	form := url.Values{"_csrf": {csrf[1]}, "nonce": {nonce[1]}, "fleet": {"prod"}, "labels": {""}, "ttl": {"24h"}, "uses": {"1"}}
	created := a.do("POST", "/tokens", form, nil)
	if created.Status != http.StatusOK || !strings.Contains(created.Header.Get("Cache-Control"), "no-store") {
		t.Fatalf("create token: status %d, Cache-Control %q", created.Status, created.Header.Get("Cache-Control"))
	}
	m := m4TokenRE.FindStringSubmatch(created.Body)
	if m == nil {
		t.Fatalf("the creation response does not show the token:\n%s", created.Body)
	}
	tok := m[1]
	id, _, _, err := tokens.ParseJoinToken(tok)
	if err != nil {
		t.Fatalf("shown token does not parse: %v", err)
	}
	f.joinToken, f.joinSeq = tok, created.Seq
	f.did["token.create"]++
	secretPart := strings.Split(tok, ".")[2] // mfj1.<id>.<secret>.<host-key hash>

	// Replaying the form shows no token and mints none.
	replay := a.do("POST", "/tokens", form, nil)
	if replay.Status != http.StatusOK || strings.Contains(replay.Body, tok) || strings.Contains(replay.Body, secretPart) || m4TokenRE.MatchString(replay.Body) {
		t.Errorf("replayed form: status %d, token shown: %v", replay.Status, strings.Contains(replay.Body, secretPart))
	}
	if toks, err := e.db.ListTokens(ctx, "prod"); err != nil || len(toks) != 1 {
		t.Errorf("tokens after a replay: %d %v", len(toks), err)
	}
	// The lists never carry the secret.
	for _, p := range []string{"/tokens", "/tokens?finished=1", "/fleets/prod", "/tokens/new"} {
		body := a.get(p).Body
		if strings.Contains(body, secretPart) {
			t.Errorf("GET %s shows the token secret", p)
		}
		if p == "/tokens" && !strings.Contains(body, id) {
			t.Errorf("the token list does not list token %s", id)
		}
	}

	// The runner joins over the real enroll path and waits for approval.
	_, err = keys.Generate(e.dir, "db-1", "db-1")
	must(t, err)
	jr, _, err := apiclient.Join(ctx, e.addr, tok, "db-1", nil, e.signer("db-1"))
	must(t, err)
	if jr.Status != "pending" || jr.Fleet != "prod" {
		t.Fatalf("join: %+v", jr)
	}
	r, err := e.db.GetRunner(ctx, store.Ref{Fleet: "prod", Runner: "db-1"})
	must(t, err)
	e.ids["db-1"] = r.ID
	link := `href="/runners/prod/db-1"`
	if !strings.Contains(a.get("/runners?status=pending").Body, link) {
		t.Error("db-1 is not listed as pending")
	}
	if strings.Contains(a.get("/runners?status=active").Body, link) {
		t.Error("db-1 is listed as active before approval")
	}
	if p := a.post("/runners/prod/db-1", "/runners/prod/db-1/approve", url.Values{"back": {"detail"}}); p.Status != http.StatusSeeOther || p.loc() != "/runners/prod/db-1" {
		t.Fatalf("approve: status %d, Location %q", p.Status, p.loc())
	}
	f.did["runner.approve"]++
	if r, err = e.db.GetRunner(ctx, store.Ref{Fleet: "prod", Runner: "db-1"}); err != nil || r.Status != store.StatusActive {
		t.Fatalf("db-1 after approve: %+v %v", r, err)
	}
	if !strings.Contains(a.get("/runners?status=active").Body, link) {
		t.Error("db-1 is not listed as active after approval")
	}

	// It heartbeats: online, and its detail page lists tail-logs with its pattern.
	e.startRunner("db-1")
	e.waitOnline("db-1")
	waitFor(t, func() bool { return strings.Contains(a.get("/runners?online=yes").Body, link) })
	waitFor(t, func() bool {
		d := a.get("/runners/prod/db-1")
		return d.Status == http.StatusOK && strings.Contains(d.Body, "tail-logs")
	})
	if d := a.get("/runners/prod/db-1").Body; !strings.Contains(d, "[0-9]{1,4}") {
		t.Errorf("the runner detail does not show the pattern of tail-logs")
	}
}

// ---- 7. Revoke cascade ----

func m4RevokeCascade(t *testing.T, f *m4Fixture) {
	e, a, ctx := f.e, f.a.at(t), m3ctx(t)
	e.addOperator("opx")
	cl := e.opClient("opx")
	callCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res := make(chan error, 1)
	go func() {
		_, err := cl.ActionRun(callCtx, api.ActionRunParams{Fleet: "prod", Runner: "db-1", Action: "long", Confirm: "prod"})
		res <- err
	}()
	waitFor(t, func() bool { return e.hasActionRow(service.OutcomeStart) })

	rev := func(typed string) *m4Page {
		return a.post("/runners/prod/db-1/revoke", "/runners/prod/db-1/revoke", url.Values{"confirm": {typed}, "back": {"detail"}})
	}
	for _, typed := range []string{"", "db-1", "dev/db-1"} {
		if r := rev(typed); r.Status != http.StatusUnprocessableEntity {
			t.Errorf("revoke with %q: status %d, want 422", typed, r.Status)
		}
	}
	if r, err := e.db.GetRunner(ctx, store.Ref{Fleet: "prod", Runner: "db-1"}); err != nil || r.Status != store.StatusActive {
		t.Fatalf("db-1 after refused revokes: %+v %v", r, err)
	}
	select {
	case err := <-res:
		t.Fatalf("the action ended after refused revokes: %v", err)
	default:
	}
	if r := rev("prod/db-1"); r.Status != http.StatusSeeOther {
		t.Fatalf("revoke with the name: status %d", r.Status)
	}
	f.did["runner.revoke"]++
	err := waitErr(t, res, 10*time.Second)
	if err == nil {
		t.Fatal("the long action survived the revoke")
	}
	wantCode(t, err, api.CodeUnavailable)
	waitFor(t, func() bool {
		for _, row := range e.auditRowsFor(api.MethodActionRun) {
			if row.Outcome == service.OutcomeCancelled && row.Message == service.RevokeReason {
				return true
			}
		}
		return false
	})
	if service.RevokeReason != "runner revoked" {
		t.Errorf("RevokeReason = %q, want %q", service.RevokeReason, "runner revoked")
	}
	rows := f.guiRows("runner.revoke")
	if len(rows) != 1 || rows[0].Subject != "prod/db-1" || rows[0].Actor != "admin" {
		t.Errorf("runner.revoke audit rows: %+v", rows)
	}
	waitFor(t, func() bool { return len(e.online()) == 0 })
	if !e.authRejected(transport.UserRunner, "db-1") {
		t.Fatal("the revoked runner's key still authenticates")
	}
	if !strings.Contains(a.get("/runners?status=revoked").Body, `href="/runners/prod/db-1"`) {
		t.Error("the revoked runner is not listed under status revoked")
	}
}

// ---- 8. Operators ----

var m4CodeRE = regexp.MustCompile(`id="copy-reg-code">([^<]+)</pre>`)

func m4Operators(t *testing.T, f *m4Fixture) {
	e, a, ctx := f.e, f.a.at(t), m3ctx(t)
	showCode := func() string {
		t.Helper()
		waitCodeWindow()
		p := a.post("/operators", "/operators/code", nil)
		if p.Status != http.StatusOK || !strings.Contains(p.Header.Get("Cache-Control"), "no-store") {
			t.Fatalf("show code: status %d, Cache-Control %q", p.Status, p.Header.Get("Cache-Control"))
		}
		m := m4CodeRE.FindStringSubmatch(p.Body)
		if m == nil {
			t.Fatalf("no registration code in the response:\n%s", p.Body)
		}
		f.did["operator.code"]++
		return m[1]
	}
	if strings.Contains(a.get("/operators").Body, "copy-reg-code") {
		t.Error("the operators list shows a code without a POST")
	}
	register := func(code, name string) error {
		_, err := keys.Generate(e.dir, name, name)
		must(t, err)
		_, _, err = apiclient.Register(ctx, e.addr, code, name, e.signer(name))
		return err
	}

	code := showCode()
	rows := f.guiRows("operator.code")
	if len(rows) != 1 || rows[0].Actor != "admin" {
		t.Fatalf("operator.code audit rows: %+v", rows)
	}
	if b, _ := json.Marshal(rows[0]); strings.Contains(string(b), code) {
		t.Error("the audit row of operator.code contains the code")
	}
	must(t, register(code, "alice"))
	if !strings.Contains(a.get("/operators").Body, "alice") {
		t.Error("alice is not listed after registering")
	}
	al := e.opClient("alice")
	if _, err := al.FleetsList(ctx); err != nil {
		t.Fatalf("alice cannot list fleets: %v", err)
	}

	// Rotate the secret: the old code is refused, a new code works.
	sec, err := e.db.GetSetting(ctx, store.SettingRegistrationSecret)
	must(t, err)
	if p := a.post("/operators/rotate", "/operators/rotate", nil); p.Status != http.StatusSeeOther {
		t.Fatalf("rotate: status %d", p.Status)
	}
	f.did["operator.rotate_secret"]++
	sec2, err := e.db.GetSetting(ctx, store.SettingRegistrationSecret)
	must(t, err)
	if bytes.Equal(sec, sec2) {
		t.Fatal("rotate did not change the registration secret")
	}
	f.secrets = append(f.secrets, sec2)
	wantCode(t, register(code, "carol"), api.CodeUnauthenticated)
	if _, err := e.db.GetOperatorByKey(ctx, e.signer("carol").PublicKey().Marshal()); err == nil {
		t.Fatal("the old code registered an operator after the rotation")
	}
	code2 := showCode()
	if code2 == code {
		t.Fatal("the code did not change with the secret")
	}
	must(t, register(code2, "dave"))

	// Revoke alice: wrong typed names are refused, the right one drops her connection.
	rev := func(typed string) *m4Page {
		return a.post("/operators/alice/revoke", "/operators/alice/revoke", url.Values{"confirm": {typed}})
	}
	for _, typed := range []string{"", "bob", "ALICE"} {
		if r := rev(typed); r.Status != http.StatusUnprocessableEntity {
			t.Errorf("revoke alice with %q: status %d, want 422", typed, r.Status)
		}
	}
	if _, err := al.FleetsList(ctx); err != nil {
		t.Fatalf("alice was cut off by refused revokes: %v", err)
	}
	if r := rev("alice"); r.Status != http.StatusSeeOther {
		t.Fatalf("revoke alice: status %d", r.Status)
	}
	f.did["operator.revoke"]++
	waitFor(t, func() bool { _, err := al.FleetsList(ctx); return err != nil })
	if !e.authRejected(transport.UserOperator, "alice") {
		t.Error("alice's key still authenticates")
	}
	if _, err := e.opClient("dave").FleetsList(ctx); err != nil {
		t.Errorf("dave was affected by alice's revoke: %v", err)
	}
}

// ---- 9. Workspaces ----

func m4Workspaces(t *testing.T, f *m4Fixture) {
	e, a, ctx := f.e, f.a.at(t), m3ctx(t)
	m3Enrol(e, "dev", "wr1", nil)
	f.fakeWS = newM3Fake()
	wc, err := wsconfig.Parse([]byte(m3FakeYAML))
	must(t, err)
	e.startRunner("wr1", func(c *runner.Config) { c.Workspace, c.WorkspaceConfig, c.Actions = f.fakeWS, wc, nil })
	waitFor(t, func() bool {
		for _, si := range e.reg.Snapshot() {
			if si.Name == "wr1" && si.Online && si.Slots == 1 && si.Executor == "containerd" {
				return true
			}
		}
		return false
	})
	e.addOperator("bob")
	bo := e.opClient("bob")
	op, err := bo.WorkspaceOpen(ctx, api.WorkspaceOpenParams{Fleet: "dev", Session: "s1"})
	must(t, err)
	id := op.Workspace.ID
	f.wsID = id
	if n := m4Containers(t, f.fakeWS); n != 1 {
		t.Fatalf("containers after open: %d", n)
	}
	list := a.get("/workspaces")
	for _, want := range []string{"bob", "wr1", "s1", "/workspaces/" + id + "/kill"} {
		if !strings.Contains(list.Body, want) {
			t.Errorf("the workspaces page lacks %q", want)
		}
	}
	if r := a.post("/workspaces", "/workspaces/"+id+"/kill", nil); r.Status != http.StatusSeeOther || !strings.HasPrefix(r.loc(), "/workspaces") {
		t.Fatalf("kill: status %d, Location %q", r.Status, r.loc())
	}
	f.did["workspace.kill"] += 2 // the service writes a start row and an ok row
	if w, err := e.db.GetWorkspace(ctx, id); err != nil || w.State != store.WorkspaceClosed {
		t.Fatalf("workspace after kill: %+v %v", w, err)
	}
	if n := m4Containers(t, f.fakeWS); n != 0 {
		t.Errorf("containers after kill: %d, the fake executor got no destroy", n)
	}
	if strings.Contains(a.get("/workspaces").Body, "/workspaces/"+id+"/kill") {
		t.Error("a closed workspace is still offered for kill in the live list")
	}
	if !strings.Contains(a.get("/workspaces?state=all").Body, "/workspaces/"+id+"/kill") && !strings.Contains(a.get("/workspaces?state=all").Body, id[:8]) {
		t.Error("the closed workspace is missing from the full list")
	}
	if rows := f.guiRows("workspace.kill"); len(rows) != 2 || rows[0].Actor != "admin" || rows[0].Outcome != service.OutcomeStart || rows[1].Outcome != service.OutcomeOK {
		t.Errorf("workspace.kill audit rows: %+v", rows)
	}
}

func m4Containers(t *testing.T, fk *m3Fake) int {
	t.Helper()
	infos, err := fk.List(context.Background())
	must(t, err)
	return len(infos)
}

// ---- 10. Audit ----

func m4Audit(t *testing.T, f *m4Fixture) {
	e, ctx := f.e, m3ctx(t)
	a := f.login(t)
	f.did["gui.login"]++

	// Enough GUI rows for two pages: toggle the guard of dev (on needs no name, off needs it).
	for i := 0; i < 48; i++ {
		if r := a.post("/fleets/dev/guard", "/fleets/dev/guard", url.Values{"to": {"on"}}); r.Status != http.StatusSeeOther {
			t.Fatalf("guard on: status %d", r.Status)
		}
		if r := a.post("/fleets/dev/guard", "/fleets/dev/guard", url.Values{"to": {"off"}, "confirm": {"dev"}}); r.Status != http.StatusSeeOther {
			t.Fatalf("guard off: status %d", r.Status)
		}
		f.did["fleet.set_confirm"] += 2
	}
	// A crafted value from a runner-supplied or operator-supplied field, to see the CSV neutralise it.
	_, err := e.db.RecordAudit(ctx, store.AuditEvent{Time: f.clk.now(), Source: "system", ActorKind: "system", Actor: "=cmd|' /C calc'!A0",
		Action: "test.crafted", Fleet: "prod", Subject: "+1+1", Message: "@SUM(1+1)", Outcome: "ok"})
	must(t, err)
	isGUI := func(r store.AuditEvent) bool { return r.Source == "gui" }

	// Filter source=gui: only GUI rows, newest 50 first.
	gp := a.get("/audit?source=gui")
	if gp.Status != http.StatusOK {
		t.Fatalf("GET /audit?source=gui: status %d", gp.Status)
	}
	all := f.wantIDs(isGUI, 0)
	if len(all) < 100 {
		t.Fatalf("only %d GUI audit rows; the paging check needs two full pages", len(all))
	}
	page1 := m4AuditIDs(gp.Body)
	if !m4EqualIDs(page1, all[:50]) {
		t.Errorf("source=gui page 1 = %v, want %v", page1, all[:50])
	}
	if m4Link(gp.Body, "Newer") != "" {
		t.Error("the first page offers a Newer link")
	}
	// Paging both ways.
	older := m4Link(gp.Body, "Older")
	if older == "" {
		t.Fatal("the first page has no Older link")
	}
	p2 := a.get(older)
	page2 := m4AuditIDs(p2.Body)
	if !m4EqualIDs(page2, all[50:100]) {
		t.Errorf("page 2 = %v, want %v", page2, all[50:100])
	}
	newer := m4Link(p2.Body, "Newer")
	if newer == "" {
		t.Fatal("the second page has no Newer link")
	}
	if back := m4AuditIDs(a.get(newer).Body); !m4EqualIDs(back, page1) {
		t.Errorf("Newer from page 2 = %v, want page 1 %v", back, page1)
	}
	// Filter by fleet and by action prefix.
	byFleet := m4AuditIDs(a.get("/audit?fleet=prod").Body)
	if want := f.wantIDs(func(r store.AuditEvent) bool { return r.Fleet == "prod" }, 50); len(want) == 0 || !m4EqualIDs(byFleet, want) {
		t.Errorf("fleet=prod page = %v, want %v", byFleet, want)
	}
	byPrefix := m4AuditIDs(a.get("/audit?action=runner.").Body)
	if want := f.wantIDs(func(r store.AuditEvent) bool { return strings.HasPrefix(r.Action, "runner.") }, 50); len(want) == 0 || !m4EqualIDs(byPrefix, want) {
		t.Errorf("action=runner. page = %v, want %v", byPrefix, want)
	}
	// The same through the explicit prefix flag, and an invalid filter shows no rows.
	if got := m4AuditIDs(a.get("/audit?action=runner&prefix=1").Body); !m4EqualIDs(got, byPrefix) {
		t.Errorf("action=runner&prefix=1 = %v, want %v", got, byPrefix)
	}
	if bad := a.get("/audit?source=bogus"); bad.Status != http.StatusOK || len(m4AuditIDs(bad.Body)) != 0 {
		t.Errorf("invalid filter: status %d, rows %v", bad.Status, m4AuditIDs(bad.Body))
	}

	// CSV export: header, neutralised formulas, and the audit.export row.
	exportsBefore := len(f.rows())
	cx := a.get("/audit/export?format=csv")
	f.did["audit.export"]++
	if cx.Status != http.StatusOK || !strings.HasPrefix(cx.Header.Get("Content-Type"), "text/csv") ||
		!strings.Contains(cx.Header.Get("Content-Disposition"), "attachment") || !strings.Contains(cx.Header.Get("Cache-Control"), "no-store") {
		t.Fatalf("csv export: status %d, headers %v", cx.Status, cx.Header)
	}
	cr := csv.NewReader(strings.NewReader(cx.Body))
	cr.FieldsPerRecord = -1
	recs, err := cr.ReadAll()
	must(t, err)
	if len(recs) < 3 || recs[0][0] != "id" || recs[0][9] != "action" || recs[0][4] != "actor" {
		t.Fatalf("csv header: %v", recs[0])
	}
	found := false
	for _, rec := range recs[1:] {
		for _, cell := range rec {
			if cell != "" && strings.ContainsRune("=+-@\t\r", rune(cell[0])) {
				t.Errorf("csv cell %q starts with a formula character", cell)
			}
		}
		if rec[9] == "test.crafted" {
			found = true
			if rec[4] != "'=cmd|' /C calc'!A0" || rec[8] != "'+1+1" || rec[14] != "'@SUM(1+1)" {
				t.Errorf("crafted row not neutralised: actor %q subject %q message %q", rec[4], rec[8], rec[14])
			}
		}
	}
	if !found {
		t.Error("the crafted row is missing from the CSV")
	}
	if last := recs[len(recs)-1]; last[9] != "audit.export" || last[2] != "gui" || last[4] != "admin" {
		t.Errorf("the export does not end with its own audit.export row: %v", last)
	}
	if n := len(f.rows()); n != exportsBefore+1 {
		t.Errorf("a CSV export added %d audit rows, want 1", n-exportsBefore)
	}

	// JSON Lines, GUI rows only: every line parses; every GUI change above is there as admin.
	jx := a.get("/audit/export?format=jsonl&source=gui")
	f.did["audit.export"]++
	if jx.Status != http.StatusOK || !strings.HasPrefix(jx.Header.Get("Content-Type"), "application/x-ndjson") {
		t.Fatalf("jsonl export: status %d, Content-Type %q", jx.Status, jx.Header.Get("Content-Type"))
	}
	locals := f.w.localAddrs()
	seen := map[string]int{}
	var lastAction string
	for i, line := range strings.Split(strings.TrimRight(jx.Body, "\n"), "\n") {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("jsonl line %d is not JSON: %v: %.120s", i+1, err, line)
		}
		if row["truncated"] != nil {
			t.Errorf("jsonl line %d is a truncation marker: %v", i+1, row)
			continue
		}
		if row["source"] != "gui" {
			t.Errorf("jsonl line %d has source %v under source=gui", i+1, row["source"])
		}
		act, _ := row["action"].(string)
		lastAction = act
		if act == "gui.lockout" || act == "gui.setup_token" {
			continue // written by the limiter on behalf of an address, not by the admin
		}
		if row["actor"] != "admin" {
			t.Errorf("jsonl row %v (%s) has actor %v", row["id"], act, row["actor"])
		}
		if rem, _ := row["remote"].(string); !locals[rem] {
			t.Errorf("jsonl row %v (%s) has remote %q, not an address a browser dialed from", row["id"], act, rem)
		}
		seen[act]++
	}
	if lastAction != "audit.export" {
		t.Errorf("the jsonl export ends with %q, want its own audit.export row", lastAction)
	}
	for _, act := range []string{"gui.setup", "gui.login", "gui.logout", "fleet.create", "fleet.set_confirm", "token.create", "runner.approve", "runner.revoke",
		"operator.code", "operator.rotate_secret", "operator.revoke", "workspace.kill", "audit.export"} {
		if seen[act] == 0 {
			t.Errorf("no GUI audit row %q in the export", act)
		}
	}
	f.crafted = true
}

// ---- 11. Sessions ----

func m4Sessions11(t *testing.T, f *m4Fixture) {
	htmx := map[string]string{"HX-Request": "true"}

	// A polled fragment does not extend the idle timer.
	s := f.login(t)
	f.did["gui.login"]++
	f.clk.advance(10 * time.Minute)
	if r := s.do("GET", "/runners/live", nil, htmx); r.Status != http.StatusOK {
		t.Fatalf("poll after 10 min: status %d", r.Status)
	}
	f.clk.advance(10 * time.Minute)
	if r := s.do("GET", "/runners/live", nil, htmx); r.Status != http.StatusOK {
		t.Fatalf("poll after 20 min: status %d", r.Status)
	}
	f.clk.advance(11 * time.Minute) // 31 minutes since the last real request
	if r := s.get("/"); !m4Refused(r) || !strings.HasPrefix(r.loc(), "/login") {
		t.Errorf("a normal request after 31 idle minutes (polling meanwhile): status %d, Location %q; want a redirect to /login", r.Status, r.loc())
	}
	// An htmx request gets HX-Redirect instead of a redirect.
	s2 := f.login(t)
	f.did["gui.login"]++
	f.clk.advance(31 * time.Minute)
	if r := s2.do("GET", "/runners/live", nil, htmx); r.Status != http.StatusOK || !strings.HasPrefix(r.Header.Get("HX-Redirect"), "/login") {
		t.Errorf("htmx poll of an idle session: status %d, HX-Redirect %q", r.Status, r.Header.Get("HX-Redirect"))
	}
	// A normal request does extend it.
	s3 := f.login(t)
	f.did["gui.login"]++
	f.clk.advance(20 * time.Minute)
	if r := s3.get("/"); r.Status != http.StatusOK {
		t.Fatalf("normal request after 20 min: status %d", r.Status)
	}
	f.clk.advance(20 * time.Minute)
	if r := s3.get("/"); r.Status != http.StatusOK {
		t.Errorf("normal request 20 min after the last one: status %d; the idle timer was not extended", r.Status)
	}
	// Past 12 hours an active session dies too.
	s4 := f.login(t)
	f.did["gui.login"]++
	var elapsed time.Duration
	for {
		f.clk.advance(29 * time.Minute)
		elapsed += 29 * time.Minute
		r := s4.get("/")
		if elapsed < 12*time.Hour {
			if r.Status != http.StatusOK {
				t.Fatalf("active session refused after %v: status %d", elapsed, r.Status)
			}
			continue
		}
		if !m4Refused(r) {
			t.Errorf("session still alive after %v (status %d); the absolute limit is 12 h", elapsed, r.Status)
		}
		break
	}
	// Logout makes the old cookie useless and deletes the row.
	s5 := f.login(t)
	f.did["gui.login"]++
	cookie := s5.session()
	if !m4HasSession(m4Sessions(t, f.dbPath), cookie) {
		t.Fatal("no gui_sessions row for a live session")
	}
	if r := s5.post("/account", "/logout", nil); r.Status != http.StatusSeeOther || s5.session() != "" {
		t.Fatalf("logout: status %d, cookie %q", r.Status, s5.session())
	}
	f.did["gui.logout"]++
	replay := f.w.newBrowser(t)
	replay.setCookie(gui.SessionCookieName, cookie)
	if r := replay.get("/"); !m4Refused(r) {
		t.Errorf("the cookie of a logged-out session still works: status %d", r.Status)
	}
	if m4HasSession(m4Sessions(t, f.dbPath), cookie) {
		t.Error("the gui_sessions row survived the logout")
	}
	// s3 (and every session but the one made by the next step) is long dead from the 12 h loop.
	if s3.at(t).alive() {
		t.Error("s3 survived the 12 h loop; it was idle for hours")
	}
}

// ---- 12. Restart and credential binding ----

func m4Restart(t *testing.T, f *m4Fixture) {
	ctx := m3ctx(t)
	A, B := f.login(t), f.login(t)
	f.did["gui.login"] += 2
	cookieA, cookieB := A.session(), B.session()
	if cookieA == cookieB || !A.alive() || !B.alive() {
		t.Fatal("two fresh sessions are not both alive and distinct")
	}
	f.captureHashes(t)
	f.checkSessionTable(t)

	// Discard the GUI server and the service; build new ones over the same SQLite file.
	fp := f.srv.Fingerprint()
	db2, err := sqlite.Open(ctx, f.dbPath)
	must(t, err)
	f.rootT.Cleanup(func() { db2.Close() })
	svc2, err := service.New(service.Config{Store: db2, Policy: policy.TablePolicy{}, Live: f.e.reg, HostKey: f.e.hostSigner.PublicKey(),
		Now: f.clk.now, OperatorConns: f.e.ops, Logf: f.e.logf, ReaperInterval: time.Hour})
	must(t, err)
	srv2, ts2 := f.startGUI(svc2)
	if path, created, err := srv2.PrepareSetup(ctx); err != nil || created || path != "" {
		t.Fatalf("PrepareSetup after the restart = %q %v %v", path, created, err)
	}
	if _, err := os.Stat(srv2.SetupTokenPath()); !os.IsNotExist(err) {
		t.Fatalf("a setup token file exists after the restart: %v", err)
	}
	f.ts.Close()
	f.srv, f.ts, f.svc = srv2, ts2, svc2
	f.w.setBase(ts2.URL)
	if srv2.Fingerprint() != fp {
		t.Errorf("the certificate changed over the restart: %s -> %s", fp, srv2.Fingerprint())
	}
	if !A.alive() || !B.alive() {
		t.Fatal("a session did not survive the restart of the GUI and the service")
	}

	// A password change from A: A keeps working with a re-issued cookie, B is refused at once.
	p := A.post("/account", "/account/password", url.Values{"current": {f.pw}, "new": {m4Pass2}, "new2": {m4Pass2}})
	if p.Status != http.StatusSeeOther {
		t.Fatalf("password change: status %d:\n%s", p.Status, p.Body)
	}
	f.pw = m4Pass2
	f.passwords = append(f.passwords, m4Pass2)
	f.did["gui.password_change"]++
	if A.session() == cookieA || A.session() == "" {
		t.Error("the password change did not re-issue A's cookie")
	}
	if !A.alive() {
		t.Error("A was logged out by its own password change")
	}
	if r := B.get("/account"); !m4Refused(r) {
		t.Errorf("B after A's password change: status %d, Location %q; want a redirect to /login", r.Status, r.loc())
	}
	rows := m4Sessions(t, f.dbPath)
	if m4HasSession(rows, cookieB) || m4HasSession(rows, cookieA) || !m4HasSession(rows, A.session()) {
		t.Error("gui_sessions after the password change should hold exactly the re-issued session of A")
	}
	f.captureHashes(t)
	f.checkSessionTable(t)

	// Reset from the admin socket's side: A is refused at once, the table is empty, a token comes back.
	tok, err := svc2.ResetGUIAdmin(ctx, f.e.admin, service.SourceCLI)
	must(t, err)
	if tok == "" {
		t.Fatal("reset-admin returned no setup token")
	}
	f.setupTokens = append(f.setupTokens, tok)
	if r := A.get("/account"); !m4Refused(r) {
		t.Errorf("A after reset-admin: status %d, Location %q", r.Status, r.loc())
	}
	if rows := m4Sessions(t, f.dbPath); len(rows) != 0 {
		t.Errorf("gui_sessions after reset-admin has %d rows", len(rows))
	}
	if rows := m3AuditSource(f, "gui.reset_admin"); len(rows) != 1 || rows[0] != "cli" {
		t.Errorf("gui.reset_admin audit sources: %v, want [cli]", rows)
	}

	// Set up again with the token reset-admin printed, then purge: no expired row is left.
	nb := f.w.newBrowser(t)
	if r := nb.post("/setup", "/setup", url.Values{"token": {tok}, "username": {"admin"}, "password": {m4Pass3}, "password2": {m4Pass3}}); r.Status != http.StatusSeeOther || nb.session() == "" {
		t.Fatalf("setup with the reset token: status %d:\n%s", r.Status, r.Body)
	}
	f.pw = m4Pass3
	f.passwords = append(f.passwords, m4Pass3)
	f.did["gui.setup"]++
	f.did["gui.login"]++ // the setup signed in nb
	x2 := f.login(t)
	f.did["gui.login"]++
	f.clk.advance(13 * time.Hour) // past the idle and the absolute limits of both sessions
	z := f.login(t)
	f.did["gui.login"]++
	n, err := svc2.PurgeGUISessions(ctx)
	must(t, err)
	if n != 2 {
		t.Errorf("PurgeGUISessions removed %d rows, want 2", n)
	}
	now := f.clk.now().UnixMilli()
	rows = m4Sessions(t, f.dbPath)
	if len(rows) != 1 || !m4HasSession(rows, z.session()) {
		t.Fatalf("gui_sessions after the purge: %d rows, want only the live session", len(rows))
	}
	for _, r := range rows {
		if r.ExpiresMS <= now || r.LastActiveMS <= now-int64(30*time.Minute/time.Millisecond) {
			t.Errorf("an expired row survived the purge: %+v (now %d)", r, now)
		}
	}
	if !z.alive() || nb.alive() || x2.alive() {
		t.Error("after the purge only the live session may work")
	}
	f.captureHashes(t)
	f.checkSessionTable(t)
}

// m3AuditSource lists the sources of the audit rows with the given action.
func m3AuditSource(f *m4Fixture, action string) []string {
	var out []string
	for _, r := range f.rows() {
		if r.Action == action {
			out = append(out, r.Source)
		}
	}
	return out
}

// ---- 13. Hygiene ----

func m4Hygiene(t *testing.T, f *m4Fixture) {
	e := f.e
	resps := f.w.responses()
	if len(resps) < 100 {
		t.Fatalf("only %d responses recorded", len(resps))
	}
	// The join token is shown once, in the creation response, and nowhere else.
	if f.joinToken == "" {
		t.Fatal("no join token recorded")
	}
	secretPart := strings.Split(f.joinToken, ".")[2] // mfj1.<id>.<secret>.<host-key hash>
	for _, r := range resps {
		if r.Seq != f.joinSeq && (strings.Contains(r.Text, f.joinToken) || strings.Contains(r.Text, secretPart)) {
			t.Errorf("response %d (%s %s) contains the join token", r.Seq, r.Method, r.Path)
		}
	}
	// Never in any response: the registration secret, a password hash, a setup token, a password,
	// private key material.
	f.captureHashes(t)
	var never []string
	for _, s := range f.secrets {
		never = append(never, m4Encodings(s)...)
	}
	for h := range f.pwHashes {
		never = append(never, h)
	}
	never = append(never, f.setupTokens...)
	never = append(never, f.passwords...)
	never = append(never, "$argon2", "PRIVATE KEY", "BEGIN OPENSSH")
	for _, dir := range []string{e.dir, filepath.Join(f.guiDir, "gui")} {
		ents, _ := os.ReadDir(dir)
		for _, ent := range ents {
			b, err := os.ReadFile(filepath.Join(dir, ent.Name()))
			if err != nil || !bytes.Contains(b, []byte("PRIVATE KEY")) {
				continue
			}
			for _, line := range strings.Split(string(b), "\n") {
				if line = strings.TrimSpace(line); len(line) > 20 && !strings.HasPrefix(line, "-----") {
					never = append(never, line)
				}
			}
		}
	}
	for _, r := range resps {
		for _, n := range never {
			if n != "" && strings.Contains(r.Text, n) {
				t.Errorf("response %d (%s %s) contains a value that must never be served: %.12s...", r.Seq, r.Method, r.Path, n)
			}
		}
	}
	// The central log, the database and its WAL hold no cookie, setup token, password or join secret.
	logText := e.logText()
	dbBytes, _ := os.ReadFile(f.dbPath)
	wal, _ := os.ReadFile(f.dbPath + "-wal")
	dbBytes = append(dbBytes, wal...)
	secretsAbsent := append(append([]string{secretPart}, f.setupTokens...), f.passwords...)
	secretsAbsent = append(secretsAbsent, f.w.sessionCookies()...)
	for _, s := range secretsAbsent {
		if strings.Contains(logText, s) {
			t.Errorf("the central log contains a secret: %.12s...", s)
		}
		if bytes.Contains(dbBytes, []byte(s)) {
			t.Errorf("the database contains a secret in the clear: %.12s...", s)
		}
	}
	f.checkSessionTable(t)

	// Every GUI state change has its audit row with source gui, and nothing the GUI did was
	// recorded as api.
	for action, n := range f.did {
		if got := len(f.guiRows(action)); got != n {
			t.Errorf("audit rows %q with source gui: %d, want %d: %+v", action, got, n, f.guiRows(action))
		}
	}
	adminActions := map[string]bool{"fleet.create": true, "fleet.set_confirm": true, "fleet.workspace_set": true, "token.create": true, "token.revoke": true,
		"runner.approve": true, "runner.disable": true, "runner.enable": true, "runner.revoke": true, "operator.code": true,
		"operator.rotate_secret": true, "operator.revoke": true, "workspace.kill": true, "audit.export": true, "gui.setup": true,
		"gui.login": true, "gui.logout": true, "gui.password_change": true, "gui.session_revoke": true}
	for _, r := range f.rows() {
		if r.Source == "api" && (r.ActorKind != "operator" || adminActions[r.Action] || strings.HasPrefix(r.Action, "gui.")) {
			t.Errorf("audit row %d has source api but looks like an admin action: %+v", r.ID, r)
		}
		if r.Actor == "admin" && r.ActorKind == "admin" && r.Source != "gui" {
			t.Errorf("audit row %d (%s) by the GUI admin has source %q", r.ID, r.Action, r.Source)
		}
	}
	if len(f.guiRows("gui.reset_admin")) != 0 {
		t.Error("gui.reset_admin was recorded with source gui")
	}
}
