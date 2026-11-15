package gui

// Audit screen (docs/M4.md 3.8): filtered list with keyset paging and a streamed CSV / JSON Lines
// export. Every filter and the page position live in the query string, so a view can be
// bookmarked. Paging: "Older" is a descending query with BeforeID; "Newer" is an ascending query
// with AfterID whose rows are reversed here (AuditFilter Desc+AfterID would return the newest
// rows overall, not the next newer page; G-09).

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
)

const (
	auditPageSize = 50
	// auditExportBatch is the rows per ListAudit call while exporting (the store caps a call at 1000).
	auditExportBatch = 1000
)

// Variables so tests can shrink them. auditExportTimeout is read when the routes are registered.
var (
	auditExportCap     = 100000
	auditExportTimeout = 10 * time.Minute
)

var (
	auditSources  = []string{"cli", "api", "gui", "enroll", "system"}
	auditOutcomes = []string{"ok", "denied", "error", "start", "cancelled", "timeout"}
)

func (s *Server) routesAudit(mux *http.ServeMux) {
	mux.Handle("GET /audit", s.requireAdmin(http.HandlerFunc(s.handleAudit)))
	mux.Handle("GET /audit/export", s.requireAdmin(http.HandlerFunc(s.handleAuditExport)))
	s.RouteTimeout("GET /audit/export", auditExportTimeout)
}

// auditQuery is the parsed query string: the text of every filter (to refill the form) plus the
// store filter built from it.
type auditQuery struct {
	From, To, Fleet, Runner, Actor, Source, Outcome string
	Action, Workspace, Request                      string
	Prefix                                          bool
	Before, After                                   int64
	Errs                                            map[string]string
	Filter                                          store.AuditFilter
}

