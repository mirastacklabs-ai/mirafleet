package gui

// Auth screens (docs/M4.md 2.3 to 2.6, 3.1): setup, login, logout, the requireAdmin middleware
// and the PageDefaults hook. The setup token never reaches a response or a log; it lives in a
// 0600 file in central's data directory and in the setup form's POST body.

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
)

const (
	setupTokenFile = "setup-token"
	maxFieldBytes  = 512 // username and setup token fields: longer is refused before any work
)

// authState is the per-server state of the auth screens.
type authState struct {
	login *LoginLimiter // /login and password-change attempts
	setup *LoginLimiter // /setup attempts (same limits, separate buckets)
	sem   *HashSem
	csrf  *CSRF
}

// LoginData is Page.Data of the login screen.
type LoginData struct {
	Next     string
	Username string
}

// SetupData is Page.Data of the setup screen. Errors is keyed by field (token, username,
// password, password2, form).
type SetupData struct {
	Fingerprint string
	Username    string
	Errors      map[string]string
}

func init() {
	RegisterFlash("logged-out", "ok", "You are signed out.")
}

func (s *Server) routesAuth(mux *http.ServeMux) {
	s.auth = &authState{
		login: NewLoginLimiter(s.now),
		setup: NewLoginLimiter(s.now),
		sem:   DefaultHashSem(),
		csrf:  NewCSRF(),
	}
	s.PageDefaults = s.pageDefaults
	mux.HandleFunc("GET /setup", s.handleSetupForm)
	mux.HandleFunc("POST /setup", s.handleSetup)
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.Handle("POST /logout", s.requireAdmin(http.HandlerFunc(s.handleLogout)))
}

// ---- setup token file ----

// SetupTokenPath is where the setup token file lives (empty without a data directory).
func (s *Server) SetupTokenPath() string {
	if s.cfg.DataDir == "" {
		return ""
	}
	return filepath.Join(s.cfg.DataDir, "gui", setupTokenFile)
}

// PrepareSetup runs at GUI start. With no active admin and no live setup token it has the
// service create one and writes it to the token file (0600, O_EXCL, through os.Root, replacing
// a stale file). With an admin present it removes a leftover file. It returns the file path
// when a new token was written. It never logs the token.
func (s *Server) PrepareSetup(ctx context.Context) (path string, created bool, err error) {
	if s.cfg.DataDir == "" {
		return "", false, errors.New("gui: a data directory is required for the setup token file")
	}
	dir := filepath.Join(s.cfg.DataDir, "gui")
	tok, created, err := s.cfg.Backend.EnsureGUISetup(ctx)
	if err != nil {
		return "", false, err
	}
	if !created {
		if open, oerr := s.cfg.Backend.GUISetupOpen(ctx); oerr == nil && !open {
			s.removeSetupFile()
		}
		return "", false, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", false, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", false, err
	}
	defer root.Close()
	if err := root.Remove(setupTokenFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", false, err
	}
	f, err := root.OpenFile(setupTokenFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", false, err
	}
	if _, err := f.WriteString(tok + "\n"); err != nil {
		f.Close()
		_ = root.Remove(setupTokenFile)
		return "", false, err
	}
	if err := f.Close(); err != nil {
		_ = root.Remove(setupTokenFile)
		return "", false, err
	}
	return s.SetupTokenPath(), true, nil
}

// removeSetupFile deletes the token file through os.Root; a missing file is fine.
func (s *Server) removeSetupFile() {
	if s.cfg.DataDir == "" {
		return
	}
	root, err := os.OpenRoot(filepath.Join(s.cfg.DataDir, "gui"))
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			s.logf("gui: setup token file: %v", err)
		}
		return
	}
	defer root.Close()
	if err := root.Remove(setupTokenFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.logf("gui: remove setup token file: %v", err)
	}
}

// ---- shared helpers ----

// setupOpen reports whether no active admin exists.
func (s *Server) setupOpen(r *http.Request) (bool, error) {
	return s.cfg.Backend.GUISetupOpen(r.Context())
}

