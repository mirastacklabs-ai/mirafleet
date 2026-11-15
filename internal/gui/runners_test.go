package gui

// Tests for the Runners screens (M4 step S9). They use the shared harness (helpers_test.go),
// signed in through loginForTest. Runner-reported data a real runner would send over the
// heartbeat (info, advertised actions) is injected by a Backend wrapper.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// s9Backend adds hostile heartbeat data to every runner the service lists.
type s9Backend struct {
	Backend
	hostile bool
}

func (b s9Backend) ListRunnersAll(ctx context.Context, p auth.Principal, src service.Source, fleet string, rev bool) ([]service.RunnerView, error) {
	rvs, err := b.Backend.ListRunnersAll(ctx, p, src, fleet, rev)
	if err != nil || !b.hostile {
		return rvs, err
	}
	def := "javascript:alert(1)\xe2\x80\xae"
	for i := range rvs {
		rvs[i].Info = map[string]string{
			"hostname": "<script>alert(1)</script>",
			"version":  "\"><img src=x onerror=alert(1)>",
			"os":       "linux\xe2\x80\xaeevil\x1b[31m",
			"<b>k</b>": "v",
		}
		rvs[i].Actions = []transport.ActionInfo{{Name: "<script>alert('a')</script>",
			Params: []transport.ActionParam{{Name: "p\xe2\x80\xae", Pattern: "javascript:\xe2\x80\xae^.*$<svg onload=alert(1)>", Default: &def}}}}
	}
	return rvs, nil
}

// newRunnersHarness is a harness with a signed-in cookie-jar client in h.c; hostile makes every
// listed runner carry hostile heartbeat data.
func newRunnersHarness(t *testing.T, hostile bool) *guiHarness {
	t.Helper()
	h := newGUIHarness(t, withBackend(func(b Backend) Backend { return s9Backend{Backend: b, hostile: hostile} }))
	h.c = loginForTest(t, h)
	return h
}

// mkFleet creates an actions-mode fleet.
func (h *guiHarness) mkFleet(name string) {
	h.t.Helper()
	if _, err := h.svc.CreateFleet(context.Background(), h.adm, service.SourceGUI, name, store.ModeActions, false, nil); err != nil {
		h.t.Fatal(err)
	}
}

// addRunner enrols a runner through a real join token; auto-approve makes it active.
func (h *guiHarness) addRunner(fleet, name string, approve bool, labels map[string]string) {
	h.t.Helper()
	ctx := context.Background()
	ct, err := h.svc.CreateToken(ctx, h.adm, service.SourceGUI, service.TokenParams{Fleet: fleet, Labels: labels, Approve: approve})
	if err != nil {
		h.t.Fatal(err)
	}
	h.joinWith(ct.Token, name)
}

func (h *guiHarness) status(fleet, name string) store.Status {
	h.t.Helper()
	rvs, err := h.svc.ListRunnersAll(context.Background(), h.adm, service.SourceGUI, fleet, true)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, rv := range rvs {
		if rv.Name == name {
			return rv.Status
		}
	}
	h.t.Fatalf("runner %s/%s not found", fleet, name)
	return ""
}

// act posts a form to target with the CSRF token of the page at formPage.
func (h *guiHarness) act(formPage, target string, form url.Values) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.c.post(formPage, target, form)
}

