package gui

// Tests for the Overview and Fleets screens (M4 step S8). They run against the shared harness
// (helpers_test.go): a real service on a temp SQLite file, signed in with loginDirect.

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
)

func wsValues(extra map[string]string) url.Values {
	v := url.Values{"name": {"dev"}, "mode": {"workspace"}, "image": {"docker.io/library/debian:12"}}
	for k, x := range extra {
		v.Set(k, x)
	}
	return v
}


func TestFleetsCreateBothModes(t *testing.T) {
	h := newAdminHarness(t)
	wantBody(t, h.get("/fleets"), 200, "No fleets yet")
	wantBody(t, h.get("/fleets/new"), 200, `name="executor"`, `name="idle_ttl"`, `value="24h"`, `value="4GiB"`)

	// Actions mode, guarded.
	rec := h.post("/fleets", url.Values{"name": {"prod"}, "mode": {"actions"}, "guarded": {"1"}})
	if loc := rec.Header().Get("Location"); loc != "/fleets/prod" {
		t.Fatalf("Location = %q (status %d)", loc, rec.Code)
	}
	wantBody(t, h.follow(rec), 200, "Fleet prod created.", "Guarded")

	// Workspace mode with explicit units; blank fields take the defaults.
	rec = h.post("/fleets", wsValues(map[string]string{"memory": "8GiB", "cpus": "0.5", "idle_ttl": "12h",
		"images": "docker.io/library/debian:*\n", "network": "egress", "egress": "example.org:443"}))
	wantBody(t, h.follow(rec), 200, "Fleet dev created.", "docker.io/library/debian:12", "8GiB", "example.org:443")

	f, err := h.fleet("dev")
	if err != nil || f.Mode != store.ModeWorkspace || f.Workspace == nil {
		t.Fatalf("dev = %+v, %v", f, err)
	}
	c := f.Workspace
	if c.MemoryMB != 8192 || c.CPUMilli != 500 || c.IdleTTL != 12*time.Hour || c.Pids != 1024 || c.MaxTTL != 168*time.Hour ||
		c.Network != store.NetworkEgress || len(c.Egress) != 1 || f.Confirm {
		t.Errorf("config = %+v confirm=%v", c, f.Confirm)
	}
	if p, err := h.fleet("prod"); err != nil || p.Mode != store.ModeActions || !p.Confirm || p.Workspace != nil {
		t.Errorf("prod = %+v %v", p, err)
	}

	// The list shows both.
	wantBody(t, h.get("/fleets"), 200, "/fleets/prod", "/fleets/dev", "containerd, docker.io/library/debian:12, network egress")

	// Audit rows: source gui, actor = the admin's username.
	for _, a := range []string{api.MethodFleetCreate} {
		evs := h.audit(a)
		if len(evs) != 2 {
			t.Fatalf("%s rows = %d", a, len(evs))
		}
		for _, e := range evs {
			if e.Source != "gui" || e.Actor != testUser || e.ActorKind != "admin" || e.Outcome != "ok" {
				t.Errorf("audit row = %+v", e)
			}
		}
	}
}

