package gui

// Credentials screens: a masked list, create and replace through a
// write-only form, per-runner delivery status, assignment to runners or whole fleets, rotate-now
// and a typed-confirmation delete. The secret value exists in this package only inside the POST
// handler that forwards it to the service: it is never rendered, flashed, logged, put in a URL or
// echoed into a re-rendered form. Everything shown is metadata and status from the service's
// masked views. The browser form is urlencoded only, so the value is pasted; a
// file upload is available through the CLI (`central credential set NAME < secretfile`).

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

func init() {
	RegisterFlash("credential.saved", "ok", "Credential %s saved. Connected runners that hold it are sent the new value now; the rest get it on their next heartbeat.")
	RegisterFlash("credential.deleted", "warn", "Credential %s deleted. Connected runners are told now; any that miss it drop it on their next heartbeat.")
	RegisterFlash("credential.rotate_busy", "warn", "A rotation of %s is already in progress. Reload in a moment.")
	RegisterFlash("credential.rotate_lost", "bad", "GitLab issued a new token for %s but it could not be stored; the old token is revoked. Create a new token in GitLab and replace the value.")
	RegisterFlash("credential.assigned", "ok", "Credential %s assigned.")
	RegisterFlash("credential.unassigned", "warn", "Credential %s unassigned.")
	RegisterFlash("credential.assign_failed", "warn", "The assignment of %s was not changed: the fleet or runner does not exist, or the assignment is already (or no longer) there.")
	RegisterFlash("credential.rotated", "ok", "Credential %s rotated.")
	RegisterFlash("credential.rotate_failed", "bad", "Rotation of %s failed. The old value keeps working until it expires; see the credential for the reason.")
}

const credTextMax = 120

type credAssignView struct {
	Target, Fleet, Runner, Label string // Target is the form value: "fleet" or "fleet/runner"
}

type credDeliveryView struct {
	Fleet, Runner, RunnerURL string
	Status                   badgeView
	Version                  int64
}

type credRow struct {
	Name, URL, Kind, Fingerprint string
	Hosts                        []string
	Expires                      timeView
	HasExpiry                    bool
	Status                       badgeView
	Version                      int64
	Assigned                     string
	Rotate                       string
}

type credDetail struct {
	credRow
	User, Auth, Header, HostBinding, RotateProject, RotateError string
	Created, Updated                                            timeView
	Assignments                                                 []credAssignView
	Delivery                                                    []credDeliveryView
	AssignOpt                                                   []selOpt
	NoTargets                                                   bool
	ReplaceURL, DeleteURL, RotateURL, AssignURL, UnassignURL    string
}

// credForm is the form model of create and replace. It has no field for the secret value.
type credForm struct {
	Replace                                   bool
	Name, Kind, Hosts, KnownHosts, User, Auth string
	Header, HostBinding, Expires, RotateProj  string
	Rotate                                    bool
	Action, Cancel                            string
	KindOpt, AuthOpt, BindOpt                 []selOpt
	Errs                                      map[string]string
}

type credentialsData struct {
	View string // list | new | detail | delete

	Rows []credRow

	F credForm

	D credDetail

	// delete
	Name, Typed, Err, ActionURL, DetailURL string
	Holders                                int
}

var (
	credKinds = []selOpt{
		{Value: transport.CredSSHKey, Label: "SSH key (OpenSSH private key)"},
		{Value: transport.CredHTTPSToken, Label: "HTTPS token"},
		{Value: transport.CredRegistry, Label: "Registry (user and password or token)"},
	}
	credAuths = []selOpt{
		{Value: transport.AuthBearer, Label: "Bearer token"},
		{Value: transport.AuthBasic, Label: "Basic (user and token)"},
		{Value: transport.AuthHeader, Label: "Custom header"},
	}
	credBinds = []selOpt{
		{Value: transport.BindRequired, Label: "Required (OpenSSH 8.9 or newer)"},
		{Value: transport.BindOff, Label: "Off (older OpenSSH)"},
	}
)

