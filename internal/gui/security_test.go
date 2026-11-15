package gui

// S14: the GUI security suite (docs/M4.md 5, rows S14, 2.4-2.7, 7 and G-02). The route list is
// derived from the registration code itself (every mux.Handle / mux.HandleFunc call in the
// non-test sources of this package), so a new route cannot be missed; no test hook is needed.

import (
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/service"
)

// ---- route discovery ----

type secRoute struct {
	method, pattern, path string
	passive               bool
}

var (
	routeRE  = regexp.MustCompile(`\b(?:Handle|HandleFunc|Passive)\("(GET|POST|PUT|PATCH|DELETE|HEAD) ([^"]+)"`)
	anyRegRE = regexp.MustCompile(`\b(?:Handle|HandleFunc|Passive)\("`)
	wildRE   = regexp.MustCompile(`\{[^}]*\}`)
)

// secRoutes parses every route registration in the package sources.
func secRoutes(t *testing.T) []secRoute {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var out []secRoute
	calls := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			calls += len(anyRegRE.FindAllString(line, -1))
			m := routeRE.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			out = append(out, secRoute{method: m[1], pattern: m[1] + " " + m[2], path: concretePath(m[2]),
				passive: strings.Contains(line, "Passive(")})
		}
	}
	if calls != len(out) {
		t.Fatalf("found %d route registrations but parsed %d: the route regexp needs updating", calls, len(out))
	}
	if len(out) < 30 {
		t.Fatalf("only %d routes found; route discovery is broken", len(out))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].pattern < out[j].pattern })
	return out
}

func concretePath(p string) string {
	p = strings.ReplaceAll(p, "{$}", "")
	return wildRE.ReplaceAllStringFunc(p, func(w string) string {
		if w == "{op}" {
			return "revoke"
		}
		return "x1"
	})
}

func exemptPath(p string) bool {
	return p == "/login" || p == "/setup" || strings.HasPrefix(p, "/static/")
}

func isRedirect(c int) bool { return c >= 300 && c < 400 }

// secAdminEnv is an env with the admin created, and a signed-in client.
func secAdminEnv(t *testing.T) (*guiHarness, *testClient) {
	t.Helper()
	e := newGUIHarness(t)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("server log:\n%s", e.logText())
		}
	})
	return e, loginForTest(t, e)
}

func rawDB(t *testing.T, e *guiHarness) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(e.dir, "mirafleet.db")+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func isLoginRedirect(rec *httptest.ResponseRecorder) bool {
	return isRedirect(rec.Code) && strings.HasPrefix(rec.Header().Get("Location"), "/login")
}

// ---- authentication on every route ----

func TestSecurityAuthRequiredOnEveryRoute(t *testing.T) {
	e, _ := secAdminEnv(t)
	routes := secRoutes(t)
	anon := e.newClient()
	for _, r := range routes {
		if exemptPath(r.path) {
			continue
		}
		var rec *httptest.ResponseRecorder
		if r.method == "POST" {
			rec = anon.do("POST", r.path, url.Values{"x": {"y"}}, nil)
		} else {
			rec = anon.get(r.path)
		}
		if !isLoginRedirect(rec) {
			t.Errorf("%s anonymous: status %d location %q, want redirect to /login", r.pattern, rec.Code, rec.Header().Get("Location"))
		}
		if len(rec.Result().Cookies()) > 0 {
			for _, ck := range rec.Result().Cookies() {
				if ck.Name == SessionCookieName && ck.Value != "" {
					t.Errorf("%s anonymous: set a session cookie", r.pattern)
				}
			}
		}
		if b := rec.Body.String(); strings.Contains(b, testUser) || strings.Contains(b, "central-01") {
			t.Errorf("%s anonymous: body leaks data: %q", r.pattern, b)
		}
		// htmx requests get HX-Redirect, not a body.
		hx := anon.do(r.method, r.path, map[bool]url.Values{true: {"x": {"y"}}, false: nil}[r.method == "POST"],
			func(q *http.Request) { q.Header.Set("HX-Request", "true") })
		if hx.Code != 200 || !strings.HasPrefix(hx.Header().Get("HX-Redirect"), "/login") {
			t.Errorf("%s anonymous htmx: status %d HX-Redirect %q", r.pattern, hx.Code, hx.Header().Get("HX-Redirect"))
		}
	}
	// The exempt routes behave as documented once an admin exists.
	if rec := anon.get("/login"); rec.Code != 200 {
		t.Errorf("GET /login = %d", rec.Code)
	}
	if rec := anon.get("/setup"); rec.Code != http.StatusNotFound {
		t.Errorf("GET /setup after setup = %d, want 404", rec.Code)
	}
	if rec := anon.do("POST", "/setup", url.Values{"x": {"y"}}, nil); rec.Code != http.StatusNotFound {
		t.Errorf("POST /setup after setup = %d, want 404", rec.Code)
	}
}

