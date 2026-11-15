package gui

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
)

var (
	inlineScriptRE = regexp.MustCompile(`(?i)<script(\s[^>]*)?>\s*[^<\s]`)
	inlineHandler  = regexp.MustCompile(`(?i)\son[a-z]+\s*=`)
	inlineStyleRE  = regexp.MustCompile(`(?i)\sstyle\s*=|<style`)
)

func assertInert(t *testing.T, name, body string) {
	t.Helper()
	if inlineScriptRE.MatchString(body) || inlineHandler.MatchString(body) || inlineStyleRE.MatchString(body) {
		t.Errorf("%s has inline script, style or handler:\n%s", name, body)
	}
}

func TestSetupFlow(t *testing.T) {
	e := newAuthEnv(t)
	ctx := context.Background()
	c := e.newClient()

	// Before an admin exists everything but /setup, /static/ and error pages goes to /setup.
	for _, p := range []string{"/account", "/login", "/account?x=1"} {
		rec := c.get(p)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/setup" {
			t.Errorf("GET %s before setup = %d %q, want 303 /setup", p, rec.Code, rec.Header().Get("Location"))
		}
	}
	if rec := c.get("/nothing-here"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown path = %d, want the 404 page", rec.Code)
	}

	path, created, err := e.srv.PrepareSetup(ctx)
	if err != nil || !created {
		t.Fatalf("PrepareSetup = %q %v %v", path, created, err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v, mode %v, want 0600", err, st)
	}
	raw, _ := os.ReadFile(path)
	tok := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(tok, "mfs1.") {
		t.Fatalf("token file content %q", tok)
	}
	if _, created, _ := e.srv.PrepareSetup(ctx); created {
		t.Error("a second PrepareSetup created another token")
	}

	rec := c.get("/setup")
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, e.srv.Fingerprint()) || !strings.Contains(body, "check that the certificate fingerprint") {
		t.Fatalf("setup page %d lacks the fingerprint line:\n%s", rec.Code, body)
	}
	assertInert(t, "setup", body)
	if strings.Contains(body, `href="/account"`) || strings.Contains(body, "Sign out") {
		t.Error("setup page shows the authenticated chrome")
	}
	csrf := c.token("/setup")

	good := url.Values{"token": {tok}, "username": {testUser}, "password": {testPW}, "password2": {testPW}, "_csrf": {csrf}}
	with := func(k, v string) url.Values {
		f := url.Values{}
		for kk, vv := range good {
			f[kk] = vv
		}
		f.Set(k, v)
		return f
	}
	// CSRF: no token, wrong token, cross-site.
	if r := c.do("POST", "/setup", with("_csrf", ""), nil); r.Code != 403 {
		t.Errorf("setup without CSRF token = %d", r.Code)
	}
	if r := c.do("POST", "/setup", with("_csrf", "AAAA"), nil); r.Code != 403 {
		t.Errorf("setup with a wrong CSRF token = %d", r.Code)
	}
	if r := c.do("POST", "/setup", good, func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }); r.Code != 403 {
		t.Errorf("cross-site setup = %d", r.Code)
	}
	// Field errors: 422, nothing created, token not echoed.
	for name, f := range map[string]url.Values{
		"mismatch": with("password2", "something else entirely"),
		"short":    with("password", "short"),
		"badtoken": with("token", "mfs1.not-the-token"),
		"baduser":  with("username", "Bad User!"),
	} {
		if name == "short" {
			f.Set("password2", "short")
		}
		r := c.do("POST", "/setup", f, nil)
		if r.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status %d, want 422", name, r.Code)
		}
		if strings.Contains(r.Body.String(), tok) || strings.Contains(r.Body.String(), testPW) {
			t.Errorf("%s: response echoes a secret", name)
		}
	}
	if open, _ := e.svc.GUISetupOpen(ctx); !open {
		t.Fatal("an invalid setup created the admin")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("token file lost after failed attempts: %v", err)
	}

	rec = c.do("POST", "/setup", good, nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("setup = %d %q:\n%s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("token file still there after setup: %v", err)
	}
	if _, ok := c.jar[SessionCookieName]; !ok {
		t.Error("no session cookie after setup")
	}
	if _, ok := c.jar[PreCookieName]; ok {
		t.Error("pre-session cookie not cleared")
	}
	var sess *http.Cookie
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == SessionCookieName {
			sess = ck
		}
	}
	if sess == nil || !sess.Secure || !sess.HttpOnly || sess.SameSite != http.SameSiteStrictMode {
		t.Errorf("session cookie attributes: %+v", sess)
	}
	if r := c.get("/setup"); r.Code != 404 {
		t.Errorf("/setup after setup = %d, want 404", r.Code)
	}
	r := c.get("/account")
	if r.Code != 200 || !strings.Contains(r.Body.String(), "this session") {
		t.Errorf("account after setup = %d", r.Code)
	}
	// Reusing the token fails generically and the secrets never appear anywhere.
	for _, secret := range []string{tok, sess.Value, testPW} {
		if strings.Contains(e.logText(), secret) {
			t.Errorf("secret %q... is in the log", secret[:6])
		}
		if strings.Contains(e.seenText(), secret) && secret != sess.Value {
			t.Errorf("secret %q... appears in a response", secret[:6])
		}
	}
	// Set-Cookie carries the session token once, by design; no body does.
	for _, b := range []string{c.get("/account").Body.String(), c.get("/login").Body.String()} {
		if strings.Contains(b, sess.Value) || strings.Contains(b, tok) {
			t.Error("a page body contains a token")
		}
	}
}

