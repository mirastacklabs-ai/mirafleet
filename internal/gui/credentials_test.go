package gui

// Tests for the Credentials screens (M7 step S14): create, replace, assign, delete with a typed
// confirmation, the runner detail panel, and a decoy scan proving a secret value appears in no
// response, log or audit row.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

const decoyToken = "glpat-DECOY-8f3a91c2b7d4e605-NEVER-SHOWN"

func newCredHarness(t *testing.T, opts ...harnessOpt) *guiHarness {
	t.Helper()
	h := newGUIHarness(t, opts...)
	h.c = loginForTest(t, h)
	return h
}

func tokenCredForm(name string) url.Values {
	return url.Values{"name": {name}, "kind": {"https-token"}, "hosts": {"gitlab.example.com\ngit.example.com:8443"},
		"value": {decoyToken + "\r\n"}, "auth": {"bearer"}, "host_binding": {"required"}}
}

func (h *guiHarness) createCred(name string) {
	h.t.Helper()
	rec := h.c.post("/credentials/new", "/credentials", tokenCredForm(name))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/credentials/"+name {
		h.t.Fatalf("create %s: %d %q\n%s", name, rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
}

// leakText is everything a decoy must never appear in: responses, the server log and audit rows.
func (h *guiHarness) leakText() string {
	h.t.Helper()
	evs, err := h.db.ListAudit(context.Background(), 0, 1000)
	if err != nil {
		h.t.Fatal(err)
	}
	var b strings.Builder
	for _, ev := range evs {
		fmt.Fprintf(&b, "%+v\n", ev)
	}
	return h.seenText() + "\n" + h.logText() + "\n" + b.String()
}

func TestCredentialCreateMaskedAndAudited(t *testing.T) {
	h := newCredHarness(t)
	h.createCred("gitlab-ci")

	// The flash and the detail page show metadata only.
	page := h.c.get("/credentials/gitlab-ci")
	body := page.Body.String()
	for _, want := range []string{"gitlab-ci", "https-token", "gitlab.example.com", "git.example.com:8443", "SHA256:", "saved", "write-only"} {
		if !strings.Contains(body, want) {
			t.Errorf("detail lacks %q", want)
		}
	}
	list := h.c.get("/credentials").Body.String()
	if !strings.Contains(list, `href="/credentials/gitlab-ci"`) || !strings.Contains(list, "Not assigned") {
		t.Errorf("list:\n%s", list)
	}
	if n := h.auditCount("credential.set"); n != 1 {
		t.Errorf("credential.set audit rows = %d, want 1", n)
	}

	// Walk every page that can show it, then scan.
	for _, p := range []string{"/credentials/new", "/credentials/gitlab-ci/replace", "/credentials/gitlab-ci/delete", "/", "/audit", "/runners", "/tokens", "/fleets"} {
		h.c.get(p)
	}
	for _, r := range secRoutes(t) {
		if r.method == "GET" && !strings.HasPrefix(r.path, "/static/") {
			h.c.get(r.path)
		}
	}
	if all := h.leakText(); strings.Contains(all, "DECOY") {
		t.Fatal("the secret value appears in a response, the log or an audit row")
	}
}

func TestCredentialValidationDoesNotEchoValue(t *testing.T) {
	h := newCredHarness(t)
	f := tokenCredForm("bad")
	f.Set("hosts", "")
	rec := h.c.post("/credentials/new", "/credentials", f)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d", rec.Code)
	}
	b := rec.Body.String()
	if strings.Contains(b, "DECOY") || !strings.Contains(b, "List 1 to") || !strings.Contains(b, `value="bad"`) {
		t.Errorf("form re-render:\n%s", b)
	}
	if n := h.auditCount("credential.set"); n != 0 {
		t.Errorf("audit rows = %d", n)
	}

	// An existing name is refused on create (replace is explicit).
	h.createCred("dup")
	rec = h.c.post("/credentials/new", "/credentials", tokenCredForm("dup"))
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "Replace value") {
		t.Errorf("duplicate create: %d", rec.Code)
	}
	// Server-side validation (basic auth without a user) is a form error too.
	f = tokenCredForm("basic")
	f.Set("auth", "basic")
	if rec = h.c.post("/credentials/new", "/credentials", f); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("basic without user: %d", rec.Code)
	}
	if strings.Contains(h.leakText(), "DECOY") {
		t.Error("value leaked")
	}
}