func TestFleetsCreate422CreatesNothing(t *testing.T) {
	h := newAdminHarness(t)
	cases := []struct {
		name string
		form url.Values
		subs []string
	}{
		{"bad name", url.Values{"name": {"Bad Name!"}, "mode": {"actions"}}, []string{"Use 1 to 63 characters"}},
		{"empty name", url.Values{"name": {""}, "mode": {"actions"}}, []string{"Use 1 to 63 characters"}},
		{"reserved", url.Values{"name": {"new"}, "mode": {"actions"}}, []string{"reserved"}},
		{"bad mode", url.Values{"name": {"x"}, "mode": {"bogus"}}, []string{"Choose actions or workspace"}},
		{"no image", wsValues(map[string]string{"image": ""}), []string{"A default image is required"}},
		{"bad units", wsValues(map[string]string{"memory": "lots", "cpus": "-1", "idle_ttl": "soon", "pids": "0"}),
			[]string{"Want a size such as", "Want a number of CPUs", "Want a whole number of seconds", "Want a whole number from 1"}},
		{"not allowlisted", wsValues(map[string]string{"images": "docker.io/library/alpine:3"}), []string{"is not in the allowlist"}},
		{"egress without network", wsValues(map[string]string{"egress": "example.org:443"}), []string{"require network egress"}},
		{"ttl order", wsValues(map[string]string{"idle_ttl": "48h", "max_ttl": "1h"}), []string{"idle ttl"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := wantBody(t, h.post("/fleets", tc.form), 422, `class="field-error"`, `id="fleet-form"`)
			for _, s := range tc.subs {
				if !strings.Contains(b, s) {
					t.Errorf("body lacks %q", s)
				}
			}
			if n := len(h.fleets()); n != 0 {
				t.Fatalf("%d fleets created from bad input", n)
			}
			if n := len(h.audit(api.MethodFleetCreate)); n != 0 {
				t.Fatalf("%d audit rows from bad input", n)
			}
		})
	}
	// The typed values come back so the form can be corrected.
	b := wantBody(t, h.post("/fleets", url.Values{"name": {"Bad Name"}, "mode": {"actions"}}), 422)
	if !strings.Contains(b, `value="Bad Name"`) {
		t.Error("typed name not shown back")
	}

	// A duplicate name is a field error, not a second fleet.
	h.post("/fleets", url.Values{"name": {"prod"}, "mode": {"actions"}})
	wantBody(t, h.post("/fleets", url.Values{"name": {"prod"}, "mode": {"actions"}}), 422, "A fleet with this name already exists")
	if n := len(h.fleets()); n != 1 {
		t.Fatalf("%d fleets, want 1", n)
	}
}

func TestFleetGuardToggleOffNeedsName(t *testing.T) {
	h := newAdminHarness(t)
	h.post("/fleets", url.Values{"name": {"prod"}, "mode": {"actions"}, "guarded": {"1"}})

	// The detail page offers the dialog as a plain link (works without JS).
	wantBody(t, h.get("/fleets/prod"), 200, `href="/fleets/prod/guard"`)
	wantBody(t, h.get("/fleets/prod/guard"), 200, `name="confirm"`, "Turn the guard off for prod")

	for _, typed := range []string{"", "prd", "PROD"} {
		wantBody(t, h.post("/fleets/prod/guard", url.Values{"to": {"off"}, "confirm": {typed}}), 422, "Type the fleet name exactly")
		if f, _ := h.fleet("prod"); !f.Confirm {
			t.Fatalf("guard turned off with confirm %q", typed)
		}
	}
	rec := h.post("/fleets/prod/guard", url.Values{"to": {"off"}, "confirm": {"prod"}})
	wantBody(t, h.follow(rec), 200, "Not guarded")
	if f, _ := h.fleet("prod"); f.Confirm {
		t.Fatal("guard still on")
	}
	// Turning it on needs no name.
	rec = h.post("/fleets/prod/guard", url.Values{"to": {"on"}})
	wantBody(t, h.follow(rec), 200, "Guarded")
	if f, _ := h.fleet("prod"); !f.Confirm {
		t.Fatal("guard not back on")
	}
	wantBody(t, h.post("/fleets/prod/guard", url.Values{"to": {"maybe"}}), 400)
	if n := len(h.audit(api.MethodFleetSetConfirm)); n != 2 {
		t.Errorf("confirm audit rows = %d, want 2", n)
	}
}

func TestFleetWorkspaceEditRoundTrip(t *testing.T) {
	h := newAdminHarness(t)
	h.post("/fleets", wsValues(nil))
	wantBody(t, h.get("/fleets/dev/workspace"), 200, `value="docker.io/library/debian:12"`, `value="4GiB"`)

	rec := h.post("/fleets/dev/workspace", wsValues(map[string]string{"memory": "2GiB", "cpus": "1", "idle_ttl": "30m"}))
	wantBody(t, h.follow(rec), 200, "2GiB")
	f, err := h.fleet("dev")
	if err != nil || f.Workspace.MemoryMB != 2048 || f.Workspace.CPUMilli != 1000 || f.Workspace.IdleTTL != 30*time.Minute {
		t.Fatalf("saved = %+v %v", f.Workspace, err)
	}
	// The edit form shows the saved values.
	wantBody(t, h.get("/fleets/dev/workspace"), 200, `value="2GiB"`, `value="30m"`)

	// Bad input: 422, nothing changed.
	wantBody(t, h.post("/fleets/dev/workspace", wsValues(map[string]string{"memory": "junk"})), 422, `class="field-error"`)
	if g, _ := h.fleet("dev"); g.Workspace.MemoryMB != 2048 {
		t.Error("config changed by a rejected edit")
	}
	// An actions fleet has no workspace settings.
	h.post("/fleets", url.Values{"name": {"prod"}, "mode": {"actions"}})
	if rec := h.get("/fleets/prod/workspace"); rec.Code != 404 && rec.Code != 400 {
		t.Errorf("actions fleet workspace form status %d", rec.Code)
	}
	for _, e := range h.audit(api.MethodFleetWorkspaceSet) {
		if e.Source != "gui" || e.Actor != testUser {
			t.Errorf("audit row = %+v", e)
		}
	}
}

