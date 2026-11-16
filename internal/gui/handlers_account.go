package gui

// Account screen: password change, the admin's sessions, the About section and
// the theme switch. Every route is behind requireAdmin and checks the session CSRF token.

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
)

// viewTime renders an absolute UTC time for the ui/time partial (timeView is in handlers_fleets.go).
func viewTime(t time.Time) timeView {
	t = t.UTC()
	return timeView{ISO: t.Format(time.RFC3339), Text: t.Format("2006-01-02 15:04") + " UTC"}
}

// SessionRow is one line of the sessions table.
type SessionRow struct {
	ID      int64
	Created timeView
	Active  timeView
	Remote  string
	Browser string
	Current bool
}

// AboutData is the About section.
type AboutData struct {
	Version            string
	CertFingerprint    string
	CertExpiry         timeView
	Listen             string
	Notice             string
	HostKeyFingerprint string
	SSHAdvertise       string
}

// AccountData is Page.Data of the account screen. Errors is keyed by field (current, new,
// new2, form).
type AccountData struct {
	Username string
	Sessions []SessionRow
	Others   bool // another session exists
	About    AboutData
	Errors   map[string]string
}

func init() {
	RegisterFlash("pw-changed", "ok", "Password changed. Your other sessions were signed out.")
	RegisterFlash("session-revoked", "ok", "Session signed out.")
	RegisterFlash("others-revoked", "ok", "Other sessions signed out (%s).")
}

func (s *Server) routesAccount(mux *http.ServeMux) {
	mux.Handle("GET /account", s.requireAdmin(http.HandlerFunc(s.handleAccount)))
	mux.Handle("POST /account/password", s.requireAdmin(http.HandlerFunc(s.handlePassword)))
	mux.Handle("POST /account/sessions/revoke-others", s.requireAdmin(http.HandlerFunc(s.handleRevokeOthers)))
	mux.Handle("POST /account/sessions/{id}/revoke", s.requireAdmin(http.HandlerFunc(s.handleRevokeOne)))
	mux.Handle("POST /account/theme", s.requireAdmin(http.HandlerFunc(s.handleTheme)))
}

func (s *Server) accountPage(w http.ResponseWriter, r *http.Request, status int, errs map[string]string) {
	p, cur, _ := SessionFrom(r.Context())
	list, err := s.cfg.Backend.ListGUISessions(r.Context(), p)
	if err != nil {
		s.logf("gui: list sessions: %v", err)
		s.ErrorPage(w, r, http.StatusInternalServerError)
		return
	}
	d := AccountData{Username: p.Name, Errors: errs}
	now := s.now()
	for _, g := range list {
		if !g.ExpiresAt.After(now) {
			continue // expired, not yet purged
		}
		d.Sessions = append(d.Sessions, SessionRow{ID: g.ID, Created: viewTime(g.CreatedAt), Active: viewTime(g.LastActiveAt),
			Remote: g.Remote, Browser: g.UserAgent, Current: g.ID == cur})
		if g.ID != cur {
			d.Others = true
		}
	}
	d.About = AboutData{Version: s.cfg.Version, CertFingerprint: s.Fingerprint(), CertExpiry: viewTime(s.CertExpiry()),
		Listen: s.Addr(), Notice: s.Notice(), HostKeyFingerprint: s.cfg.HostKeyFingerprint, SSHAdvertise: s.cfg.SSHAdvertise}
	pg := s.Page(w, r, "Account", "account")
	pg.Crumbs = []Crumb{{Text: "Account"}}
	pg.Data = d
	s.Render(w, r, status, "account", "", pg)
}

func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request) {
	s.accountPage(w, r, http.StatusOK, nil)
}