func TestCredentialNoSecretsKeyIs503(t *testing.T) {
	h := newCredHarness(t, withoutSecretsKey())
	rec := h.c.post("/credentials/new", "/credentials", tokenCredForm("nokey"))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "MIRAFLEET_SECRETS_KEY") {
		t.Fatalf("%d\n%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(h.leakText(), "DECOY") {
		t.Error("value leaked")
	}
	if rec := h.c.get("/credentials"); rec.Code != http.StatusOK {
		t.Errorf("list without key: %d", rec.Code)
	}
}

func TestCredentialSSHKeyKind(t *testing.T) {
	h := newCredHarness(t)
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	blk, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	pemText := string(pem.EncodeToMemory(blk))
	pub, _ := ssh.NewPublicKey(priv.Public())
	kh := "gitlab.example.com " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))
	rec := h.c.post("/credentials/new", "/credentials", url.Values{"name": {"deploy-key"}, "kind": {"ssh-key"},
		"hosts": {"gitlab.example.com"}, "value": {strings.ReplaceAll(pemText, "\n", "\r\n")}, "known_hosts": {kh},
		"host_binding": {"required"}, "auth": {"bearer"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create ssh-key: %d\n%s", rec.Code, rec.Body.String())
	}
	body := h.c.get("/credentials/deploy-key").Body.String()
	if !strings.Contains(body, "ssh-key") || !strings.Contains(body, "SHA256:") || !strings.Contains(body, "required") {
		t.Errorf("detail:\n%s", body)
	}
	// No known_hosts is a field error.
	rec = h.c.post("/credentials/new", "/credentials", url.Values{"name": {"k2"}, "kind": {"ssh-key"}, "hosts": {"gitlab.example.com"}, "value": {pemText}})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "known_hosts line") {
		t.Errorf("no known_hosts: %d", rec.Code)
	}
	body64 := strings.Split(strings.TrimSpace(pemText), "\n")[1]
	if strings.Contains(h.leakText(), body64) {
		t.Error("key material leaked")
	}
}

func TestCredentialReplaceBumpsVersion(t *testing.T) {
	h := newCredHarness(t)
	h.createCred("rot")
	form := h.c.get("/credentials/rot/replace").Body.String()
	for _, want := range []string{"gitlab.example.com", "never shown", `action="/credentials/rot/replace"`} {
		if !strings.Contains(form, want) {
			t.Errorf("replace form lacks %q", want)
		}
	}
	f := tokenCredForm("ignored")
	f.Set("value", "glpat-DECOY-second-value")
	rec := h.c.post("/credentials/rot/replace", "/credentials/rot/replace", f)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/credentials/rot" {
		t.Fatalf("replace: %d", rec.Code)
	}
	if !strings.Contains(h.c.get("/credentials/rot").Body.String(), `<dt>Version</dt><dd class="mono">2</dd>`) {
		t.Error("version not bumped")
	}
	if n := h.auditCount("credential.set"); n != 2 {
		t.Errorf("audit rows = %d, want 2", n)
	}
	if rec := h.c.get("/credentials/nope/replace"); rec.Code != http.StatusNotFound {
		t.Errorf("replace of a missing credential: %d", rec.Code)
	}
	if strings.Contains(h.leakText(), "DECOY") {
		t.Error("value leaked")
	}
}