// clip cuts v to at most n bytes without splitting a UTF-8 sequence.
func clip(v string, n int) string {
	if len(v) <= n {
		return v
	}
	for n > 0 && v[n]&0xC0 == 0x80 {
		n--
	}
	return v[:n]
}

// prePage returns the form token for the login and setup forms, minting the pre-session cookie
// when the request has none.
func prePage(w http.ResponseWriter, r *http.Request) (formToken string) {
	tok, ok := PreToken(r)
	if !ok {
		var err error
		if tok, err = NewToken(); err != nil {
			return ""
		}
		SetPreCookie(w, tok)
	}
	return FormToken(PreCSRFKey(tok))
}

// parseAndCheck is the request hygiene of every GUI POST: urlencoded body (415), cap (413),
// ParseForm (400), then the CSRF layers against key. It writes the error page itself.
func (s *Server) parseAndCheck(w http.ResponseWriter, r *http.Request, key []byte) (url.Values, bool) {
	form, status := ParseForm(w, r)
	if status != 0 {
		s.ErrorPage(w, r, status)
		return nil, false
	}
	if err := s.auth.csrf.Check(r, key); err != nil {
		s.ErrorPage(w, r, http.StatusForbidden)
		return nil, false
	}
	return form, true
}

// parseAndCheckSession is parseAndCheck for an authenticated request (key from the session
// cookie). Every POST of the other screens uses it.
func (s *Server) parseAndCheckSession(w http.ResponseWriter, r *http.Request) (url.Values, bool) {
	tok, _ := SessionToken(r)
	return s.parseAndCheck(w, r, CSRFKey(tok))
}

func (s *Server) hashBusy(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Retry-After", "1")
	s.ErrorPage(w, r, http.StatusServiceUnavailable)
}

func apiCode(err error) string {
	var ae *api.Error
	if errors.As(err, &ae) {
		return ae.Code
	}
	return ""
}

// recordFailure notes a failed attempt and writes the one gui.lockout audit row when it locks
// the address out.
func (s *Server) recordFailure(r *http.Request, l *LoginLimiter) {
	if l.Failure(r.RemoteAddr) {
		if err := s.cfg.Backend.RecordGUILockout(r.Context(), r.RemoteAddr); err != nil {
			s.logf("gui: record lockout: %v", err)
		}
		s.logf("gui: lockout for %s", LimiterKey(r.RemoteAddr))
	}
}

func lockoutText(wait time.Duration) string {
	mins := int((wait + time.Minute - 1) / time.Minute)
	switch {
	case wait < time.Minute:
		return "Too many attempts. Wait a few seconds and try again."
	case mins == 1:
		return "Too many attempts. Try again in 1 minute."
	}
	return "Too many attempts. Try again in " + strconv.Itoa(mins) + " minutes."
}

// ---- middleware and page defaults ----

// requireAdmin wraps a handler: cookie, then GUIAuthenticate on every request, then the handler
// with the session in its context. Any failure fails closed: before an admin exists the browser
// goes to /setup, otherwise to /login (htmx requests get HX-Redirect).
func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, id, err := authenticate(r, s.cfg.Backend)
		if err != nil {
			if open, oerr := s.setupOpen(r); oerr == nil && open {
				s.Redirect(w, r, "/setup", "", "")
				return
			}
			s.toLogin(w, r)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithSession(r.Context(), p, id)))
	})
}

// toLogin sends the client to /login, carrying a local GET path as next.
func (s *Server) toLogin(w http.ResponseWriter, r *http.Request) {
	target := "/login"
	if r.Method == http.MethodGet && r.Header.Get("HX-Request") != "true" {
		if u := r.URL.RequestURI(); u != "/" && localPath(u) {
			target += "?next=" + url.QueryEscape(u)
		}
	}
	s.Redirect(w, r, target, "", "")
}

