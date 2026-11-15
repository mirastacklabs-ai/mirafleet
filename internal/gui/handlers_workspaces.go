package gui

// Workspaces screen (docs/M4.md 3.7): the admin list (polled, passive), filters and kill.

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
)

// wsKillTimeout covers the runner destroy call (central's wsCloseOuterTimeout is 90 s).
const wsKillTimeout = 100 * time.Second

func init() {
	RegisterFlash("ws.closed", "ok", "Workspace %s is closed.")
	RegisterFlash("ws.closing", "warn", "Workspace %s is closing. The runner did not confirm (it may be offline); the reaper finishes it.")
	RegisterFlash("ws.ended", "info", "Workspace %s had already ended.")
}

type wsRow struct {
	ID, ShortID, Fleet, Runner, Operator, Session, Image, KillURL string
	State                                                         badgeView
	CanKill                                                       bool
	Created, LastUsed, Expires                                    timeView
	Age                                                           string
}

type workspacesData struct {
	View      string // list | kill
	Rows      []wsRow
	Fleets    []string
	Fleet     string // selected filter
	State     string // live | all
	LiveURL   string
	Updated   timeView
	Empty     emptyView
	Row       wsRow // kill
	Action    string
	CancelURL string
}

func (s *Server) routesWorkspaces(mux *http.ServeMux) {
	mux.HandleFunc("GET /workspaces", s.workspacesPage)
	mux.Handle("GET /workspaces/live", Passive(http.HandlerFunc(s.workspacesPage)))
	mux.HandleFunc("GET /workspaces/{id}/kill", s.workspaceKillForm)
	mux.HandleFunc("POST /workspaces/{id}/kill", s.workspaceKill)
	s.RouteTimeout("POST /workspaces/{id}/kill", wsKillTimeout)
}

func wsBadge(st store.WorkspaceState) badgeView {
	switch st {
	case store.WorkspaceOpen:
		return badgeView{"ok", string(st)}
	case store.WorkspaceOpening, store.WorkspaceClosing:
		return badgeView{"warn", string(st)}
	case store.WorkspaceClosed:
		return badgeView{"off", string(st)}
	}
	return badgeView{"bad", string(st)}
}

func (s *Server) wsRowOf(w store.Workspace) wsRow {
	short := w.WsID
	if len(short) > 8 {
		short = short[:8]
	}
	end := s.now()
	if !w.ClosedAt.IsZero() {
		end = w.ClosedAt
	}
	return wsRow{ID: w.WsID, ShortID: short, Fleet: w.Fleet, Runner: w.Runner, Operator: w.Operator, Session: w.Session,
		Image: w.Image, State: wsBadge(w.State), CanKill: !w.State.Terminal(),
		KillURL: "/workspaces/" + PathEscape(w.WsID) + "/kill",
		Created: s.timeOf(w.CreatedAt), LastUsed: s.timeOf(w.LastUsedAt), Expires: s.timeOf(w.ExpiresAt),
		Age: strings.TrimSuffix(relAge(end, w.CreatedAt), " ago")}
}

func (s *Server) workspacesPage(w http.ResponseWriter, r *http.Request) {
	a, ok := s.screenAuth(w, r)
	if !ok {
		return
	}
	ctx, be, src := r.Context(), s.cfg.Backend, service.SourceGUI
	fleets, err := be.ListFleets(ctx, a.P, src)
	if err != nil {
		s.backendFail(w, r, err)
		return
	}
	d := workspacesData{View: "list", State: "live",
		Empty: emptyView{Title: "No workspaces", Body: "Workspaces are opened by operators in workspace-mode fleets.", CLI: "mirafleet central workspace list"}}
	q := r.URL.Query()
	if q.Get("state") == "all" {
		d.State = "all"
	}
	for _, f := range fleets {
		d.Fleets = append(d.Fleets, f.Name)
		if q.Get("fleet") == f.Name { // only a known fleet name is used or echoed
			d.Fleet = f.Name
		}
	}
	list, err := be.AdminListWorkspaces(ctx, a.P, src, d.Fleet)
	if err != nil {
		s.backendFail(w, r, err)
		return
	}
	for _, ws := range list {
		if d.State == "live" && !ws.State.Live() {
			continue
		}
		d.Rows = append(d.Rows, s.wsRowOf(ws))
	}
	lq := url.Values{"state": {d.State}}
	if d.Fleet != "" {
		lq.Set("fleet", d.Fleet)
	}
	d.LiveURL = "/workspaces/live?" + lq.Encode()
	d.Updated = s.timeOf(s.now())
	pg := s.screenPage(w, r, a, "Workspaces", "workspaces")
	pg.Data = d
	s.Render(w, r, http.StatusOK, "workspaces", "workspaces/list", pg)
}

// findWorkspace returns the workspace named by the path or answers 404.
func (s *Server) findWorkspace(w http.ResponseWriter, r *http.Request, a adminReq) (store.Workspace, bool) {
	id := r.PathValue("id")
	if !workspace.ValidID(workspace.ID(id)) {
		s.ErrorPage(w, r, http.StatusNotFound)
		return store.Workspace{}, false
	}
	list, err := s.cfg.Backend.AdminListWorkspaces(r.Context(), a.P, service.SourceGUI, "")
	if err != nil {
		s.backendFail(w, r, err)
		return store.Workspace{}, false
	}
	for _, ws := range list {
		if ws.WsID == id {
			return ws, true
		}
	}
	s.ErrorPage(w, r, http.StatusNotFound)
	return store.Workspace{}, false
}

func (s *Server) workspaceKillForm(w http.ResponseWriter, r *http.Request) {
	a, ok := s.screenAuth(w, r)
	if !ok {
		return
	}
	ws, ok := s.findWorkspace(w, r, a)
	if !ok {
		return
	}
	row := s.wsRowOf(ws)
	pg := s.screenPage(w, r, a, "Kill workspace "+row.ShortID, "workspaces",
		Crumb{"Workspaces", "/workspaces"}, Crumb{"Kill " + row.ShortID, ""})
	pg.Data = workspacesData{View: "kill", Row: row, Action: row.KillURL, CancelURL: "/workspaces"}
	s.Render(w, r, http.StatusOK, "workspaces", "workspaces/kill", pg)
}

func (s *Server) workspaceKill(w http.ResponseWriter, r *http.Request) {
	a, ok := s.screenAuth(w, r)
	if !ok {
		return
	}
	if _, ok := s.screenForm(w, r, a); !ok {
		return
	}
	id := r.PathValue("id")
	if !workspace.ValidID(workspace.ID(id)) {
		s.ErrorPage(w, r, http.StatusNotFound)
		return
	}
	// The destroy call can take up to the runner timeout; lift the server's write deadline.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(wsKillTimeout + 10*time.Second))
	ws, err := s.cfg.Backend.KillWorkspace(r.Context(), a.P, service.SourceGUI, id)
	if err != nil {
		s.backendFail(w, r, err)
		return
	}
	flash := "ws.ended"
	switch ws.State {
	case store.WorkspaceClosed:
		flash = "ws.closed"
	case store.WorkspaceClosing:
		flash = "ws.closing"
	}
	s.Redirect(w, r, "/workspaces?state=all", flash, id[:8])
}