func (s *Server) routesCredentials(mux adminMux) {
	mux.HandleFunc("GET /credentials", s.credentialsList)
	mux.HandleFunc("GET /credentials/new", s.credentialNewForm)
	mux.HandleFunc("POST /credentials", s.credentialCreate)
	mux.HandleFunc("GET /credentials/{name}", s.credentialDetailPage)
	mux.HandleFunc("GET /credentials/{name}/replace", s.credentialReplaceForm)
	mux.HandleFunc("POST /credentials/{name}/replace", s.credentialReplace)
	mux.HandleFunc("POST /credentials/{name}/assign", s.credentialAssign)
	mux.HandleFunc("POST /credentials/{name}/unassign", s.credentialUnassign)
	mux.HandleFunc("POST /credentials/{name}/rotate", s.credentialRotate)
	mux.HandleFunc("GET /credentials/{name}/delete", s.credentialDeleteForm)
	mux.HandleFunc("POST /credentials/{name}/delete", s.credentialDelete)
}

func credURL(name string) string { return "/credentials/" + PathEscape(name) }

// credStatus is the badge of a credential's own expiry (delivery has its own badge per runner).
func (s *Server) credStatus(ci api.CredentialInfo) badgeView {
	switch {
	case ci.RotateError != "":
		return badgeView{Kind: "bad", Text: "Rotation failed"}
	case ci.Expires == nil:
		return badgeView{Kind: "ok", Text: "No expiry"}
	}
	now := s.now()
	switch {
	case !ci.Expires.After(now):
		return badgeView{Kind: "bad", Text: "Expired"}
	case ci.Expires.Sub(now) < service.ExpiringWindow:
		return badgeView{Kind: "warn", Text: "Expiring"}
	}
	return badgeView{Kind: "ok", Text: "Valid"}
}

func deliveryBadge(st string) badgeView {
	switch st {
	case transport.CredOK:
		return badgeView{Kind: "ok", Text: "OK"}
	case transport.CredExpiring:
		return badgeView{Kind: "warn", Text: "Expiring"}
	case "pending":
		return badgeView{Kind: "off", Text: "Pending"}
	case transport.CredExpired:
		return badgeView{Kind: "bad", Text: "Expired"}
	case transport.CredInvalid:
		return badgeView{Kind: "bad", Text: "Invalid"}
	}
	return badgeView{Kind: "off", Text: inertRunnerText(capital(st), 20)}
}

func assignText(ci api.CredentialInfo) string {
	fleets, runners := 0, 0
	for _, a := range ci.Assignments {
		if a.Runner == "" {
			fleets++
		} else {
			runners++
		}
	}
	if fleets+runners == 0 {
		return "Not assigned"
	}
	var parts []string
	if fleets > 0 {
		parts = append(parts, plural(fleets, "fleet"))
	}
	if runners > 0 {
		parts = append(parts, plural(runners, "runner"))
	}
	return strings.Join(parts, ", ")
}

func plural(n int, w string) string {
	s := strconv.Itoa(n) + " " + w
	if n != 1 {
		s += "s"
	}
	return s
}

func (s *Server) credRowOf(ci api.CredentialInfo) credRow {
	r := credRow{Name: ci.Name, URL: credURL(ci.Name), Kind: ci.Kind, Fingerprint: inertRunnerText(ci.Fingerprint, credTextMax),
		Status: s.credStatus(ci), Version: ci.Version, Assigned: assignText(ci), Rotate: ci.Rotate}
	for _, h := range ci.Hosts {
		r.Hosts = append(r.Hosts, inertRunnerText(h, credTextMax))
	}
	if ci.Expires != nil {
		r.HasExpiry, r.Expires = true, s.timeOf(*ci.Expires)
	}
	return r
}

func (s *Server) credentialsList(w http.ResponseWriter, r *http.Request) {
	a, ok := s.screenAuth(w, r)
	if !ok {
		return
	}
	cis, err := s.cfg.Backend.ListCredentials(r.Context(), a.P, service.SourceGUI, "")
	if err != nil {
		s.backendFail(w, r, err)
		return
	}
	d := credentialsData{View: "list"}
	for _, ci := range cis {
		d.Rows = append(d.Rows, s.credRowOf(ci))
	}
	pg := s.screenPage(w, r, a, "Credentials", "credentials")
	pg.Data = d
	s.Render(w, r, http.StatusOK, "credentials", "credentials/list", pg)
}

// findCredential resolves {name}; it answers 404 itself.
func (s *Server) findCredential(w http.ResponseWriter, r *http.Request, a adminReq) (api.CredentialInfo, bool) {
	name := r.PathValue("name")
	if !transport.ValidCredName(name) {
		s.ErrorPage(w, r, http.StatusNotFound)
		return api.CredentialInfo{}, false
	}
	cis, err := s.cfg.Backend.ListCredentials(r.Context(), a.P, service.SourceGUI, name)
	if err != nil {
		s.backendFail(w, r, err)
		return api.CredentialInfo{}, false
	}
	if len(cis) != 1 {
		s.ErrorPage(w, r, http.StatusNotFound)
		return api.CredentialInfo{}, false
	}
	return cis[0], true
}

