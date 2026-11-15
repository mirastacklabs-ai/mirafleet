package gui

// Tests for the Overview and Fleets screens (M4 step S8). They run against a real service on a
// temp SQLite file. Authentication goes through s8Login, which creates a session through the
// Backend and sets the cookie; swap it for loginForTest once S7's helper is in.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/central/registry"
	"github.com/mirastacklabs-ai/mirafleet/internal/policy"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/store/sqlite"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

const (
	s8Host = "central-01.example.org"
	s8User = "root-admin"
	s8Pass = "correct horse battery"
)

type s8Live struct{}

func (s8Live) Exec(context.Context, int64, transport.ExecRequest) (*transport.ExecResult, error) {
	return nil, nil
}
func (s8Live) Workspace(context.Context, int64, transport.WorkspaceRequest, io.Reader, io.Writer) (*transport.WorkspaceResult, error) {
	return nil, nil
}
func (s8Live) Kick(int64, string)                       {}
func (s8Live) Tombstone(int64)                          {}
func (s8Live) ClearTombstone(int64)                     {}
func (s8Live) SetStatus(int64, registry.Status) error   { return nil }
func (s8Live) Snapshot() []registry.SessionInfo         { return nil }

type s8Conns struct{}

func (s8Conns) KickOperator(int64, string) {}

// s8Spy records the passive flag of every GUIAuthenticate call.
type s8Spy struct {
	Backend
	mu      sync.Mutex
	passive []bool
}

func (b *s8Spy) GUIAuthenticate(ctx context.Context, remote, tok string, passive bool) (auth.Principal, int64, error) {
	b.mu.Lock()
	b.passive = append(b.passive, passive)
	b.mu.Unlock()
	return b.Backend.GUIAuthenticate(ctx, remote, tok, passive)
}

type s8 struct {
	t   *testing.T
	e   *testEnv
	svc *service.Service
	spy *s8Spy
	tok string // session cookie value ("" = signed out)
	adm auth.Principal
}

func newS8(t *testing.T) *s8 {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "mirafleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	host, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := service.New(service.Config{Store: db, Policy: policy.TablePolicy{}, Live: s8Live{}, HostKey: host,
		OperatorConns: s8Conns{}, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	h := &s8{t: t, svc: svc, spy: &s8Spy{Backend: svc}}
	h.e = newEnv(t, func(c *Config) { c.Backend = h.spy })
	h.login()
	return h
}

// login creates the first admin and a session through the Backend and keeps its cookie.
func (h *s8) login() {
	h.t.Helper()
	ctx := context.Background()
	st, _, err := h.svc.EnsureGUISetup(ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	h.tok, h.adm, err = h.svc.CompleteGUISetup(ctx, "192.0.2.77:40000", "test", st, s8User, s8Pass)
	if err != nil {
		h.t.Fatal(err)
	}
}

func (h *s8) csrf() string { return FormToken(CSRFKey(h.tok)) }

func (h *s8) do(method, target string, form url.Values, mod func(*http.Request)) *httptest.ResponseRecorder {
	h.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, target, body)
	r.Host, r.RemoteAddr = s8Host, "192.0.2.77:40000"
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
	h.e.s.Handler().ServeHTTP(rec, r)
	return rec
}

func (h *s8) get(target string) *httptest.ResponseRecorder { return h.do("GET", target, nil, nil) }

// post sends a form with a valid CSRF token added.
func (h *s8) post(target string, form url.Values) *httptest.ResponseRecorder {
	form.Set("_csrf", h.csrf())
	return h.do("POST", target, form, nil)
}

// follow GETs the redirect target of rec, carrying the flash cookie.
func (h *s8) follow(rec *httptest.ResponseRecorder) *httptest.ResponseRecorder {
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

func (h *s8) fleet(name string) (store.Fleet, error) {
	return h.svc.GetFleet(context.Background(), h.adm, service.SourceGUI, name)
}

func (h *s8) fleets() []store.Fleet {
	h.t.Helper()
	fl, err := h.svc.ListFleets(context.Background(), h.adm, service.SourceGUI)
	if err != nil {
		h.t.Fatal(err)
	}
	return fl
}

func (h *s8) audit(action string) []store.AuditEvent {
	h.t.Helper()
	evs, err := h.svc.ListAudit(context.Background(), h.adm, service.SourceGUI, store.AuditFilter{Action: action})
	if err != nil {
		h.t.Fatal(err)
	}
	return evs
}

func wsValues(extra map[string]string) url.Values {
	v := url.Values{"name": {"dev"}, "mode": {"workspace"}, "image": {"docker.io/library/debian:12"}}
	for k, x := range extra {
		v.Set(k, x)
	}
	return v
}

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

func TestFleetsCreateBothModes(t *testing.T) {
	h := newS8(t)
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
			if e.Source != "gui" || e.Actor != s8User || e.ActorKind != "admin" || e.Outcome != "ok" {
				t.Errorf("audit row = %+v", e)
			}
		}
	}
}

func TestFleetsCreate422CreatesNothing(t *testing.T) {
	h := newS8(t)
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
