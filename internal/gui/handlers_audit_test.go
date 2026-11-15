package gui

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
)

var auditT0 = time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)

func seedAudit(t *testing.T, e *guiHarness, evs ...store.AuditEvent) {
	t.Helper()
	for _, ev := range evs {
		if ev.Source == "" {
			ev.Source = "cli"
		}
		if ev.ActorKind == "" {
			ev.ActorKind = "admin"
		}
		if ev.Actor == "" {
			ev.Actor = "seed"
		}
		if ev.Outcome == "" {
			ev.Outcome = "ok"
		}
		if _, err := e.db.RecordAudit(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
	}
}

func hasAction(body, action string) bool { return strings.Contains(body, ">"+action+"<") }

func TestAuditRequiresLogin(t *testing.T) {
	e := newGUIHarness(t)
	loginForTest(t, e)
	rec := e.newClient().get("/audit")
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/login") {
		t.Fatalf("anonymous /audit: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	rec = e.newClient().get("/audit/export?format=csv")
	if rec.Code != http.StatusSeeOther || strings.Contains(rec.Body.String(), "mirafleet-audit") {
		t.Fatalf("anonymous export: %d", rec.Code)
	}
}

func TestAuditEveryFilter(t *testing.T) {
	e := newGUIHarness(t)
	c := loginForTest(t, e)
	seedAudit(t, e,
		store.AuditEvent{Time: auditT0, Action: "f.one", Fleet: "fa", Runner: "ra", Actor: "alice", Source: "cli", Outcome: "ok", RequestID: "req-1"},
		store.AuditEvent{Time: auditT0.Add(time.Hour), Action: "f.two", Fleet: "fb", Runner: "rb", Actor: "bob", Source: "api", Outcome: "denied", Workspace: "ws-9", RequestID: "req-2"},
		store.AuditEvent{Time: auditT0.Add(48 * time.Hour), Action: "g.three", Fleet: "fa", Runner: "rb", Actor: "alice", Source: "gui", Outcome: "error", RequestID: "req-3"},
		store.AuditEvent{Time: auditT0.Add(72 * time.Hour), Action: "g.four", Fleet: "fc", Actor: "carol", Source: "enroll", Outcome: "timeout", RequestID: "req-4"},
		store.AuditEvent{Time: auditT0.Add(96 * time.Hour), Action: "h.five", Actor: "dave", Source: "system", Outcome: "cancelled", RequestID: "req-5"},
	)
	all := []string{"f.one", "f.two", "g.three", "g.four", "h.five"}
	cases := []struct {
		q    string
		want []string
	}{
		{"fleet=fa", []string{"f.one", "g.three"}},
		{"runner=rb", []string{"f.two", "g.three"}},
		{"actor=alice", []string{"f.one", "g.three"}},
		{"source=api", []string{"f.two"}},
		{"source=enroll", []string{"g.four"}},
		{"outcome=denied", []string{"f.two"}},
		{"outcome=timeout", []string{"g.four"}},
		{"action=g.three", []string{"g.three"}},
		{"action=g.", []string{"g.three", "g.four"}},         // trailing dot = prefix
		{"action=g&prefix=1", []string{"g.three", "g.four"}}, // checkbox
		{"action=g", nil},                                    // exact, no match
		{"workspace=ws-9", []string{"f.two"}},
		{"request=req-5", []string{"h.five"}},
		{"from=2026-03-03", []string{"g.three", "g.four", "h.five"}},
		{"to=2026-03-01", []string{"f.one", "f.two"}}, // a date-only "to" covers that whole day
		{"from=2026-03-01T10:30&to=2026-03-04T10:00", []string{"f.two", "g.three", "g.four"}},
		{"fleet=fa&actor=alice&outcome=error", []string{"g.three"}},
	}
	for _, tc := range cases {
		body := c.get("/audit?" + tc.q).Body.String()
		for _, a := range all {
			want := false
			for _, w := range tc.want {
				want = want || w == a
			}
			if got := hasAction(body, a); got != want {
				t.Errorf("?%s: action %s shown=%v want %v", tc.q, a, got, want)
			}
		}
	}
	// Invalid filters show an error and no rows.
	for _, q := range []string{"source=bogus", "outcome=bogus", "from=yesterday", "to=nope", "from=2026-03-05&to=2026-03-01"} {
		rec := c.get("/audit?" + q)
		b := rec.Body.String()
		if rec.Code != 200 || !strings.Contains(b, "Fix the filter") || hasAction(b, "f.one") {
			t.Errorf("?%s: code %d, body lacks error or shows rows", q, rec.Code)
		}
	}
}

func TestAuditBookmarkableQuery(t *testing.T) {
	e := newGUIHarness(t)
	c := loginForTest(t, e)
	seedAudit(t, e, store.AuditEvent{Time: auditT0, Action: "bm.one", Fleet: "fa", Actor: "alice", RequestID: "req-bm"})
	q := "/audit?action=bm.&actor=alice&fleet=fa&from=2026-03-01&outcome=ok&request=req-bm&source=cli&to=2026-03-02&workspace=w1"
	body := c.get(q).Body.String()
	for _, want := range []string{`value="bm."`, `value="alice"`, `value="fa"`, `value="2026-03-01"`, `value="req-bm"`, `value="2026-03-02"`,
		`value="cli" selected`, `value="ok" selected`, `value="w1"`} {
		if !strings.Contains(body, want) {
			t.Errorf("form not refilled: missing %s", want)
		}
	}
	// The export links carry exactly the filter.
	m := regexp.MustCompile(`href="(/audit/export\?[^"]*format=csv[^"]*)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no csv export link")
	}
	link := html.UnescapeString(m[1])
	for _, want := range []string{"action=bm.", "actor=alice", "fleet=fa", "request=req-bm", "source=cli", "workspace=w1", "format=csv"} {
		if !strings.Contains(link, want) {
			t.Errorf("export link %q lacks %s", link, want)
		}
	}
	// A GET with the same query string renders the same rows (no state outside the URL).
	other := loginForTest(t, e)
	strip := func(s string) string {
		return regexp.MustCompile(`name="_csrf" value="[^"]*"|X-CSRF-Token":"[^"]*"|datetime="[^"]*"|title="[^"]*"|>[^<]*ago<|request [0-9a-f]{8,}|Request ID <span class="mono">[0-9a-f]+`).ReplaceAllString(s, "")
	}
	a, b := strip(other.get(q).Body.String()), strip(body)
	if a != b {
		i := 0
		for i < len(a) && i < len(b) && a[i] == b[i] {
			i++
		}
		t.Errorf("same query string gave a different page near %q vs %q", a[max(0, i-60):min(len(a), i+60)], b[max(0, i-60):min(len(b), i+60)])
	}
}

