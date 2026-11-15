package gui

// Overview screen (docs/M4.md 3.2): counts, pending runners and recent audit. The live part is
// one fragment that polls itself every 10 s through a Passive route (no session touch).

import (
	"net/http"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
)

const (
	overviewAuditRows = 15
	overviewDeniedAge = 24 * time.Hour
)

type pendingRow struct{ Fleet, Name, URL, ApproveURL string }

type auditRow struct {
	Time          timeView
	Actor, Source string
	Action        string
	Target        string
	Badge         badgeView
}

type badgeView struct{ Kind, Text string }

type emptyView struct{ Title, Body, CLI string }

type overviewData struct {
	Fleets, FleetsActions, FleetsWorkspace int
	Runners, Pending, Active, Disabled     int
	Online, Offline                        int
	LiveWorkspaces, TokensUsable           int
	PendingRunners                         []pendingRow
	Recent, Denied                         []auditRow
	Updated                                timeView
	NoPending, NoRecent                    emptyView
}

func (s *Server) routesOverview(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", s.overviewPage)
	mux.Handle("GET /overview/live", Passive(http.HandlerFunc(s.overviewPage)))
}

func (s *Server) overviewPage(w http.ResponseWriter, r *http.Request) {
	a, ok := s.screenAuth(w, r)
	if !ok {
		return
	}
	d, err := s.overviewData(r, a)
	if err != nil {
		s.backendFail(w, r, err)
		return
	}
	pg := s.screenPage(w, r, a, "Overview", "overview")
	pg.Data = d
	s.Render(w, r, http.StatusOK, "overview", "overview/live", pg)
}

func (s *Server) overviewData(r *http.Request, a adminReq) (overviewData, error) {
	ctx, be, src, now := r.Context(), s.cfg.Backend, service.SourceGUI, s.now()
	d := overviewData{
		NoPending: emptyView{Title: "No runners waiting", Body: "New runners appear here after they join with a token that does not auto-approve."},
		NoRecent:  emptyView{Title: "Nothing recorded yet", Body: "Every change made here or from the CLI shows up in the audit log.", CLI: "mirafleet central audit"},
	}
	fleets, err := be.ListFleets(ctx, a.P, src)
	if err != nil {
		return d, err
	}
	d.Fleets = len(fleets)
	for _, f := range fleets {
		if f.Mode == store.ModeWorkspace {
			d.FleetsWorkspace++
		} else {
			d.FleetsActions++
		}
		rs, err := be.ListRunnersAll(ctx, a.P, src, f.Name, false)
		if err != nil {
			return d, err
		}
		for _, rv := range rs {
			d.Runners++
			switch rv.Status {
			case store.StatusPending:
				d.Pending++
				d.PendingRunners = append(d.PendingRunners, pendingRow{Fleet: f.Name, Name: rv.Name,
					URL:        "/runners/" + PathEscape(f.Name) + "/" + PathEscape(rv.Name),
					ApproveURL: "/runners/" + PathEscape(f.Name) + "/" + PathEscape(rv.Name) + "/approve"})
				continue
			case store.StatusActive:
				d.Active++
			case store.StatusDisabled:
				d.Disabled++
			}
			if rv.Online {
				d.Online++
			} else {
				d.Offline++
			}
		}
	}
	wss, err := be.AdminListWorkspaces(ctx, a.P, src, "")
	if err != nil {
		return d, err
	}
	for _, ws := range wss {
		if ws.State == store.WorkspaceOpen || ws.State == store.WorkspaceOpening {
			d.LiveWorkspaces++
		}
	}
	toks, err := be.ListTokens(ctx, a.P, src, "")
	if err != nil {
		return d, err
	}
	for _, t := range toks {
		if t.Usable(now) {
			d.TokensUsable++
		}
	}
	recent, err := be.ListAudit(ctx, a.P, src, store.AuditFilter{Desc: true, Limit: overviewAuditRows})
	if err != nil {
		return d, err
	}
	denied, err := be.ListAudit(ctx, a.P, src, store.AuditFilter{Desc: true, Limit: overviewAuditRows,
		Outcome: service.OutcomeDenied, From: now.Add(-overviewDeniedAge)})
	if err != nil {
		return d, err
	}
	d.Recent, d.Denied = s.auditRows(recent), s.auditRows(denied)
	d.Updated = s.timeOf(now)
	return d, nil
}

func (s *Server) auditRows(evs []store.AuditEvent) []auditRow {
	out := make([]auditRow, 0, len(evs))
	for _, e := range evs {
		target := e.Subject
		if target == "" {
			target = e.Fleet
			if e.Runner != "" {
				target += "/" + e.Runner
			}
		}
		kind := "warn"
		switch e.Outcome {
		case "ok":
			kind = "ok"
		case "denied":
			kind = "bad"
		}
		out = append(out, auditRow{Time: s.timeOf(e.Time), Actor: e.Actor, Source: e.Source, Action: e.Action,
			Target: target, Badge: badgeView{Kind: kind, Text: e.Outcome}})
	}
	return out
}
