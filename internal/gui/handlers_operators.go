package gui

// Operators screen: the operator list, the registration-code panel, secret
// rotation and revocation. The code is shown only in the response to an explicit POST (each show
// is audited by service.OperatorCode); no GET and no polled fragment ever carries it.

import (
	"net/http"
	"strings"

	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
)

func init() {
	RegisterFlash("operators.revoked", "ok", "Operator %s revoked.")
	RegisterFlash("operators.rotated", "ok", "Registration secret rotated. Every code shown so far stops working.")
}

type operatorRow struct {
	Name, Fingerprint, RevokeURL string
	Status                       badgeView
	CanRevoke                    bool
	Registered, LastSeen         timeView
}

// codeView is the registration code shown after a POST. It exists only in that response.
type codeView struct {
	Seconds                   int
	CodeCopy, FPCopy, CmdCopy copyView
}

type operatorsData struct {
	View    string // list | rotate | revoke
	Rows    []operatorRow
	Empty   emptyView
	Code    *codeView // nil unless this response answers "Show code"
	Name    string    // revoke
	Typed   string
	Err     string
	Action  string
	CancelU string
}

func (s *Server) routesOperators(mux adminMux) {
	mux.HandleFunc("GET /operators", s.operatorsList)
	mux.HandleFunc("POST /operators/code", s.operatorsShowCode)
	mux.HandleFunc("GET /operators/rotate", s.operatorsRotateForm)
	mux.HandleFunc("POST /operators/rotate", s.operatorsRotate)
	mux.HandleFunc("GET /operators/{name}/revoke", s.operatorsRevokeForm)
	mux.HandleFunc("POST /operators/{name}/revoke", s.operatorsRevoke)
}