func pagerLink(body, label string) string {
	m := regexp.MustCompile(`<a class="btn btn-quiet btn-sm" href="([^"]+)">` + label + `</a>`).FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return html.UnescapeString(m[1])
}

func pageActions(body string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`<td class="mono">(page\.\d+)</td>`).FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}

func TestAuditKeysetPagingBothWays(t *testing.T) {
	e := newGUIHarness(t)
	c := loginForTest(t, e)
	for i := 1; i <= 120; i++ {
		seedAudit(t, e, store.AuditEvent{Time: auditT0.Add(time.Duration(i) * time.Minute), Action: fmt.Sprintf("page.%d", i)})
	}
	seq := func(from, to int) []string { // from down to to
		var o []string
		for i := from; i >= to; i-- {
			o = append(o, fmt.Sprintf("page.%d", i))
		}
		return o
	}
	eq := func(name string, got, want []string) {
		t.Helper()
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: got %v want %v", name, got, want)
		}
	}
	p1 := c.get("/audit?action=page.").Body.String()
	eq("page 1", pageActions(p1), seq(120, 71))
	if pagerLink(p1, "Newer") != "" || pagerLink(p1, "Older") == "" {
		t.Fatal("page 1 pager wrong")
	}
	p2 := c.get(pagerLink(p1, "Older")).Body.String()
	eq("page 2", pageActions(p2), seq(70, 21))
	if pagerLink(p2, "Newer") == "" || pagerLink(p2, "Older") == "" {
		t.Fatal("page 2 pager wrong")
	}
	p3 := c.get(pagerLink(p2, "Older")).Body.String()
	eq("page 3", pageActions(p3), seq(20, 1))
	if pagerLink(p3, "Older") != "" || pagerLink(p3, "Newer") == "" {
		t.Fatal("page 3 pager wrong")
	}
	// Newer, back up: ascending query reversed, so the order is still newest first.
	n2 := c.get(pagerLink(p3, "Newer")).Body.String()
	eq("newer to page 2", pageActions(n2), seq(70, 21))
	if pagerLink(n2, "Older") == "" || pagerLink(n2, "Newer") == "" {
		t.Fatal("newer page 2 pager wrong")
	}
	n1 := c.get(pagerLink(n2, "Newer")).Body.String()
	eq("newer to page 1", pageActions(n1), seq(120, 71))
	if pagerLink(n1, "Newer") != "" {
		t.Error("page 1 reached by Newer still offers Newer")
	}
	// The paging links keep the filter.
	if !strings.Contains(pagerLink(p1, "Older"), "action=page.") {
		t.Error("Older link lost the filter")
	}
	// An after id with nothing newer falls back to the first page.
	eq("after the end", pageActions(c.get("/audit?action=page.&after=99999999").Body.String()), seq(120, 71))
}

