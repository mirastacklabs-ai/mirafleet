package gui

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Domain-separation labels. The CSRF key is derived from the cookie token and never stored.
const (
	csrfKeyLabel = "mirafleet-gui-csrf-v1"
	preKeyLabel  = "mirafleet-gui-csrf-pre-v1"

	// CSRFFormField and CSRFHeader carry the synchronizer token.
	CSRFFormField = "_csrf"
	CSRFHeader    = "X-CSRF-Token"
)

// CSRF errors; each layer fails on its own.
var (
	ErrCrossOrigin = errors.New("gui: cross-origin request refused")
	ErrCSRFToken   = errors.New("gui: missing or invalid CSRF token")
)

func hmacSum(key []byte, parts ...string) []byte {
	m := hmac.New(sha256.New, key)
	for i, p := range parts {
		if i > 0 {
			m.Write([]byte{0})
		}
		m.Write([]byte(p))
	}
	return m.Sum(nil)
}

// CSRFKey derives the per-session key from the cookie token: HMAC-SHA256(token, label).
// Nothing derived from it is stored; a database copy cannot produce it.
// It returns nil unless the token is a valid 43-character base64url encoding of 32 bytes, so an
// empty or malformed cookie can never yield a usable key.
func CSRFKey(sessionToken string) []byte {
	if !validToken(sessionToken) {
		return nil
	}
	return hmacSum([]byte(sessionToken), csrfKeyLabel)
}

// PreCSRFKey derives the key for the pre-session (double-submit) cookie; a different label keeps
// it apart from session keys. Like CSRFKey it returns nil for a malformed token.
func PreCSRFKey(preToken string) []byte {
	if !validToken(preToken) {
		return nil
	}
	return hmacSum([]byte(preToken), preKeyLabel)
}

// validToken reports whether s is exactly the base64url (no padding) form of 32 bytes.
func validToken(s string) bool {
	if len(s) != 43 {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	return err == nil && len(b) == 32
}

// FormToken is the synchronizer token for a key: HMAC-SHA256(key, "form"), base64url.
func FormToken(key []byte) string {
	return base64.RawURLEncoding.EncodeToString(hmacSum(key, "form"))
}

// CSRF enforces the three layers of M4 §2.6 on unsafe methods. Layer 3 (SameSite=Strict) is a
// cookie attribute enforced by the browser; see SetSessionCookie.
type CSRF struct {
	cop *http.CrossOriginProtection
}

// NewCSRF returns the checker (layer 1 is the stdlib CrossOriginProtection).
func NewCSRF() *CSRF { return &CSRF{cop: http.NewCrossOriginProtection()} }

// Check validates r. key is CSRFKey(sessionToken) or PreCSRFKey(preToken); a nil key means
// there is no cookie, which refuses every unsafe request. Safe methods pass. For the form field
// the caller must already have run ParseForm (body capped): Check reads r.PostForm and never
// parses a body itself, so an unparsed body (for example multipart) is never read here.
func (c *CSRF) Check(r *http.Request, key []byte) error {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return nil
	}
	if err := c.cop.Check(r); err != nil { // layer 1
		return ErrCrossOrigin
	}
	if len(key) == 0 {
		return ErrCSRFToken
	}
	got := r.Header.Get(CSRFHeader) // layer 2
	if got == "" {
		if r.PostForm == nil {
			return ErrCSRFToken
		}
		got = r.PostForm.Get(CSRFFormField)
	}
	if got == "" || !constEq(got, FormToken(key)) {
		return ErrCSRFToken
	}
	return nil
}

func constEq(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// signValue returns base64url(value) "." base64url(MAC(key, purpose, exp, value)), valid until
// now+ttl. Used for flash messages and token-form nonces.
func signValue(key []byte, purpose, value string, now time.Time, ttl time.Duration) string {
	exp := strconv.FormatInt(now.Add(ttl).Unix(), 10)
	mac := hmacSum(key, purpose, exp, value)
	return exp + "." + base64.RawURLEncoding.EncodeToString([]byte(value)) + "." +
		base64.RawURLEncoding.EncodeToString(mac)
}

// openValue verifies and returns the value signed by signValue for the same key and purpose.
func openValue(key []byte, purpose, s string, now time.Time) (string, bool) {
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return "", false
	}
	exp, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || now.Unix() > exp {
		return "", false
	}
	vb, err1 := base64.RawURLEncoding.DecodeString(parts[1])
	mb, err2 := base64.RawURLEncoding.DecodeString(parts[2])
	if err1 != nil || err2 != nil {
		return "", false
	}
	if !hmac.Equal(mb, hmacSum(key, purpose, parts[0], string(vb))) {
		return "", false
	}
	return string(vb), true
}

// SignFlash signs a flash message for the session key, valid for ttl.
func SignFlash(key []byte, msg string, now time.Time, ttl time.Duration) string {
	return signValue(key, "flash", msg, now, ttl)
}

// OpenFlash returns the flash message if the MAC and expiry hold for key.
func OpenFlash(key []byte, s string, now time.Time) (string, bool) {
	return openValue(key, "flash", s, now)
}

// SignNonce signs a token-form nonce (single-use secret pages) bound to key.
func SignNonce(key []byte, nonce string, now time.Time, ttl time.Duration) string {
	return signValue(key, "nonce", nonce, now, ttl)
}

// OpenNonce returns the nonce if the MAC and expiry hold for key. Replay tracking is the
// caller's (the store or a map); the MAC only proves origin.
func OpenNonce(key []byte, s string, now time.Time) (string, bool) {
	return openValue(key, "nonce", s, now)
}