// parseAuditTime parses a UTC filter time. For an upper bound (end) the value stands for its
// whole unit and the bound is the unit's last millisecond (the store compares in ms,
// inclusive): "2026-11-14" ends at 23:59:59.999, "...T09:30" at 09:30:59.999 (G-35). An RFC
// 3339 value with a fraction is taken as given.
func parseAuditTime(v string, end bool) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, v, time.UTC); err == nil {
			if end {
				switch {
				case layout == "2006-01-02":
					t = t.Add(24*time.Hour - time.Millisecond)
				case layout == "2006-01-02T15:04" || layout == "2006-01-02 15:04":
					t = t.Add(time.Minute - time.Millisecond)
				case !strings.Contains(v, "."):
					t = t.Add(time.Second - time.Millisecond)
				}
			}
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

func oneOf(v string, list []string) bool {
	for _, x := range list {
		if v == x {
			return true
		}
	}
	return false
}

func parseAuditQuery(q url.Values) auditQuery {
	get := func(k string) string { return strings.TrimSpace(q.Get(k)) }
	a := auditQuery{From: get("from"), To: get("to"), Fleet: get("fleet"), Runner: get("runner"), Actor: get("actor"),
		Source: get("source"), Outcome: get("outcome"), Action: get("action"), Workspace: get("workspace"),
		Request: get("request"), Prefix: q.Get("prefix") == "1", Errs: map[string]string{}}
	f := &a.Filter
	f.Fleet, f.Runner, f.Actor, f.Workspace, f.RequestID = a.Fleet, a.Runner, a.Actor, a.Workspace, a.Request
	f.Action = a.Action
	f.ActionPrefix = a.Action != "" && (a.Prefix || strings.HasSuffix(a.Action, "."))
	for k, v := range map[string]string{"fleet": a.Fleet, "runner": a.Runner, "actor": a.Actor, "action": a.Action,
		"workspace": a.Workspace, "request": a.Request} {
		if len(v) > 256 {
			a.Errs[k] = "Use at most 256 characters."
		}
	}
	if a.Source != "" {
		if oneOf(a.Source, auditSources) {
			f.Source = a.Source
		} else {
			a.Errs["source"] = "Pick one of the listed sources."
		}
	}
	if a.Outcome != "" {
		if oneOf(a.Outcome, auditOutcomes) {
			f.Outcome = a.Outcome
		} else {
			a.Errs["outcome"] = "Pick one of the listed outcomes."
		}
	}
	if a.From != "" {
		if t, ok := parseAuditTime(a.From, false); ok {
			f.From = t
		} else {
			a.Errs["from"] = "Use a UTC date or time such as 2026-11-14 or 2026-11-14T09:30."
		}
	}
	if a.To != "" {
		if t, ok := parseAuditTime(a.To, true); ok {
			f.To = t
		} else {
			a.Errs["to"] = "Use a UTC date or time such as 2026-11-14 or 2026-11-14T09:30."
		}
	}
	if !f.From.IsZero() && !f.To.IsZero() && f.To.Before(f.From) {
		a.Errs["to"] = "The end must not be before the start."
	}
	a.Before, _ = strconv.ParseInt(q.Get("before"), 10, 64)
	a.After, _ = strconv.ParseInt(q.Get("after"), 10, 64)
	if a.Before < 0 {
		a.Before = 0
	}
	if a.After < 0 {
		a.After = 0
	}
	if a.After > 0 {
		a.Before = 0 // "Newer" wins when both are given
	}
	return a
}

// values renders the filter (not the paging position) back into a query string.
func (a auditQuery) values() url.Values {
	v := url.Values{}
	for k, x := range map[string]string{"from": a.From, "to": a.To, "fleet": a.Fleet, "runner": a.Runner,
		"actor": a.Actor, "source": a.Source, "outcome": a.Outcome, "action": a.Action,
		"workspace": a.Workspace, "request": a.Request} {
		if x != "" {
			v.Set(k, x)
		}
	}
	if a.Prefix {
		v.Set("prefix", "1")
	}
	return v
}

func auditURL(base string, v url.Values, k string, id int64) string {
	c := url.Values{}
	for kk, vv := range v {
		c[kk] = vv
	}
	if k != "" {
		c.Set(k, strconv.FormatInt(id, 10))
	}
	if len(c) == 0 {
		return base
	}
	return base + "?" + c.Encode()
}

type auditDetail struct{ K, V string }

type auditListRow struct {
	ID            int64
	Time          timeView
	Actor, Source string
	ActorKind     string
	Action        string
	Target        string
	Badge         badgeView
	Details       []auditDetail
	Message       string
	RequestID     string
	RequestURL    string
	ExitCode, Dur string
	HasMessage    bool
	HasRequest    bool
}

type auditData struct {
	Q                      auditQuery
	Sources, Outcomes      []string
	Rows                   []auditListRow
	Pager                  struct{ NewerURL, OlderURL string }
	ExportCSV, ExportJSONL string
	Filtered               bool
	Invalid                bool
	Empty                  emptyView
	PageSize, ExportCap    int
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	a, ok := s.screenAuth(w, r)
	if !ok {
		return
	}
	q := parseAuditQuery(r.URL.Query())
	d := auditData{Q: q, Sources: auditSources, Outcomes: auditOutcomes, PageSize: auditPageSize, ExportCap: auditExportCap}
	fv := q.values()
	d.Filtered = len(fv) > 0
	d.Invalid = len(q.Errs) > 0
	ev := url.Values{}
	for k, v := range fv {
		ev[k] = v
	}
	d.ExportCSV = auditURL("/audit/export", withFormat(ev, "csv"), "", 0)
	d.ExportJSONL = auditURL("/audit/export", withFormat(ev, "jsonl"), "", 0)
	d.Empty = emptyView{Title: "Nothing recorded yet", Body: "Every change made here or from the CLI shows up in the audit log.", CLI: "mirafleet central audit"}
	if d.Filtered {
		d.Empty = emptyView{Title: "No rows match", Body: "Widen the time range or clear a filter."}
	}
	if !d.Invalid {
		evs, older, newer, err := s.auditPage(r.Context(), a, q)
		if err != nil {
			s.backendFail(w, r, err)
			return
		}
		for _, e := range evs {
			d.Rows = append(d.Rows, s.auditListRowOf(e))
		}
		if len(evs) > 0 {
			if older {
				d.Pager.OlderURL = auditURL("/audit", fv, "before", evs[len(evs)-1].ID)
			}
			if newer {
				d.Pager.NewerURL = auditURL("/audit", fv, "after", evs[0].ID)
			}
		}
	}
	pg := s.screenPage(w, r, a, "Audit", "audit")
	pg.Data = d
	s.Render(w, r, http.StatusOK, "audit", "audit/list", pg)
}

func withFormat(v url.Values, f string) url.Values {
	c := url.Values{}
	for k, x := range v {
		c[k] = x
	}
	c.Set("format", f)
	return c
}

// auditPage returns one page, newest first, and whether older and newer rows exist.
func (s *Server) auditPage(ctx context.Context, a adminReq, q auditQuery) (rows []store.AuditEvent, older, newer bool, err error) {
	be := s.cfg.Backend
	f := q.Filter
	f.Limit = auditPageSize + 1
	if q.After > 0 {
		f.AfterID, f.Desc = q.After, false
		rows, err = be.ListAudit(ctx, a.P, service.SourceGUI, f)
		if err != nil {
			return nil, false, false, err
		}
		if len(rows) > 0 {
			if newer = len(rows) > auditPageSize; newer {
				rows = rows[:auditPageSize]
			}
			for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
				rows[i], rows[j] = rows[j], rows[i]
			}
			return rows, true, newer, nil
		}
		// Nothing newer: fall through to the first page.
	}
	f.AfterID, f.Desc, f.BeforeID = 0, true, q.Before
	rows, err = be.ListAudit(ctx, a.P, service.SourceGUI, f)
	if err != nil {
		return nil, false, false, err
	}
	if older = len(rows) > auditPageSize; older {
		rows = rows[:auditPageSize]
	}
	return rows, older, q.Before > 0, nil
}