// ---- CSRF layers on every POST ----

type csrfCase struct {
	name string
	want int
	mod  func(f url.Values) (url.Values, func(*http.Request))
}

func csrfCases(good, other string) []csrfCase {
	set := func(tok string) func(url.Values) (url.Values, func(*http.Request)) {
		return func(f url.Values) (url.Values, func(*http.Request)) {
			if tok != "" {
				f.Set(CSRFFormField, tok)
			}
			return f, nil
		}
	}
	return []csrfCase{
		{"cross-site fetch metadata", 403, func(f url.Values) (url.Values, func(*http.Request)) {
			f.Set(CSRFFormField, good)
			return f, func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }
		}},
		{"foreign Origin", 403, func(f url.Values) (url.Values, func(*http.Request)) {
			f.Set(CSRFFormField, good)
			return f, func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }
		}},
		{"missing token", 403, set("")},
		{"wrong token", 403, set(strings.Repeat("A", 43))},
		{"other session's token", 403, set(other)},
		{"token in the wrong header only", 403, func(f url.Values) (url.Values, func(*http.Request)) {
			return f, func(r *http.Request) { r.Header.Set(CSRFHeader, other) }
		}},
	}
}

func TestSecurityCSRFOnEveryPost(t *testing.T) {
	e, c := secAdminEnv(t)
	c2 := loginForTest(t, e)
	good, other := c.token("/account"), c2.token("/account")
	if good == other {
		t.Fatal("two sessions share a CSRF token")
	}
	routes := secRoutes(t)
	nPost := 0
	for _, r := range routes {
		if r.method != "POST" || exemptPath(r.path) {
			continue
		}
		nPost++
		for _, cs := range csrfCases(good, other) {
			form, mod := cs.mod(url.Values{})
			rec := c.do("POST", r.path, form, mod)
			if rec.Code != cs.want {
				t.Errorf("%s with %s: status %d, want %d", r.pattern, cs.name, rec.Code, cs.want)
			}
		}
	}
	if nPost < 10 {
		t.Fatalf("only %d session POST routes found", nPost)
	}
	// Both sessions are still alive: refused posts have no side effects on the session.
	if rec := c.get("/account"); rec.Code != 200 {
		t.Errorf("session lost after refused posts: %d", rec.Code)
	}
	// /login: its own (pre-session) token; the signed-in session's token is not accepted.
	anon, anon2 := e.newClient(), e.newClient()
	pre, preOther := anon.token("/login"), anon2.token("/login")
	cases := append(csrfCases(pre, preOther), csrfCase{"session token on the login form", 403,
		func(f url.Values) (url.Values, func(*http.Request)) { f.Set(CSRFFormField, good); return f, nil }})
	for _, cs := range cases {
		form, mod := cs.mod(url.Values{"username": {testUser}, "password": {testPW}})
		if rec := anon.do("POST", "/login", form, mod); rec.Code != cs.want {
			t.Errorf("POST /login with %s: status %d, want %d", cs.name, rec.Code, cs.want)
		}
	}
	if len(anon.jar) > 1 || anon.jar[SessionCookieName] != "" {
		t.Errorf("a refused login set a session: %v", anon.jar)
	}
}

func TestSecurityCSRFOnSetup(t *testing.T) {
	e := newGUIHarness(t)
	if _, created, err := e.srv.PrepareSetup(t.Context()); err != nil || !created {
		t.Fatal(created, err)
	}
	a, b := e.newClient(), e.newClient()
	good, other := a.token("/setup"), b.token("/setup")
	form := url.Values{"token": {"nope"}, "username": {testUser}, "password": {testPW}, "password2": {testPW}}
	for _, cs := range csrfCases(good, other) {
		f := url.Values{}
		for k, v := range form {
			f[k] = v
		}
		f, mod := cs.mod(f)
		if rec := a.do("POST", "/setup", f, mod); rec.Code != cs.want {
			t.Errorf("POST /setup with %s: status %d, want %d", cs.name, rec.Code, cs.want)
		}
	}
	if open, _ := e.svc.GUISetupOpen(t.Context()); !open {
		t.Error("setup completed through a refused post")
	}
}

// ---- security headers, Cache-Control ----