var navKeys = map[string]string{"": "overview", "fleets": "fleets", "runners": "runners", "tokens": "tokens",
	"operators": "operators", "workspaces": "workspaces", "audit": "audit", "account": "account"}

// pageDefaults fills Authed, CSRF and (for error pages) Nav from the session cookie. A page for
// a request without a live session stays unauthenticated: no nav, no sign-out, no token.
func (s *Server) pageDefaults(r *http.Request, pg *Page) {
	tok, ok := SessionToken(r)
	if !ok {
		return
	}
	if _, _, in := SessionFrom(r.Context()); !in {
		if _, _, err := s.cfg.Backend.GUIAuthenticate(r.Context(), r.RemoteAddr, tok, true); err != nil {
			return
		}
	}
	key := CSRFKey(tok)
	if key == nil {
		return
	}
	pg.Authed = true
	pg.CSRF = FormToken(key)
	if pg.Nav == "" {
		first, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		pg.Nav = navKeys[first]
	}
}

// nextParam returns v if it is a local path, else "".
func nextParam(v string) string {
	if v != "" && localPath(v) {
		return v
	}
	return ""
}

// ---- login ----

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request, status int, formToken, next, username string, flash *Flash) {
	pg := s.Page(w, r, "Sign in", "")
	pg.Authed, pg.CSRF, pg.Nav = false, formToken, ""
	if flash != nil {
		pg.Flash = flash
	}
	pg.Data = LoginData{Next: next, Username: username}
	s.Render(w, r, status, "login", "", pg)
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if open, err := s.setupOpen(r); err == nil && open {
		s.Redirect(w, r, "/setup", "", "")
		return
	}
	next := nextParam(r.URL.Query().Get("next"))
	if _, _, err := authenticate(r, s.cfg.Backend); err == nil {
		if next == "" {
			next = "/"
		}
		s.Redirect(w, r, next, "", "")
		return
	}
	s.loginPage(w, r, http.StatusOK, prePage(w, r), next, "", nil)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	tok, _ := PreToken(r)
	form, ok := s.parseAndCheck(w, r, PreCSRFKey(tok))
	if !ok {
		return
	}
	if open, err := s.setupOpen(r); err == nil && open {
		s.Redirect(w, r, "/setup", "", "")
		return
	}
	ft := FormToken(PreCSRFKey(tok))
	next := nextParam(form.Get("next"))
	username, password := form.Get("username"), form.Get("password")

	if allowed, _, wait := s.auth.login.Allow(r.RemoteAddr); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(RetryAfterSeconds(wait)))
		s.loginPage(w, r, http.StatusTooManyRequests, ft, next, "", &Flash{Kind: "bad", Text: lockoutText(wait)})
		return
	}
	wrong := func() {
		s.loginPage(w, r, http.StatusUnauthorized, ft, next, clip(username, 64), &Flash{Kind: "bad", Text: "Wrong username or password."})
	}
	// G-13: nothing is looked up or hashed for an oversized field.
	if PasswordTooLong(password) || len(username) > maxFieldBytes {
		s.recordFailure(r, s.auth.login)
		wrong()
		return
	}
	release, err := s.auth.sem.Acquire(r.Context())
	if err != nil {
		s.hashBusy(w, r)
		return
	}
	sessTok, _, err := s.cfg.Backend.GUILogin(r.Context(), r.RemoteAddr, clip(r.UserAgent(), store.MaxGUIUserAgent), username, password)
	release()
	if err != nil {
		if apiCode(err) != api.CodeUnauthenticated {
			s.logf("gui: login: %v", err)
			s.ErrorPage(w, r, http.StatusInternalServerError)
			return
		}
		s.recordFailure(r, s.auth.login)
		s.logf("gui: failed login from %s user=%q", LimiterKey(r.RemoteAddr), clip(username, 64))
		wrong()
		return
	}
	s.auth.login.Success(r.RemoteAddr)
	SetSessionCookie(w, sessTok)
	ClearPreCookie(w)
	if next == "" {
		next = "/"
	}
	s.Redirect(w, r, next, "", "")
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.parseAndCheckSession(w, r); !ok {
		return
	}
	p, id, _ := SessionFrom(r.Context())
	if err := s.cfg.Backend.GUILogout(r.Context(), p, id); err != nil {
		s.logf("gui: logout: %v", err)
	}
	ClearSessionCookie(w)
	s.Redirect(w, r, "/login", "logged-out", "")
}