func TestAuditExportCSV(t *testing.T) {
	e := newGUIHarness(t)
	c := loginForTest(t, e)
	exit := -1
	seedAudit(t, e,
		store.AuditEvent{Time: auditT0, Action: "csv.a", Actor: "=HYPERLINK(\"x\")", Fleet: "+1", Runner: "-2", Subject: "@s",
			Message: "\tleading tab", RequestID: "\rcr", Remote: "ok,with \"quotes\"", ExitCode: &exit,
			Params: map[string]string{"k": "=v"}},
		store.AuditEvent{Time: auditT0.Add(time.Second), Action: "csv.b", Message: "line1\nline2"},
	)
	rec := c.get("/audit/export?format=csv&action=csv.")
	if rec.Code != 200 {
		t.Fatalf("status %d: %.200s", rec.Code, rec.Body.String())
	}
	h := rec.Header()
	if h.Get("Cache-Control") != "no-store" || !strings.HasPrefix(h.Get("Content-Disposition"), `attachment; filename="mirafleet-audit-`) ||
		!strings.HasSuffix(h.Get("Content-Disposition"), `.csv"`) || !strings.HasPrefix(h.Get("Content-Type"), "text/csv") {
		t.Errorf("headers: %v", h)
	}
	recs, err := csv.NewReader(rec.Body).ReadAll()
	if err != nil {
		t.Fatalf("csv: %v", err)
	}
	if len(recs) != 3 || recs[0][0] != "id" || recs[1][9] != "csv.a" || recs[2][9] != "csv.b" {
		t.Fatalf("rows (oldest first): %v", recs)
	}
	r := recs[1]
	for i, want := range map[int]string{4: `'=HYPERLINK("x")`, 6: "'+1", 7: "'-2", 8: "'@s", 14: "'\tleading tab", 15: "'\rcr", 5: `ok,with "quotes"`, 12: "-1"} {
		if r[i] != want {
			t.Errorf("cell %d = %q want %q", i, r[i], want)
		}
	}
	if r[10] != `{"k":"=v"}` {
		t.Errorf("params %q", r[10])
	}
	if recs[2][14] != "line1\nline2" {
		t.Errorf("newline cell %q", recs[2][14])
	}
	for _, rr := range recs[1:] {
		for _, cell := range rr {
			if cell != "" && strings.ContainsRune("=+@\t\r", rune(cell[0])) {
				t.Errorf("unguarded cell %q", cell)
			}
		}
	}
	if rec := c.get("/audit/export?format=xml"); rec.Code != 404 {
		t.Errorf("bad format: %d", rec.Code)
	}
	if rec := c.get("/audit/export?format=csv&source=bogus"); rec.Code != 404 {
		t.Errorf("bad filter: %d", rec.Code)
	}
}

func TestAuditExportJSONLValidPerLine(t *testing.T) {
	e := newGUIHarness(t)
	c := loginForTest(t, e)
	msg := "has \"quotes\"\nand newline, " + string(rune(0x2028)) + " sep"
	seedAudit(t, e,
		store.AuditEvent{Time: auditT0, Action: "jl.a", Message: msg, Params: map[string]string{"p": "=x"}},
		store.AuditEvent{Time: auditT0.Add(time.Second), Action: "jl.b", Actor: "=not-guarded-in-json"},
	)
	rec := c.get("/audit/export?format=jsonl&action=jl.")
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/x-ndjson") ||
		!strings.HasSuffix(rec.Header().Get("Content-Disposition"), `.jsonl"`) || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status %d headers %v", rec.Code, rec.Header())
	}
	lines := strings.Split(strings.TrimSuffix(rec.Body.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines: %q", lines)
	}
	var first, second map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("line 0: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("line 1: %v", err)
	}
	if first["action"] != "jl.a" || second["action"] != "jl.b" || first["message"] != msg {
		t.Errorf("content: %v / %v", first, second)
	}
}

