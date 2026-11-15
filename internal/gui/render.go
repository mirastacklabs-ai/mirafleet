package gui

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// templatesFS holds the page templates: layout.html, partials.html and one file per screen.
//
//go:embed templates/*.html
var templatesFS embed.FS

// Page is the root value every template receives. The named fields are the contract in the
// header comment of templates/partials.html; Data carries the screen's own view model.
type Page struct {
	Title     string
	Nav       string // overview fleets runners tokens operators workspaces audit account, or ""
	Authed    bool
	CSRF      string
	Theme     string // light | dark | system
	Flash     *Flash
	Crumbs    []Crumb
	RequestID string
	Version   string
	Path      string // request path and query of a GET page, for the theme switch to return to; else ""
	Data      any
}

// Flash is a one-shot message shown after a redirect. Kind is ok, warn, bad or info.
type Flash struct{ Kind, Text string }

// Crumb is one breadcrumb; URL is empty for the current page.
type Crumb struct{ Text, URL string }

// ErrorData is Page.Data of the error pages.
type ErrorData struct {
	Status  int
	Heading string
	Message string
}

const (
	// FlashCookieName and ThemeCookieName are the cookies render.go reads and writes.
	FlashCookieName = "__Host-mf_flash"
	ThemeCookieName = "mf_theme"
	flashTTL        = 2 * time.Minute
	flashMaxSubject = 128
)

type flashDef struct{ kind, format string }

var (
	flashMu    sync.RWMutex
	flashTable = map[string]flashDef{}
)

// RegisterFlash adds a message to the fixed flash table (call it from init in a handler file).
// format may hold one %s, replaced with the subject (a name or id) at display time. The cookie
// carries only the id and the subject, so no message text is ever taken from a request.
func RegisterFlash(id, kind, format string) {
	flashMu.Lock()
	defer flashMu.Unlock()
	flashTable[id] = flashDef{kind: kind, format: format}
}

// renderer holds one parsed template set per screen (layout + partials + that screen).
type renderer struct {
	sets map[string]*template.Template
}

func newRenderer(funcs template.FuncMap) (*renderer, error) {
	entries, err := fs.ReadDir(templatesFS, "templates")
	if err != nil {
		return nil, err
	}
	r := &renderer{sets: map[string]*template.Template{}}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || name == "layout.html" || name == "partials.html" || path.Ext(name) != ".html" {
			continue
		}
		set, err := template.New(name).Funcs(funcs).ParseFS(templatesFS,
			"templates/layout.html", "templates/partials.html", "templates/"+name)
		if err != nil {
			return nil, fmt.Errorf("gui: template %s: %w", name, err)
		}
		r.sets[strings.TrimSuffix(name, ".html")] = set
	}
	if r.sets["errors"] == nil {
		return nil, fmt.Errorf("gui: templates/errors.html is missing")
	}
	return r, nil
}

// Page returns the page data for a request: request id, version, theme and (on full-page GETs)
// the pending flash message, which is consumed. PageDefaults then fills Authed, CSRF and the like.
func (s *Server) Page(w http.ResponseWriter, r *http.Request, title, nav string) Page {
	pg := Page{Title: title, Nav: nav, Theme: themeOf(r), RequestID: RequestID(r.Context()), Version: s.cfg.Version}
	if r.Method == http.MethodGet {
		pg.Path = r.URL.RequestURI()
	}
	if r.Header.Get("HX-Request") != "true" {
		pg.Flash = s.takeFlash(w, r)
	}
	if s.PageDefaults != nil {
		s.PageDefaults(r, &pg)
	}
	return pg
}

func themeOf(r *http.Request) string {
	if c, err := r.Cookie(ThemeCookieName); err == nil {
		switch c.Value {
		case "light", "dark", "system":
			return c.Value
		}
	}
	return "system"
}

