package gui

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
)

// Cookie names. The __Host- prefix makes browsers require Secure, Path=/ and no Domain.
const (
	SessionCookieName = "__Host-mf_session"
	PreCookieName     = "__Host-mf_pre"

	// PurgeInterval is how often expired and dead session rows are deleted (M4 §2.4).
	PurgeInterval = 10 * time.Minute

	preCookieMaxAge = 15 * 60 // seconds; the pre-session cookie is short-lived
)

// NewToken returns 32 random bytes, base64url without padding (256 bits).
func NewToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// SetSessionCookie sets the session cookie: session-lifetime (no Max-Age), Secure, HttpOnly,
// SameSite=Strict, Path=/, no Domain.
func SetSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookieName, Value: token, Path: "/",
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
}

// ClearSessionCookie expires the session cookie with the same attributes.
func ClearSessionCookie(w http.ResponseWriter) { clearCookie(w, SessionCookieName) }

// SetPreCookie sets the short-lived pre-session (double-submit) cookie used by /login and /setup.
func SetPreCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name: PreCookieName, Value: token, Path: "/", MaxAge: preCookieMaxAge,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
}

// ClearPreCookie expires the pre-session cookie.
func ClearPreCookie(w http.ResponseWriter) { clearCookie(w, PreCookieName) }

func clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: "/", MaxAge: -1,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
}

// SessionToken returns the session cookie value, if present and well-formed (43 base64url
// characters, so garbage never reaches the store).
func SessionToken(r *http.Request) (string, bool) { return cookieToken(r, SessionCookieName) }

// PreToken returns the pre-session cookie value, if present and well-formed.
func PreToken(r *http.Request) (string, bool) { return cookieToken(r, PreCookieName) }

func cookieToken(r *http.Request, name string) (string, bool) {
	c, err := r.Cookie(name)
	if err != nil || len(c.Value) != 43 {
		return "", false
	}
	if b, err := base64.RawURLEncoding.DecodeString(c.Value); err != nil || len(b) != 32 {
		return "", false
	}
	return c.Value, true
}

type passiveKey struct{}

// Passive marks a handler as a polled fragment endpoint: requests through it never touch the
// session row (M4 §2.4). Apply it at route registration.
func Passive(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), passiveKey{}, true)))
	})
}

// IsPassive reports whether the request went through Passive.
func IsPassive(ctx context.Context) bool {
	v, _ := ctx.Value(passiveKey{}).(bool)
	return v
}

// ErrNoSession means the request carries no usable session cookie.
var ErrNoSession = errors.New("gui: no session cookie")

// authenticate reads the cookie and calls the backend with the request's remote address and
// passive flag. Any error means no session (fail closed): callers must not look at the other
// results then.
func authenticate(r *http.Request, be Backend) (auth.Principal, int64, error) {
	tok, ok := SessionToken(r)
	if !ok {
		return auth.Principal{}, 0, ErrNoSession
	}
	p, id, err := be.GUIAuthenticate(r.Context(), r.RemoteAddr, tok, IsPassive(r.Context()))
	if err != nil {
		return auth.Principal{}, 0, err
	}
	return p, id, nil
}

type sessionCtxKey struct{}

type sessionCtx struct {
	p  auth.Principal
	id int64
}

// WithSession returns ctx carrying the authenticated principal and session id.
func WithSession(ctx context.Context, p auth.Principal, sessionID int64) context.Context {
	return context.WithValue(ctx, sessionCtxKey{}, sessionCtx{p: p, id: sessionID})
}

// SessionFrom returns the principal and session id stored by WithSession; ok is false outside an
// authenticated request.
func SessionFrom(ctx context.Context) (p auth.Principal, sessionID int64, ok bool) {
	v, ok := ctx.Value(sessionCtxKey{}).(sessionCtx)
	return v.p, v.id, ok
}

// RunPurge deletes expired session rows on every tick until ctx is cancelled. ticks and now are
// injected so tests need no real time; purge runs with ctx, so cancelling aborts a slow purge.
// onErr (optional) receives purge failures. It returns when ctx is done.
func RunPurge(ctx context.Context, ticks <-chan time.Time, now func() time.Time,
	purge func(ctx context.Context, now time.Time) error, onErr func(error)) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			if err := purge(ctx, now()); err != nil && ctx.Err() == nil && onErr != nil {
				onErr(err)
			}
		}
	}
}

// StartPurge purges once now (GUI start), then every PurgeInterval on a real ticker. The
// returned channel closes when the goroutine has stopped after ctx is cancelled.
func StartPurge(ctx context.Context, now func() time.Time,
	purge func(ctx context.Context, now time.Time) error, onErr func(error)) <-chan struct{} {
	done := make(chan struct{})
	if err := purge(ctx, now()); err != nil && onErr != nil {
		onErr(err)
	}
	t := time.NewTicker(PurgeInterval)
	go func() {
		defer close(done)
		defer t.Stop()
		RunPurge(ctx, t.C, now, purge, onErr)
	}()
	return done
}
