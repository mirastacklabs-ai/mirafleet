package gui

// Fleets screens (docs/M4.md 3.3): list, create (both modes), detail, workspace config edit and
// the guard toggle. Also home of the small helpers the overview shares (screenAuth, screenPage,
// backendFail, relAge).

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/units"
)

func init() {
	RegisterFlash("fleet.created", "ok", "Fleet %s created.")
	RegisterFlash("fleet.ws_updated", "ok", "Workspace settings of fleet %s saved.")
	RegisterFlash("fleet.guard_on", "ok", "Guard of fleet %s is on.")
	RegisterFlash("fleet.guard_off", "warn", "Guard of fleet %s is off.")
}

// screenCSRFChecker is the shared layer-1 and layer-2 CSRF checker for these screens.
var screenCSRFChecker = NewCSRF()

// adminReq is the authenticated admin of a request plus the CSRF key of its session.
type adminReq struct {
	P    auth.Principal
	Form string // the synchronizer token for forms
	Key  []byte
}

// screenAuth returns the signed-in admin. Without a valid session it redirects to /login and
// returns false. It reuses a session an outer middleware already established.
func (s *Server) screenAuth(w http.ResponseWriter, r *http.Request) (adminReq, bool) {
	// The route is wrapped in requireAdmin, which authenticated the session; a handler reached
	// without it fails closed.
	p, _, have := SessionFrom(r.Context())
	tok, hasTok := SessionToken(r)
	if !have || !hasTok {
		s.loginRedirect(w, r)
		return adminReq{}, false
	}
	if p.Kind != auth.KindAdmin {
		s.ErrorPage(w, r, http.StatusForbidden)
		return adminReq{}, false
	}
	key := CSRFKey(tok)
	return adminReq{P: p, Key: key, Form: FormToken(key)}, true
}

// screenForm parses a POST body and checks CSRF (after ParseForm, as csrf.go requires). It
// answers the error itself and returns false.
func (s *Server) screenForm(w http.ResponseWriter, r *http.Request, a adminReq) (url.Values, bool) {
	form, st := ParseForm(w, r)
	if st != 0 {
		s.ErrorPage(w, r, st)
		return nil, false
	}
	if err := screenCSRFChecker.Check(r, a.Key); err != nil {
		s.ErrorPage(w, r, http.StatusForbidden)
		return nil, false
	}
	return form, true
}

// screenPage builds the Page for an authenticated screen.
func (s *Server) screenPage(w http.ResponseWriter, r *http.Request, a adminReq, title, nav string, crumbs ...Crumb) Page {
	pg := s.Page(w, r, title, nav)
	pg.Authed, pg.CSRF, pg.Crumbs = true, a.Form, crumbs
	return pg
}

// backendFail answers a failed backend call: 404 and 403 as such, anything else as a logged 500.
func (s *Server) backendFail(w http.ResponseWriter, r *http.Request, err error) {
	var ae *api.Error
	if errors.As(err, &ae) {
		switch ae.Code {
		case api.CodeNotFound:
			s.ErrorPage(w, r, http.StatusNotFound)
			return
		case api.CodeForbidden:
			s.ErrorPage(w, r, http.StatusForbidden)
			return
		}
	}
	s.logf("gui: backend: %v", err)
	s.ErrorPage(w, r, http.StatusInternalServerError)
}

func errCode(err error) string {
	var ae *api.Error
	if errors.As(err, &ae) {
		return ae.Code
	}
	return ""
}

// timeView feeds the ui/time component.
type timeView struct{ ISO, Text string }

func (s *Server) timeOf(t time.Time) timeView {
	if t.IsZero() {
		return timeView{Text: "never"}
	}
	return timeView{ISO: t.UTC().Format(time.RFC3339), Text: relAge(s.now(), t)}
}