var secWantHeaders = map[string]string{
	"Content-Security-Policy":      "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; font-src 'self'; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'",
	"X-Content-Type-Options":       "nosniff",
	"Referrer-Policy":              "no-referrer",
	"X-Frame-Options":              "DENY",
	"Cross-Origin-Opener-Policy":   "same-origin",
	"Cross-Origin-Resource-Policy": "same-origin",
	"Permissions-Policy":           "camera=(), microphone=(), geolocation=(), usb=(), payment=(), clipboard-read=()",
}

func TestSecurityHeadersAndCacheControlOnEveryResponse(t *testing.T) {
	e, c := secAdminEnv(t)
	routes := secRoutes(t)
	anon := e.newClient()
	type sample struct {
		label, path string
		rec         *httptest.ResponseRecorder
	}
	var all []sample
	add := func(label, path string, rec *httptest.ResponseRecorder) { all = append(all, sample{label, path, rec}) }
	for _, r := range routes {
		if r.method == "POST" {
			add("anon "+r.pattern, r.path, anon.do("POST", r.path, url.Values{}, nil))
			add("authed no-csrf "+r.pattern, r.path, c.do("POST", r.path, url.Values{}, nil))
		} else {
			add("anon "+r.pattern, r.path, anon.get(r.path))
			add("authed "+r.pattern, r.path, c.get(r.path))
		}
	}
	add("404", "/no/such/page", c.get("/no/such/page"))
	add("404 anon", "/no/such/page", anon.get("/no/such/page"))
	add("405", "/fleets", c.do("DELETE", "/fleets", nil, nil))
	add("415", "/login", anon.do("POST", "/login", nil, func(r *http.Request) { r.Header.Set("Content-Type", "application/json") }))
	add("421", "/", anon.do("GET", "/", nil, func(r *http.Request) { r.Host = "evil.example" }))
	add("413", "/login", anon.do("POST", "/login", nil, func(r *http.Request) {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.ContentLength = 1 << 30
	}))
	// A content-hashed static file, found through the layout.
	var static []string
	for _, m := range regexp.MustCompile(`(?:src|href)="(/static/[^"]+)"`).FindAllStringSubmatch(c.get("/").Body.String(), -1) {
		static = append(static, m[1])
	}
	if len(static) < 3 {
		t.Fatalf("expected static references in the layout, got %v", static)
	}
	for _, p := range static {
		rec := anon.get(p)
		add("static", p, rec)
		if rec.Code != 200 || rec.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
			t.Errorf("static %s: %d Cache-Control %q", p, rec.Code, rec.Header().Get("Cache-Control"))
		}
	}
	add("static 404", "/static/nope/x.css", anon.get("/static/nope/x.css"))
	add("static not hashed", "/static/htmx.min.js", anon.get("/static/htmx.min.js"))

	for _, s := range all {
		for k, v := range secWantHeaders {
			if got := s.rec.Header().Get(k); got != v {
				t.Errorf("%s (%s): header %s = %q, want %q", s.label, s.path, k, got, v)
			}
		}
		if s.rec.Header().Get("X-Request-ID") == "" {
			t.Errorf("%s: no X-Request-ID", s.label)
		}
		cc := s.rec.Header().Get("Cache-Control")
		ct := s.rec.Header().Get("Content-Type")
		static200 := s.label == "static" && s.rec.Code == 200
		if static200 {
			continue
		}
		if cc != "no-store" {
			t.Errorf("%s (%s): status %d type %q Cache-Control %q, want no-store", s.label, s.path, s.rec.Code, ct, cc)
		}
	}
}

// ---- polled fragments are Passive ----

var (
	everyTagRE = regexp.MustCompile(`<[^>]*\shx-trigger="[^"]*\bevery\b[^"]*"[^>]*>`)
	hxGetRE    = regexp.MustCompile(`\shx-get="([^"]*)"`)
)