func (s *Server) handlePassword(w http.ResponseWriter, r *http.Request) {
	form, ok := s.parseAndCheckSession(w, r)
	if !ok {
		return
	}
	p, id, _ := SessionFrom(r.Context())
	cur, next, next2 := form.Get("current"), form.Get("new"), form.Get("new2")
	invalid := func(errs map[string]string) { s.accountPage(w, r, http.StatusUnprocessableEntity, errs) }

	if allowed, _, wait := s.auth.login.Allow(r.RemoteAddr); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(RetryAfterSeconds(wait)))
		s.accountPage(w, r, http.StatusTooManyRequests, map[string]string{"form": lockoutText(wait)})
		return
	}
	switch {
	case cur == "":
		invalid(map[string]string{"current": "Enter your current password."})
		return
	case PasswordTooLong(cur):
		s.recordFailure(r, s.auth.login)
		invalid(map[string]string{"current": "Your current password is incorrect."})
		return
	case PasswordTooLong(next):
		invalid(map[string]string{"new": "That password is too long."})
		return
	case next != next2:
		invalid(map[string]string{"new2": "The two new passwords differ."})
		return
	}
	release, err := s.auth.sem.Acquire(r.Context())
	if err != nil {
		s.hashBusy(w, r)
		return
	}
	tok, err := s.cfg.Backend.ChangeGUIPassword(r.Context(), p, id, cur, next)
	release()
	if err != nil {
		var ae *api.Error
		if errors.Is(err, context.DeadlineExceeded) {
			s.hashBusy(w, r)
			return
		}
		if errors.Is(err, service.ErrWrongCurrentPassword) {
			s.recordFailure(r, s.auth.login)
			invalid(map[string]string{"current": "Your current password is incorrect."})
			return
		}
		if errors.As(err, &ae) && ae.Code == api.CodeBadRequest {
			invalid(map[string]string{"new": userMessage(ae.Message)})
			return
		}
		if errors.As(err, &ae) && ae.Code == api.CodeConflict { // the account changed under the form (G-39)
			invalid(map[string]string{"form": userMessage(ae.Message)})
			return
		}
		s.logf("gui: change password: %v", err)
		s.ErrorPage(w, r, http.StatusInternalServerError)
		return
	}
	s.auth.login.Success(r.RemoteAddr)
	SetSessionCookie(w, tok)
	s.Redirect(w, r, "/account", "pw-changed", "")
}

func (s *Server) handleRevokeOne(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.parseAndCheckSession(w, r); !ok {
		return
	}
	p, cur, _ := SessionFrom(r.Context())
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.ErrorPage(w, r, http.StatusNotFound)
		return
	}
	if id == cur { // signing out this very session is a logout (CSRF was checked above)
		s.logout(w, r)
		return
	}
	if err := s.cfg.Backend.RevokeGUISession(r.Context(), p, id); err != nil {
		var ae *api.Error
		if errors.As(err, &ae) && ae.Code == api.CodeNotFound {
			s.ErrorPage(w, r, http.StatusNotFound)
			return
		}
		s.logf("gui: revoke session: %v", err)
		s.ErrorPage(w, r, http.StatusInternalServerError)
		return
	}
	s.Redirect(w, r, "/account", "session-revoked", "")
}

func (s *Server) handleRevokeOthers(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.parseAndCheckSession(w, r); !ok {
		return
	}
	p, cur, _ := SessionFrom(r.Context())
	n, err := s.cfg.Backend.RevokeOtherGUISessions(r.Context(), p, cur)
	if err != nil {
		s.logf("gui: revoke other sessions: %v", err)
		s.ErrorPage(w, r, http.StatusInternalServerError)
		return
	}
	s.Redirect(w, r, "/account", "others-revoked", strconv.Itoa(n))
}

func (s *Server) handleTheme(w http.ResponseWriter, r *http.Request) {
	form, ok := s.parseAndCheckSession(w, r)
	if !ok {
		return
	}
	theme := form.Get("theme")
	switch theme {
	case "light", "dark", "system":
	default:
		s.ErrorPage(w, r, http.StatusBadRequest)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: ThemeCookieName, Value: theme, Path: "/", MaxAge: 365 * 24 * 3600,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	next := form.Get("next") // the page the switch was used on (G-39); Redirect refuses non-local targets
	if next == "" || next == "/account/theme" {
		next = "/account"
	}
	s.Redirect(w, r, next, "", "")
}