// relAge renders a coarse relative time ("3m ago", "in 2h").
func relAge(now, t time.Time) string {
	d := now.Sub(t)
	suffix := " ago"
	if d < 0 {
		d, suffix = -d, ""
	}
	var n string
	switch {
	case d < time.Minute:
		n = fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		n = fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 48*time.Hour:
		n = fmt.Sprintf("%dh", int(d/time.Hour))
	default:
		n = fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
	if suffix == "" {
		return "in " + n
	}
	return n + suffix
}

func fleetURL(name string) string { return "/fleets/" + PathEscape(name) }

func (s *Server) routesFleets(mux adminMux) {
	mux.HandleFunc("GET /fleets", s.fleetsList)
	mux.HandleFunc("GET /fleets/new", s.fleetNewForm)
	mux.HandleFunc("POST /fleets", s.fleetCreate)
	mux.HandleFunc("GET /fleets/{fleet}", s.fleetDetail)
	mux.HandleFunc("GET /fleets/{fleet}/workspace", s.fleetWSForm)
	mux.HandleFunc("POST /fleets/{fleet}/workspace", s.fleetWSSave)
	mux.HandleFunc("GET /fleets/{fleet}/guard", s.fleetGuardForm)
	mux.HandleFunc("POST /fleets/{fleet}/guard", s.fleetGuardSave)
}

// ---- view models ----

type fleetRow struct {
	Name, URL, Mode string
	Guarded         bool
	Runners         int
	Workspace       string // summary, "" for actions fleets
}

type formField struct {
	Key, Label, Value, Hint, Err string
	Kind                         string // text | select | textarea
	Options                      []string
}

type fleetsData struct {
	View string // list | new | detail | wsedit | guard

	Rows []fleetRow // list

	// new fleet form
	Name    string
	Mode    string
	Guarded bool
	Errs    map[string]string
	Fields  []formField
	Action  string // form action for new, wsedit and guard

	// detail, wsedit and guard
	Fleet     string
	FleetURL  string
	Created   timeView
	Runners   []runnerRow
	Tokens    []tokenRow
	WS        []kvRow
	HasWS     bool
	GuardOn   bool
	TurnOff   bool   // guard dialog: turning off (needs the name)
	Typed     string // guard dialog: what was typed
	NoRunners bool
}

type runnerRow struct {
	Name, URL, Status, Kind string
	Online                  bool
	Age                     string
}

type tokenRow struct {
	ID, Uses, State, Kind string
	Auto                  bool
	Expires               timeView
}

type kvRow struct{ K, V string }

func wsSummary(c *store.WorkspaceConfig) string {
	if c == nil {
		return ""
	}
	return fmt.Sprintf("%s, %s, network %s", c.Executor, c.DefaultImage, c.Network)
}

func statusKind(st store.Status) string {
	switch st {
	case store.StatusActive:
		return "ok"
	case store.StatusPending:
		return "warn"
	case store.StatusDisabled:
		return "off"
	}
	return "bad"
}

func capital(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// ---- workspace form model ----

// wsForm holds the workspace fields as typed text, so a rejected form is shown back as typed.
type wsForm struct {
	Executor, Image, Images, Pull, Network, Egress, CPUs, Memory, Pids, Tmpfs, Disk string
	IdleTTL, MaxTTL, ExecDefault, ExecMax                                           string
}

func wsFormFromConfig(c store.WorkspaceConfig) wsForm {
	return wsForm{
		Executor: string(c.Executor), Image: c.DefaultImage, Images: strings.Join(c.Images, "\n"),
		Pull: string(c.Pull), Network: string(c.Network), Egress: strings.Join(c.Egress, "\n"),
		CPUs: units.FormatCPUMilli(c.CPUMilli), Memory: units.FormatSizeMB(c.MemoryMB),
		Pids: strconv.Itoa(c.Pids), Tmpfs: units.FormatSizeMB(c.TmpfsMB), Disk: units.FormatSizeMB(c.DiskMB),
		IdleTTL: units.FormatDuration(c.IdleTTL), MaxTTL: units.FormatDuration(c.MaxTTL),
		ExecDefault: units.FormatDuration(c.ExecDefault), ExecMax: units.FormatDuration(c.ExecMax),
	}
}

func defaultWSForm() wsForm {
	return wsFormFromConfig(service.WorkspaceDefaults(store.WorkspaceConfig{}))
}

func wsFormFromValues(v url.Values) wsForm {
	g := func(k string) string { return strings.TrimSpace(v.Get(k)) }
	return wsForm{Executor: g("executor"), Image: g("image"), Images: v.Get("images"), Pull: g("pull"),
		Network: g("network"), Egress: v.Get("egress"), CPUs: g("cpus"), Memory: g("memory"), Pids: g("pids"),
		Tmpfs: g("tmpfs"), Disk: g("disk"), IdleTTL: g("idle_ttl"), MaxTTL: g("max_ttl"),
		ExecDefault: g("exec_default"), ExecMax: g("exec_max")}
}

func splitLines(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == '\r' || r == ',' }) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parse converts typed text to a config (blank numeric fields stay zero and take the default
// later) and returns per-field errors.
func (w wsForm) parse() (store.WorkspaceConfig, map[string]string) {
	errs := map[string]string{}
	c := store.WorkspaceConfig{
		Executor: store.Executor(w.Executor), DefaultImage: w.Image, Images: splitLines(w.Images),
		Pull: store.PullPolicy(w.Pull), Network: store.NetworkMode(w.Network), Egress: splitLines(w.Egress),
	}
	if w.Executor != "" && !c.Executor.Valid() {
		errs["executor"] = "Choose containerd or opensandbox."
	}
	if w.Pull != "" && !c.Pull.Valid() {
		errs["pull"] = "Choose if-not-present, always or never."
	}
	if w.Network != "" && !c.Network.Valid() {
		errs["network"] = "Choose none or egress."
	}
	if w.Image == "" {
		errs["image"] = "A default image is required."
	}
	if w.CPUs != "" {
		if m, err := units.ParseCPUMilli(w.CPUs, store.MaxCPUMilli/1000); err != nil {
			errs["cpus"] = capital(err.Error()) + "."
		} else {
			c.CPUMilli = m
		}
	}
	for _, f := range []struct {
		key, raw string
		dst      *int
	}{{"memory", w.Memory, &c.MemoryMB}, {"tmpfs", w.Tmpfs, &c.TmpfsMB}, {"disk", w.Disk, &c.DiskMB}} {
		if f.raw == "" {
			continue
		}
		if n, err := units.ParseSizeMB(f.raw); err != nil {
			errs[f.key] = capital(err.Error()) + "."
		} else {
			*f.dst = n
		}
	}
	if w.Pids != "" {
		if n, err := strconv.Atoi(w.Pids); err != nil || n < 1 || n > store.MaxPids {
			errs["pids"] = fmt.Sprintf("Want a whole number from 1 to %d.", store.MaxPids)
		} else {
			c.Pids = n
		}
	}
	for _, f := range []struct {
		key, raw string
		limit    time.Duration
		dst      *time.Duration
	}{
		{"idle_ttl", w.IdleTTL, store.MaxWorkspaceTTL, &c.IdleTTL},
		{"max_ttl", w.MaxTTL, store.MaxWorkspaceTTL, &c.MaxTTL},
		{"exec_default", w.ExecDefault, store.MaxExecCeiling, &c.ExecDefault},
		{"exec_max", w.ExecMax, store.MaxExecCeiling, &c.ExecMax},
	} {
		if f.raw == "" {
			continue
		}
		if d, err := units.ParseSeconds(f.raw, f.limit); err != nil {
			errs[f.key] = capital(err.Error()) + "."
		} else {
			*f.dst = d
		}
	}
	return c, errs
}

// wsKeywords maps a validation message from the store to the field it is about.
var wsKeywords = []struct{ word, key string }{
	{"default image", "image"}, {"allowlist", "images"}, {"egress", "egress"}, {"executor", "executor"},
	{"pull", "pull"}, {"network", "network"}, {"cpu_milli", "cpus"}, {"memory_mb", "memory"},
	{"pids", "pids"}, {"tmpfs_mb", "tmpfs"}, {"disk_mb", "disk"}, {"max ttl must", "max_ttl"},
	{"idle ttl", "idle_ttl"}, {"exec", "exec_max"},
}

// wsErrField puts a service validation message on the field it names, or on the form.
func wsErrField(errs map[string]string, msg string) {
	msg = strings.TrimPrefix(msg, "workspace config: ")
	low := strings.ToLower(msg)
	for _, k := range wsKeywords {
		if strings.Contains(low, k.word) {
			errs[k.key] = capital(msg) + "."
			return
		}
	}
	errs["form"] = capital(msg) + "."
}

func (w wsForm) fields(errs map[string]string) []formField {
	return []formField{
		{Key: "executor", Label: "Executor", Kind: "select", Value: w.Executor, Options: []string{"containerd", "opensandbox"}},
		{Key: "image", Label: "Default image", Kind: "text", Value: w.Image, Hint: "An exact reference that the allowlist below permits, for example docker.io/library/debian@sha256:..."},
		{Key: "images", Label: "Images allowlist", Kind: "textarea", Value: w.Images, Hint: "One per line: exact references, or repo:* and repo@sha256:*. Blank means only the default image."},
		{Key: "pull", Label: "Pull policy", Kind: "select", Value: w.Pull, Options: []string{"if-not-present", "always", "never"}},
		{Key: "network", Label: "Network", Kind: "select", Value: w.Network, Options: []string{"none", "egress"}},
		{Key: "egress", Label: "Egress allowlist", Kind: "textarea", Value: w.Egress, Hint: "One per line: host[:port], *.domain:443 or cidr:port. Only used with network egress."},
		{Key: "cpus", Label: "CPUs", Kind: "text", Value: w.CPUs, Hint: "For example 2 or 0.5."},
		{Key: "memory", Label: "Memory", Kind: "text", Value: w.Memory, Hint: "For example 4GiB or 512MiB."},
		{Key: "pids", Label: "Process limit", Kind: "text", Value: w.Pids},
		{Key: "tmpfs", Label: "Temporary space (/tmp)", Kind: "text", Value: w.Tmpfs, Hint: "For example 512MiB."},
		{Key: "disk", Label: "Disk quota (/workspace)", Kind: "text", Value: w.Disk, Hint: "For example 10GiB."},
		{Key: "idle_ttl", Label: "Idle time limit", Kind: "text", Value: w.IdleTTL, Hint: "Close a workspace unused for this long, for example 24h."},
		{Key: "max_ttl", Label: "Maximum age", Kind: "text", Value: w.MaxTTL, Hint: "Close a workspace after this long, for example 168h."},
		{Key: "exec_default", Label: "Default command timeout", Kind: "text", Value: w.ExecDefault, Hint: "For example 2m."},
		{Key: "exec_max", Label: "Maximum command timeout", Kind: "text", Value: w.ExecMax, Hint: "At most 24h."},
	}
}

func withErrs(fs []formField, errs map[string]string) []formField {
	for i := range fs {
		fs[i].Err = errs[fs[i].Key]
	}
	return fs
}

// ---- handlers ----

func (s *Server) fleetsList(w http.ResponseWriter, r *http.Request) {
	a, ok := s.screenAuth(w, r)
	if !ok {
		return
	}
	fl, err := s.cfg.Backend.ListFleets(r.Context(), a.P, service.SourceGUI)
	if err != nil {
		s.backendFail(w, r, err)
		return
	}
	d := fleetsData{View: "list"}
	for _, f := range fl {
		d.Rows = append(d.Rows, fleetRow{Name: f.Name, URL: fleetURL(f.Name), Mode: string(f.Mode),
			Guarded: f.Confirm, Runners: f.RunnerCount, Workspace: wsSummary(f.Workspace)})
	}
	pg := s.screenPage(w, r, a, "Fleets", "fleets", Crumb{Text: "Fleets"})
	pg.Data = d
	s.Render(w, r, http.StatusOK, "fleets", "fleets/list", pg)
}

func (s *Server) newFormData(name, mode string, guarded bool, ws wsForm, errs map[string]string) fleetsData {
	return fleetsData{View: "new", Name: name, Mode: mode, Guarded: guarded, Errs: errs, Action: "/fleets",
		Fields: withErrs(ws.fields(errs), errs)}
}

func (s *Server) renderNew(w http.ResponseWriter, r *http.Request, a adminReq, status int, d fleetsData) {
	pg := s.screenPage(w, r, a, "New fleet", "fleets", Crumb{Text: "Fleets", URL: "/fleets"}, Crumb{Text: "New fleet"})
	pg.Data = d
	s.Render(w, r, status, "fleets", "fleets/form", pg)
}

func (s *Server) fleetNewForm(w http.ResponseWriter, r *http.Request) {
	a, ok := s.screenAuth(w, r)
	if !ok {
		return
	}
	s.renderNew(w, r, a, http.StatusOK, s.newFormData("", "actions", true, defaultWSForm(), map[string]string{}))
}

const nameHint = "Use 1 to 63 characters: lowercase letters, digits, dot, dash or underscore, starting with a letter or digit."

func (s *Server) fleetCreate(w http.ResponseWriter, r *http.Request) {
	a, ok := s.screenAuth(w, r)
	if !ok {
		return
	}
	form, ok := s.screenForm(w, r, a)
	if !ok {
		return
	}
	name := strings.TrimSpace(form.Get("name"))
	mode := form.Get("mode")
	guarded := form.Get("guarded") != ""
	ws := wsFormFromValues(form)
	errs := map[string]string{}
	switch {
	case name == "new":
		errs["name"] = "This name is reserved. Choose another."
	case !store.ValidFleetName(name):
		errs["name"] = nameHint
	}
	if mode != "actions" && mode != "workspace" {
		errs["mode"] = "Choose actions or workspace."
	}
	var cfg *store.WorkspaceConfig
	if mode == "workspace" {
		c, werrs := ws.parse()
		for k, v := range werrs {
			errs[k] = v
		}
		cfg = &c
	}
	if len(errs) == 0 {
		fl, err := s.cfg.Backend.CreateFleet(r.Context(), a.P, service.SourceGUI, name, store.Mode(mode), guarded, cfg)
		switch {
		case err == nil:
			s.Redirect(w, r, fleetURL(fl.Name), "fleet.created", fl.Name)
			return
		case errCode(err) == api.CodeConflict:
			errs["name"] = "A fleet with this name already exists."
		case errCode(err) == api.CodeBadRequest:
			wsErrField(errs, errMsg(err))
		default:
			s.backendFail(w, r, err)
			return
		}
	}
	s.renderNew(w, r, a, http.StatusUnprocessableEntity, s.newFormData(name, mode, guarded, ws, errs))
}

func (s *Server) fleetDetail(w http.ResponseWriter, r *http.Request) {
	a, ok := s.screenAuth(w, r)
	if !ok {
		return
	}
	name := r.PathValue("fleet")
	ctx, be := r.Context(), s.cfg.Backend
	f, err := be.GetFleet(ctx, a.P, service.SourceGUI, name)
	if err != nil {
		s.backendFail(w, r, err)
		return
	}
	runners, err := be.ListRunnersAll(ctx, a.P, service.SourceGUI, name, false)
	if err != nil {
		s.backendFail(w, r, err)
		return
	}
	tokens, err := be.ListTokens(ctx, a.P, service.SourceGUI, name)
	if err != nil {
		s.backendFail(w, r, err)
		return
	}
	now := s.now()
	d := fleetsData{View: "detail", Fleet: f.Name, FleetURL: fleetURL(f.Name), Mode: string(f.Mode),
		GuardOn: f.Confirm, Created: s.timeOf(f.CreatedAt)}
	for _, rv := range runners {
		age := ""
		if !rv.LastHeartbeat.IsZero() {
			age = relAge(now, rv.LastHeartbeat)
		}
		d.Runners = append(d.Runners, runnerRow{Name: rv.Name, URL: "/runners/" + PathEscape(rv.Fleet) + "/" + PathEscape(rv.Name),
			Status: capital(string(rv.Status)), Kind: statusKind(rv.Status), Online: rv.Online, Age: age})
	}
	for _, t := range tokens {
		st, kind := "usable", "ok"
		switch {
		case !t.RevokedAt.IsZero():
			st, kind = "revoked", "bad"
		case t.Uses >= t.MaxUses:
			st, kind = "used up", "off"
		case !now.Before(t.ExpiresAt):
			st, kind = "expired", "off"
		}
		d.Tokens = append(d.Tokens, tokenRow{ID: t.TokenID, Uses: fmt.Sprintf("%d / %d", t.Uses, t.MaxUses), State: capital(st),
			Kind: kind, Auto: t.AutoApprove, Expires: s.timeOf(t.ExpiresAt)})
	}
	if c := f.Workspace; c != nil {
		d.HasWS = true
		d.WS = []kvRow{
			{"Executor", string(c.Executor)}, {"Default image", c.DefaultImage},
			{"Images allowlist", strings.Join(c.Images, ", ")}, {"Pull policy", string(c.Pull)},
			{"Network", string(c.Network)}, {"Egress allowlist", orNone(strings.Join(c.Egress, ", "))},
			{"CPUs", units.FormatCPUMilli(c.CPUMilli)}, {"Memory", units.FormatSizeMB(c.MemoryMB)},
			{"Process limit", strconv.Itoa(c.Pids)}, {"Temporary space", units.FormatSizeMB(c.TmpfsMB)},
			{"Disk quota", units.FormatSizeMB(c.DiskMB)}, {"Idle time limit", units.FormatDuration(c.IdleTTL)},
			{"Maximum age", units.FormatDuration(c.MaxTTL)}, {"Default command timeout", units.FormatDuration(c.ExecDefault)},
			{"Maximum command timeout", units.FormatDuration(c.ExecMax)},
		}
	}
	pg := s.screenPage(w, r, a, "Fleet "+f.Name, "fleets", Crumb{Text: "Fleets", URL: "/fleets"}, Crumb{Text: f.Name})
	pg.Data = d
	s.Render(w, r, http.StatusOK, "fleets", "fleets/detail", pg)
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// workspaceFleet loads a fleet for the workspace edit pages (404 unless workspace mode).
func (s *Server) workspaceFleet(w http.ResponseWriter, r *http.Request, a adminReq) (store.Fleet, bool) {
	f, err := s.cfg.Backend.GetFleet(r.Context(), a.P, service.SourceGUI, r.PathValue("fleet"))
	if err != nil {
		s.backendFail(w, r, err)
		return f, false
	}
	if f.Mode != store.ModeWorkspace || f.Workspace == nil {
		s.ErrorPage(w, r, http.StatusNotFound)
		return f, false
	}
	return f, true
}

func (s *Server) renderWSEdit(w http.ResponseWriter, r *http.Request, a adminReq, status int, f store.Fleet, ws wsForm, errs map[string]string) {
	pg := s.screenPage(w, r, a, "Workspace settings of "+f.Name, "fleets", Crumb{Text: "Fleets", URL: "/fleets"},
		Crumb{Text: f.Name, URL: fleetURL(f.Name)}, Crumb{Text: "Workspace settings"})
	pg.Data = fleetsData{View: "wsedit", Fleet: f.Name, FleetURL: fleetURL(f.Name), Errs: errs,
		Action: fleetURL(f.Name) + "/workspace", Fields: withErrs(ws.fields(errs), errs)}
	s.Render(w, r, status, "fleets", "fleets/ws-form", pg)
}

func (s *Server) fleetWSForm(w http.ResponseWriter, r *http.Request) {
	a, ok := s.screenAuth(w, r)
	if !ok {
		return
	}
	f, ok := s.workspaceFleet(w, r, a)
	if !ok {
		return
	}
	s.renderWSEdit(w, r, a, http.StatusOK, f, wsFormFromConfig(*f.Workspace), map[string]string{})
}

func (s *Server) fleetWSSave(w http.ResponseWriter, r *http.Request) {
	a, ok := s.screenAuth(w, r)
	if !ok {
		return
	}
	form, ok := s.screenForm(w, r, a)
	if !ok {
		return
	}
	f, ok := s.workspaceFleet(w, r, a)
	if !ok {
		return
	}
	ws := wsFormFromValues(form)
	cfg, errs := ws.parse()
	if len(errs) == 0 {
		// SetFleetWorkspace is a full replace: blank fields take the documented defaults.
		_, err := s.cfg.Backend.SetFleetWorkspace(r.Context(), a.P, service.SourceGUI, f.Name, service.WorkspaceDefaults(cfg))
		switch {
		case err == nil:
			s.Redirect(w, r, fleetURL(f.Name), "fleet.ws_updated", f.Name)
			return
		case errCode(err) == api.CodeBadRequest:
			wsErrField(errs, errMsg(err))
		default:
			s.backendFail(w, r, err)
			return
		}
	}
	s.renderWSEdit(w, r, a, http.StatusUnprocessableEntity, f, ws, errs)
}

func (s *Server) renderGuard(w http.ResponseWriter, r *http.Request, a adminReq, status int, f store.Fleet, typed string, errs map[string]string) {
	pg := s.screenPage(w, r, a, "Guard of "+f.Name, "fleets", Crumb{Text: "Fleets", URL: "/fleets"},
		Crumb{Text: f.Name, URL: fleetURL(f.Name)}, Crumb{Text: "Guard"})
	pg.Data = fleetsData{View: "guard", Fleet: f.Name, FleetURL: fleetURL(f.Name), GuardOn: f.Confirm,
		TurnOff: f.Confirm, Typed: typed, Errs: errs, Action: fleetURL(f.Name) + "/guard"}
	s.Render(w, r, status, "fleets", "fleets/guard", pg)
}

func (s *Server) fleetGuardForm(w http.ResponseWriter, r *http.Request) {
	a, ok := s.screenAuth(w, r)
	if !ok {
		return
	}
	f, err := s.cfg.Backend.GetFleet(r.Context(), a.P, service.SourceGUI, r.PathValue("fleet"))
	if err != nil {
		s.backendFail(w, r, err)
		return
	}
	s.renderGuard(w, r, a, http.StatusOK, f, "", map[string]string{})
}

func (s *Server) fleetGuardSave(w http.ResponseWriter, r *http.Request) {
	a, ok := s.screenAuth(w, r)
	if !ok {
		return
	}
	form, ok := s.screenForm(w, r, a)
	if !ok {
		return
	}
	f, err := s.cfg.Backend.GetFleet(r.Context(), a.P, service.SourceGUI, r.PathValue("fleet"))
	if err != nil {
		s.backendFail(w, r, err)
		return
	}
	to := form.Get("to")
	if to != "on" && to != "off" {
		s.ErrorPage(w, r, http.StatusBadRequest)
		return
	}
	typed := strings.TrimSpace(form.Get("confirm"))
	if to == "off" && typed != f.Name {
		s.renderGuard(w, r, a, http.StatusUnprocessableEntity, f, typed,
			map[string]string{"confirm": "Type the fleet name exactly to turn the guard off."})
		return
	}
	if _, err := s.cfg.Backend.SetFleetConfirm(r.Context(), a.P, service.SourceGUI, f.Name, to == "on"); err != nil {
		s.backendFail(w, r, err)
		return
	}
	s.Redirect(w, r, fleetURL(f.Name), "fleet.guard_"+to, f.Name)
}

// errMsg is the wire message of an api error (fixed text per failure, safe to show).
func errMsg(err error) string {
	var ae *api.Error
	if errors.As(err, &ae) {
		return ae.Message
	}
	return ""
}
