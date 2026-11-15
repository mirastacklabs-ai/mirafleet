package gui

// S16: every flow runs with JavaScript off. A plain net/http client with a cookie jar stands in
// for the browser: it never sends HX-Request or any other htmx header, fills in the forms the
// server rendered (hidden fields included) and follows redirects, over real TLS.

import (
	"context"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
)

type plainBrowser struct {
	t    *testing.T
	c    *http.Client
	base string
	last *http.Response
	body string
}

var (
	formRE  = regexp.MustCompile(`(?s)<form\b([^>]*)>(.*?)</form>`)
	attrRE  = regexp.MustCompile(`([a-zA-Z-]+)="([^"]*)"`)
	inputRE = regexp.MustCompile(`<input\b[^>]*>`)
)

func attrs(tag string) map[string]string {
	m := map[string]string{}
	for _, a := range attrRE.FindAllStringSubmatch(tag, -1) {
		m[a[1]] = html.UnescapeString(a[2])
	}
	return m
}

func (b *plainBrowser) read(resp *http.Response, err error) {
	b.t.Helper()
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	b.last, b.body = resp, string(raw)
	if resp.Header.Get("HX-Redirect") != "" || resp.Header.Get("HX-Trigger") != "" {
		b.t.Errorf("%s: htmx response header on a request without htmx", resp.Request.URL.Path)
	}
}

func (b *plainBrowser) get(path string) {
	b.t.Helper()
	b.read(b.c.Get(b.base + path))
}

// path is the path (and query) the browser ended up on after redirects.
func (b *plainBrowser) path() string { return b.last.Request.URL.RequestURI() }

// submit finds the form posting to action on the current page, fills it as a browser would
// (every named input with its value, then over), and sends it.
func (b *plainBrowser) submit(action string, over map[string]string) {
	b.t.Helper()
	for _, m := range formRE.FindAllStringSubmatch(b.body, -1) {
		fa := attrs(m[1])
		if fa["action"] != action {
			continue
		}
		if !strings.EqualFold(fa["method"], "post") {
			b.t.Fatalf("form %s is not a POST", action)
		}
		v := url.Values{}
		for _, in := range inputRE.FindAllString(m[2], -1) {
			a := attrs(in)
			if a["name"] == "" || (a["type"] == "checkbox" || a["type"] == "radio") && !strings.Contains(in, " checked") {
				continue
			}
			v.Set(a["name"], a["value"])
		}
		for k, x := range over {
			v.Set(k, x)
		}
		b.read(b.c.PostForm(b.base+action, v))
		return
	}
	b.t.Fatalf("no form with action %q on %s", action, b.path())
}

func (b *plainBrowser) wantPath(p string) {
	b.t.Helper()
	if got := b.last.Request.URL.Path; got != p {
		b.t.Fatalf("on %s (status %d), want %s\n%.600s", b.path(), b.last.StatusCode, p, b.body)
	}
	if b.last.StatusCode != http.StatusOK {
		b.t.Fatalf("%s: status %d\n%.600s", p, b.last.StatusCode, b.body)
	}
}