// assignOptions lists every fleet (as a whole) and every non-revoked runner as assign targets.
func (s *Server) assignOptions(w http.ResponseWriter, r *http.Request, a adminReq) ([]selOpt, bool) {
	be, ctx := s.cfg.Backend, r.Context()
	fleets, err := be.ListFleets(ctx, a.P, service.SourceGUI)
	if err != nil {
		s.backendFail(w, r, err)
		return nil, false
	}
	var opts []selOpt
	for _, f := range fleets {
		opts = append(opts, selOpt{Value: f.Name, Label: f.Name + " (every runner in the fleet)"})
		rvs, err := be.ListRunners(ctx, a.P, service.SourceGUI, f.Name)
		if err != nil {
			s.backendFail(w, r, err)
			return nil, false
		}
		for _, rv := range rvs {
			opts = append(opts, selOpt{Value: f.Name + "/" + rv.Name, Label: f.Name + "/" + inertRunnerText(rv.Name, 64)})
		}
	}
	return opts, true
}

func (s *Server) credentialDetailPage(w http.ResponseWriter, r *http.Request) {
	a, ok := s.screenAuth(w, r)
	if !ok {
		return
	}
	ci, ok := s.findCredential(w, r, a)
	if !ok {
		return
	}
	opts, ok := s.assignOptions(w, r, a)
	if !ok {
		return
	}
	u := credURL(ci.Name)
	d := credDetail{credRow: s.credRowOf(ci), User: inertRunnerText(ci.User, credTextMax), Auth: ci.Auth,
		Header: inertRunnerText(ci.Header, credTextMax), HostBinding: ci.HostBinding,
		RotateProject: inertRunnerText(ci.RotateProject, credTextMax), RotateError: inertRunnerText(ci.RotateError, 200),
		Created: s.timeOf(ci.Created), Updated: s.timeOf(ci.Updated), AssignOpt: opts, NoTargets: len(opts) == 0,
		ReplaceURL: u + "/replace", DeleteURL: u + "/delete", RotateURL: u + "/rotate", AssignURL: u + "/assign", UnassignURL: u + "/unassign"}
	for _, as := range ci.Assignments {
		v := credAssignView{Fleet: as.Fleet, Runner: as.Runner, Target: as.Fleet, Label: as.Fleet + " (every runner)"}
		if as.Runner != "" {
			v.Target, v.Label = as.Fleet+"/"+as.Runner, as.Fleet+"/"+inertRunnerText(as.Runner, 64)
		}
		d.Assignments = append(d.Assignments, v)
	}
	for _, dl := range ci.Delivery {
		d.Delivery = append(d.Delivery, credDeliveryView{Fleet: dl.Fleet, Runner: inertRunnerText(dl.Runner, 64),
			RunnerURL: runnerURL(dl.Fleet, dl.Runner), Status: deliveryBadge(dl.Status), Version: dl.Version})
	}
	pg := s.screenPage(w, r, a, "Credential "+ci.Name, "credentials",
		Crumb{Text: "Credentials", URL: "/credentials"}, Crumb{Text: ci.Name})
	pg.Data = credentialsData{View: "detail", D: d}
	s.Render(w, r, http.StatusOK, "credentials", "credentials/detail", pg)
}

// ---- create and replace ----

func markSel(opts []selOpt, v string) []selOpt {
	out := make([]selOpt, len(opts))
	for i, o := range opts {
		o.Sel = o.Value == v
		out[i] = o
	}
	return out
}

func (f credForm) withOptions() credForm {
	if f.Kind == "" {
		f.Kind = transport.CredHTTPSToken
	}
	if f.Auth == "" {
		f.Auth = transport.AuthBearer
	}
	if f.HostBinding == "" {
		f.HostBinding = transport.BindRequired
	}
	f.KindOpt, f.AuthOpt, f.BindOpt = markSel(credKinds, f.Kind), markSel(credAuths, f.Auth), markSel(credBinds, f.HostBinding)
	if f.Errs == nil {
		f.Errs = map[string]string{}
	}
	return f
}

