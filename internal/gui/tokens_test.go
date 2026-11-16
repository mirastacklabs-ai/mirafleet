package gui

// Tests for the Join tokens screens: the token is shown once, the signed nonce plus
// the atomic MarkUsed stop a second token, and the token is in no other response.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	nonceRE = regexp.MustCompile(`name="nonce" value="([^"]+)"`)
	tokenRE = regexp.MustCompile(`id="copy-token">([^<]+)</pre>`)
)

// tokenForm loads /tokens/new and returns its CSRF token and nonce.
func (h *guiHarness) tokenForm() (csrf, nonce string) {
	h.t.Helper()
	rec := h.c.get("/tokens/new")
	if rec.Code != http.StatusOK {
		h.t.Fatalf("GET /tokens/new: %d", rec.Code)
	}
	c, n := csrfRE.FindStringSubmatch(rec.Body.String()), nonceRE.FindStringSubmatch(rec.Body.String())
	if c == nil || n == nil {
		h.t.Fatalf("form lacks csrf or nonce:\n%s", rec.Body.String())
	}
	return c[1], n[1]
}

func tokenValues(csrf, nonce string) url.Values {
	return url.Values{"_csrf": {csrf}, "nonce": {nonce}, "fleet": {"dev"}, "labels": {"env=ci\nteam=core"},
		"uses": {"1"}, "ttl": {"24h"}, "approve": {"1"}}
}

func (h *guiHarness) tokenCount() int {
	h.t.Helper()
	ts, err := h.svc.ListTokens(context.Background(), h.adm, "gui", "")
	if err != nil {
		h.t.Fatal(err)
	}
	return len(ts)
}

func TestTokenShownOnceNoStore(t *testing.T) {
	h := newRunnersHarness(t, false)
	h.mkFleet("dev")
	csrf, nonce := h.tokenForm()

	rec := h.c.do("POST", "/tokens", tokenValues(csrf, nonce), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d\n%s", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q", cc)
	}
	body := rec.Body.String()
	m := tokenRE.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no token in the create response:\n%s", body)
	}
	tok := m[1]
	for _, want := range []string{"cannot be shown again", "mirafleet runner join central.example.org:2222 -name NAME -token -", "Expires"} {
		if !strings.Contains(body, want) {
			t.Errorf("result lacks %q", want)
		}
	}
	if h.tokenCount() != 1 {
		t.Fatalf("tokens = %d", h.tokenCount())
	}

	// Every other page, the list included, is free of the token (and its secret half).
	for _, p := range []string{"/tokens", "/tokens?finished=1", "/tokens/new", "/", "/fleets/dev", "/audit", "/runners"} {
		h.c.get(p)
	}
	if n := strings.Count(h.seenText(), tok); n != 1 {
		t.Errorf("token appears %d times across all responses, want exactly 1 (the create response)", n)
	}
	list := h.c.get("/tokens").Body.String()
	if !strings.Contains(list, "env=ci") || strings.Contains(list, tok) {
		t.Errorf("list page:\n%s", list)
	}
	if logs := h.logText(); strings.Contains(logs, tok) {
		t.Error("token in the server log")
	}

	// The token is a working join token.
	h.addRunnerWithToken("dev", "r1", tok)
}

func TestTokenNonceReplayShowsNoSecondToken(t *testing.T) {
	h := newRunnersHarness(t, false)
	h.mkFleet("dev")
	csrf, nonce := h.tokenForm()
	first := h.c.do("POST", "/tokens", tokenValues(csrf, nonce), nil)
	if tokenRE.FindString(first.Body.String()) == "" {
		t.Fatal("first submit minted no token page")
	}
	for i := 0; i < 3; i++ { // refresh / back-button resubmissions
		rec := h.c.do("POST", "/tokens", tokenValues(csrf, nonce), nil)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Cache-Control"), "no-store") {
			t.Fatalf("replay: %d %q", rec.Code, rec.Header().Get("Cache-Control"))
		}
		b := rec.Body.String()
		if tokenRE.MatchString(b) || !strings.Contains(b, "already created") || !strings.Contains(b, "not shown again") {
			t.Fatalf("replay body:\n%s", b)
		}
	}
	if h.tokenCount() != 1 {
		t.Fatalf("tokens = %d, want 1", h.tokenCount())
	}
	if n := h.auditCount("token.create"); n != 1 {
		t.Errorf("token.create audit rows = %d", n)
	}
}