func TestCredentialAssignDeliveryAndRunnerDetail(t *testing.T) {
	h := newCredHarness(t)
	h.mkFleet("dev")
	h.addRunner("dev", "r1", true, nil)
	h.createCred("gl")

	page := h.c.get("/credentials/gl").Body.String()
	if !strings.Contains(page, `<option value="dev">`) || !strings.Contains(page, `<option value="dev/r1">`) {
		t.Fatalf("assign targets missing:\n%s", page)
	}
	rec := h.c.post("/credentials/gl", "/credentials/gl/assign", url.Values{"target": {"dev/r1"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("assign: %d", rec.Code)
	}
	page = h.c.get("/credentials/gl").Body.String()
	for _, want := range []string{"dev/r1", "Pending", "Assigned", "Unassign"} {
		if !strings.Contains(page, want) {
			t.Errorf("detail lacks %q", want)
		}
	}
	if !strings.Contains(h.c.get("/credentials").Body.String(), "1 runner") {
		t.Error("list lacks the assignment count")
	}
	rd := h.c.get("/runners/dev/r1").Body.String()
	if !strings.Contains(rd, "Credentials held") || !strings.Contains(rd, `href="/credentials/gl"`) || !strings.Contains(rd, "gitlab.example.com") {
		t.Errorf("runner detail lacks the credential:\n%s", rd)
	}
	if n := h.auditCount("credential.assign"); n != 1 {
		t.Errorf("assign audit rows = %d", n)
	}

	// Whole-fleet assignment, a duplicate and a bad target all end in a flash, not an error page.
	if rec = h.c.post("/credentials/gl", "/credentials/gl/assign", url.Values{"target": {"dev"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("assign fleet: %d", rec.Code)
	}
	for _, tgt := range []string{"dev", "nofleet", "dev/../x", ""} {
		if rec = h.c.post("/credentials/gl", "/credentials/gl/assign", url.Values{"target": {tgt}}); rec.Code != http.StatusSeeOther {
			t.Errorf("assign %q: %d", tgt, rec.Code)
		}
	}
	rec = h.c.post("/credentials/gl", "/credentials/gl/unassign", url.Values{"target": {"dev/r1"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("unassign: %d", rec.Code)
	}
	if n := h.auditCount("credential.unassign"); n != 1 {
		t.Errorf("unassign audit rows = %d", n)
	}
	if strings.Contains(h.leakText(), "DECOY") {
		t.Error("value leaked")
	}
}

func TestCredentialDeleteNeedsTypedName(t *testing.T) {
	h := newCredHarness(t)
	h.createCred("gone")
	if body := h.c.get("/credentials/gone/delete").Body.String(); !strings.Contains(body, "Type <code>gone</code>") {
		t.Errorf("delete page:\n%s", body)
	}
	for _, typed := range []string{"", "gon", "GONE", "gone-x"} {
		rec := h.c.post("/credentials/gone/delete", "/credentials/gone/delete", url.Values{"confirm": {typed}})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("typed %q: %d", typed, rec.Code)
		}
	}
	if rec := h.c.get("/credentials/gone"); rec.Code != http.StatusOK {
		t.Fatalf("credential vanished: %d", rec.Code)
	}
	rec := h.c.post("/credentials/gone/delete", "/credentials/gone/delete", url.Values{"confirm": {"gone"}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/credentials" {
		t.Fatalf("delete: %d", rec.Code)
	}
	if rec := h.c.get("/credentials/gone"); rec.Code != http.StatusNotFound {
		t.Errorf("after delete: %d", rec.Code)
	}
	if n := h.auditCount("credential.delete"); n != 1 {
		t.Errorf("delete audit rows = %d", n)
	}
}

func TestCredentialRotateRefusedWhenNotConfigured(t *testing.T) {
	h := newCredHarness(t)
	h.createCred("plain")
	if strings.Contains(h.c.get("/credentials/plain").Body.String(), "Rotate now") {
		t.Error("rotate offered for a credential without auto-rotation")
	}
	rec := h.c.post("/credentials/plain", "/credentials/plain/rotate", url.Values{})
	if rec.Code != http.StatusSeeOther {
		t.Errorf("rotate: %d", rec.Code)
	}
	// Rotation needs an expiry and a project.
	f := tokenCredForm("auto")
	f.Set("rotate", "1")
	if rec = h.c.post("/credentials/new", "/credentials", f); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("rotate without project/expiry: %d", rec.Code)
	}
	f.Set("rotate_project", "group/proj")
	f.Set("expires", "2099-01-02")
	if rec = h.c.post("/credentials/new", "/credentials", f); rec.Code != http.StatusSeeOther {
		t.Fatalf("rotate create: %d\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(h.c.get("/credentials/auto").Body.String(), "Rotate now") {
		t.Error("no Rotate now button")
	}
}

func TestCredentialRoutesNeedCSRF(t *testing.T) {
	h := newCredHarness(t)
	h.createCred("c")
	for _, p := range []string{"/credentials", "/credentials/c/replace", "/credentials/c/assign", "/credentials/c/unassign", "/credentials/c/rotate", "/credentials/c/delete"} {
		if rec := h.c.do("POST", p, url.Values{"confirm": {"c"}}, nil); rec.Code != http.StatusForbidden {
			t.Errorf("POST %s without a token: %d", p, rec.Code)
		}
	}
	if rec := h.c.get("/credentials/c"); rec.Code != http.StatusOK {
		t.Errorf("credential gone: %d", rec.Code)
	}
}