func (s *Server) renderCredForm(w http.ResponseWriter, r *http.Request, a adminReq, status int, f credForm) {
	f = f.withOptions()
	title, crumb := "New credential", "New"
	if f.Replace {
		title, crumb = "Replace credential "+f.Name, "Replace"
		f.Action, f.Cancel = credURL(f.Name)+"/replace", credURL(f.Name)
	} else {
		f.Action, f.Cancel = "/credentials", "/credentials"
	}
	crumbs := []Crumb{{Text: "Credentials", URL: "/credentials"}}
	if f.Replace {
		crumbs = append(crumbs, Crumb{Text: f.Name, URL: credURL(f.Name)})
	}
	crumbs = append(crumbs, Crumb{Text: crumb})
	pg := s.screenPage(w, r, a, title, "credentials", crumbs...)
	pg.Data = credentialsData{View: "new", F: f}
	s.Render(w, r, status, "credentials", "credentials/form", pg)
}

func (s *Server) credentialNewForm(w http.ResponseWriter, r *http.Request) {
	a, ok := s.screenAuth(w, r)
	if !ok {
		return
	}
	s.renderCredForm(w, r, a, http.StatusOK, credForm{})
}

func (s *Server) credentialReplaceForm(w http.ResponseWriter, r *http.Request) {
	a, ok := s.screenAuth(w, r)
	if !ok {
		return
	}
	ci, ok := s.findCredential(w, r, a)
	if !ok {
		return
	}
	f := credForm{Replace: true, Name: ci.Name, Kind: ci.Kind, Hosts: strings.Join(ci.Hosts, "\n"), User: ci.User, Auth: ci.Auth,
		Header: ci.Header, HostBinding: ci.HostBinding, Rotate: ci.Rotate != "", RotateProj: ci.RotateProject}
	if ci.Expires != nil {
		f.Expires = ci.Expires.UTC().Format("2006-01-02")
	}
	s.renderCredForm(w, r, a, http.StatusOK, f)
}

// knownHostLines splits known_hosts text by line, keeping each line whole (it holds spaces).
func knownHostLines(text string) []string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// credentialParams builds the service parameters from a submitted form and returns the secret
// value as bytes. It reports field errors by name; the value never leaves this function except
// as the returned slice.
func credentialParams(form url.Values, name string, now time.Time) (api.CredentialSetParams, []byte, credForm, map[string]string) {
	f := credForm{Name: name, Kind: form.Get("kind"), Hosts: form.Get("hosts"), KnownHosts: form.Get("known_hosts"),
		User: strings.TrimSpace(form.Get("user")), Auth: form.Get("auth"), Header: strings.TrimSpace(form.Get("header")),
		HostBinding: form.Get("host_binding"), Expires: strings.TrimSpace(form.Get("expires")),
		Rotate: form.Get("rotate") == "1", RotateProj: strings.TrimSpace(form.Get("rotate_project"))}
	errs := map[string]string{}
	if !transport.ValidCredName(name) {
		errs["name"] = "Use 1 to 64 letters, digits, dot, dash or underscore, starting with a letter or digit."
	}
	if !transport.ValidCredKind(f.Kind) {
		errs["kind"] = "Choose a kind."
	}
	p := api.CredentialSetParams{}
	p.Name, p.Kind = name, f.Kind
	p.Hosts = splitLines(f.Hosts)
	if err := transport.ValidateHosts(p.Hosts); err != nil {
		errs["hosts"] = "List 1 to " + strconv.Itoa(transport.MaxCredHosts) + " hosts as host or host:port, lowercase, one per line."
	}
	switch f.Kind {
	case transport.CredSSHKey:
		p.User, p.HostBinding = f.User, f.HostBinding
		p.KnownHosts = knownHostLines(f.KnownHosts)
		if len(p.KnownHosts) == 0 {
			errs["known_hosts"] = "Paste at least one known_hosts line for the server."
		}
	case transport.CredHTTPSToken:
		p.Auth = f.Auth
		switch f.Auth {
		case transport.AuthBasic:
			p.User = f.User
			if p.User == "" {
				errs["user"] = "Basic needs a user."
			}
		case transport.AuthHeader:
			p.Header = f.Header
			if p.Header == "" {
				errs["header"] = "Name the header."
			}
		case transport.AuthBearer:
		default:
			errs["auth"] = "Choose how the token is sent."
		}
	case transport.CredRegistry:
		p.User = f.User
		if p.User == "" {
			errs["user"] = "A registry credential needs a user."
		}
	}
	if f.Expires != "" {
		d, err := time.Parse("2006-01-02", f.Expires)
		if err != nil {
			errs["expires"] = "Use a date as YYYY-MM-DD."
		} else {
			e := d.Add(24*time.Hour - time.Second).UTC()
			if !e.After(now) {
				errs["expires"] = "The expiry must be in the future."
			}
			p.Expires = &e
		}
	}
	if f.Rotate {
		p.Rotate, p.RotateProject = api.RotateGitLabSelf, f.RotateProj
		if f.Kind != transport.CredHTTPSToken {
			errs["rotate"] = "Rotation works for HTTPS tokens only."
		}
		if f.RotateProj == "" {
			errs["rotate_project"] = "Name the GitLab project (id or path)."
		}
		if p.Expires == nil {
			errs["expires"] = "Rotation needs the expiry date of the token."
		}
	}
	value := form.Get("value")
	if f.Kind == transport.CredSSHKey {
		value = strings.ReplaceAll(value, "\r\n", "\n")
		value = strings.TrimSpace(value) + "\n"
	} else {
		value = strings.TrimSpace(value)
	}
	if strings.TrimSpace(value) == "" {
		errs["value"] = "Paste the secret. It is write-only and cannot be shown again."
	} else if len(value) > api.MaxCredentialValue {
		errs["value"] = "The secret is too long (limit " + strconv.Itoa(api.MaxCredentialValue) + " bytes)."
	}
	b := []byte(value)
	sum := sha256.Sum256(b)
	p.Length, p.SHA256 = len(b), hex.EncodeToString(sum[:])
	return p, b, f, errs
}