func (s *Server) auditListRowOf(e store.AuditEvent) auditListRow {
	base := s.auditRows([]store.AuditEvent{e})[0]
	row := auditListRow{ID: e.ID, Time: base.Time, Actor: e.Actor, Source: e.Source, ActorKind: e.ActorKind,
		Action: e.Action, Target: base.Target, Badge: base.Badge, Message: e.Message, HasMessage: e.Message != "",
		RequestID: e.RequestID, HasRequest: e.RequestID != "", Dur: strconv.FormatInt(e.DurationMS, 10) + " ms"}
	if e.RequestID != "" {
		row.RequestURL = "/audit?request=" + QueryEscape(e.RequestID)
	}
	if e.ExitCode != nil {
		row.ExitCode = strconv.Itoa(*e.ExitCode)
	}
	add := func(k, v string) {
		if v != "" {
			row.Details = append(row.Details, auditDetail{k, v})
		}
	}
	add("Fleet", e.Fleet)
	add("Runner", e.Runner)
	add("Subject", e.Subject)
	add("Workspace", e.Workspace)
	add("Remote", e.Remote)
	for _, k := range sortedKeys(e.Params) {
		add("param "+k, e.Params[k])
	}
	return row
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	for i := 1; i < len(ks); i++ {
		for j := i; j > 0 && ks[j] < ks[j-1]; j-- {
			ks[j], ks[j-1] = ks[j-1], ks[j]
		}
	}
	return ks
}

// ---- export ----

var auditCSVHeader = []string{"id", "time", "source", "actor_kind", "actor", "remote", "fleet", "runner",
	"subject", "action", "params", "outcome", "exit_code", "duration_ms", "message", "request_id", "workspace"}

// csvCell neutralises spreadsheet formulas: a cell starting with = + - @ tab or CR gets a
// leading apostrophe.
func csvCell(v string) string {
	if v != "" {
		switch v[0] {
		case '=', '+', '-', '@', '\t', '\r':
			return "'" + v
		}
	}
	return v
}

func auditCSVRecord(e store.AuditEvent) []string {
	params := ""
	if len(e.Params) > 0 {
		b, _ := json.Marshal(e.Params) // map[string]string: keys sorted, cannot fail
		params = string(b)
	}
	exit := ""
	if e.ExitCode != nil {
		exit = strconv.Itoa(*e.ExitCode)
	}
	return []string{strconv.FormatInt(e.ID, 10), e.Time.UTC().Format(time.RFC3339Nano), csvCell(e.Source),
		csvCell(e.ActorKind), csvCell(e.Actor), csvCell(e.Remote), csvCell(e.Fleet), csvCell(e.Runner),
		csvCell(e.Subject), csvCell(e.Action), csvCell(params), csvCell(e.Outcome), exit,
		strconv.FormatInt(e.DurationMS, 10), csvCell(e.Message), csvCell(e.RequestID), csvCell(e.Workspace)}
}

type auditJSONLine struct {
	ID         int64             `json:"id"`
	Time       string            `json:"time"`
	Source     string            `json:"source"`
	ActorKind  string            `json:"actor_kind"`
	Actor      string            `json:"actor"`
	Remote     string            `json:"remote,omitempty"`
	Fleet      string            `json:"fleet,omitempty"`
	Runner     string            `json:"runner,omitempty"`
	Subject    string            `json:"subject,omitempty"`
	Action     string            `json:"action"`
	Params     map[string]string `json:"params,omitempty"`
	Outcome    string            `json:"outcome"`
	ExitCode   *int              `json:"exit_code,omitempty"`
	DurationMS int64             `json:"duration_ms"`
	Message    string            `json:"message,omitempty"`
	RequestID  string            `json:"request_id,omitempty"`
	Workspace  string            `json:"workspace,omitempty"`
}

