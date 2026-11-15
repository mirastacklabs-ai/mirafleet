package gui

import "net/http"

// adminMux registers routes that need a signed-in admin: every handler is wrapped in
// requireAdmin, so screenAuth only reads the session it left in the context.
type adminMux struct {
	s   *Server
	mux *http.ServeMux
}

// HandleFunc registers h behind requireAdmin.
func (m adminMux) HandleFunc(pattern string, h http.HandlerFunc) {
	m.mux.Handle(pattern, m.s.requireAdmin(h))
}

// Passive registers a polled fragment endpoint: Passive is outermost, so authentication sees the
// flag and does not touch the session row.
func (m adminMux) Passive(pattern string, h http.HandlerFunc) {
	m.mux.Handle(pattern, Passive(m.s.requireAdmin(h)))
}

// routes builds the mux. Each screen's step registers its own routes in its handlers_*.go file;
// nobody else edits this one.
func (s *Server) routes() {
	mux := s.mux
	mux.Handle("GET /static/", s.assets.Handler())
	s.routesAuth(mux)
	am := adminMux{s: s, mux: mux}
	s.routesOverview(am)
	s.routesFleets(am)
	s.routesRunners(am)
	s.routesTokens(am)
	s.routesOperators(am)
	s.routesWorkspaces(am)
	s.routesAudit(mux)
	s.routesAccount(mux)
}
