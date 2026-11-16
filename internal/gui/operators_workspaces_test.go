package gui

// Tests for the Operators and Workspaces screens. They use the shared harness
// (helpers_test.go) and seed operators, runners and workspaces through its store handle.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/tokens"
)

const hostile = `<script>alert(1)</script>"onmouseover="x`

// newOpsHarness is a signed-in harness with the operator registration secret set and the SSH
// advertise address the operators screen prints.
func newOpsHarness(t *testing.T) *guiHarness {
	t.Helper()
	return newAdminHarness(t, withRegistrationSecret(),
		withConfig(func(c *Config) { c.SSHAdvertise = "central-01.example.org:2222" }))
}

func (h *guiHarness) operator(name, fp string) store.Operator {
	h.t.Helper()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	k, _ := ssh.NewPublicKey(pub)
	if fp == "" {
		fp = ssh.FingerprintSHA256(k)
	}
	op, _, err := h.db.RegisterOperator(context.Background(), name, k.Marshal(), fp, time.Now(),
		store.AuditEvent{Time: time.Now(), Source: "enroll", ActorKind: "enroll", Action: "enroll.register", Outcome: "ok"})
	if err != nil {
		h.t.Fatal(err)
	}
	return op
}

func (h *guiHarness) runner(fleet, name string) store.Runner {
	h.t.Helper()
	ctx := context.Background()
	if _, err := h.fleet(fleet); err != nil {
		if _, err := h.svc.CreateFleet(ctx, h.adm, service.SourceGUI, fleet, "", false, nil); err != nil {
			h.t.Fatal(err)
		}
	}
	ct, err := h.svc.CreateToken(ctx, h.adm, service.SourceGUI, service.TokenParams{Fleet: fleet, Approve: true})
	if err != nil {
		h.t.Fatal(err)
	}
	id, secret, _, err := tokens.ParseJoinToken(ct.Token)
	if err != nil {
		h.t.Fatal(err)
	}
	sum := sha256.Sum256(secret)
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	k, _ := ssh.NewPublicKey(pub)
	r, err := h.db.Enroll(ctx, id, sum[:], time.Now(), store.Runner{Name: name, PubKey: k.Marshal(), Fingerprint: ssh.FingerprintSHA256(k)},
		store.AuditEvent{Time: time.Now(), Source: "enroll", ActorKind: "enroll", Action: "enroll.join", Outcome: "ok", Fleet: fleet, Runner: name})
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

// ws seeds an open workspace in fleet on runner r owned by op.
func (h *guiHarness) ws(fleet string, r store.Runner, op store.Operator, session, image string) store.Workspace {
	h.t.Helper()
	ctx := context.Background()
	f, err := h.fleet(fleet)
	if err != nil {
		h.t.Fatal(err)
	}
	h.n++
	now := time.Now()
	ev := store.AuditEvent{Time: now, Source: "system", ActorKind: "system", Action: "test.row", Outcome: "ok"}
	w, _, err := h.db.ReserveWorkspace(ctx, store.Workspace{WsID: fmt.Sprintf("%032x", h.n), FleetID: f.ID, RunnerID: r.ID,
		OperatorID: op.ID, Session: session, Image: image, Spec: `{}`, ExpiresAt: now.Add(24 * time.Hour)}, 10, 0, now, ev)
	if err != nil {
		h.t.Fatal(err)
	}
	w, err = h.db.SetWorkspaceState(ctx, w.WsID, []store.WorkspaceState{store.WorkspaceOpening}, store.WorkspaceOpen, "test", now, ev)
	if err != nil {
		h.t.Fatal(err)
	}
	return w
}

func (h *guiHarness) wsState(id string) store.WorkspaceState {
	h.t.Helper()
	w, err := h.db.GetWorkspace(context.Background(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return w.State
}

func (h *guiHarness) opStatus(name string) store.OperatorStatus {
	h.t.Helper()
	ops, err := h.svc.ListOperators(context.Background(), h.adm, service.SourceGUI, true)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, o := range ops {
		if o.Name == name {
			return o.Status
		}
	}
	h.t.Fatalf("operator %q not found", name)
	return ""
}

var codeRE = regexp.MustCompile(`id="copy-reg-code">([^<]+)<`)

func showCode(t *testing.T, h *guiHarness, mod func(*http.Request)) (string, *httptest.ResponseRecorder) {
	t.Helper()
	rec := h.do("POST", "/operators/code", url.Values{"_csrf": {h.csrf()}}, mod)
	if rec.Code != 200 {
		t.Fatalf("show code: status %d: %.300s", rec.Code, rec.Body.String())
	}
	m := codeRE.FindStringSubmatch(rec.Body.String())
	if m == nil {
		t.Fatalf("no code in body: %.600s", rec.Body.String())
	}
	return m[1], rec
}

func TestOperatorsCodeOnlyOnPostAuditedOncePerShow(t *testing.T) {
	h := newOpsHarness(t)
	h.operator("alice", "")

	body := wantBody(t, h.get("/operators"), 200, "Show code", "alice", `action="/operators/code"`)
	if codeRE.MatchString(body) || strings.Contains(body, "reg-fp") {
		t.Fatal("GET /operators carries a code")
	}
	// Only <main> is checked: the layout carries htmx attributes of its own.
	mainStart, mainEnd := strings.Index(body, "<main"), strings.Index(body, "</main>")
	if mainStart < 0 || mainEnd < mainStart {
		t.Fatal("operators page has no <main>")
	}
	if strings.Contains(body[mainStart:mainEnd], "hx-trigger") {
		t.Error("operators page polls")
	}
	if n := len(h.audit(api.MethodOperatorCode)); n != 0 {
		t.Fatalf("audit rows after GET = %d", n)
	}
	if rec := h.get("/operators/code"); rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound {
		t.Errorf("GET /operators/code = %d", rec.Code)
	}

	code, rec := showCode(t, h, nil)
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}
	for _, s := range []string{"central-01.example.org:2222 -name NAME -code -", "SHA256:", `data-countdown="`, "Show the next code"} {
		if !strings.Contains(rec.Body.String(), s) {
			t.Errorf("code page lacks %q", s)
		}
	}
	if rec.Header().Get("Location") != "" {
		t.Error("show code must render directly, not redirect")
	}
	evs := h.audit(api.MethodOperatorCode)
	if len(evs) != 1 || evs[0].Source != "gui" || evs[0].Actor != testUser {
		t.Fatalf("audit after one show = %+v", evs)
	}
	if strings.Contains(fmt.Sprint(evs[0]), code) {
		t.Error("audit row holds the code")
	}

	// htmx gets only the panel; a second show is a second audit row.
	code2, rec := showCode(t, h, func(r *http.Request) { r.Header.Set("HX-Request", "true") })
	if strings.Contains(rec.Body.String(), "<html") || !strings.Contains(rec.Body.String(), `id="operators-code"`) {
		t.Errorf("htmx answer is not the panel: %.200s", rec.Body.String())
	}
	if n := len(h.audit(api.MethodOperatorCode)); n != 2 {
		t.Errorf("audit rows after two shows = %d", n)
	}

	// Nothing a GET or poll returns holds the code, and no GET adds an audit row.
	for _, p := range []string{"/operators", "/workspaces", "/workspaces/live", "/overview/live", "/", "/fleets"} {
		for _, hx := range []bool{false, true} {
			hx := hx
			rec := h.do("GET", p, nil, func(r *http.Request) {
				if hx {
					r.Header.Set("HX-Request", "true")
				}
			})
			if strings.Contains(rec.Body.String(), code) || strings.Contains(rec.Body.String(), code2) {
				t.Errorf("GET %s (hx=%v) shows the code", p, hx)
			}
		}
	}
	if n := len(h.audit(api.MethodOperatorCode)); n != 2 {
		t.Errorf("audit rows after GETs = %d", n)
	}
}

func TestOperatorsRotateSecret(t *testing.T) {
	h := newOpsHarness(t)
	before, _ := showCode(t, h, nil)

	wantBody(t, h.get("/operators/rotate"), 200, "Rotate the registration secret", `action="/operators/rotate"`, "stops working")
	if n := len(h.audit(api.MethodOperatorRotateSecret)); n != 0 {
		t.Fatalf("rotate audited by GET: %d", n)
	}
	rec := h.post("/operators/rotate", url.Values{})
	if rec.Header().Get("Location") != "/operators" {
		t.Fatalf("rotate: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	wantBody(t, h.follow(rec), 200, "Registration secret rotated")
	if n := len(h.audit(api.MethodOperatorRotateSecret)); n != 1 {
		t.Errorf("rotate audit rows = %d", n)
	}
	if after, _ := showCode(t, h, nil); after == before {
		t.Error("the code did not change after rotation")
	}
}

func TestOperatorsRevokeNeedsName(t *testing.T) {
	h := newOpsHarness(t)
	h.operator("alice", "")
	h.operator("bob", "")

	wantBody(t, h.get("/operators"), 200, "/operators/alice/revoke", "/operators/bob/revoke")
	wantBody(t, h.get("/operators/alice/revoke"), 200, "<code>alice</code> to confirm", "dropped", "btn-danger")

	for _, typed := range []string{"", "bob", "ALICE", "alic"} {
		rec := h.post("/operators/alice/revoke", url.Values{"confirm": {typed}})
		wantBody(t, rec, 422, "Type the operator name exactly")
		if st := h.opStatus("alice"); st != store.OperatorActive {
			t.Fatalf("typed %q: status %s", typed, st)
		}
	}
	if n := len(h.audit(api.MethodOperatorRevoke)); n != 0 {
		t.Fatalf("revoke audit rows = %d", n)
	}
	rec := h.post("/operators/alice/revoke", url.Values{"confirm": {"alice"}})
	wantBody(t, h.follow(rec), 200, "Operator alice revoked.")
	if h.opStatus("alice") != store.OperatorRevoked || h.opStatus("bob") != store.OperatorActive {
		t.Error("wrong operators revoked")
	}
	if n := len(h.audit(api.MethodOperatorRevoke)); n != 1 {
		t.Errorf("revoke audit rows = %d", n)
	}
	// Revoked, unknown and malformed names are 404 for the dialog and the POST.
	for _, p := range []string{"/operators/alice/revoke", "/operators/nobody/revoke", "/operators/A%2Fb/revoke"} {
		if rec := h.get(p); rec.Code != 404 {
			t.Errorf("GET %s = %d", p, rec.Code)
		}
		if rec := h.post(p, url.Values{"confirm": {"alice"}}); rec.Code != 404 {
			t.Errorf("POST %s = %d", p, rec.Code)
		}
	}
	// htmx 422 swaps the dialog.
	rec = h.do("POST", "/operators/bob/revoke", url.Values{"_csrf": {h.csrf()}, "confirm": {"x"}},
		func(r *http.Request) { r.Header.Set("HX-Request", "true") })
	if b := wantBody(t, rec, 422, `id="op-dialog"`); strings.Contains(b, "<html") {
		t.Error("fragment is a full page")
	}
}

func TestWorkspacesListFiltersAndPassivePoll(t *testing.T) {
	h := newOpsHarness(t)
	wantBody(t, h.get("/workspaces"), 200, "No workspaces")
	op := h.operator("alice", "")
	r1, r2 := h.runner("prod", "p1"), h.runner("dev", "d1")
	w1 := h.ws("prod", r1, op, "sess-a", "docker.io/library/debian:12")
	w2 := h.ws("dev", r2, op, "sess-b", "docker.io/library/alpine:3")

	wantBody(t, h.get("/workspaces"), 200, w1.WsID, w2.WsID[:8], "prod", "p1", "alice", "sess-a", "docker.io/library/debian:12",
		"open", "<time datetime=", `/workspaces/`+w1.WsID+`/kill`, `hx-get="/workspaces/live?state=live"`)
	b := wantBody(t, h.get("/workspaces?fleet=dev"), 200, "sess-b")
	if strings.Contains(b, "sess-a") {
		t.Error("fleet filter ignored")
	}
	if b := h.get("/workspaces?fleet=" + url.QueryEscape(hostile)).Body.String(); !strings.Contains(b, "sess-a") || strings.Contains(b, "<script>alert") {
		t.Error("unknown fleet filter should be ignored and never echoed")
	}

	// Closed workspaces show only under state=all.
	if _, err := h.svc.KillWorkspace(context.Background(), h.adm, service.SourceGUI, w1.WsID); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h.get("/workspaces").Body.String(), "sess-a") {
		t.Error("closed workspace in the live view")
	}
	wantBody(t, h.get("/workspaces?state=all"), 200, "sess-a", "closed")

	// The poll goes through the passive route and returns a fragment.
	h.spy.mu.Lock()
	h.spy.passive = nil
	h.spy.mu.Unlock()
	rec := h.do("GET", "/workspaces/live?state=all", nil, func(r *http.Request) { r.Header.Set("HX-Request", "true") })
	if b := wantBody(t, rec, 200, `id="ws-live"`); strings.Contains(b, "<html") {
		t.Error("poll returned a full page")
	}
	h.spy.mu.Lock()
	defer h.spy.mu.Unlock()
	if len(h.spy.passive) == 0 || !h.spy.passive[len(h.spy.passive)-1] {
		t.Errorf("poll was not passive: %v", h.spy.passive)
	}
}

func TestWorkspaceKillClosedOrClosing(t *testing.T) {
	h := newOpsHarness(t)
	op := h.operator("alice", "")
	r := h.runner("prod", "p1")
	w1 := h.ws("prod", r, op, "a", "img")
	w2 := h.ws("prod", r, op, "b", "img")

	wantBody(t, h.get("/workspaces/"+w1.WsID+"/kill"), 200, "Kill workspace "+w1.WsID[:8], w1.WsID, "btn-danger")
	if st := h.wsState(w1.WsID); st != store.WorkspaceOpen {
		t.Fatalf("GET changed state: %s", st)
	}

	// Runner confirms: closed.
	rec := h.post("/workspaces/"+w1.WsID+"/kill", url.Values{})
	if loc := rec.Header().Get("Location"); loc != "/workspaces?state=all" {
		t.Fatalf("Location %q (status %d)", loc, rec.Code)
	}
	wantBody(t, h.follow(rec), 200, "is closed.", "closed")
	if st := h.wsState(w1.WsID); st != store.WorkspaceClosed {
		t.Errorf("state = %s, want closed", st)
	}

	// Runner unavailable: closing, explained.
	h.live.res, h.live.err = nil, fmt.Errorf("runner offline")
	rec = h.post("/workspaces/"+w2.WsID+"/kill", url.Values{})
	wantBody(t, h.follow(rec), 200, "is closing.", "reaper")
	if st := h.wsState(w2.WsID); st != store.WorkspaceClosing {
		t.Errorf("state = %s, want closing", st)
	}
	if n := len(h.audit(api.MethodWorkspaceKill)); n < 2 {
		t.Errorf("kill audit rows = %d", n)
	}

	// A finished workspace has no kill button; a bad or unknown id is 404.
	if b := h.get("/workspaces?state=all").Body.String(); strings.Contains(b, "/workspaces/"+w1.WsID+"/kill") {
		t.Error("closed workspace offers Kill")
	}
	for _, id := range []string{"nope", strings.Repeat("0", 32)} {
		if rec := h.get("/workspaces/" + id + "/kill"); rec.Code != 404 {
			t.Errorf("GET kill %s = %d", id, rec.Code)
		}
	}
	if rec := h.post("/workspaces/nope/kill", url.Values{}); rec.Code != 404 {
		t.Errorf("POST kill nope = %d", rec.Code)
	}
}

func TestOperatorsWorkspacesHostileDataInert(t *testing.T) {
	h := newOpsHarness(t)
	op := h.operator("alice", hostile)
	r := h.runner("prod", "p1")
	w := h.ws("prod", r, op, hostile, hostile)

	for _, p := range []string{"/operators", "/workspaces", "/workspaces/live"} {
		b := h.get(p).Body.String()
		if strings.Contains(b, "<script>alert") || strings.Contains(b, `"onmouseover="x`) {
			t.Errorf("GET %s: hostile data not escaped", p)
		}
		if !strings.Contains(b, "&lt;script&gt;alert(1)&lt;/script&gt;") {
			t.Errorf("GET %s: escaped form missing", p)
		}
	}
	if b := h.get("/workspaces/" + w.WsID + "/kill").Body.String(); strings.Contains(b, "<script>alert") {
		t.Error("kill dialog: hostile data not escaped")
	}
}

func TestOperatorsWorkspacesCSRFOnEveryPost(t *testing.T) {
	h := newOpsHarness(t)
	op := h.operator("alice", "")
	r := h.runner("prod", "p1")
	w := h.ws("prod", r, op, "a", "img")

	posts := []struct {
		path string
		form url.Values
	}{
		{"/operators/code", url.Values{}},
		{"/operators/rotate", url.Values{}},
		{"/operators/alice/revoke", url.Values{"confirm": {"alice"}}},
		{"/workspaces/" + w.WsID + "/kill", url.Values{}},
	}
	for _, p := range posts {
		for name, set := range map[string]func(url.Values){
			"missing":       func(url.Values) {},
			"wrong":         func(v url.Values) { v.Set("_csrf", "AAAA") },
			"other-session": func(v url.Values) { v.Set("_csrf", FormToken(CSRFKey("another-session-token"))) },
		} {
			f := url.Values{}
			for k, v := range p.form {
				f[k] = v
			}
			set(f)
			if rec := h.do("POST", p.path, f, nil); rec.Code != 403 {
				t.Errorf("POST %s (%s csrf) = %d, want 403", p.path, name, rec.Code)
			}
		}
		// A cross-site request with a valid token is refused too.
		f := url.Values{"_csrf": {h.csrf()}}
		for k, v := range p.form {
			f[k] = v
		}
		if rec := h.do("POST", p.path, f, func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }); rec.Code != 403 {
			t.Errorf("POST %s cross-site = %d, want 403", p.path, rec.Code)
		}
	}
	if n := len(h.audit(api.MethodOperatorCode)) + len(h.audit(api.MethodOperatorRotateSecret)) +
		len(h.audit(api.MethodOperatorRevoke)) + len(h.audit(api.MethodWorkspaceKill)); n != 0 {
		t.Errorf("a refused POST had an effect (%d audit rows)", n)
	}
	if h.opStatus("alice") != store.OperatorActive || h.wsState(w.WsID) != store.WorkspaceOpen {
		t.Error("state changed")
	}
}

// Session and image text goes through inertRunnerText, so a bidi control never reaches the page.
func TestWorkspacesShowInertText(t *testing.T) {
	h := newOpsHarness(t)
	op := h.operator("alice", "fp-bidi")
	r := h.runner("prod", "p1")
	h.ws("prod", r, op, "sess‮evil", "img‮gpj")
	if b := h.get("/workspaces").Body.String(); strings.Contains(b, "‮") || !strings.Contains(b, "sess") {
		t.Errorf("bidi control reached the workspaces page (or the row is missing)")
	}
}