// Render writes a screen. A normal request gets the full page; with HX-Request: true and a
// non-empty fragment, only the template "<fragment>" (named "<screen>/<part>"). The same Page is
// used for both. Output is buffered, so a template failure becomes a 500 page, not a torn page.
func (s *Server) Render(w http.ResponseWriter, r *http.Request, status int, screen, fragment string, pg Page) {
	set := s.tmpl.sets[screen]
	if set == nil {
		s.logf("gui: render: unknown screen %q", screen)
		s.renderError(w, r, http.StatusInternalServerError)
		return
	}
	name := "layout"
	if fragment != "" && r.Header.Get("HX-Request") == "true" {
		name = fragment
	}
	var buf bytes.Buffer
	if err := set.ExecuteTemplate(&buf, name, pg); err != nil {
		s.logf("gui: render %s/%s req=%s: %v", screen, name, pg.RequestID, err)
		s.renderError(w, r, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

var errorTexts = map[int][2]string{
	http.StatusForbidden:             {"Forbidden", "You are not allowed to do that."},
	http.StatusNotFound:              {"Not found", "There is nothing at this address."},
	http.StatusMethodNotAllowed:      {"Method not allowed", "This address does not accept that kind of request."},
	http.StatusRequestEntityTooLarge: {"Request too large", "The request was bigger than this server accepts."},
	http.StatusUnsupportedMediaType:  {"Unsupported content type", "This server accepts only ordinary form submissions."},
	http.StatusMisdirectedRequest:    {"Unknown host name", "This server does not answer for the host name in your address. Use an address from central's start-up log, or list the name with -gui-host."},
	http.StatusTooManyRequests:       {"Too many requests", "Wait a little and try again."},
	http.StatusInternalServerError:   {"Something went wrong", "The error was logged. Quote the request ID when you ask for help."},
	http.StatusServiceUnavailable:    {"Busy", "The server is handling as many requests as it can. Try again in a moment."},
}

// ErrorPage writes a branded error page (or its fragment for htmx) with the request id. Use it
// from handlers for 403, 404 and the other statuses in the table; message detail never comes from
// an internal error.
func (s *Server) ErrorPage(w http.ResponseWriter, r *http.Request, status int) {
	s.renderError(w, r, status)
}

func (s *Server) renderError(w http.ResponseWriter, r *http.Request, status int) {
	txt, ok := errorTexts[status]
	if !ok {
		txt = [2]string{http.StatusText(status), "The request could not be completed."}
	}
	pg := Page{Title: txt[0], Theme: themeOf(r), RequestID: RequestID(r.Context()), Version: s.cfg.Version,
		Data: ErrorData{Status: status, Heading: txt[0], Message: txt[1]}}
	// 421 and 503 come before the Host check or the in-flight cap: no session lookup for them.
	if s.PageDefaults != nil && status != http.StatusMisdirectedRequest && status != http.StatusServiceUnavailable {
		s.PageDefaults(r, &pg)
	}
	name := "layout"
	if r.Header.Get("HX-Request") == "true" {
		name = "errors/panel"
	}
	var buf bytes.Buffer
	if err := s.tmpl.sets["errors"].ExecuteTemplate(&buf, name, pg); err != nil {
		s.logf("gui: render error page: %v", err)
		http.Error(w, http.StatusText(status), status)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// localPath reports whether u is a path on this server ("/x", not "//host" or "/\host"). Control
// bytes are refused anywhere (browsers strip tabs and newlines, so "/\t/evil" would become
// "//evil").
func localPath(u string) bool {
	for i := 0; i < len(u); i++ {
		if u[i] < 0x20 || u[i] == 0x7f {
			return false
		}
	}
	if !strings.HasPrefix(u, "/") || strings.HasPrefix(u, "//") || strings.Contains(u, "\\") {
		return false
	}
	p, err := url.Parse(u)
	if err != nil || p.Scheme != "" || p.Host != "" || p.Opaque != "" || p.User != nil {
		return false
	}
	return strings.HasPrefix(p.Path, "/") && !strings.HasPrefix(p.Path, "//")
}

// Redirect ends a POST. A normal request gets 303 (PRG); an htmx request gets 200 with
// HX-Redirect, so the browser navigates instead of swapping the redirect target into a fragment.
// target must be a local path. A non-empty flashID (see RegisterFlash) is shown once on the next
// page, with subject filled in.
func (s *Server) Redirect(w http.ResponseWriter, r *http.Request, target, flashID, subject string) {
	if !localPath(target) {
		target = "/"
	}
	if flashID != "" {
		s.setFlash(w, r, flashID, subject)
	}
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// flashKeyFor is the MAC key for the flash cookie: always the random per-process secret, so the
// flash cannot be forged and does not depend on a session lookup (a flash set while logging out
// or logging in still verifies).
func (s *Server) flashKeyFor(_ *http.Request) []byte { return s.flashSecret }

func (s *Server) setFlash(w http.ResponseWriter, r *http.Request, id, subject string) {
	flashMu.RLock()
	_, ok := flashTable[id]
	flashMu.RUnlock()
	if !ok {
		s.logf("gui: unknown flash id %q", id)
		return
	}
	if len(subject) > flashMaxSubject {
		cut := flashMaxSubject
		for cut > 0 && !utf8.RuneStart(subject[cut]) {
			cut--
		}
		subject = subject[:cut]
	}
	val := SignFlash(s.flashKeyFor(r), id+"\x00"+subject, s.now(), flashTTL)
	http.SetCookie(w, &http.Cookie{Name: FlashCookieName, Value: val, Path: "/", Secure: true,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: int(flashTTL / time.Second)})
}

// takeFlash returns the pending flash (nil when absent, forged or expired) and expires the cookie.
func (s *Server) takeFlash(w http.ResponseWriter, r *http.Request) *Flash {
	c, err := r.Cookie(FlashCookieName)
	if err != nil {
		return nil
	}
	http.SetCookie(w, &http.Cookie{Name: FlashCookieName, Value: "", Path: "/", Secure: true,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	msg, ok := OpenFlash(s.flashKeyFor(r), c.Value, s.now())
	if !ok {
		return nil
	}
	id, subject, _ := strings.Cut(msg, "\x00")
	flashMu.RLock()
	def, ok := flashTable[id]
	flashMu.RUnlock()
	if !ok {
		return nil
	}
	text := def.format
	if strings.Contains(text, "%s") {
		text = fmt.Sprintf(text, subject)
	}
	return &Flash{Kind: def.kind, Text: text}
}

// QueryEscape and PathEscape are re-exported helpers for building URLs in handlers.
func PathEscape(s string) string  { return url.PathEscape(s) }
func QueryEscape(s string) string { return url.QueryEscape(s) }