func TestLoginLogoutNoJS(t *testing.T) {
	e := newAuthEnv(t)
	first := loginForTest(t, e) // setup
	_ = first
	c := e.newClient()

	rec := c.get("/account?tab=1")
	if rec.Code != 303 || rec.Header().Get("Location") != "/login?next=%2Faccount%3Ftab%3D1" {
		t.Fatalf("unauthenticated /account = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	hx := c.do("GET", "/account", nil, func(r *http.Request) { r.Header.Set("HX-Request", "true") })
	if hx.Code != 200 || hx.Header().Get("HX-Redirect") != "/login" {
		t.Errorf("htmx /account = %d HX-Redirect %q", hx.Code, hx.Header().Get("HX-Redirect"))
	}
	page := c.get("/login?next=%2Faccount")
	if page.Code != 200 || !strings.Contains(page.Body.String(), `name="next" value="/account"`) {
		t.Fatalf("login page %d:\n%s", page.Code, page.Body.String())
	}
	assertInert(t, "login", page.Body.String())
	if strings.Contains(page.Body.String(), "Sign out") {
		t.Error("login page shows the authenticated chrome")
	}

	bad := c.post("/login", "/login", url.Values{"username": {testUser}, "password": {"wrong password here"}, "next": {"/account"}})
	if bad.Code != 401 || !strings.Contains(bad.Body.String(), "Wrong username or password.") {
		t.Fatalf("wrong password = %d", bad.Code)
	}
	unk := c.post("/login", "/login", url.Values{"username": {"nobody"}, "password": {"wrong password here"}})
	if unk.Code != 401 || !strings.Contains(unk.Body.String(), "Wrong username or password.") {
		t.Fatalf("unknown user = %d", unk.Code)
	}
	if _, ok := c.jar[SessionCookieName]; ok {
		t.Fatal("session cookie set by a failed login")
	}

	for _, next := range []string{"//evil.example/x", "https://evil.example/", "/\\evil", ""} {
		cc := e.newClient()
		ok := cc.post("/login", "/login", url.Values{"username": {testUser}, "password": {testPW}, "next": {next}})
		if ok.Code != 303 || ok.Header().Get("Location") != "/" {
			t.Errorf("next=%q: %d %q, want 303 /", next, ok.Code, ok.Header().Get("Location"))
		}
	}
	ok := c.post("/login", "/login", url.Values{"username": {testUser}, "password": {testPW}, "next": {"/account"}})
	if ok.Code != 303 || ok.Header().Get("Location") != "/account" {
		t.Fatalf("login = %d %q", ok.Code, ok.Header().Get("Location"))
	}
	if _, pre := c.jar[PreCookieName]; pre {
		t.Error("pre-session cookie survives login")
	}
	old := c.jar[SessionCookieName]
	if rec := c.get("/login"); rec.Code != 303 || rec.Header().Get("Location") != "/" {
		t.Errorf("/login while signed in = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if r := c.get("/account"); r.Code != 200 || !strings.Contains(r.Body.String(), "Sign out") {
		t.Fatalf("account = %d", r.Code)
	}

	out := c.post("/account", "/logout", nil)
	if out.Code != 303 || out.Header().Get("Location") != "/login" {
		t.Fatalf("logout = %d %q", out.Code, out.Header().Get("Location"))
	}
	if _, ok := c.jar[SessionCookieName]; ok {
		t.Error("session cookie not cleared")
	}
	if r := c.get("/login"); !strings.Contains(r.Body.String(), "You are signed out.") {
		t.Errorf("no sign-out flash:\n%s", r.Body.String())
	}
	// The old cookie is dead server-side.
	c.jar[SessionCookieName] = old
	if r := c.get("/account"); r.Code != 303 {
		t.Errorf("old session still works: %d", r.Code)
	}
	if e.auditCount("gui.logout") != 1 || e.auditCount("gui.login") < 3 {
		t.Errorf("audit: logout=%d login=%d", e.auditCount("gui.logout"), e.auditCount("gui.login"))
	}
	for _, s := range []string{old, testPW} {
		if strings.Contains(e.logText(), s) {
			t.Error("log contains a secret")
		}
	}
}

func TestCSRFRefusedOnEveryPost(t *testing.T) {
	e := newAuthEnv(t)
	c := loginForTest(t, e)
	good := c.token("/account")
	posts := map[string]url.Values{
		"/logout":                         nil,
		"/account/password":               {"current": {testPW}, "new": {"another long password"}, "new2": {"another long password"}},
		"/account/sessions/revoke-others": nil,
		"/account/sessions/999/revoke":    nil,
		"/account/theme":                  {"theme": {"dark"}},
	}
	for path, f := range posts {
		mk := func(tok string) url.Values {
			v := url.Values{}
			for k, x := range f {
				v[k] = x
			}
			if tok != "" {
				v.Set("_csrf", tok)
			}
			return v
		}
		if r := c.do("POST", path, mk(""), nil); r.Code != 403 {
			t.Errorf("%s without token = %d", path, r.Code)
		}
		if r := c.do("POST", path, mk("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"), nil); r.Code != 403 {
			t.Errorf("%s with wrong token = %d", path, r.Code)
		}
		if r := c.do("POST", path, mk(good), func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }); r.Code != 403 {
			t.Errorf("%s cross-site = %d", path, r.Code)
		}
		// A token of another session is refused.
		other := loginForTest(t, e)
		if r := c.do("POST", path, mk(other.token("/account")), nil); r.Code != 403 {
			t.Errorf("%s with another session's token = %d", path, r.Code)
		}
		if r := c.get("/account"); r.Code != 200 {
			t.Fatalf("session lost after refused %s: %d", path, r.Code)
		}
	}
	// Login: no pre cookie, no token, cross-site.
	fresh := e.newClient()
	form := url.Values{"username": {testUser}, "password": {testPW}}
	if r := fresh.do("POST", "/login", form, nil); r.Code != 403 {
		t.Errorf("login without pre cookie = %d", r.Code)
	}
	tok := fresh.token("/login")
	form.Set("_csrf", tok)
	if r := fresh.do("POST", "/login", form, func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }); r.Code != 403 {
		t.Errorf("cross-site login = %d", r.Code)
	}
	form.Set("_csrf", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	if r := fresh.do("POST", "/login", form, nil); r.Code != 403 {
		t.Errorf("login with wrong token = %d", r.Code)
	}
	// An unauthenticated POST to a protected route goes to login, not to the handler.
	anon := e.newClient()
	if r := anon.do("POST", "/account/theme", url.Values{"theme": {"dark"}}, nil); r.Code != 303 {
		t.Errorf("anonymous POST = %d", r.Code)
	}
	// Non-form bodies are refused.
	r := c.do("POST", "/account/theme", nil, func(r *http.Request) {
		r.Header.Set("Content-Type", "application/json")
		r.Body = http.NoBody
		r.ContentLength = 2
	})
	if r.Code != 415 {
		t.Errorf("json POST = %d, want 415", r.Code)
	}
}

func TestLoginLockout(t *testing.T) {
	e := newAuthEnv(t)
	loginForTest(t, e)
	c := e.newClient()
	try := func(pw string) *http.Response {
		rec := c.post("/login", "/login", url.Values{"username": {testUser}, "password": {pw}})
		return rec.Result()
	}
	// Burst of 5 is allowed; then the bucket refuses with 429 and Retry-After.
	for i := 0; i < 5; i++ {
		if r := try("wrong password " + string(rune('a'+i))); r.StatusCode != 401 {
			t.Fatalf("attempt %d = %d", i, r.StatusCode)
		}
	}
	r := try("wrong password f")
	if r.StatusCode != 429 || r.Header.Get("Retry-After") == "" {
		t.Fatalf("6th attempt = %d Retry-After %q", r.StatusCode, r.Header.Get("Retry-After"))
	}
	// Space the rest out so only the lockout (10 consecutive failures) stops us.
	for i := 5; i < 10; i++ {
		e.clk.advance(13 * time.Second)
		if r := try("wrong password " + string(rune('a'+i))); r.StatusCode != 401 {
			t.Fatalf("attempt %d = %d", i, r.StatusCode)
		}
	}
	e.clk.advance(13 * time.Second)
	rec := c.post("/login", "/login", url.Values{"username": {testUser}, "password": {testPW}})
	if rec.Code != 429 {
		t.Fatalf("attempt during lockout (correct password) = %d, want 429", rec.Code)
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" || ra == "0" {
		t.Errorf("Retry-After %q", ra)
	}
	if !strings.Contains(rec.Body.String(), "minutes") {
		t.Errorf("lockout page lacks the minutes left:\n%s", rec.Body.String())
	}
	if _, ok := c.jar[SessionCookieName]; ok {
		t.Error("a locked-out login got a session")
	}
	// More attempts during the lockout do not add audit rows: one gui.lockout in all.
	c.post("/login", "/login", url.Values{"username": {testUser}, "password": {"x"}})
	if n := e.auditCount("gui.lockout"); n != 1 {
		t.Errorf("gui.lockout rows = %d, want 1", n)
	}
	// Another client is unaffected; the lockout ends after 15 minutes.
	if o := e.newClient(); o.login(testUser, testPW).Code != 303 {
		t.Error("other address affected by the lockout")
	}
	e.clk.advance(16 * time.Minute)
	if rec := c.post("/login", "/login", url.Values{"username": {testUser}, "password": {testPW}}); rec.Code != 303 {
		t.Errorf("login after the lockout = %d", rec.Code)
	}
}

func TestLoginOversizedPasswordAndBusy(t *testing.T) {
	e := newAuthEnv(t)
	loginForTest(t, e)
	c := e.newClient()
	logins := e.auditCount("gui.login")
	rec := c.post("/login", "/login", url.Values{"username": {testUser}, "password": {strings.Repeat("x", auth.MaxPasswordBytes+1)}})
	if rec.Code != 401 || !strings.Contains(rec.Body.String(), "Wrong username or password.") {
		t.Errorf("oversized password = %d", rec.Code)
	}
	// Even the right prefix of the password does not matter; nothing was looked up.
	if e.auditCount("gui.login") != logins {
		t.Error("a login row was written")
	}
	// A body past the cap is refused before parsing.
	huge := url.Values{"username": {testUser}, "password": {strings.Repeat("y", MaxFormBytes+10)}, "_csrf": {c.token("/login")}}
	if r := c.do("POST", "/login", huge, nil); r.Code != 413 {
		t.Errorf("huge body = %d, want 413", r.Code)
	}

	// All hash slots taken: 503 with Retry-After, and no login happens.
	e.srv.auth.sem = NewHashSem(1, time.Second, func(time.Duration) <-chan time.Time {
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	})
	release, err := e.srv.auth.sem.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rec = c.login(testUser, testPW)
	if rec.Code != 503 || rec.Header().Get("Retry-After") == "" {
		t.Errorf("busy login = %d", rec.Code)
	}
	release()
	if rec = c.login(testUser, testPW); rec.Code != 303 {
		t.Errorf("login after release = %d", rec.Code)
	}
}

func TestSetupLimiterAndStalePage(t *testing.T) {
	e := newAuthEnv(t)
	c := e.newClient()
	if _, created, err := e.srv.PrepareSetup(context.Background()); err != nil || !created {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		r := c.post("/setup", "/setup", url.Values{"token": {"mfs1.wrong"}, "username": {testUser}, "password": {testPW}, "password2": {testPW}})
		if r.Code != 422 {
			t.Fatalf("attempt %d = %d", i, r.Code)
		}
	}
	r := c.post("/setup", "/setup", url.Values{"token": {"mfs1.wrong"}, "username": {testUser}, "password": {testPW}, "password2": {testPW}})
	if r.Code != 429 || r.Header().Get("Retry-After") == "" {
		t.Errorf("6th setup attempt = %d", r.Code)
	}
	// With the admin in place PrepareSetup removes a leftover token file.
	d := loginForTest(t, newAuthEnvWithAdmin(t))
	_ = d
}

// newAuthEnvWithAdmin returns an env that already has the admin (created through the setup flow).
func newAuthEnvWithAdmin(t *testing.T) *authEnv {
	t.Helper()
	e := newAuthEnv(t)
	loginForTest(t, e)
	return e
}

func TestPrepareSetupRemovesStaleFile(t *testing.T) {
	e := newAuthEnvWithAdmin(t)
	path := e.srv.SetupTokenPath()
	if err := os.WriteFile(path, []byte("stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if p, created, err := e.srv.PrepareSetup(context.Background()); err != nil || created || p != "" {
		t.Fatalf("PrepareSetup with an admin = %q %v %v", p, created, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("stale token file remains: %v", err)
	}
}