func TestS16NoJSFlows(t *testing.T) {
	h := newGUIHarness(t, withConfig(func(c *Config) {
		c.Interfaces = func() ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil }
	}))
	ts := httptest.NewTLSServer(h.srv.Handler())
	defer ts.Close()
	jar, _ := cookiejar.New(nil)
	cl := ts.Client()
	cl.Jar = jar
	b := &plainBrowser{t: t, c: cl, base: ts.URL}
	ctx := context.Background()

	// First run: everything redirects to /setup; the token file is the proof of host access.
	path, _, err := h.srv.PrepareSetup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := os.ReadFile(path)
	b.get("/")
	b.wantPath("/setup")
	b.submit("/setup", map[string]string{"token": strings.TrimSpace(string(tok)), "username": testUser, "password": testPW, "password2": testPW})
	b.wantPath("/")
	h.adm = auth.Principal{Kind: auth.KindAdmin, Name: testUser, Remote: "test"}

	// Sign out and in again.
	b.submit("/logout", nil)
	b.wantPath("/login")
	b.get("/fleets")
	b.wantPath("/login") // signed out: protected pages bounce to the form
	b.submit("/login", map[string]string{"username": testUser, "password": testPW})
	b.wantPath("/fleets") // back to the page that was asked for

	// Fleets: empty state, create (guarded), turn the guard off with the typed name.
	b.get("/fleets")
	if !strings.Contains(b.body, "No fleets yet") || !strings.Contains(b.body, `href="/fleets/new"`) {
		t.Error("empty fleets screen lacks its explanation or the way forward")
	}
	b.get("/fleets/new")
	b.submit("/fleets", map[string]string{"name": "prod", "mode": "actions", "guarded": "1"})
	b.wantPath("/fleets/prod")
	b.get("/fleets/prod/guard")
	b.submit("/fleets/prod/guard", map[string]string{"to": "off", "confirm": "prod"})
	b.wantPath("/fleets/prod")
	if f, err := h.fleet("prod"); err != nil || f.Confirm {
		t.Errorf("guard still on: %+v %v", f, err)
	}

	// Join token: shown once on the direct response, then a runner joins with it.
	b.get("/tokens/new")
	b.submit("/tokens", map[string]string{"fleet": "prod", "uses": "1", "ttl": "24h"})
	m := tokenRE.FindStringSubmatch(b.body)
	if m == nil || b.last.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("token result: %v %q", m, b.last.Header.Get("Cache-Control"))
	}
	h.joinWith(html.UnescapeString(m[1]), "r1")
	b.get("/runners")
	b.wantPath("/runners")
	if !strings.Contains(b.body, "r1") {
		t.Error("joined runner not listed")
	}

	// Runner: approve, disable, enable, then revoke on its confirmation page.
	b.get("/runners/prod/r1")
	for _, step := range []struct {
		op   string
		want store.Status
	}{{"approve", store.StatusActive}, {"disable", store.StatusDisabled}, {"enable", store.StatusActive}} {
		b.submit("/runners/prod/r1/"+step.op, nil)
		b.wantPath("/runners/prod/r1")
		if got := h.status("prod", "r1"); got != step.want {
			t.Fatalf("after %s: %s, want %s", step.op, got, step.want)
		}
	}
	b.get("/runners/prod/r1/revoke")
	b.submit("/runners/prod/r1/revoke", map[string]string{"confirm": "prod/r1"})
	if got := h.status("prod", "r1"); got != store.StatusRevoked {
		t.Fatalf("revoke: %s", got)
	}

	// Operators: the code is shown by a POST, and revoking needs the typed name.
	if err := h.svc.InitRegistrationSecret(ctx, h.adm, service.SourceGUI); err != nil {
		t.Fatal(err)
	}
	h.operator("alice", "")
	b.get("/operators")
	b.wantPath("/operators")
	b.submit("/operators/code", nil)
	if b.last.StatusCode != http.StatusOK || b.last.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("code page: %d %q", b.last.StatusCode, b.last.Header.Get("Cache-Control"))
	}
	b.get("/operators/alice/revoke")
	b.submit("/operators/alice/revoke", map[string]string{"confirm": "alice"})
	if got := h.opStatus("alice"); got != store.OperatorRevoked {
		t.Errorf("alice = %s", got)
	}

	// Workspaces: kill from the confirmation page.
	r := h.runner("dev", "build-1")
	w := h.ws("dev", r, h.operator("bob", ""), "s1", "debian:12")
	b.get("/workspaces")
	b.wantPath("/workspaces")
	b.get("/workspaces/" + w.WsID + "/kill")
	b.submit("/workspaces/"+w.WsID+"/kill", nil)
	b.wantPath("/workspaces")
	if st := h.wsState(w.WsID); st == store.WorkspaceOpen {
		t.Errorf("workspace still %s after kill", st)
	}

	// Audit: filter by plain GET, then the export download.
	b.get("/audit?action=fleet.create")
	b.wantPath("/audit")
	if !hasAction(b.body, "fleet.create") {
		t.Error("audit filter lost the fleet.create row")
	}
	b.get("/audit/export?format=csv&action=fleet.create")
	if ct := b.last.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") || !strings.Contains(b.body, "fleet.create") {
		t.Errorf("export: %q\n%.300s", ct, b.body)
	}

	// Account: theme, password change, then sign in with the new password.
	b.get("/account")
	b.submit("/account/theme", map[string]string{"theme": "dark"})
	b.wantPath("/account")
	if !strings.Contains(b.body, `data-theme="dark"`) {
		t.Error("theme switch did not render data-theme=dark")
	}
	const newPW = "another long passphrase 42"
	b.submit("/account/password", map[string]string{"current": testPW, "new": newPW, "new2": newPW})
	if b.last.StatusCode != http.StatusOK {
		t.Fatalf("password change: %d\n%.600s", b.last.StatusCode, b.body)
	}
	b.get("/account")
	if b.last.Request.URL.Path == "/login" { // the change ended every session: sign in again
		b.submit("/login", map[string]string{"username": testUser, "password": newPW})
	}
	b.wantPath("/account")
	b.submit("/logout", nil)
	b.wantPath("/login")
}

// Every form the screens render posts without script: a method and an action, and a CSRF field
// on every POST. (htmx attributes only enhance; the same form must work as a plain submit.)
func TestS16FormsWorkWithoutScript(t *testing.T) {
	h := newAdminHarness(t, withRegistrationSecret())
	h.mkFleet("prod")
	h.addRunner("prod", "db-1", true, nil)
	for _, p := range []string{"/", "/fleets", "/fleets/new", "/fleets/prod", "/fleets/prod/guard", "/runners", "/runners/prod/db-1",
		"/runners/prod/db-1/revoke", "/tokens", "/tokens/new", "/operators", "/operators/rotate", "/workspaces", "/audit", "/account"} {
		body := h.get(p).Body.String()
		for _, m := range formRE.FindAllStringSubmatch(body, -1) {
			a := attrs(m[1])
			if a["action"] == "" || a["method"] == "" {
				t.Errorf("%s: form without method or action: <form%s>", p, m[1])
			}
			if strings.EqualFold(a["method"], "post") && !strings.Contains(m[2], `name="_csrf"`) {
				t.Errorf("%s: POST form %s without a CSRF field", p, a["action"])
			}
			if a["hx-post"] != "" && a["hx-post"] != a["action"] {
				t.Errorf("%s: hx-post %q differs from action %q", p, a["hx-post"], a["action"])
			}
			if a["hx-get"] != "" && a["hx-get"] != a["action"] {
				t.Errorf("%s: hx-get %q differs from action %q", p, a["hx-get"], a["action"])
			}
		}
	}
}