func (s *Server) credentialCreate(w http.ResponseWriter, r *http.Request) {
	a, ok := s.screenAuth(w, r)
	if !ok {
		return
	}
	form, ok := s.screenForm(w, r, a)
	if !ok {
		return
	}
	s.saveCredential(w, r, a, form, strings.TrimSpace(form.Get("name")), false)
}

func (s *Server) credentialReplace(w http.ResponseWriter, r *http.Request) {
	a, ok := s.screenAuth(w, r)
	if !ok {
		return
	}
	form, ok := s.screenForm(w, r, a)
	if !ok {
		return
	}
	ci, ok := s.findCredential(w, r, a)
	if !ok {
		return
	}
	s.saveCredential(w, r, a, form, ci.Name, true)
}

func (s *Server) saveCredential(w http.ResponseWriter, r *http.Request, a adminReq, form url.Values, name string, replace bool) {
	p, value, f, errs := credentialParams(form, name, s.now())
	f.Replace = replace
	redo := func(status int, errs map[string]string) {
		f.Errs = errs
		s.renderCredForm(w, r, a, status, f)
	}
	if len(errs) > 0 {
		redo(http.StatusUnprocessableEntity, errs)
		return
	}
	if !replace {
		if cis, err := s.cfg.Backend.ListCredentials(r.Context(), a.P, service.SourceGUI, name); err == nil && len(cis) > 0 {
			redo(http.StatusUnprocessableEntity, map[string]string{"name": "A credential with this name exists. Open it and choose Replace value."})
			return
		}
	}
	_, err := s.cfg.Backend.SetCredential(r.Context(), a.P, service.SourceGUI, p, value)
	if err != nil {
		switch errCode(err) {
		case api.CodeBadRequest:
			redo(http.StatusUnprocessableEntity, map[string]string{"form": "The credential was not saved: " + inertRunnerText(errMsg(err), 200)})
		case api.CodeUnavailable:
			redo(http.StatusServiceUnavailable, map[string]string{"form": inertRunnerText(errMsg(err), 200)})
		default:
			s.backendFail(w, r, err)
		}
		return
	}
	s.Redirect(w, r, credURL(name), "credential.saved", name)
}

// ---- assign, unassign, rotate ----

// parseTarget reads "fleet" or "fleet/runner".
func parseTarget(v string) (fleet, runner string, ok bool) {
	fleet, runner, _ = strings.Cut(strings.TrimSpace(v), "/")
	if !store.ValidFleetName(fleet) || (runner != "" && !store.ValidRunnerName(runner)) {
		return "", "", false
	}
	return fleet, runner, true
}

func (s *Server) credentialAssign(w http.ResponseWriter, r *http.Request) {
	s.credentialAssignment(w, r, true)
}

func (s *Server) credentialUnassign(w http.ResponseWriter, r *http.Request) {
	s.credentialAssignment(w, r, false)
}