func (s *Server) handleAuditExport(w http.ResponseWriter, r *http.Request) {
	a, ok := s.screenAuth(w, r)
	if !ok {
		return
	}
	// The export is a GET with a side effect (an audit row) and a download: a cross-site trigger
	// (an <img>, a link) is refused before anything is recorded. Browsers send Sec-Fetch-Site;
	// same-origin and none (typed or bookmarked URL) are the only fetches we serve.
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
	default:
		s.ErrorPage(w, r, http.StatusForbidden)
		return
	}
	format := r.URL.Query().Get("format")
	if format != "csv" && format != "jsonl" {
		s.ErrorPage(w, r, http.StatusNotFound)
		return
	}
	q := parseAuditQuery(r.URL.Query())
	if len(q.Errs) > 0 {
		s.ErrorPage(w, r, http.StatusNotFound)
		return
	}
	f := q.Filter
	ctx, be := r.Context(), s.cfg.Backend

	// The audit.export row goes in first; if it fails nothing streams.
	if err := be.RecordAuditExport(ctx, a.P, service.SourceGUI, f, format); err != nil {
		s.backendFail(w, r, err)
		return
	}

	// The default write deadline is shorter than an export may take; follow the route timeout.
	rc := http.NewResponseController(w)
	if dl, has := ctx.Deadline(); has {
		_ = rc.SetWriteDeadline(dl.Add(10 * time.Second))
	}
	h := w.Header()
	name := "mirafleet-audit-" + s.now().UTC().Format("20060102T150405Z") + "." + format
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Disposition", `attachment; filename="`+name+`"`)
	h.Set("X-Content-Type-Options", "nosniff")
	if format == "csv" {
		h.Set("Content-Type", "text/csv; charset=utf-8")
	} else {
		h.Set("Content-Type", "application/x-ndjson; charset=utf-8")
	}
	w.WriteHeader(http.StatusOK)

	bw := bufio.NewWriterSize(w, 32<<10)
	var cw *csv.Writer
	var enc *json.Encoder
	if format == "csv" {
		cw = csv.NewWriter(bw)
		_ = cw.Write(auditCSVHeader)
	} else {
		enc = json.NewEncoder(bw) // one compact object and a newline per call
	}
	marker := func(reason, msg string) {
		if cw != nil {
			_ = cw.Write([]string{"# export " + reason + ": " + msg})
		} else {
			_ = enc.Encode(map[string]any{"truncated": true, "reason": reason, "message": msg, "limit": auditExportCap})
		}
	}

	f.Desc, f.Limit, f.AfterID, f.BeforeID = false, auditExportBatch, 0, 0
	written := 0
loop:
	for {
		if err := ctx.Err(); err != nil {
			marker("timeout", "stopped before the end; narrow the filter and export again")
			break
		}
		evs, err := be.ListAudit(ctx, a.P, service.SourceGUI, f)
		if err != nil {
			if ctx.Err() != nil {
				marker("timeout", "stopped before the end; narrow the filter and export again")
			} else {
				s.logf("gui: audit export req=%s: %v", RequestID(ctx), err)
				marker("error", "stopped early because of a server error")
			}
			break
		}
		for _, e := range evs {
			if written >= auditExportCap {
				marker("cut", "stopped after "+strconv.Itoa(auditExportCap)+" rows; narrow the filter or the time range")
				break loop
			}
			if cw != nil {
				_ = cw.Write(auditCSVRecord(e))
			} else {
				_ = enc.Encode(auditJSONLine{ID: e.ID, Time: e.Time.UTC().Format(time.RFC3339Nano), Source: e.Source,
					ActorKind: e.ActorKind, Actor: e.Actor, Remote: e.Remote, Fleet: e.Fleet, Runner: e.Runner,
					Subject: e.Subject, Action: e.Action, Params: e.Params, Outcome: e.Outcome, ExitCode: e.ExitCode,
					DurationMS: e.DurationMS, Message: e.Message, RequestID: e.RequestID, Workspace: e.Workspace})
			}
			written++
			f.AfterID = e.ID
		}
		if len(evs) < auditExportBatch {
			break
		}
	}
	if cw != nil {
		cw.Flush()
	}
	_ = bw.Flush()
}