// ---- setup ----

func (s *Server) setupPage(w http.ResponseWriter, r *http.Request, status int, formToken string, d SetupData) {
	pg := s.Page(w, r, "Set up mirafleet", "")
	pg.Authed, pg.CSRF, pg.Nav = false, formToken, ""
	d.Fingerprint = s.Fingerprint()
	pg.Data = d
	s.Render(w, r, status, "setup", "", pg)
}

func (s *Server) handleSetupForm(w http.ResponseWriter, r *http.Request) {
	if open, err := s.setupOpen(r); err != nil || !open {
		s.ErrorPage(w, r, http.StatusNotFound)
		return
	}
	s.setupPage(w, r, http.StatusOK, prePage(w, r), SetupData{})
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if open, err := s.setupOpen(r); err != nil || !open {
		s.ErrorPage(w, r, http.StatusNotFound)
		return
	}
	tok, _ := PreToken(r)
	form, ok := s.parseAndCheck(w, r, PreCSRFKey(tok))
	if !ok {
		return
	}
	ft := FormToken(PreCSRFKey(tok))
	token, username := strings.TrimSpace(form.Get("token")), form.Get("username")
	pw, pw2 := form.Get("password"), form.Get("password2")
	fail := func(status int, errs map[string]string) {
		s.setupPage(w, r, status, ft, SetupData{Username: clip(username, 64), Errors: errs})
	}

	if allowed, _, wait := s.auth.setup.Allow(r.RemoteAddr); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(RetryAfterSeconds(wait)))
		fail(http.StatusTooManyRequests, map[string]string{"form": lockoutText(wait)})
		return
	}
	errs := map[string]string{}
	switch {
	case token == "" || len(token) > maxFieldBytes:
		errs["token"] = "Enter the setup token from the setup-token file."
	case len(username) > maxFieldBytes:
		errs["username"] = "That username is too long."
	case PasswordTooLong(pw) || PasswordTooLong(pw2):
		errs["password"] = "That password is too long."
	case pw != pw2:
		errs["password2"] = "The two passwords differ."
	}
	if len(errs) > 0 {
		fail(http.StatusUnprocessableEntity, errs)
		return
	}
	release, err := s.auth.sem.Acquire(r.Context())
	if err != nil {
		s.hashBusy(w, r)
		return
	}
	sessTok, _, err := s.cfg.Backend.CompleteGUISetup(r.Context(), r.RemoteAddr, clip(r.UserAgent(), store.MaxGUIUserAgent), token, username, pw)
	release()
	if err != nil {
		var ae *api.Error
		switch {
		case errors.As(err, &ae) && ae.Code == api.CodeBadRequest: // username or password rule, fixed wording
			field := "password"
			if strings.Contains(ae.Message, "username") {
				field = "username"
			}
			fail(http.StatusUnprocessableEntity, map[string]string{field: ae.Message})
		case errors.As(err, &ae) && ae.Code == api.CodeConflict: // wrong, expired or used token, or a lost race (G-07)
			s.recordFailure(r, s.auth.setup)
			fail(http.StatusUnprocessableEntity, map[string]string{"token": "Setup is not available with that token. Check the token, or restart central to get a new one."})
		default:
			s.logf("gui: setup: %v", err)
			s.ErrorPage(w, r, http.StatusInternalServerError)
		}
		return
	}
	s.removeSetupFile()
	s.auth.setup.Success(r.RemoteAddr)
	SetSessionCookie(w, sessTok)
	ClearPreCookie(w)
	s.Redirect(w, r, "/", "", "")
}
