package gui

// routes builds the mux. Each screen's step registers its own routes in its handlers_*.go file;
// nobody else edits this one.
func (s *Server) routes() {
	mux := s.mux
	mux.Handle("GET /static/", s.assets.Handler())
	s.routesAuth(mux)
	s.routesOverview(mux)
	s.routesFleets(mux)
	s.routesRunners(mux)
	s.routesTokens(mux)
	s.routesOperators(mux)
	s.routesWorkspaces(mux)
	s.routesAudit(mux)
	s.routesAccount(mux)
}