func TestFleetsCSRFAndAuth(t *testing.T) {
	h := newAdminHarness(t)
	// POST without a token is refused and creates nothing.
	rec := h.do("POST", "/fleets", url.Values{"name": {"x"}, "mode": {"actions"}}, nil)
	if rec.Code < 400 || len(h.fleets()) != 0 {
		t.Fatalf("status %d, fleets %d", rec.Code, len(h.fleets()))
	}
	h.post("/fleets", url.Values{"name": {"prod"}, "mode": {"actions"}, "guarded": {"1"}})
	rec = h.do("POST", "/fleets/prod/guard", url.Values{"to": {"off"}, "confirm": {"prod"}}, nil)
	if rec.Code < 400 {
		t.Fatalf("guard POST without token status %d", rec.Code)
	}
	if f, _ := h.fleet("prod"); !f.Confirm {
		t.Fatal("guard changed without CSRF token")
	}
	// Signed out: redirected to login, not rendered.
	h.tok = ""
	if rec := h.get("/fleets"); rec.Code != http.StatusSeeOther && rec.Code != http.StatusUnauthorized {
		t.Errorf("signed-out status %d", rec.Code)
	}
}

func TestFleetsHostileDataInert(t *testing.T) {
	h := newAdminHarness(t)
	evil := `<script>alert(1)</script>`
	h.post("/fleets", wsValues(map[string]string{"images": evil, "egress": evil}))
	b := h.post("/fleets", wsValues(map[string]string{"images": evil})).Body.String()
	if strings.Contains(b, evil) {
		t.Error("raw script in 422 page")
	}
	// A hostile name is rejected by validation and echoed escaped.
	b = h.post("/fleets", url.Values{"name": {`"><img src=x onerror=alert(1)>`}, "mode": {"actions"}}).Body.String()
	if strings.Contains(b, "<img src=x") {
		t.Error("raw markup echoed")
	}
	// Hostile text in the audit log shows on the overview inert.
	h.post("/fleets", url.Values{"name": {"ok"}, "mode": {"actions"}})
	b = wantBody(t, h.get("/"), 200)
	for _, bad := range []string{"<script>alert", "onerror=", " onclick="} {
		if strings.Contains(b, bad) {
			t.Errorf("overview contains %q", bad)
		}
	}
	if !strings.Contains(b, "fleet.create") {
		t.Error("overview lacks the recent audit row")
	}
}

func TestOverviewCountsAndPassive(t *testing.T) {
	h := newAdminHarness(t)
	wantBody(t, h.get("/"), 200, "Overview", "No runners waiting")
	h.post("/fleets", url.Values{"name": {"prod"}, "mode": {"actions"}})
	h.post("/fleets", wsValues(nil))
	b := wantBody(t, h.get("/"), 200, "1 actions, 1 workspace", `hx-get="/overview/live"`, "fleet.create", "Recent activity")
	assertInert(t, "overview", b) // inlineScriptRE, inlineHandler, inlineStyleRE
	// The Online card's denominator is Active + Disabled; none is enrolled yet.
	if !strings.Contains(b, "0 / 0") {
		t.Error("Runners online card lacks 0 / 0")
	}

	// The polled fragment has no layout and is authenticated as Passive.
	h.spy.mu.Lock()
	h.spy.passive = nil
	h.spy.mu.Unlock()
	fr := wantBody(t, h.do("GET", "/overview/live", nil, func(r *http.Request) { r.Header.Set("HX-Request", "true") }),
		200, `id="overview-live"`)
	if strings.Contains(fr, "<html") {
		t.Error("fragment carries the full layout")
	}
	h.spy.mu.Lock()
	defer h.spy.mu.Unlock()
	if len(h.spy.passive) == 0 || !h.spy.passive[len(h.spy.passive)-1] {
		t.Errorf("passive flags = %v", h.spy.passive)
	}
}