func TestTokenConcurrentDoubleSubmitMintsOne(t *testing.T) {
	h := newRunnersHarness(t, false)
	h.mkFleet("dev")
	csrf, nonce := h.tokenForm()
	form := tokenValues(csrf, nonce)
	sess := h.c.jar[SessionCookieName]

	const n = 16
	var wg sync.WaitGroup
	recs := make([]*httptest.ResponseRecorder, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := httptest.NewRequest("POST", "/tokens", strings.NewReader(form.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.Host, r.RemoteAddr = testHost, "198.51.100.200:5000"
			r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sess})
			rec := httptest.NewRecorder()
			<-start
			h.srv.Handler().ServeHTTP(rec, r)
			recs[i] = rec
		}(i)
	}
	close(start)
	wg.Wait()

	shown := 0
	for _, rec := range recs {
		if rec.Code != http.StatusOK {
			t.Errorf("status %d: %.200s", rec.Code, rec.Body.String())
		}
		if tokenRE.MatchString(rec.Body.String()) {
			shown++
		}
	}
	if shown != 1 {
		t.Errorf("%d responses showed a token, want 1", shown)
	}
	if h.tokenCount() != 1 {
		t.Errorf("tokens = %d, want 1", h.tokenCount())
	}
}

func TestTokenNonceChecks(t *testing.T) {
	h := newRunnersHarness(t, false)
	h.mkFleet("dev")
	csrf, nonce := h.tokenForm()

	// Missing, forged, truncated nonces mint nothing.
	for name, n := range map[string]string{"missing": "", "forged": "9999999999.AAAA.AAAA", "tampered": nonce + "x", "garbage": "x.y.z"} {
		rec := h.c.do("POST", "/tokens", tokenValues(csrf, n), nil)
		if rec.Code != http.StatusUnprocessableEntity || tokenRE.MatchString(rec.Body.String()) {
			t.Errorf("%s nonce: %d", name, rec.Code)
		}
	}
	// A nonce signed for another session does not verify.
	rawN, _ := NewToken()
	otherKey, _ := NewToken() // another session's key
	foreign := SignNonce(CSRFKey(otherKey), rawN, h.clk.now(), nonceTTL)
	if rec := h.c.do("POST", "/tokens", tokenValues(csrf, foreign), nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("foreign nonce: %d", rec.Code)
	}
	if h.tokenCount() != 0 {
		t.Fatalf("tokens = %d", h.tokenCount())
	}

	// Field errors do not burn the nonce.
	bad := tokenValues(csrf, nonce)
	bad.Set("uses", "0")
	rec := h.c.do("POST", "/tokens", bad, nil)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "Uses must be") {
		t.Fatalf("bad uses: %d", rec.Code)
	}
	bad.Set("uses", "1")
	bad.Set("labels", "novalue")
	if rec = h.c.do("POST", "/tokens", bad, nil); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "key=value") {
		t.Fatalf("bad labels: %d", rec.Code)
	}
	if rec = h.c.do("POST", "/tokens", tokenValues(csrf, nonce), nil); !tokenRE.MatchString(rec.Body.String()) {
		t.Fatalf("valid retry with the same nonce failed: %d", rec.Code)
	}

	// A nonce past its lifetime is refused.
	csrf, _ = h.tokenForm()
	raw, _ := NewToken()
	nonce = SignNonce(CSRFKey(h.c.jar[SessionCookieName]), raw, h.clk.now().Add(-2*nonceTTL), nonceTTL)
	if rec = h.c.do("POST", "/tokens", tokenValues(csrf, nonce), nil); rec.Code != http.StatusUnprocessableEntity || tokenRE.MatchString(rec.Body.String()) {
		t.Errorf("expired nonce: %d", rec.Code)
	}
}

