package gui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"mime"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

// Security headers set on every response (docs/M4.md 2.7). Cache-Control is not here: it is
// set per response (see statusWriter.WriteHeader), so content-hashed static files keep immutable.
var securityHeaders = [][2]string{
	{"Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; font-src 'self'; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"},
	{"X-Content-Type-Options", "nosniff"},
	{"Referrer-Policy", "no-referrer"},
	{"X-Frame-Options", "DENY"},
	{"Cross-Origin-Opener-Policy", "same-origin"},
	{"Cross-Origin-Resource-Policy", "same-origin"},
	{"Permissions-Policy", "camera=(), microphone=(), geolocation=(), usb=(), payment=(), clipboard-read=()"},
}

const (
	hstsValue = "max-age=31536000"

	// DefaultMaxInFlight is the cap on concurrent requests (503 beyond).
	DefaultMaxInFlight = 32
	// DefaultRequestTimeout is the per-request context timeout unless a route overrides it.
	DefaultRequestTimeout = 15 * time.Second
)

type ctxKey int

const requestIDKey ctxKey = 1

// RequestID returns the request id the middleware attached to ctx ("" outside a request).
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return hex.EncodeToString(b[:])
}

// statusWriter records the status for the access log and gives every response without an
// explicit Cache-Control the value "no-store" (HTML, fragments, errors, and the static handler's
// 404s: G-01). The static handler sets its own immutable value before WriteHeader, so it wins.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		if code >= 200 {
			w.status = code
		}
		if code >= 200 && w.Header().Get("Cache-Control") == "" {
			w.Header().Set("Cache-Control", "no-store")
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// Flush keeps streaming responses (audit export) working through the wrapper.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the real writer (deadlines, flush).
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// probeWriter runs the mux's own 404/405 handler without writing anything, so the middleware
// can answer with the branded error page instead.
type probeWriter struct {
	h    http.Header
	code int
}

func (p *probeWriter) Header() http.Header         { return p.h }
func (p *probeWriter) Write(b []byte) (int, error) { return len(b), nil }
func (p *probeWriter) WriteHeader(code int)        { p.code = code }

// buildHandler assembles the middleware chain around the mux. Order, outermost first: request
// id and headers, panic recovery and access log, Host allowlist, in-flight cap, request limits
// (method, content type, body size, timeout), then routing.
func (s *Server) buildHandler() http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		id := newRequestID()
		r = r.WithContext(context.WithValue(r.Context(), requestIDKey, id))
		w := &statusWriter{ResponseWriter: rw}
		s.applyHeaders(w.Header(), id)

		start := s.now()
		pattern := "-"
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				// The panic value and stack go to the log only, never to the browser.
				s.logf("gui: panic serving %s %s req=%s: %v\n%s", r.Method, pattern, id, rec, debug.Stack())
				if w.status != 0 {
					// The response is already partly sent: abort the connection.
					s.logf("gui: panic after response started req=%s", id)
					panic(http.ErrAbortHandler)
				}
				// Drop whatever the handler staged (Set-Cookie, Location, ...) and start clean.
				clear(w.Header())
				s.applyHeaders(w.Header(), id)
				s.renderError(w, r, http.StatusInternalServerError)
			}
			status := w.status
			if status == 0 {
				status = http.StatusOK
			}
			// Method, route pattern (never the raw path or query), status, duration, remote, id.
			s.logf("gui: %s %s %d %s %s req=%s", r.Method, pattern, status,
				s.now().Sub(start).Round(time.Millisecond), r.RemoteAddr, id)
		}()

		if !s.guard.allowed(r.Host) {
			s.renderError(w, r, http.StatusMisdirectedRequest)
			return
		}
		select {
		case s.sem <- struct{}{}:
			defer func() { <-s.sem }()
		default:
			w.Header().Set("Retry-After", "1")
			s.renderError(w, r, http.StatusServiceUnavailable)
			return
		}
		if !s.checkRequest(w, r) {
			return
		}

		// Unknown paths and methods get the branded page; everything else goes to the mux.
		hnd, pat := s.mux.Handler(r)
		if pat == "" {
			pw := &probeWriter{h: http.Header{}}
			hnd.ServeHTTP(pw, r)
			switch pw.code {
			case http.StatusNotFound:
				s.renderError(w, r, http.StatusNotFound)
				return
			case http.StatusMethodNotAllowed:
				if a := pw.h.Get("Allow"); a != "" {
					w.Header().Set("Allow", a)
				}
				s.renderError(w, r, http.StatusMethodNotAllowed)
				return
			}
		} else {
			pattern = pat
		}
		timeout := s.timeouts[pat]
		if timeout == 0 {
			timeout = s.reqTimeout
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		s.mux.ServeHTTP(w, r.WithContext(ctx))
		if w.status == 0 { // handler wrote nothing: still send the headers, with no-store
			w.WriteHeader(http.StatusOK)
		}
	})
}

// applyHeaders sets the security headers, HSTS (when serving a certificate-backed listener) and
// the request id on h.
func (s *Server) applyHeaders(h http.Header, id string) {
	for _, kv := range securityHeaders {
		h.Set(kv[0], kv[1])
	}
	if s.hsts {
		h.Set("Strict-Transport-Security", hstsValue)
	}
	h.Set("X-Request-ID", id)
}

// checkRequest applies the request limits before any parsing: bodies at most MaxFormBytes (413)
// and, when present, urlencoded forms only (415). It answers the error itself and returns false.
func (s *Server) checkRequest(w http.ResponseWriter, r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	if r.ContentLength > MaxFormBytes {
		s.renderError(w, r, http.StatusRequestEntityTooLarge)
		return false
	}
	ct := r.Header.Get("Content-Type")
	if ct != "" || r.ContentLength != 0 {
		mt, _, err := mime.ParseMediaType(ct)
		if err != nil || mt != "application/x-www-form-urlencoded" {
			s.renderError(w, r, http.StatusUnsupportedMediaType)
			return false
		}
	}
	// Chunked bodies have no length up front: the reader enforces the cap (handlers see
	// *http.MaxBytesError from ParseForm and answer 413; see ParseForm in limiter.go).
	r.Body = http.MaxBytesReader(w, r.Body, MaxFormBytes)
	return true
}

// RouteTimeout overrides the request timeout for a mux pattern (for example the audit export).
// Call it while registering routes, before Start.
func (s *Server) RouteTimeout(pattern string, d time.Duration) { s.timeouts[pattern] = d }

// logWriter adapts the server's logf to the io.Writer http.Server.ErrorLog wants.
type logWriter struct{ logf func(string, ...any) }

func (l logWriter) Write(p []byte) (int, error) {
	l.logf("gui: http: %s", strings.TrimSpace(string(p)))
	return len(p), nil
}
