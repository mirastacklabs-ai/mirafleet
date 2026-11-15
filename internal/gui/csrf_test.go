package gui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func csrfReq(t *testing.T, key []byte, mutate func(r *http.Request)) error {
	t.Helper()
	form := url.Values{CSRFFormField: {FormToken(key)}}
	r := httptest.NewRequest("POST", "https://central.example/x", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	if mutate != nil {
		mutate(r)
	}
	_ = r.ParseForm() // Check never parses; the handler does, with the body capped
	return NewCSRF().Check(r, key)
}

func TestCSRFEmptyOrMalformedTokenRefused(t *testing.T) {
	for _, tok := range []string{"", "short", strings.Repeat("!", 43), strings.Repeat("A", 44)} {
		if k := CSRFKey(tok); k != nil {
			t.Errorf("CSRFKey(%q) = non-nil", tok)
		}
		if k := PreCSRFKey(tok); k != nil {
			t.Errorf("PreCSRFKey(%q) = non-nil", tok)
		}
	}
	// A request that "proves" itself with the token derived from an empty key is refused.
	r := httptest.NewRequest("POST", "https://central.example/x", nil)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set(CSRFHeader, FormToken(nil))
	if err := NewCSRF().Check(r, CSRFKey("")); err != ErrCSRFToken {
		t.Fatalf("empty key: %v", err)
	}
	if err := NewCSRF().Check(r, []byte{}); err != ErrCSRFToken {
		t.Fatalf("zero-length key: %v", err)
	}
}

func TestCSRFCheckDoesNotParseBody(t *testing.T) {
	tok, _ := NewToken()
	key := CSRFKey(tok)
	body := &readCounter{r: strings.NewReader("--b\r\nContent-Disposition: form-data; name=\"" + CSRFFormField + "\"\r\n\r\n" + FormToken(key) + "\r\n--b--\r\n")}
	r := httptest.NewRequest("POST", "https://central.example/x", body)
	r.Header.Set("Content-Type", "multipart/form-data; boundary=b")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	if err := NewCSRF().Check(r, key); err != ErrCSRFToken {
		t.Fatalf("err = %v", err)
	}
	if body.n != 0 {
		t.Fatalf("Check read %d body bytes", body.n)
	}
}

type readCounter struct {
	r interface{ Read([]byte) (int, error) }
	n int
}

func (c *readCounter) Read(p []byte) (int, error) { n, err := c.r.Read(p); c.n += n; return n, err }

func TestCSRFAllLayersPass(t *testing.T) {
	tok, _ := NewToken()
	if err := csrfReq(t, CSRFKey(tok), nil); err != nil {
		t.Fatal(err)
	}
	// htmx path: header instead of form field.
	err := csrfReq(t, CSRFKey(tok), func(r *http.Request) {
		r.Body = http.NoBody
		r.Header.Set(CSRFHeader, FormToken(CSRFKey(tok)))
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCSRFLayer1RejectsAlone(t *testing.T) {
	tok, _ := NewToken()
	key := CSRFKey(tok) // valid token, valid cookie: only the origin is wrong
	for _, mut := range []func(*http.Request){
		func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") },
		func(r *http.Request) { r.Header.Del("Sec-Fetch-Site"); r.Header.Set("Origin", "https://evil.example") },
	} {
		if err := csrfReq(t, key, mut); err != ErrCrossOrigin {
			t.Fatalf("err = %v", err)
		}
	}
}

func TestCSRFLayer2RejectsAlone(t *testing.T) {
	tok, _ := NewToken()
	key := CSRFKey(tok) // same-origin request, bad or missing token
	noTok := func(r *http.Request) { r.Body = http.NoBody; r.ContentLength = 0 }
	bad := func(r *http.Request) { r.Header.Set(CSRFHeader, "AAAA") }
	for name, mut := range map[string]func(*http.Request){"missing": noTok, "bad": func(r *http.Request) { noTok(r); bad(r) }} {
		if err := csrfReq(t, key, mut); err != ErrCSRFToken {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	// No cookie at all: nil key refuses even a request carrying some token.
	r := httptest.NewRequest("POST", "https://central.example/x", nil)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set(CSRFHeader, FormToken(CSRFKey(tok)))
	if err := NewCSRF().Check(r, nil); err != ErrCSRFToken {
		t.Fatalf("nil key: %v", err)
	}
}

func TestCSRFLayer3SameSiteAttribute(t *testing.T) {
	// Layer 3 is enforced by the browser from the cookie attribute; it must be Strict on both
	// cookies, so a cross-site navigation never carries them.
	rec := httptest.NewRecorder()
	SetSessionCookie(rec, "t")
	SetPreCookie(rec, "t")
	for _, c := range rec.Result().Cookies() {
		if c.SameSite != http.SameSiteStrictMode {
			t.Fatalf("%s not SameSite=Strict", c.Name)
		}
	}
}

func TestCSRFSafeMethodsSkip(t *testing.T) {
	r := httptest.NewRequest("GET", "https://central.example/x", nil)
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	if err := NewCSRF().Check(r, nil); err != nil {
		t.Fatal(err)
	}
}

func TestCSRFTokenFromOtherSessionRefused(t *testing.T) {
	a, _ := NewToken()
	b, _ := NewToken()
	err := csrfReq(t, CSRFKey(a), func(r *http.Request) {
		r.Body = http.NoBody
		r.Header.Set(CSRFHeader, FormToken(CSRFKey(b)))
	})
	if err != ErrCSRFToken {
		t.Fatalf("err = %v", err)
	}
	// A pre-session token is not valid as a session token and vice versa.
	if FormToken(PreCSRFKey(a)) == FormToken(CSRFKey(a)) {
		t.Fatal("pre and session keys collide")
	}
}

func TestCSRFPreSessionToken(t *testing.T) {
	pre, _ := NewToken()
	if err := csrfReq(t, PreCSRFKey(pre), nil); err != nil {
		t.Fatal(err)
	}
	// The cookie value is not the form token (double submit without exposing the cookie).
	if FormToken(PreCSRFKey(pre)) == pre {
		t.Fatal("form token equals cookie")
	}
}

func TestCSRFKeyDerivedNotStored(t *testing.T) {
	tok, _ := NewToken()
	k := CSRFKey(tok)
	if string(k) == tok || strings.Contains(FormToken(k), tok) {
		t.Fatal("key or token leaks the cookie")
	}
	k2 := CSRFKey(tok)
	if string(k) != string(k2) {
		t.Fatal("not deterministic")
	}
}

func TestCSRFConstantTimeCompare(t *testing.T) {
	if !constEq("abc", "abc") || constEq("abc", "abd") || constEq("abc", "abcd") || constEq("", "a") {
		t.Fatal("constEq")
	}
	// Check goes through constEq on equal-length strings of the same alphabet: a wrong token of
	// the right length is refused without panic.
	tok, _ := NewToken()
	right := FormToken(CSRFKey(tok))
	wrong := right[:len(right)-1] + "A"
	if wrong == right {
		wrong = right[:len(right)-1] + "B"
	}
	err := csrfReq(t, CSRFKey(tok), func(r *http.Request) { r.Body = http.NoBody; r.Header.Set(CSRFHeader, wrong) })
	if err != ErrCSRFToken {
		t.Fatalf("err = %v", err)
	}
}

func TestFlashAndNonceMAC(t *testing.T) {
	now := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	a, _ := NewToken()
	b, _ := NewToken()
	ka, kb := CSRFKey(a), CSRFKey(b)
	f := SignFlash(ka, "Saved <fleet>", now, time.Minute)
	if m, ok := OpenFlash(ka, f, now.Add(30*time.Second)); !ok || m != "Saved <fleet>" {
		t.Fatalf("%q %v", m, ok)
	}
	if _, ok := OpenFlash(kb, f, now); ok {
		t.Fatal("other session opened flash")
	}
	if _, ok := OpenFlash(ka, f, now.Add(2*time.Minute)); ok {
		t.Fatal("expired flash opened")
	}
	if _, ok := OpenNonce(ka, f, now); ok {
		t.Fatal("flash accepted as nonce (purpose not separated)")
	}
	parts := strings.Split(f, ".")
	parts[0] = "9999999999" // extend expiry
	if _, ok := OpenFlash(ka, strings.Join(parts, "."), now); ok {
		t.Fatal("tampered expiry accepted")
	}
	n := SignNonce(ka, "abc123", now, time.Hour)
	if v, ok := OpenNonce(ka, n, now); !ok || v != "abc123" {
		t.Fatal("nonce")
	}
	if _, ok := OpenNonce(kb, n, now); ok {
		t.Fatal("nonce from other session")
	}
	for _, bad := range []string{"", "a.b", "a.b.c.d", "x.!!.!!"} {
		if _, ok := OpenFlash(ka, bad, now); ok {
			t.Fatalf("accepted %q", bad)
		}
	}
}
