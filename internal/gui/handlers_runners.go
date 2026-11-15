package gui

// Runners screens (docs/M4.md 3.4): list across fleets (polled, passive), detail, lifecycle
// transitions and the typed-confirmation revoke. Everything a runner reports (labels, heartbeat
// info, advertised actions) is untrusted: it is cut to a bounded length and control and format
// characters (bidi overrides, zero-width, ESC) are shown as <U+XXXX> before html/template escapes
// the rest, so it is rendered as inert text only.

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
)

func init() {
	RegisterFlash("runner.approve", "ok", "Runner %s approved.")
	RegisterFlash("runner.disable", "warn", "Runner %s disabled.")
	RegisterFlash("runner.enable", "ok", "Runner %s enabled.")
	RegisterFlash("runner.revoke", "warn", "Runner %s revoked.")
	RegisterFlash("runner.conflict", "warn", "Runner %s was not in the expected state; nothing was changed.")
}

const (
	runnerTextMax   = 120 // clip for list cells and chips
	runnerDetailMax = 256 // clip for detail values
	detailAuditRows = 20
	detailWSRows    = 20
)

// inertRunnerText makes untrusted text safe to show: invalid UTF-8, control and format
// characters (Cc, Cf: bidi controls, zero-width, tags) and line/paragraph separators become
// <U+XXXX>, and the result is cut to max runes with an ellipsis. html/template then escapes
// markup characters.
func inertRunnerText(s string, max int) string {
	var b strings.Builder
	n := 0
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		s = s[size:]
		if n >= max {
			b.WriteString("...")
			break
		}
		n++
		switch {
		case r == utf8.RuneError && size <= 1:
			b.WriteString("<U+FFFD>")
		case unicode.Is(unicode.Cc, r), unicode.Is(unicode.Cf, r), unicode.Is(unicode.Zl, r), unicode.Is(unicode.Zp, r):
			fmt.Fprintf(&b, "<U+%04X>", r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func sortedKV(m map[string]string, max int) []kvRow {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]kvRow, 0, len(keys))
	for _, k := range keys {
		out = append(out, kvRow{K: inertRunnerText(k, 64), V: inertRunnerText(m[k], max)})
	}
	return out
}

func chips(m map[string]string) []string {
	kv := sortedKV(m, runnerTextMax)
	out := make([]string, len(kv))
	for i, e := range kv {
		out[i] = e.K + "=" + e.V
	}
	return out
}

type opButton struct{ Op, Label string }

// runnerOps are the one-click transitions offered for a status; Revoke (typed confirmation) is
// separate. The service re-checks the transition atomically, so this only decides what is shown.
func runnerOps(st store.Status) []opButton {
	switch st {
	case store.StatusPending:
		return []opButton{{api.OpApprove, "Approve"}}
	case store.StatusActive:
		return []opButton{{api.OpDisable, "Disable"}}
	case store.StatusDisabled:
		return []opButton{{api.OpEnable, "Enable"}}
	}
	return nil
}

type runnerListRow struct {
	Fleet, Name, URL, Status, Kind string
	Online                         bool
	Age                            string
	Labels                         []string
	Executor, Slots, Version       string
	Ops                            []opButton
	CanRevoke                      bool
	RevokeURL                      string
}

type paramRow struct {
	Name, Pattern, Default string
	Required               bool
}

type actionRow struct {
	Name   string
	Params []paramRow
}

type runnerWSRow struct{ ID, Operator, Image, State string }

// runnerCredRow is a credential the runner holds: name, kind, hosts and status only.
type runnerCredRow struct {
	Name, URL, Kind string
	Hosts           []string
	Status          badgeView
}

type runnerDetail struct {
	Fleet, Name, URL, Status, Kind string
	Online                         bool
	Age                            string
	Fingerprint, Token             string
	Joined, LastSeen               timeView
	Labels, Info                   []kvRow
	Executor, Slots                string
	Actions                        []actionRow
	Workspaces                     []runnerWSRow
	Credentials                    []runnerCredRow
	Audit                          []auditRow
	Ops                            []opButton
	CanRevoke                      bool
	RevokeURL                      string
}

type runnersData struct {
	View string // list | detail | revoke

	// list
	Rows                     []runnerListRow
	Fleets                   []string
	FFleet, FStatus, FOnline string
	ShowRevoked              bool
	LiveURL                  string
	Updated                  timeView
	Empty                    emptyView
	NoMatch                  bool

	// detail
	D runnerDetail

	// revoke
	Ref, Typed, Err, ActionURL, Back, DetailURL string
}

func (s *Server) routesRunners(mux adminMux) {
	mux.HandleFunc("GET /runners", s.runnersList)
	mux.Passive("GET /runners/live", s.runnersList)
	mux.HandleFunc("GET /runners/{fleet}/{runner}", s.runnerDetailPage)
	mux.HandleFunc("GET /runners/{fleet}/{runner}/revoke", s.runnerRevokeForm)
	mux.HandleFunc("POST /runners/{fleet}/{runner}/{op}", s.runnerOp)
}

func runnerURL(fleet, runner string) string {
	return "/runners/" + PathEscape(fleet) + "/" + PathEscape(runner)
}

func (s *Server) hbAge(rv service.RunnerView) string {
	if rv.LastHeartbeat.IsZero() {
		return ""
	}
	return relAge(s.now(), rv.LastHeartbeat)
}

func slotsText(rv service.RunnerView) string {
	if rv.Slots <= 0 {
		return ""
	}
	return strconv.Itoa(rv.FreeSlots) + " / " + strconv.Itoa(rv.Slots)
}

func (s *Server) runnerListRowOf(rv service.RunnerView) runnerListRow {
	u := runnerURL(rv.Fleet, rv.Name)
	return runnerListRow{Fleet: rv.Fleet, Name: rv.Name, URL: u, Status: capital(string(rv.Status)),
		Kind: statusKind(rv.Status), Online: rv.Online, Age: s.hbAge(rv), Labels: chips(rv.Labels),
		Executor: inertRunnerText(rv.Executor, 40), Slots: slotsText(rv),
		Version: inertRunnerText(rv.Info["version"], 40), Ops: runnerOps(rv.Status),
		CanRevoke: rv.Status != store.StatusRevoked, RevokeURL: u + "/revoke?back=list"}
}

func (s *Server) runnersList(w http.ResponseWriter, r *http.Request) {
	a, ok := s.screenAuth(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	fFleet, fStatus, fOnline := q.Get("fleet"), q.Get("status"), q.Get("online")
	if fFleet != "" && !store.ValidFleetName(fFleet) {
		fFleet = ""
	}
	switch fStatus {
	case "pending", "active", "disabled", "revoked":
	default:
		fStatus = ""
	}
	if fOnline != "yes" && fOnline != "no" {
		fOnline = ""
	}
	showRevoked := q.Get("revoked") == "1" || fStatus == "revoked"

	ctx, be := r.Context(), s.cfg.Backend
	fleets, err := be.ListFleets(ctx, a.P, service.SourceGUI)
	if err != nil {
		s.backendFail(w, r, err)
		return
	}
	d := runnersData{View: "list", FFleet: fFleet, FStatus: fStatus, FOnline: fOnline, ShowRevoked: showRevoked}
	total := 0
	for _, f := range fleets {
		d.Fleets = append(d.Fleets, f.Name)
		if fFleet != "" && f.Name != fFleet {
			continue
		}
		rvs, err := be.ListRunnersAll(ctx, a.P, service.SourceGUI, f.Name, showRevoked)
		if err != nil {
			s.backendFail(w, r, err)
			return
		}
		total += len(rvs)
		for _, rv := range rvs {
			if fStatus != "" && string(rv.Status) != fStatus {
				continue
			}
			if (fOnline == "yes" && !rv.Online) || (fOnline == "no" && rv.Online) {
				continue
			}
			d.Rows = append(d.Rows, s.runnerListRowOf(rv))
		}
	}
	d.NoMatch = total > 0 && len(d.Rows) == 0
	d.Empty = emptyView{Title: "No runners yet",
		Body: "A runner appears here after it joins with a join token.", CLI: "mirafleet runner join CENTRAL:2222 -name NAME -token -"}
	lq := url.Values{}
	for k, v := range map[string]string{"fleet": fFleet, "status": fStatus, "online": fOnline} {
		if v != "" {
			lq.Set(k, v)
		}
	}
	if q.Get("revoked") == "1" {
		lq.Set("revoked", "1")
	}
	d.LiveURL = "/runners/live"
	if e := lq.Encode(); e != "" {
		d.LiveURL += "?" + e
	}
	d.Updated = s.timeOf(s.now())
	pg := s.screenPage(w, r, a, "Runners", "runners")
	pg.Data = d
	s.Render(w, r, http.StatusOK, "runners", "runners/live", pg)
}

// findRunner resolves {fleet}/{runner} to a runner view (revoked ones included). It answers 404
// itself for a bad name, a missing fleet or a missing runner.
func (s *Server) findRunner(w http.ResponseWriter, r *http.Request, a adminReq) (service.RunnerView, bool) {
	fleet, name := r.PathValue("fleet"), r.PathValue("runner")
	if !store.ValidFleetName(fleet) || !store.ValidRunnerName(name) {
		s.ErrorPage(w, r, http.StatusNotFound)
		return service.RunnerView{}, false
	}
	rvs, err := s.cfg.Backend.ListRunnersAll(r.Context(), a.P, service.SourceGUI, fleet, true)
	if err != nil {
		s.backendFail(w, r, err)
		return service.RunnerView{}, false
	}
	for _, rv := range rvs {
		if rv.Name == name {
			return rv, true
		}
	}
	s.ErrorPage(w, r, http.StatusNotFound)
	return service.RunnerView{}, false
}

func (s *Server) runnerDetailPage(w http.ResponseWriter, r *http.Request) {
	a, ok := s.screenAuth(w, r)
	if !ok {
		return
	}
	rv, ok := s.findRunner(w, r, a)
	if !ok {
		return
	}
	ctx, be := r.Context(), s.cfg.Backend
	u := runnerURL(rv.Fleet, rv.Name)
	det := runnerDetail{Fleet: rv.Fleet, Name: rv.Name, URL: u, Status: capital(string(rv.Status)),
		Kind: statusKind(rv.Status), Online: rv.Online, Age: s.hbAge(rv),
		Fingerprint: inertRunnerText(rv.Fingerprint, runnerDetailMax), Joined: s.timeOf(rv.CreatedAt),
		LastSeen: s.timeOf(rv.LastSeenAt), Labels: sortedKV(rv.Labels, runnerDetailMax),
		Info: sortedKV(rv.Info, runnerDetailMax), Executor: inertRunnerText(rv.Executor, 40), Slots: slotsText(rv),
		Ops: runnerOps(rv.Status), CanRevoke: rv.Status != store.StatusRevoked, RevokeURL: u + "/revoke"}
	det.Token = "deleted"
	if rv.JoinTokenID != nil {
		toks, err := be.ListTokens(ctx, a.P, service.SourceGUI, rv.Fleet)
		if err != nil {
			s.backendFail(w, r, err)
			return
		}
		for _, t := range toks {
			if t.ID == *rv.JoinTokenID {
				det.Token = t.TokenID
			}
		}
	}
	for _, ai := range rv.Actions {
		ar := actionRow{Name: inertRunnerText(ai.Name, runnerDetailMax)}
		for _, p := range ai.Params {
			pr := paramRow{Name: inertRunnerText(p.Name, 64), Pattern: inertRunnerText(p.Pattern, runnerDetailMax), Required: p.Default == nil}
			if p.Default != nil {
				pr.Default = inertRunnerText(*p.Default, runnerDetailMax)
			}
			ar.Params = append(ar.Params, pr)
		}
		det.Actions = append(det.Actions, ar)
	}
	wss, err := be.AdminListWorkspaces(ctx, a.P, service.SourceGUI, rv.Fleet)
	if err != nil {
		s.backendFail(w, r, err)
		return
	}
	for _, ws := range wss {
		if ws.Runner != rv.Name || len(det.Workspaces) >= detailWSRows {
			continue
		}
		det.Workspaces = append(det.Workspaces, runnerWSRow{ID: inertRunnerText(ws.WsID, 64),
			Operator: inertRunnerText(ws.Operator, 64), Image: inertRunnerText(ws.Image, runnerTextMax), State: string(ws.State)})
	}
	// A revoked runner holds nothing (and the store no longer resolves it by name).
	var creds []api.RunnerCredential
	if rv.Status != store.StatusRevoked {
		var err error
		if creds, err = be.RunnerCredentials(ctx, a.P, service.SourceGUI, store.Ref{Fleet: rv.Fleet, Runner: rv.Name}); err != nil {
			s.backendFail(w, r, err)
			return
		}
	}
	for _, c := range creds {
		row := runnerCredRow{Name: inertRunnerText(c.Name, 64), URL: credURL(c.Name), Kind: c.Kind, Status: deliveryBadge(c.Status)}
		for _, h := range c.Hosts {
			row.Hosts = append(row.Hosts, inertRunnerText(h, runnerTextMax))
		}
		det.Credentials = append(det.Credentials, row)
	}
	evs, err := be.ListAudit(ctx, a.P, service.SourceGUI,
		store.AuditFilter{Fleet: rv.Fleet, Runner: rv.Name, Desc: true, Limit: detailAuditRows})
	if err != nil {
		s.backendFail(w, r, err)
		return
	}
	det.Audit = s.auditRows(evs)
	pg := s.screenPage(w, r, a, "Runner "+rv.Fleet+"/"+rv.Name, "runners",
		Crumb{Text: "Runners", URL: "/runners"}, Crumb{Text: rv.Fleet + "/" + rv.Name})
	pg.Data = runnersData{View: "detail", D: det}
	s.Render(w, r, http.StatusOK, "runners", "runners/detail", pg)
}

func (s *Server) renderRevoke(w http.ResponseWriter, r *http.Request, a adminReq, status int, rv service.RunnerView, typed, errText, back string) {
	ref := rv.Fleet + "/" + rv.Name
	u := runnerURL(rv.Fleet, rv.Name)
	pg := s.screenPage(w, r, a, "Revoke "+ref, "runners",
		Crumb{Text: "Runners", URL: "/runners"}, Crumb{Text: ref, URL: u}, Crumb{Text: "Revoke"})
	pg.Data = runnersData{View: "revoke", Ref: ref, Typed: typed, Err: errText, Back: back,
		ActionURL: u + "/revoke", DetailURL: u}
	s.Render(w, r, status, "runners", "runners/revoke", pg)
}

func backOf(v string) string {
	if v == "list" {
		return "list"
	}
	return "detail"
}

func (s *Server) runnerRevokeForm(w http.ResponseWriter, r *http.Request) {
	a, ok := s.screenAuth(w, r)
	if !ok {
		return
	}
	rv, ok := s.findRunner(w, r, a)
	if !ok {
		return
	}
	if rv.Status == store.StatusRevoked {
		s.Redirect(w, r, runnerURL(rv.Fleet, rv.Name), "", "")
		return
	}
	s.renderRevoke(w, r, a, http.StatusOK, rv, "", "", backOf(r.URL.Query().Get("back")))
}

func (s *Server) runnerOp(w http.ResponseWriter, r *http.Request) {
	a, ok := s.screenAuth(w, r)
	if !ok {
		return
	}
	form, ok := s.screenForm(w, r, a)
	if !ok {
		return
	}
	op := r.PathValue("op")
	switch op {
	case api.OpApprove, api.OpDisable, api.OpEnable, api.OpRevoke:
	default:
		s.ErrorPage(w, r, http.StatusNotFound)
		return
	}
	rv, ok := s.findRunner(w, r, a)
	if !ok {
		return
	}
	ref := rv.Fleet + "/" + rv.Name
	back := backOf(form.Get("back"))
	if op == api.OpRevoke {
		typed := strings.TrimSpace(form.Get("confirm"))
		if typed != ref {
			s.renderRevoke(w, r, a, http.StatusUnprocessableEntity, rv, inertRunnerText(typed, 200),
				"Type the runner as fleet/runner exactly to revoke it.", back)
			return
		}
	}
	target := runnerURL(rv.Fleet, rv.Name)
	if back == "list" {
		target = "/runners"
	}
	if _, err := s.cfg.Backend.Lifecycle(r.Context(), a.P, service.SourceGUI, ref, op); err != nil {
		if errCode(err) == api.CodeConflict {
			s.Redirect(w, r, target, "runner.conflict", ref)
			return
		}
		s.backendFail(w, r, err)
		return
	}
	s.Redirect(w, r, target, "runner."+op, ref)
}