func TestAuditExportRowWrittenAndFilterRecorded(t *testing.T) {
	e := newGUIHarness(t)
	c := loginForTest(t, e)
	seedAudit(t, e, store.AuditEvent{Time: auditT0, Action: "rec.a", Fleet: "fx"})
	if rec := c.get("/audit/export?format=jsonl&fleet=fx&source=cli"); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	evs, err := e.svc.ListAudit(context.Background(), auth.Principal{Kind: auth.KindAdmin, Name: testUser}, service.SourceGUI, store.AuditFilter{Action: "audit.export"})
	if err != nil || len(evs) != 1 {
		t.Fatalf("audit.export rows: %v %v", evs, err)
	}
	p := evs[0].Params
	if p["format"] != "jsonl" || p["fleet"] != "fx" || p["source"] != "cli" || evs[0].Source != "gui" {
		t.Errorf("audit.export row: %+v", evs[0])
	}
}

// A cross-site fetch of the export (an <img>, a link on another site) is refused before the
// audit.export row is written; same-origin, none (typed URL) and an absent header are served.
func TestAuditExportRefusesCrossSite(t *testing.T) {
	e := newGUIHarness(t)
	c := loginForTest(t, e)
	seedAudit(t, e, store.AuditEvent{Time: auditT0, Action: "cs.a"})
	for _, site := range []string{"cross-site", "same-site", "bogus"} {
		rec := c.do("GET", "/audit/export?format=csv", nil, func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", site) })
		if rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), "cs.a") {
			t.Errorf("Sec-Fetch-Site %q: status %d", site, rec.Code)
		}
	}
	if n := e.auditCount("audit.export"); n != 0 {
		t.Fatalf("refused exports wrote %d audit.export rows", n)
	}
	for _, site := range []string{"same-origin", "none", ""} {
		rec := c.do("GET", "/audit/export?format=csv", nil, func(r *http.Request) {
			if site != "" {
				r.Header.Set("Sec-Fetch-Site", site)
			}
		})
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "cs.a") {
			t.Errorf("Sec-Fetch-Site %q: status %d", site, rec.Code)
		}
	}
	if n := e.auditCount("audit.export"); n != 3 {
		t.Errorf("audit.export rows = %d, want 3", n)
	}
}

// ---- fake backend: ordering, the 100k cap and the timeout ----

type auditFake struct {
	Backend
	mu         sync.Mutex
	calls      []string
	total      int64
	blockAfter int // block ListAudit (until the context ends) from this call on; 0 = never
	failExport bool
	listN      int
}

func (f *auditFake) RecordAuditExport(ctx context.Context, p auth.Principal, src service.Source, fl store.AuditFilter, format string) error {
	f.mu.Lock()
	f.calls = append(f.calls, "record")
	f.mu.Unlock()
	if f.failExport {
		return fmt.Errorf("disk full")
	}
	return nil
}

func (f *auditFake) ListAudit(ctx context.Context, p auth.Principal, src service.Source, fl store.AuditFilter) ([]store.AuditEvent, error) {
	f.mu.Lock()
	f.calls = append(f.calls, "list")
	f.listN++
	n := f.listN
	f.mu.Unlock()
	if f.blockAfter > 0 && n > f.blockAfter {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if fl.Limit > 1000 {
		return nil, fmt.Errorf("limit %d", fl.Limit)
	}
	var out []store.AuditEvent
	for id := fl.AfterID + 1; id <= f.total && len(out) < fl.Limit; id++ {
		out = append(out, store.AuditEvent{ID: id, Time: auditT0, Source: "cli", ActorKind: "admin", Actor: "a", Action: "x.y", Outcome: "ok"})
	}
	return out, nil
}

func (f *auditFake) order() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, ",")
}

