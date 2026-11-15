package gui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
)

// fakeBackend implements only GUIAuthenticate; the embedded nil Backend panics on any other call.
type fakeBackend struct {
	Backend
	token   string
	remote  string
	passive bool
	calls   int
	err     error
}

func (f *fakeBackend) GUIAuthenticate(_ context.Context, remote, token string, passive bool) (auth.Principal, int64, error) {
	f.token, f.remote, f.passive = token, remote, passive
	f.calls++
	if f.err != nil {
		return auth.Principal{Name: "leak"}, 9, f.err
	}
	return auth.GUIAdmin("admin", remote), 7, nil
}

func TestSessionAuthenticateFailsClosedAndContext(t *testing.T) {
	tok, _ := NewToken()
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: tok})
	p, id, err := authenticate(r, &fakeBackend{err: errors.New("boom")})
	if err == nil || p.Name != "" || id != 0 {
		t.Fatalf("error must yield no session: %+v %d %v", p, id, err)
	}
	p, id, err = authenticate(r, &fakeBackend{})
	if err != nil || p.Name != "admin" || id != 7 || p.Remote != r.RemoteAddr {
		t.Fatalf("ok: %+v %d %v", p, id, err)
	}
	ctx := WithSession(r.Context(), p, id)
	if gp, gid, ok := SessionFrom(ctx); !ok || gp.Name != "admin" || gid != 7 {
		t.Fatalf("SessionFrom: %+v %d %v", gp, gid, ok)
	}
	if _, _, ok := SessionFrom(r.Context()); ok {
		t.Fatal("session from empty context")
	}
}

func TestSessionCookieAttributes(t *testing.T) {
	tok, err := NewToken()
	if err != nil || len(tok) != 43 {
		t.Fatalf("token %q %v", tok, err)
	}
	rec := httptest.NewRecorder()
	SetSessionCookie(rec, tok)
	c := rec.Result().Cookies()[0]
	if c.Name != "__Host-mf_session" || c.Value != tok || !c.Secure || !c.HttpOnly ||
		c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.Domain != "" ||
		c.MaxAge != 0 || !c.Expires.IsZero() {
		t.Fatalf("bad cookie %+v", c)
	}
	rec = httptest.NewRecorder()
	ClearSessionCookie(rec)
	c = rec.Result().Cookies()[0]
	if c.Name != "__Host-mf_session" || c.MaxAge >= 0 || !c.Secure || !c.HttpOnly ||
		c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.Domain != "" {
		t.Fatalf("bad clear cookie %+v", c)
	}
	rec = httptest.NewRecorder()
	SetPreCookie(rec, tok)
	c = rec.Result().Cookies()[0]
	if c.Name != "__Host-mf_pre" || c.MaxAge <= 0 || !c.Secure || !c.HttpOnly ||
		c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.Domain != "" {
		t.Fatalf("bad pre cookie %+v", c)
	}
}

func TestSessionTokenMalformedRefused(t *testing.T) {
	for _, v := range []string{"", "short", "!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!"} {
		r := httptest.NewRequest("GET", "/", nil)
		r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: v})
		if _, ok := SessionToken(r); ok {
			t.Errorf("accepted %q", v)
		}
		if _, _, err := authenticate(r, &fakeBackend{}); !errors.Is(err, ErrNoSession) {
			t.Errorf("authenticate %q: %v", v, err)
		}
	}
}

func TestSessionPassiveReachesBackend(t *testing.T) {
	tok, _ := NewToken()
	for _, passive := range []bool{false, true} {
		fb := &fakeBackend{}
		var h http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, _, err := authenticate(r, fb); err != nil {
				t.Error(err)
			}
		})
		if passive {
			h = Passive(h)
		}
		r := httptest.NewRequest("GET", "/frag", nil)
		r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: tok})
		h.ServeHTTP(httptest.NewRecorder(), r)
		if fb.calls != 1 || fb.token != tok || fb.passive != passive {
			t.Fatalf("passive=%v: %+v", passive, fb)
		}
	}
}

func TestSessionPurgeTicker(t *testing.T) {
	ticks := make(chan time.Time)
	now := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	var n atomic.Int32
	got := make(chan time.Time, 4)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunPurge(ctx, ticks, func() time.Time { return now },
			func(_ context.Context, at time.Time) error { n.Add(1); got <- at; return errors.New("boom") },
			func(error) {})
	}()
	ticks <- now
	if at := <-got; !at.Equal(now) {
		t.Fatalf("purge now = %v", at)
	}
	ticks <- now
	<-got
	cancel()
	<-done
	if n.Load() != 2 {
		t.Fatalf("purges = %d", n.Load())
	}
	if PurgeInterval != 10*time.Minute {
		t.Fatal("interval")
	}
}

func TestSessionStartPurgeStopsOnCancel(t *testing.T) {
	var n atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	done := StartPurge(ctx, time.Now, func(context.Context, time.Time) error { n.Add(1); return nil }, nil)
	if n.Load() != 1 {
		t.Fatalf("initial purge = %d", n.Load())
	}
	cancel()
	<-done
}