func TestSecurityPolledFragmentsArePassive(t *testing.T) {
	e, c := secAdminEnv(t)
	routes := secRoutes(t)
	passivePaths := map[string]bool{}
	for _, r := range routes {
		if r.passive {
			if r.method != "GET" {
				t.Errorf("passive route %s is not a GET", r.pattern)
			}
			passivePaths[r.path] = true
		}
	}
	if len(passivePaths) < 3 {
		t.Fatalf("expected at least 3 passive routes, got %v", passivePaths)
	}
	polled := map[string]bool{}
	for _, r := range routes {
		if r.method != "GET" || strings.Contains(r.pattern, "{") && !strings.Contains(r.pattern, "{$}") || strings.HasPrefix(r.path, "/static/") {
			continue
		}
		rec := c.get(r.path)
		for _, tag := range everyTagRE.FindAllString(rec.Body.String(), -1) {
			m := hxGetRE.FindStringSubmatch(tag)
			if m == nil {
				t.Errorf("%s: polling element without hx-get: %s", r.path, tag)
				continue
			}
			u, err := url.Parse(strings.ReplaceAll(m[1], "&amp;", "&"))
			if err != nil {
				t.Errorf("%s: bad polled URL %q", r.path, m[1])
				continue
			}
			polled[u.Path] = true
			if !passivePaths[u.Path] {
				t.Errorf("%s polls %q, which is not registered Passive", r.path, u.Path)
			}
		}
	}
	for p := range passivePaths {
		if !polled[p] {
			t.Errorf("passive route %s is polled by no page", p)
		}
	}
	if len(polled) < 3 {
		t.Errorf("only %d polled fragments found: %v", len(polled), polled)
	}

	// Behaviour: a passive request never touches last-active; an ordinary one does.
	db := rawDB(t, e)
	lastActive := func() int64 {
		var v int64
		if err := db.QueryRow(`SELECT last_active_at FROM gui_sessions ORDER BY id DESC LIMIT 1`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	for p := range passivePaths {
		before := lastActive()
		e.clk.advance(2 * service.GUITouchEvery)
		if rec := c.get(p); rec.Code != 200 {
			t.Fatalf("passive %s: %d", p, rec.Code)
		}
		if after := lastActive(); after != before {
			t.Errorf("passive %s touched last_active_at", p)
		}
	}
	before := lastActive()
	if rec := c.get("/fleets"); rec.Code != 200 {
		t.Fatalf("/fleets: %d", rec.Code)
	}
	if lastActive() == before {
		t.Error("an ordinary request did not touch last_active_at (the passive check proves nothing)")
	}
}

// ---- before an admin exists ----

func TestSecurityBeforeSetupEverythingGoesToSetup(t *testing.T) {
	e := newGUIHarness(t)
	if _, created, err := e.srv.PrepareSetup(t.Context()); err != nil || !created {
		t.Fatal(created, err)
	}
	routes := secRoutes(t)
	leaks := []string{testUser, "central-01", "192.0.2.10", "central.example.org:2222", "SHA256:hostkeyfp", "test-1.2.3"}
	for _, r := range routes {
		if r.path == "/setup" || strings.HasPrefix(r.path, "/static/") {
			continue
		}
		c := e.newClient()
		var rec *httptest.ResponseRecorder
		if r.method == "POST" {
			rec = c.do("POST", r.path, url.Values{"x": {"y"}}, nil)
		} else {
			rec = c.get(r.path)
		}
		if r.pattern == "POST /login" && rec.Code == http.StatusForbidden {
			continue // no pre-session token exists before setup: refused outright
		}
		if !isRedirect(rec.Code) {
			t.Errorf("%s before setup: status %d, want a redirect", r.pattern, rec.Code)
			continue
		}
		for _, l := range leaks {
			if strings.Contains(rec.Body.String(), l) {
				t.Errorf("%s before setup leaks %q", r.pattern, l)
			}
		}
		// Follow the redirects: the browser must end on the setup page.
		cur, final := rec, rec
		for i := 0; i < 4 && isRedirect(cur.Code); i++ {
			final = c.get(cur.Header().Get("Location"))
			cur = final
		}
		loc := rec.Header().Get("Location")
		if !strings.HasPrefix(loc, "/setup") && !strings.HasPrefix(loc, "/login") {
			t.Errorf("%s before setup redirects to %q", r.pattern, loc)
		}
		if strings.Contains(final.Body.String(), leaks[5]) {
			t.Errorf("%s: the setup page shows the version", r.pattern)
		}
		if final.Code != 200 || !strings.Contains(final.Body.String(), `action="/setup"`) {
			t.Errorf("%s before setup does not end on the setup form (status %d)", r.pattern, final.Code)
		}
	}
	// Error pages are exempt but reveal nothing.
	c := e.newClient()
	rec := c.get("/no/such/page")
	if rec.Code != 404 {
		t.Errorf("unknown path = %d", rec.Code)
	}
	for _, l := range append(leaks[:4:4], leaks[5:]...) {
		if strings.Contains(rec.Body.String(), l) {
			t.Errorf("404 page leaks %q", l)
		}
	}
	if st := c.get("/static/"); st.Code == 500 {
		t.Errorf("/static/ = %d", st.Code)
	}
	if strings.Contains(e.seenText(), testPW) {
		t.Error("password in a response")
	}
}

// ---- the stolen database copy ----

func tokenEncodings(tok string) map[string]string {
	raw, _ := base64.RawURLEncoding.DecodeString(tok)
	out := map[string]string{
		"token": tok, "raw": string(raw), "hex": hex.EncodeToString(raw), "HEX": strings.ToUpper(hex.EncodeToString(raw)),
		"std": base64.StdEncoding.EncodeToString(raw), "rawstd": base64.RawStdEncoding.EncodeToString(raw),
		"urlpad": base64.URLEncoding.EncodeToString(raw), "escaped": url.QueryEscape(tok),
	}
	if k := CSRFKey(tok); k != nil {
		out["csrf-key-hex"] = hex.EncodeToString(k)
		out["csrf-form-token"] = FormToken(k)
	}
	return out
}

// dumpTable returns every cell of the table in every plausible text form.
func dumpTable(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT * FROM ` + table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		for _, v := range vals {
			switch x := v.(type) {
			case nil:
			case []byte:
				out = append(out, string(x), hex.EncodeToString(x), base64.RawURLEncoding.EncodeToString(x),
					base64.StdEncoding.EncodeToString(x))
			case string:
				out = append(out, x)
			default:
				out = append(out, fmt.Sprint(x))
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSecurityDBCopyYieldsNoCookie(t *testing.T) {
	e := newGUIHarness(t)
	live := map[string]bool{}
	harvest := func(c *testClient) {
		for _, v := range c.jar {
			if len(v) == 43 {
				live[v] = true
			}
		}
	}
	// Setup, with the setup token captured from its file.
	path, created, err := e.srv.PrepareSetup(t.Context())
	if err != nil || !created {
		t.Fatal(created, err)
	}
	rawTok, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	setupTok := strings.TrimSpace(string(rawTok))
	db := rawDB(t, e)
	setupRows := dumpTable(t, db, "gui_setup")
	if len(setupRows) == 0 {
		t.Fatal("gui_setup is empty before setup completes")
	}
	c1 := e.newClient()
	form := url.Values{"token": {setupTok}, "username": {testUser}, "password": {testPW}, "password2": {testPW}, "_csrf": {c1.token("/setup")}}
	harvest(c1)
	if rec := c1.do("POST", "/setup", form, nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("setup: %d", rec.Code)
	}
	harvest(c1)
	// More sessions: logins, a failed login, a logout, a password-less probe.
	login := func() *testClient {
		c := e.newClient()
		tok := c.token("/login")
		harvest(c)
		rec := c.do("POST", "/login", url.Values{"username": {testUser}, "password": {testPW}, "_csrf": {tok}}, nil)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("login: %d", rec.Code)
		}
		harvest(c)
		return c
	}
	c2, c3 := login(), login()
	bad := e.newClient()
	bt := bad.token("/login")
	harvest(bad)
	bad.do("POST", "/login", url.Values{"username": {testUser}, "password": {"wrong password here"}, "_csrf": {bt}}, nil)
	if rec := c3.post("/account", "/logout", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("logout: %d", rec.Code)
	}
	for _, c := range []*testClient{c1, c2} {
		if rec := c.get("/account"); rec.Code != 200 {
			t.Fatalf("session died: %d", rec.Code)
		}
	}
	if len(live) < 5 {
		t.Fatalf("expected at least 5 live tokens (3 sessions, pre-session cookies), got %d", len(live))
	}
	live[setupTok] = true // not a cookie, but equally must not be stored

	var stored []string
	stored = append(stored, setupRows...)
	stored = append(stored, dumpTable(t, db, "gui_sessions")...)
	stored = append(stored, dumpTable(t, db, "audit")...)
	seen := map[string]bool{}
	var uniq []string
	for _, v := range stored {
		if v != "" && !seen[v] {
			seen[v] = true
			uniq = append(uniq, v)
		}
	}
	if len(uniq) < 20 {
		t.Fatalf("suspiciously few stored values: %d", len(uniq))
	}

	// 1. no stored value (or the base64url of a stored 32-byte blob) works as a session cookie.
	attacker := e.newClient()
	for _, v := range uniq {
		attacker.jar = map[string]string{SessionCookieName: v}
		if rec := attacker.get("/account"); !isLoginRedirect(rec) {
			t.Errorf("stored value %q accepted as a cookie: status %d", clipForLog(v), rec.Code)
		}
		if _, _, err := e.svc.GUIAuthenticate(t.Context(), "198.51.100.99:1", v, true); err == nil {
			t.Errorf("stored value %q authenticates", clipForLog(v))
		}
	}
	// 2. no stored value equals or contains a live token or one of its encodings, and vice versa.
	for tok := range live {
		for name, enc := range tokenEncodings(tok) {
			if enc == "" {
				continue
			}
			for _, v := range uniq {
				if v == enc || strings.Contains(v, enc) || (len(v) >= 16 && strings.Contains(enc, v)) {
					t.Errorf("stored value %q equals or contains the %s encoding of a live token", clipForLog(v), name)
				}
			}
			if strings.Contains(e.logText(), enc) {
				t.Errorf("the log contains the %s encoding of a live token", name)
			}
		}
	}
	// The attempts did not harm the real sessions.
	if rec := c1.get("/account"); rec.Code != 200 {
		t.Errorf("real session refused after the probe: %d", rec.Code)
	}
}

func clipForLog(v string) string {
	if len(v) > 24 {
		return v[:24] + "..."
	}
	return v
}

// ---- tampered session rows ----

func TestSecurityTamperedSessionRowRefusedAndDeleted(t *testing.T) {
	e, first := secAdminEnv(t)
	db := rawDB(t, e)
	if rec := first.get("/account"); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	msFrom := func(d time.Duration) int64 { return e.clk.now().Add(d).UnixMilli() }
	cases := []struct {
		name, set string
		args      func() []any
	}{
		{"other generation", `generation = generation + 1`, func() []any { return nil }},
		{"absolute expiry passed", `expires_at = ?`, func() []any { return []any{msFrom(-time.Second)} }},
		{"idle expiry passed", `last_active_at = ?`, func() []any { return []any{msFrom(-service.GUIIdleTimeout - time.Second)} }},
	}
	for _, tc := range cases {
		c := loginForTest(t, e)
		var id int64
		if err := db.QueryRow(`SELECT id FROM gui_sessions ORDER BY id DESC LIMIT 1`).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if rec := c.get("/account"); rec.Code != 200 {
			t.Fatalf("%s: fresh session refused: %d", tc.name, rec.Code)
		}
		if _, err := db.Exec(`UPDATE gui_sessions SET `+tc.set+` WHERE id = ?`, append(tc.args(), id)...); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{"/account", "/overview/live", "/fleets"} {
			if rec := c.get(p); !isLoginRedirect(rec) {
				t.Errorf("%s: GET %s with an edited row: status %d, want redirect to /login", tc.name, p, rec.Code)
			}
			if p == "/account" {
				var n int
				if err := db.QueryRow(`SELECT count(*) FROM gui_sessions WHERE id = ?`, id).Scan(&n); err != nil {
					t.Fatal(err)
				}
				if n != 0 {
					t.Errorf("%s: the refused session row was not deleted", tc.name)
				}
			}
		}
	}
	// The untouched first session survived all of this.
	if rec := first.get("/account"); rec.Code != 200 {
		t.Errorf("unrelated session refused: %d", rec.Code)
	}
}

// ---- template lint (G-02) ----

const htmxConfigWant = `{"allowEval":false,"allowScriptTags":false,"includeIndicatorStyles":false,"historyCacheSize":0,"refreshOnHistoryMiss":true,"selfRequestsOnly":true,"defaultSwapStyle":"outerHTML","responseHandling":[{"code":"204","swap":false},{"code":"[23]..","swap":true},{"code":"422","swap":true},{"code":"[45]..","swap":false,"error":true}]}`

var (
	tagRE        = regexp.MustCompile(`(?s)<[a-zA-Z][^>]*>`)
	onAttrRE     = regexp.MustCompile(`(?i)\son[a-z]+\s*=`)
	styleAttrRE  = regexp.MustCompile(`(?i)\sstyle\s*=`)
	hxVarsRE     = regexp.MustCompile(`(?i)hx-vars`)
	jsColonRE    = regexp.MustCompile(`(?i)\bjs:`)
	javascriptRE = regexp.MustCompile(`(?i)javascript:`)
	dataHxRE     = regexp.MustCompile(`(?i)\sdata-hx-`)
	hxAttrRE     = regexp.MustCompile(`(?i)\s(hx-[a-z0-9:_.-]*)`)
	hxTriggerRE  = regexp.MustCompile(`(?is)\shx-trigger\s*=\s*(?:"([^"]*)"|'([^']*)')`)
	scriptRE     = regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script\s*>`)
	srcAttrRE    = regexp.MustCompile(`(?i)\ssrc\s*=\s*["'{]`)
	styleTagRE   = regexp.MustCompile(`(?i)<style\b`)
	triggerOKRE  = regexp.MustCompile(`^(?:load|every [0-9]+s|change|submit|keyup changed delay:[0-9]+s)(?:\s*,\s*(?:load|every [0-9]+s|change|submit|keyup changed delay:[0-9]+s))*$`)
	// Source-only: the G-02 expressions applied to the raw template text.
	sourceRE = map[string]*regexp.Regexp{
		"on* handler": regexp.MustCompile(`(?i)\son[a-z]+\s*=`),
		"style attr":  regexp.MustCompile(`(?i)\sstyle\s*=`),
		"hx-vars":     hxVarsRE, "js:": jsColonRE, "data-hx-": dataHxRE,
	}
	allowedHx = map[string]bool{"hx-get": true, "hx-post": true, "hx-target": true, "hx-swap": true, "hx-trigger": true,
		"hx-select": true, "hx-confirm": true, "hx-indicator": true, "hx-headers": true, "hx-disabled-elt": true, "hx-include": true}
)

// lintHTML returns the violations of docs/M4.md 2.7 / G-02 in an HTML (or template) text.
func lintHTML(s string, source bool) []string {
	var bad []string
	for _, tag := range tagRE.FindAllString(s, -1) {
		if tag == "" {
			continue
		}
		for name, re := range map[string]*regexp.Regexp{"on* handler": onAttrRE, "style attribute": styleAttrRE, "hx-vars": hxVarsRE,
			"js:": jsColonRE, "javascript:": javascriptRE, "data-hx-*": dataHxRE} {
			if re.MatchString(tag) {
				bad = append(bad, name+" in "+tag)
			}
		}
		for _, m := range hxAttrRE.FindAllStringSubmatch(tag, -1) {
			if n := strings.ToLower(m[1]); !allowedHx[n] {
				bad = append(bad, "htmx attribute "+n+" not allowed in "+tag)
			}
		}
		for _, m := range hxTriggerRE.FindAllStringSubmatch(tag, -1) {
			v := m[1] + m[2]
			if strings.Contains(v, "[") {
				bad = append(bad, "[ in hx-trigger "+v)
			} else if !strings.Contains(v, "{{") && !triggerOKRE.MatchString(strings.TrimSpace(v)) {
				bad = append(bad, "hx-trigger form not allowed: "+v)
			}
		}
	}
	if source {
		for name, re := range sourceRE {
			if re.MatchString(s) {
				bad = append(bad, "raw text: "+name)
			}
		}
	}
	for _, m := range scriptRE.FindAllStringSubmatch(s, -1) {
		if strings.TrimSpace(m[2]) != "" {
			bad = append(bad, "non-empty script body")
		}
		if !srcAttrRE.MatchString(m[1]) {
			bad = append(bad, "script without src")
		}
	}
	if n, m := len(regexp.MustCompile(`(?i)<script\b`).FindAllString(s, -1)), len(scriptRE.FindAllString(s, -1)); n != m {
		bad = append(bad, "unterminated <script")
	}
	if styleTagRE.MatchString(s) {
		bad = append(bad, "<style> element")
	}
	return bad
}

// htmlSamples renders the pages a browser sees: sign-in, setup, error, and every screen.
func htmlSamples(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	pre := newGUIHarness(t)
	if _, created, err := pre.srv.PrepareSetup(t.Context()); err != nil || !created {
		t.Fatal(created, err)
	}
	pc := pre.newClient()
	out["setup"] = pc.get("/setup").Body.String()
	out["setup 404"] = pc.get("/nope").Body.String()
	e, c := secAdminEnv(t)
	anon := e.newClient()
	out["login"] = anon.get("/login").Body.String()
	out["404"] = anon.get("/nope").Body.String()
	out["403"] = c.do("POST", "/fleets", url.Values{}, nil).Body.String()
	for _, r := range secRoutes(t) {
		if r.method != "GET" || strings.HasPrefix(r.path, "/static/") || r.pattern == "GET /audit/export" {
			continue
		}
		rec := c.get(r.path)
		if strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
			out[r.pattern+" ("+fmt.Sprint(rec.Code)+")"] = rec.Body.String()
		}
	}
	// Screens with data: a fleet page and the create/guard forms.
	if rec := c.post("/fleets/new", "/fleets", url.Values{"name": {"alpha"}, "mode": {"actions"}}); isRedirect(rec.Code) {
		for _, p := range []string{"/fleets/alpha", "/fleets/alpha/workspace", "/fleets/alpha/guard"} {
			out[p] = c.get(p).Body.String()
		}
	}
	return out
}

func TestTemplateLintSelfTest(t *testing.T) {
	for _, bad := range []string{
		`<a onclick="x()">`, `<a  onMouseOver = "x()">`, `<p style="color:red">`, `<div hx-vars="a:1">`, `<div hx-get="js:foo">`,
		`<div hx-trigger="click[ctrlKey]">`, `<div data-hx-get="/x">`, `<div hx-on:click="x()">`, `<div hx-boost="true">`,
		`<div hx-push-url="true">`, `<div hx-trigger="mouseenter">`, `<script>alert(1)</script>`, `<script src="/a.js">x</script>`,
		`<script>`, `<a href="javascript:x()">`, `<style>a{}</style>`, `<script defer></script>`,
	} {
		if len(lintHTML(bad, true)) == 0 {
			t.Errorf("lint accepts %s", bad)
		}
	}
	for _, ok := range []string{
		`<script src="/static/x/a.js" defer></script>`, `<div hx-get="/x" hx-trigger="every 10s" hx-swap="outerHTML">`,
		`<input hx-trigger="keyup changed delay:1s" hx-include="#a" hx-select="#b">`, `<body hx-headers='{"X-CSRF-Token":"t"}'>`,
		`<form hx-post="{{.Data.Action}}" hx-confirm="Sure?" hx-indicator="#i" hx-disabled-elt="this" hx-target="#t">`,
		`<p>based on this = that, style: bold, js: not a tag</p>`,
	} {
		if bad := lintHTML(ok, false); len(bad) != 0 {
			t.Errorf("lint rejects %s: %v", ok, bad)
		}
	}
}

func TestTemplateLintSources(t *testing.T) {
	files, err := filepath.Glob("templates/*.html")
	if err != nil || len(files) < 10 {
		t.Fatalf("templates: %v %v", files, err)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range lintHTML(string(raw), true) {
			t.Errorf("%s: %s", f, b)
		}
	}
}

func TestTemplateLintRenderedPages(t *testing.T) {
	samples := htmlSamples(t)
	if len(samples) < 15 {
		t.Fatalf("only %d rendered pages", len(samples))
	}
	for name, body := range samples {
		if body == "" {
			t.Errorf("%s: empty body", name)
		}
		for _, b := range lintHTML(body, false) {
			t.Errorf("%s: %s", name, b)
		}
	}
}

func TestTemplateHTMXConfigMetaExact(t *testing.T) {
	re := regexp.MustCompile(`<meta name="htmx-config" content='([^']*)'>`)
	var want any
	if err := json.Unmarshal([]byte(htmxConfigWant), &want); err != nil {
		t.Fatal(err)
	}
	pages := 0
	for name, body := range htmlSamples(t) {
		if !strings.Contains(body, "<html") {
			continue // a fragment
		}
		pages++
		ms := re.FindAllStringSubmatch(body, -1)
		if len(ms) != 1 {
			t.Errorf("%s: %d htmx-config metas, want exactly 1", name, len(ms))
			continue
		}
		if ms[0][1] != htmxConfigWant {
			t.Errorf("%s: htmx-config differs from docs/M4.md 2.7:\n%s", name, ms[0][1])
		}
		var got any
		if err := json.Unmarshal([]byte(ms[0][1]), &got); err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%s: htmx-config JSON: %v", name, err)
		}
	}
	if pages < 10 {
		t.Errorf("only %d full pages checked", pages)
	}
}