func TestRunnerTransitionsPerStatus(t *testing.T) {
	h := newRunnersHarness(t, false)
	h.mkFleet("dev")
	h.addRunner("dev", "r1", false, nil)
	page := "/runners/dev/r1"

	has := func(body string, ops ...string) {
		t.Helper()
		for _, op := range []string{"approve", "disable", "enable"} {
			want := false
			for _, o := range ops {
				want = want || o == op
			}
			if got := strings.Contains(body, `action="`+page+"/"+op+`"`); got != want {
				t.Errorf("op %s offered = %v, want %v\n%s", op, got, want, body)
			}
		}
		if !strings.Contains(body, page+"/revoke") {
			t.Errorf("no revoke link\n%s", body)
		}
	}

	if st := h.status("dev", "r1"); st != store.StatusPending {
		t.Fatalf("status %s", st)
	}
	has(h.c.get(page).Body.String(), "approve")

	step := func(op string, want store.Status, offered ...string) {
		t.Helper()
		rec := h.act(page, page+"/"+op, url.Values{"back": {"detail"}})
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != page {
			t.Fatalf("%s: status %d loc %q", op, rec.Code, rec.Header().Get("Location"))
		}
		if st := h.status("dev", "r1"); st != want {
			t.Fatalf("%s: status %s, want %s", op, st, want)
		}
		has(h.c.get(page).Body.String(), offered...)
	}
	step("approve", store.StatusActive, "disable")
	step("disable", store.StatusDisabled, "enable")
	step("enable", store.StatusActive, "disable")

	// A transition that does not fit the status changes nothing and says so.
	rec := h.act(page, page+"/approve", url.Values{"back": {"detail"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("approve on active: %d", rec.Code)
	}
	if st := h.status("dev", "r1"); st != store.StatusActive {
		t.Fatalf("status %s", st)
	}
	follow := h.c.do("GET", rec.Header().Get("Location"), nil, func(r *http.Request) {
		for _, ck := range rec.Result().Cookies() {
			if ck.Name == FlashCookieName {
				r.AddCookie(ck)
			}
		}
	})
	if !strings.Contains(follow.Body.String(), "nothing was changed") {
		t.Errorf("no conflict flash:\n%s", follow.Body.String())
	}
	// Unknown operation and unknown runner.
	if rec := h.act(page, page+"/explode", url.Values{}); rec.Code != http.StatusNotFound {
		t.Errorf("unknown op: %d", rec.Code)
	}
	if rec := h.c.get("/runners/dev/nope"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown runner: %d", rec.Code)
	}
}

func TestRunnerRevokeNeedsTypedRef(t *testing.T) {
	h := newRunnersHarness(t, false)
	h.mkFleet("dev")
	h.addRunner("dev", "r1", true, nil)
	page := "/runners/dev/r1"

	rec := h.c.get(page + "/revoke")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "dev/r1") || !strings.Contains(rec.Body.String(), "never enrol") {
		t.Fatalf("confirm page: %d\n%s", rec.Code, rec.Body.String())
	}
	for _, typed := range []string{"", "dev", "r1", "dev/r2", "DEV/R1"} {
		rec := h.act(page+"/revoke", page+"/revoke", url.Values{"confirm": {typed}, "back": {"detail"}})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("typed %q: status %d", typed, rec.Code)
		}
		if st := h.status("dev", "r1"); st != store.StatusActive {
			t.Fatalf("typed %q revoked the runner", typed)
		}
	}
	// A plain POST without the confirmation field does nothing either.
	if rec := h.act(page, page+"/revoke", url.Values{}); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("no confirm: %d", rec.Code)
	}
	rec = h.act(page+"/revoke", page+"/revoke", url.Values{"confirm": {"dev/r1"}, "back": {"detail"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("revoke: %d", rec.Code)
	}
	if st := h.status("dev", "r1"); st != store.StatusRevoked {
		t.Fatalf("status %s", st)
	}
	// A revoked runner offers no transitions and is hidden from the default list.
	body := h.c.get(page).Body.String()
	for _, op := range []string{"/approve", "/disable", "/enable", "/revoke"} {
		if strings.Contains(body, page+op) {
			t.Errorf("revoked runner still offers %s", op)
		}
	}
	if strings.Contains(h.c.get("/runners").Body.String(), "r1") {
		t.Error("revoked runner in the default list")
	}
	if !strings.Contains(h.c.get("/runners?revoked=1").Body.String(), "r1") {
		t.Error("revoked runner missing with revoked=1")
	}
	if h.auditCount("runner.revoke") != 1 {
		t.Errorf("revoke audit rows = %d", h.auditCount("runner.revoke"))
	}
}

func TestRunnerHostileDataIsInert(t *testing.T) {
	h := newRunnersHarness(t, true)
	h.mkFleet("dev")
	h.addRunner("dev", "r1", true, map[string]string{
		"<script>alert(1)</script>": "javascript:alert(1)",
		"role":                      "\xe2\x80\xaeevil\"><img src=x onerror=alert(1)>",
	})
	for _, target := range []string{"/runners", "/runners/dev/r1", "/runners/live"} {
		rec := h.c.do("GET", target, nil, func(r *http.Request) {
			if target == "/runners/live" {
				r.Header.Set("HX-Request", "true")
			}
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d", target, rec.Code)
		}
		body := rec.Body.String()
		for _, bad := range []string{"<script>alert", "<img", "<svg onload", "<b>k</b>", "\xe2\x80\xae", "\x1b", `href="javascript:`, "onerror=alert(1)>"} {
			if strings.Contains(body, bad) {
				t.Errorf("%s: contains %q", target, bad)
			}
		}
		if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
			t.Errorf("%s: escaped script label missing", target)
		}
		if !strings.Contains(body, "&lt;U&#43;202E&gt;") && !strings.Contains(body, "&lt;U+202E&gt;") {
			t.Errorf("%s: bidi control not shown as an escape", target)
		}
	}
	// The detail page shows the advertised action, its pattern and default, still inert.
	body := h.c.get("/runners/dev/r1").Body.String()
	for _, want := range []string{"Advertised actions", "Informational: the runner", "&lt;svg onload=alert(1)&gt;"} {
		if !strings.Contains(body, want) {
			t.Errorf("detail: missing %q", want)
		}
	}
}

func TestRunnersListFiltersAndPolling(t *testing.T) {
	h := newRunnersHarness(t, false)
	h.mkFleet("dev")
	h.mkFleet("prod")
	h.addRunner("dev", "run-a", false, map[string]string{"env": "dev"})
	h.addRunner("prod", "run-b", true, nil)

	body := h.c.get("/runners").Body.String()
	for _, want := range []string{"run-a", "run-b", "env=dev", `hx-get="/runners/live"`, `hx-trigger="every 10s"`} {
		if !strings.Contains(body, want) {
			t.Errorf("list missing %q", want)
		}
	}
	body = h.c.get("/runners?status=pending").Body.String()
	if !strings.Contains(body, "run-a") || strings.Contains(body, "run-b") {
		t.Errorf("status filter:\n%s", body)
	}
	if body = h.c.get("/runners?fleet=prod").Body.String(); strings.Contains(body, "run-a") || !strings.Contains(body, "run-b") {
		t.Errorf("fleet filter")
	}
	if body = h.c.get("/runners?online=yes").Body.String(); !strings.Contains(body, "No runners match") {
		t.Errorf("online filter")
	}
	// The polled fragment is the table only, and keeps the filters in its URL.
	rec := h.c.do("GET", "/runners/live?fleet=prod", nil, func(r *http.Request) { r.Header.Set("HX-Request", "true") })
	frag := rec.Body.String()
	if strings.Contains(frag, "<html") || !strings.Contains(frag, `hx-get="/runners/live?fleet=prod"`) || !strings.Contains(frag, "run-b") {
		t.Errorf("fragment:\n%s", frag)
	}
	// Inline script, style and handlers are never emitted.
	for _, bad := range []string{" onclick=", " style=", "<script>"} {
		if strings.Contains(body, bad) {
			t.Errorf("page contains %q", bad)
		}
	}
}

func TestRunnersNeedAdminAndCSRF(t *testing.T) {
	h := newRunnersHarness(t, false)
	h.mkFleet("dev")
	h.addRunner("dev", "r1", false, nil)

	anon := h.newClient()
	for _, target := range []string{"/runners", "/runners/dev/r1", "/runners/dev/r1/revoke", "/tokens", "/tokens/new"} {
		rec := anon.get(target)
		if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/login") {
			t.Errorf("anon GET %s: %d %q", target, rec.Code, rec.Header().Get("Location"))
		}
	}
	// A POST without the synchronizer token is refused and changes nothing.
	rec := h.c.do("POST", "/runners/dev/r1/approve", url.Values{"back": {"detail"}}, nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("no csrf: %d", rec.Code)
	}
	if st := h.status("dev", "r1"); st != store.StatusPending {
		t.Errorf("status %s", st)
	}
}

// joinWith enrols a runner with an existing join token string.
func (h *guiHarness) joinWith(tok, name string) {
	h.t.Helper()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.svc.Join(context.Background(), auth.Principal{Kind: auth.KindEnroll, Remote: "test"},
		service.SourceEnroll, tok, name, key); err != nil {
		h.t.Fatal(err)
	}
}