func (s *Server) operatorRows(r *http.Request, a adminReq) ([]operatorRow, error) {
	ops, err := s.cfg.Backend.ListOperators(r.Context(), a.P, service.SourceGUI, true)
	if err != nil {
		return nil, err
	}
	rows := make([]operatorRow, 0, len(ops))
	for _, o := range ops {
		row := operatorRow{Name: o.Name, Fingerprint: o.Fingerprint, Registered: s.timeOf(o.CreatedAt), LastSeen: s.timeOf(o.LastSeenAt)}
		switch o.Status {
		case store.OperatorActive:
			row.Status = badgeView{"ok", "active"}
			row.CanRevoke = true
			row.RevokeURL = "/operators/" + PathEscape(o.Name) + "/revoke"
		default:
			row.Status = badgeView{"off", string(o.Status)}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func (s *Server) operatorsPage(w http.ResponseWriter, r *http.Request, a adminReq, status int, d operatorsData, title string, crumbs ...Crumb) {
	pg := s.screenPage(w, r, a, title, "operators", crumbs...)
	pg.Data = d
	frag := "operators/code"
	if d.View != "list" {
		frag = "operators/dialog"
	}
	w.Header().Set("Cache-Control", "no-store")
	s.Render(w, r, status, "operators", frag, pg)
}

func (s *Server) operatorsList(w http.ResponseWriter, r *http.Request) {
	a, ok := s.screenAuth(w, r)
	if !ok {
		return
	}
	s.listPage(w, r, a, nil)
}

func (s *Server) listPage(w http.ResponseWriter, r *http.Request, a adminReq, code *codeView) {
	rows, err := s.operatorRows(r, a)
	if err != nil {
		s.backendFail(w, r, err)
		return
	}
	d := operatorsData{View: "list", Rows: rows, Code: code, Action: "/operators/code",
		Empty: emptyView{Title: "No operators yet", Body: "An operator registers with the code above from the machine that runs Claude Code.",
			CLI: "mirafleet mcp login CENTRAL:2222 -name NAME -code -"}}
	s.operatorsPage(w, r, a, http.StatusOK, d, "Operators")
}

func (s *Server) operatorsShowCode(w http.ResponseWriter, r *http.Request) {
	a, ok := s.screenAuth(w, r)
	if !ok {
		return
	}
	if _, ok := s.screenForm(w, r, a); !ok {
		return
	}
	res, err := s.cfg.Backend.OperatorCode(r.Context(), a.P, service.SourceGUI)
	if err != nil {
		s.backendFail(w, r, err)
		return
	}
	adv := s.cfg.SSHAdvertise
	if adv == "" {
		adv = "CENTRAL:2222"
	}
	s.listPage(w, r, a, &codeView{Seconds: res.SecondsLeft,
		CodeCopy: copyView{"reg-code", res.Code, "Copy"},
		FPCopy:   copyView{"reg-fp", res.HostFingerprint, "Copy"},
		CmdCopy:  copyView{"reg-cmd", "mirafleet mcp login " + adv + " -name NAME -code -", "Copy"}})
}

func (s *Server) operatorsRotateForm(w http.ResponseWriter, r *http.Request) {
	a, ok := s.screenAuth(w, r)
	if !ok {
		return
	}
	s.operatorsPage(w, r, a, http.StatusOK, operatorsData{View: "rotate", Action: "/operators/rotate", CancelU: "/operators"},
		"Rotate registration secret", Crumb{"Operators", "/operators"}, Crumb{"Rotate secret", ""})
}

func (s *Server) operatorsRotate(w http.ResponseWriter, r *http.Request) {
	a, ok := s.screenAuth(w, r)
	if !ok {
		return
	}
	if _, ok := s.screenForm(w, r, a); !ok {
		return
	}
	if err := s.cfg.Backend.RotateRegistrationSecret(r.Context(), a.P, service.SourceGUI); err != nil {
		s.backendFail(w, r, err)
		return
	}
	s.Redirect(w, r, "/operators", "operators.rotated", "")
}

// findOperator returns the active operator named by the path, or answers 404.
func (s *Server) findOperator(w http.ResponseWriter, r *http.Request, a adminReq) (string, bool) {
	name := r.PathValue("name")
	if !store.ValidOperatorName(name) {
		s.ErrorPage(w, r, http.StatusNotFound)
		return "", false
	}
	ops, err := s.cfg.Backend.ListOperators(r.Context(), a.P, service.SourceGUI, false)
	if err != nil {
		s.backendFail(w, r, err)
		return "", false
	}
	for _, o := range ops {
		if o.Name == name && o.Status == store.OperatorActive {
			return name, true
		}
	}
	s.ErrorPage(w, r, http.StatusNotFound)
	return "", false
}

func (s *Server) operatorsRevokeForm(w http.ResponseWriter, r *http.Request) {
	a, ok := s.screenAuth(w, r)
	if !ok {
		return
	}
	name, ok := s.findOperator(w, r, a)
	if !ok {
		return
	}
	s.revokeDialog(w, r, a, http.StatusOK, name, "", "")
}

func (s *Server) revokeDialog(w http.ResponseWriter, r *http.Request, a adminReq, status int, name, typed, errText string) {
	s.operatorsPage(w, r, a, status, operatorsData{View: "revoke", Name: name, Typed: typed, Err: errText,
		Action: "/operators/" + PathEscape(name) + "/revoke", CancelU: "/operators"},
		"Revoke operator "+name, Crumb{"Operators", "/operators"}, Crumb{"Revoke " + name, ""})
}

func (s *Server) operatorsRevoke(w http.ResponseWriter, r *http.Request) {
	a, ok := s.screenAuth(w, r)
	if !ok {
		return
	}
	form, ok := s.screenForm(w, r, a)
	if !ok {
		return
	}
	name, ok := s.findOperator(w, r, a)
	if !ok {
		return
	}
	typed := strings.TrimSpace(form.Get("confirm"))
	if typed != name {
		s.revokeDialog(w, r, a, http.StatusUnprocessableEntity, name, typed, "Type the operator name exactly to confirm.")
		return
	}
	if _, err := s.cfg.Backend.RevokeOperator(r.Context(), a.P, service.SourceGUI, name); err != nil {
		s.backendFail(w, r, err)
		return
	}
	s.Redirect(w, r, "/operators", "operators.revoked", name)
}