var (
	trustedTypeRE = regexp.MustCompile(`\btemplate\.(HTML|JS|JSStr|CSS|URL|HTMLAttr|Srcset)\b`)
	funcDeclRE    = regexp.MustCompile(`^func\s+(?:\([^)]*\)\s*)?([A-Za-z0-9_]+)`)
	// allowedTrustedHelpers are the functions allowed to mint trusted template types. There are none
	// today; the single sanctioned helper, if one is ever needed, is listed here by name.
	allowedTrustedHelpers = map[string]bool{"trustedHTML": true}
)

func TestTemplateNoTrustedTypesOutsideHelper(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	used := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		fn := ""
		for i, line := range strings.Split(string(raw), "\n") {
			if m := funcDeclRE.FindStringSubmatch(line); m != nil {
				fn = m[1]
			}
			if strings.HasPrefix(strings.TrimSpace(line), "//") || !trustedTypeRE.MatchString(line) {
				continue
			}
			used++
			if !allowedTrustedHelpers[fn] {
				t.Errorf("%s:%d: %s outside the one helper (in %q)", f, i+1, trustedTypeRE.FindString(line), fn)
			}
		}
	}
	if used > 1 {
		t.Errorf("%d uses of trusted template types; at most the one helper is allowed", used)
	}
}
