package gui

// Tests for the Join tokens screens (M4 step S9): the token is shown once, the signed nonce plus
// the atomic MarkUsed stop a second token, and the token is in no other response.

import (
	"context"
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
func (h *s9) tokenForm() (csrf, nonce string) {
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

func (h *s9) tokenCount() int {
	h.t.Helper()
	ts, err := h.e.svc.ListTokens(context.Background(), h.adm, "gui", "")
	if err != nil {
		h.t.Fatal(err)
	}
	return len(ts)
}

func TestTokenShownOnceNoStore(t *testing.T) {
	h := newS9(t, false)
	h.fleet("dev")
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
	if n := strings.Count(h.e.seenText(), tok); n != 1 {
		t.Errorf("token appears %d times across all responses, want exactly 1 (the create response)", n)
	}
	list := h.c.get("/tokens").Body.String()
	if !strings.Contains(list, "env=ci") || strings.Contains(list, tok) {
		t.Errorf("list page:\n%s", list)
	}
	if logs := h.e.logText(); strings.Contains(logs, tok) {
		t.Error("token in the server log")
	}

	// The token is a working join token.
	h.addRunnerWithToken("dev", "r1", tok)
}

func TestTokenNonceReplayShowsNoSecondToken(t *testing.T) {
	h := newS9(t, false)
	h.fleet("dev")
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
	if n := h.e.auditCount("token.create"); n != 1 {
		t.Errorf("token.create audit rows = %d", n)
	}
}

func TestTokenConcurrentDoubleSubmitMintsOne(t *testing.T) {
	h := newS9(t, false)
	h.fleet("dev")
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
			h.e.srv.Handler().ServeHTTP(rec, r)
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
	h := newS9(t, false)
	h.fleet("dev")
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
	foreign := SignNonce(CSRFKey(otherKey), rawN, h.e.clk.now(), nonceTTL)
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
	nonce = SignNonce(CSRFKey(h.c.jar[SessionCookieName]), raw, h.e.clk.now().Add(-2*nonceTTL), nonceTTL)
	if rec = h.c.do("POST", "/tokens", tokenValues(csrf, nonce), nil); rec.Code != http.StatusUnprocessableEntity || tokenRE.MatchString(rec.Body.String()) {
		t.Errorf("expired nonce: %d", rec.Code)
	}
}

func TestTokenRevokeAndFilters(t *testing.T) {
	h := newS9(t, false)
	h.fleet("dev")
	csrf, nonce := h.tokenForm()
	h.c.do("POST", "/tokens", tokenValues(csrf, nonce), nil)
	ts, _ := h.e.svc.ListTokens(context.Background(), h.adm, "gui", "")
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
	if nonceTTL > nonceRetention {
		t.Fatalf("nonce TTL %v exceeds retention %v", nonceTTL, nonceRetention)
	}
	var n nonceSet
	now := time.Now()
	if !n.MarkUsed("a", now) || n.MarkUsed("a", now) {
		t.Fatal("MarkUsed must accept once")
	}
	// Past the retention a nonce is forgotten (its signature has long expired by then).
	if !n.MarkUsed("a", now.Add(nonceRetention+time.Second)) {
		t.Error("expired entry was not dropped")
	}
	// The set is bounded; the newest entries survive.
	for i := 0; i < nonceMax+50; i++ {
		n.MarkUsed("n"+strings.Repeat("x", i%7)+time.Duration(i).String(), now)
	}
	if len(n.used) > nonceMax || len(n.order) > nonceMax {
		t.Errorf("set grew to %d/%d", len(n.used), len(n.order))
	}
}

// addRunnerWithToken joins a runner with an existing token string.
func (h *s9) addRunnerWithToken(fleet, name, tok string) {
	h.t.Helper()
	h.joinWith(tok, name)
	if st := h.status(fleet, name); st == "" {
		h.t.Fatal("runner did not join")
	}
}