// fakeServer builds a second GUI server over fake, sharing e's sessions through the real service.
func fakeServer(t *testing.T, e *guiHarness, fake *auditFake) *Server {
	t.Helper()
	fake.Backend = e.svc
	return e.newServer(fake, t.TempDir(), nil)
}

func fakeGet(s *Server, c *testClient, target string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", target, nil)
	r.Host, r.RemoteAddr = testHost, c.remote
	r.Header.Set("User-Agent", "TestBrowser/1.0")
	for k, v := range c.jar {
		r.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	return rec
}

func TestAuditExportRowBeforeData(t *testing.T) {
	e := newGUIHarness(t)
	c := loginForTest(t, e)
	fake := &auditFake{total: 2500}
	rec := fakeGet(fakeServer(t, e, fake), c, "/audit/export?format=csv")
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if got := fake.order(); !strings.HasPrefix(got, "record,list") || strings.Count(got, "record") != 1 || strings.Count(got, "list") != 3 {
		t.Errorf("call order %q, want record first, once, then 3 list batches", got)
	}
	recs, _ := csv.NewReader(rec.Body).ReadAll()
	if len(recs) != 2501 {
		t.Errorf("rows %d, want 2501 with header", len(recs))
	}
	// If recording the export fails nothing streams.
	fake = &auditFake{total: 10, failExport: true}
	rec = fakeGet(fakeServer(t, e, fake), c, "/audit/export?format=csv")
	if rec.Code != 500 || strings.Contains(rec.Body.String(), "x.y") || strings.Contains(fake.order(), "list") {
		t.Errorf("failed record: %d order %q", rec.Code, fake.order())
	}
}

func TestAuditExport100kCapMarker(t *testing.T) {
	e := newGUIHarness(t)
	c := loginForTest(t, e)
	fake := &auditFake{total: int64(auditExportCap) + 7}
	s := fakeServer(t, e, fake)
	rec := fakeGet(s, c, "/audit/export?format=csv")
	cr := csv.NewReader(rec.Body)
	cr.FieldsPerRecord = -1 // the marker record has one field
	recs, err := cr.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != auditExportCap+2 { // header + cap rows + marker
		t.Fatalf("records %d, want %d", len(recs), auditExportCap+2)
	}
	if last := recs[len(recs)-1]; len(last) != 1 || !strings.HasPrefix(last[0], "# export cut:") || !strings.Contains(last[0], "100000") {
		t.Errorf("marker %v", last)
	}
	rec = fakeGet(s, c, "/audit/export?format=jsonl")
	lines := strings.Split(strings.TrimSuffix(rec.Body.String(), "\n"), "\n")
	if len(lines) != auditExportCap+1 {
		t.Fatalf("jsonl lines %d", len(lines))
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &m); err != nil || m["truncated"] != true || m["reason"] != "cut" {
		t.Errorf("marker %q %v", lines[len(lines)-1], err)
	}
	// Exactly the cap is not cut: no marker.
	fake = &auditFake{total: int64(auditExportCap)}
	rec = fakeGet(fakeServer(t, e, fake), c, "/audit/export?format=jsonl")
	if strings.Contains(rec.Body.String(), "truncated") {
		t.Error("marker on an export of exactly the cap")
	}
}

func TestAuditExportTimeout(t *testing.T) {
	old := auditExportTimeout
	auditExportTimeout = 300 * time.Millisecond
	t.Cleanup(func() { auditExportTimeout = old })
	e := newGUIHarness(t)
	c := loginForTest(t, e)
	fake := &auditFake{total: 5000, blockAfter: 1}
	s := fakeServer(t, e, fake)
	start := time.Now()
	rec := fakeGet(s, c, "/audit/export?format=jsonl")
	if time.Since(start) > 5*time.Second {
		t.Fatal("export did not stop at its timeout")
	}
	lines := strings.Split(strings.TrimSuffix(rec.Body.String(), "\n"), "\n")
	if len(lines) != 1001 {
		t.Fatalf("lines %d, want 1000 rows + marker", len(lines))
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(lines[1000]), &m); err != nil || m["reason"] != "timeout" || m["truncated"] != true {
		t.Errorf("marker %q %v", lines[1000], err)
	}
	// The route uses the export timeout, not the 15 s default.
	if s.timeouts["GET /audit/export"] != 300*time.Millisecond {
		t.Errorf("route timeout %v", s.timeouts["GET /audit/export"])
	}
}