func TestTokenRevokeAndFilters(t *testing.T) {
	h := newRunnersHarness(t, false)
	h.mkFleet("dev")
	csrf, nonce := h.tokenForm()
	h.c.do("POST", "/tokens", tokenValues(csrf, nonce), nil)
	ts, _ := h.svc.ListTokens(context.Background(), h.adm, "gui", "")
	id := ts[0].TokenID

	if body := h.c.get("/tokens").Body.String(); !strings.Contains(body, id) || !strings.Contains(body, "Usable") {
		t.Fatalf("list:\n%s", body)
	}
	if rec := h.c.get("/tokens/" + id + "/revoke"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), id) {
		t.Fatalf("confirm page: %d", rec.Code)
	}
	if rec := h.c.get("/tokens/not-an-id/revoke"); rec.Code != http.StatusNotFound {
		t.Errorf("bad id: %d", rec.Code)
	}
	if rec := h.c.do("POST", "/tokens/"+id+"/revoke", url.Values{}, nil); rec.Code != http.StatusForbidden {
		t.Errorf("revoke without csrf: %d", rec.Code)
	}
	rec := h.c.post("/tokens/"+id+"/revoke", "/tokens/"+id+"/revoke", url.Values{})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/tokens" {
		t.Fatalf("revoke: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	h.c.get("/tokens") // shows and clears the flash, which names the id
	if body := h.c.get("/tokens").Body.String(); strings.Contains(body, id) || !strings.Contains(body, "Show finished") {
		t.Errorf("revoked token still in default list")
	}
	if body := h.c.get("/tokens?finished=1").Body.String(); !strings.Contains(body, id) || !strings.Contains(body, "Revoked") {
		t.Errorf("finished list lacks the revoked token")
	}
	if rec := h.c.get("/tokens/0123456789abcdef/revoke"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown id: %d", rec.Code)
	}
}

func TestNonceSet(t *testing.T) {
	var n nonceSet
	now := time.Now()
	if ok, full := n.MarkUsed("a", now); !ok || full {
		t.Fatal("first use must be accepted")
	}
	if ok, full := n.MarkUsed("a", now); ok || full {
		t.Fatal("second use must be a replay, not a full set")
	}
	// Past nonceTTL a nonce is forgotten (its signature has expired by then).
	if ok, _ := n.MarkUsed("a", now.Add(nonceTTL+time.Second)); !ok {
		t.Error("expired entry was not dropped")
	}
}

// A set full of live nonces refuses new ones instead of evicting a live one (which would let its
// form be replayed); entries older than nonceTTL are evicted to make room.
func TestNonceSetFullRefusesAndNeverEvictsLive(t *testing.T) {
	var n nonceSet
	now := time.Now()
	for i := 0; i < nonceMax; i++ {
		if ok, full := n.MarkUsed(fmt.Sprintf("n%d", i), now); !ok || full {
			t.Fatalf("fill %d: ok=%v full=%v", i, ok, full)
		}
	}
	if ok, full := n.MarkUsed("extra", now.Add(time.Minute)); ok || !full {
		t.Fatalf("full set: ok=%v full=%v, want refused", ok, full)
	}
	if ok, full := n.MarkUsed("n0", now.Add(time.Minute)); ok || full {
		t.Error("a live nonce was forgotten while the set was full")
	}
	if len(n.used) != nonceMax {
		t.Errorf("set size %d, want %d", len(n.used), nonceMax)
	}
	if ok, full := n.MarkUsed("extra", now.Add(nonceTTL+time.Second)); !ok || full {
		t.Errorf("after the TTL: ok=%v full=%v, want accepted", ok, full)
	}
}

// Each Server has its own nonce set (no package-level state).
func TestNonceSetPerServer(t *testing.T) {
	a, b := newGUIHarness(t), newGUIHarness(t)
	if ok, _ := a.srv.nonces.MarkUsed("same", time.Now()); !ok {
		t.Fatal("first use refused")
	}
	if ok, _ := b.srv.nonces.MarkUsed("same", time.Now()); !ok {
		t.Error("a nonce used on one server is seen as used on another")
	}
}

// addRunnerWithToken joins a runner with an existing token string.
func (h *guiHarness) addRunnerWithToken(fleet, name, tok string) {
	h.t.Helper()
	h.joinWith(tok, name)
	if st := h.status(fleet, name); st == "" {
		h.t.Fatal("runner did not join")
	}
}

// The result page's join command carries the advertised SSH address (-advertise), not a placeholder.
func TestTokenResultShowsAdvertisedAddress(t *testing.T) {
	h := newRunnersHarness(t, false)
	h.mkFleet("dev")
	csrf, nonce := h.tokenForm()
	rec := h.c.do("POST", "/tokens", tokenValues(csrf, nonce), nil)
	body := wantBody(t, rec, http.StatusOK, "mirafleet runner join central.example.org:2222 -name NAME")
	if strings.Contains(body, "CENTRAL:2222") {
		t.Error("placeholder address shown")
	}
}