func (s *Server) credentialAssignment(w http.ResponseWriter, r *http.Request, add bool) {
	a, ok := s.screenAuth(w, r)
	if !ok {
		return
	}
	form, ok := s.screenForm(w, r, a)
	if !ok {
		return
	}
	ci, ok := s.findCredential(w, r, a)
	if !ok {
		return
	}
	fleet, runner, ok := parseTarget(form.Get("target"))
	if !ok {
		s.Redirect(w, r, credURL(ci.Name), "credential.assign_failed", ci.Name)
		return
	}
	var err error
	be, ctx := s.cfg.Backend, r.Context()
	if add {
		_, err = be.AssignCredential(ctx, a.P, service.SourceGUI, api.CredentialAssignParams{Name: ci.Name, Fleet: fleet, Runner: runner})
	} else {
		_, err = be.UnassignCredential(ctx, a.P, service.SourceGUI, api.CredentialUnassignParams{Name: ci.Name, Fleet: fleet, Runner: runner})
	}
	if err != nil {
		switch errCode(err) {
		case api.CodeBadRequest, api.CodeNotFound, api.CodeConflict:
			s.Redirect(w, r, credURL(ci.Name), "credential.assign_failed", ci.Name)
		default:
			s.backendFail(w, r, err)
		}
		return
	}
	id := "credential.unassigned"
	if add {
		id = "credential.assigned"
	}
	s.Redirect(w, r, credURL(ci.Name), id, ci.Name)
}

func (s *Server) credentialRotate(w http.ResponseWriter, r *http.Request) {
	a, ok := s.screenAuth(w, r)
	if !ok {
		return
	}
	if _, ok := s.screenForm(w, r, a); !ok {
		return
	}
	ci, ok := s.findCredential(w, r, a)
	if !ok {
		return
	}
	if _, err := s.cfg.Backend.RotateCredential(r.Context(), a.P, service.SourceGUI, ci.Name); err != nil {
		switch errCode(err) {
		case api.CodeConflict:
			s.Redirect(w, r, credURL(ci.Name), "credential.rotate_busy", ci.Name)
		case api.CodeBadRequest, api.CodeUnavailable:
			id := "credential.rotate_failed"
			if strings.Contains(errMsg(err), "could not be stored") {
				id = "credential.rotate_lost"
			}
			s.Redirect(w, r, credURL(ci.Name), id, ci.Name)
		default:
			s.backendFail(w, r, err)
		}
		return
	}
	s.Redirect(w, r, credURL(ci.Name), "credential.rotated", ci.Name)
}

// ---- delete (typed confirmation) ----

func (s *Server) renderCredDelete(w http.ResponseWriter, r *http.Request, a adminReq, status int, ci api.CredentialInfo, typed, errText string) {
	pg := s.screenPage(w, r, a, "Delete credential "+ci.Name, "credentials",
		Crumb{Text: "Credentials", URL: "/credentials"}, Crumb{Text: ci.Name, URL: credURL(ci.Name)}, Crumb{Text: "Delete"})
	pg.Data = credentialsData{View: "delete", Name: ci.Name, Typed: typed, Err: errText,
		ActionURL: credURL(ci.Name) + "/delete", DetailURL: credURL(ci.Name), Holders: len(ci.Delivery)}
	s.Render(w, r, status, "credentials", "credentials/delete", pg)
}

func (s *Server) credentialDeleteForm(w http.ResponseWriter, r *http.Request) {
	a, ok := s.screenAuth(w, r)
	if !ok {
		return
	}
	ci, ok := s.findCredential(w, r, a)
	if !ok {
		return
	}
	s.renderCredDelete(w, r, a, http.StatusOK, ci, "", "")
}

func (s *Server) credentialDelete(w http.ResponseWriter, r *http.Request) {
	a, ok := s.screenAuth(w, r)
	if !ok {
		return
	}
	form, ok := s.screenForm(w, r, a)
	if !ok {
		return
	}
	ci, ok := s.findCredential(w, r, a)
	if !ok {
		return
	}
	if typed := strings.TrimSpace(form.Get("confirm")); typed != ci.Name {
		s.renderCredDelete(w, r, a, http.StatusUnprocessableEntity, ci, inertRunnerText(typed, 200),
			"Type the credential name exactly to delete it.")
		return
	}
	if err := s.cfg.Backend.DeleteCredential(r.Context(), a.P, service.SourceGUI, ci.Name); err != nil {
		s.backendFail(w, r, err)
		return
	}
	s.Redirect(w, r, "/credentials", "credential.deleted", ci.Name)
}
