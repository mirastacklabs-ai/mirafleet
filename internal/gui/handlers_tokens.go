package gui

// Join tokens screens: list, create and revoke. Creating a token is one of the
// two POSTs that answer directly instead of redirecting, because they show a secret once:
//
//   - The form carries a signed nonce (32 random bytes, MACed with the session's CSRF key, valid
//     nonceTTL). The handler consumes it with an atomic MarkUsed before CreateToken, so a
//     resubmitted form (back button, refresh, double click) mints nothing and shows "already
//     created" instead.
//   - The response is Cache-Control: no-store and is the only place the token ever appears: it is
//     not stored, flashed or put in a URL, and the used-nonce set holds nonces only.

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
)

func init() {
	RegisterFlash("token.revoke", "warn", "Join token %s revoked.")
}

const (
	// A used nonce is remembered for nonceTTL after use: its signature expires nonceTTL after it
	// was issued, which is never later than that, so a forgotten nonce can no longer verify.
	nonceTTL = 30 * time.Minute
	nonceMax = 1024
)

// nonceSet remembers used nonces for nonceTTL, at most nonceMax of them. Live entries are never
// evicted (that would let a replay mint a second token): when the set is full of live nonces the
// create is refused instead. MarkUsed is the single atomic check-and-insert.
type nonceSet struct {
	mu    sync.Mutex
	used  map[string]time.Time
	order []string
}

// MarkUsed records nonce and reports (true, false), or (false, false) when it was already used,
// or (false, true) when the set is full of live nonces.
func (n *nonceSet) MarkUsed(nonce string, now time.Time) (ok, full bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.used == nil {
		n.used = map[string]time.Time{}
	}
	for len(n.order) > 0 && now.Sub(n.used[n.order[0]]) >= nonceTTL {
		delete(n.used, n.order[0])
		n.order = n.order[1:]
	}
	if _, dup := n.used[nonce]; dup {
		return false, false
	}
	if len(n.order) >= nonceMax {
		return false, true
	}
	n.used[nonce] = now
	n.order = append(n.order, nonce)
	return true, false
}

type selOpt struct {
	Value, Label string
	Sel          bool
}

type tokenListRow struct {
	ID, Fleet, Uses, State, Kind string
	Labels                       []string
	Auto                         bool
	Expires                      timeView
	RevokeURL                    string
}

type copyView struct{ ID, Value, Label string }

type tokenResult struct {
	ID, Command, Fleet string
	Copy               copyView // Value is the token: set only on the create response
	Expires            timeView
	Uses               int
	Replay             bool
}

type tokensData struct {
	View string // list | new | result | revoke

	// list
	Rows         []tokenListRow
	Fleets       []string
	FFleet       string
	ShowFinished bool
	NoMatch      bool

	// new
	Nonce    string
	FleetOpt []selOpt
	TTLOpt   []selOpt
	Labels   string
	Approve  bool
	Uses     string
	Errs     map[string]string
	NoFleets bool

	// result
	Res tokenResult

	// revoke
	TokenID, ActionURL, Fleet string
}

var tokenTTLs = []selOpt{
	{Value: "1h", Label: "1 hour"}, {Value: "6h", Label: "6 hours"}, {Value: "24h", Label: "24 hours"},
	{Value: "72h", Label: "3 days"}, {Value: "168h", Label: "7 days"}, {Value: "720h", Label: "30 days"},
}

const defaultTokenTTLOpt = "24h"

func (s *Server) routesTokens(mux adminMux) {
	mux.HandleFunc("GET /tokens", s.tokensList)
	mux.HandleFunc("GET /tokens/new", s.tokenNewForm)
	mux.HandleFunc("POST /tokens", s.tokenCreate)
	mux.HandleFunc("GET /tokens/{id}/revoke", s.tokenRevokeForm)
	mux.HandleFunc("POST /tokens/{id}/revoke", s.tokenRevoke)
}

func tokenState(t store.JoinToken, now time.Time) (text, kind string, usable bool) {
	switch {
	case !t.RevokedAt.IsZero():
		return "Revoked", "bad", false
	case t.Uses >= t.MaxUses:
		return "Used up", "off", false
	case !now.Before(t.ExpiresAt):
		return "Expired", "off", false
	}
	return "Usable", "ok", true
}

func (s *Server) tokensList(w http.ResponseWriter, r *http.Request) {
	a, ok := s.screenAuth(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	fFleet := q.Get("fleet")
	if fFleet != "" && !store.ValidFleetName(fFleet) {
		fFleet = ""
	}
	ctx, be := r.Context(), s.cfg.Backend
	fleets, err := be.ListFleets(ctx, a.P, service.SourceGUI)
	if err != nil {
		s.backendFail(w, r, err)
		return
	}
	toks, err := be.ListTokens(ctx, a.P, service.SourceGUI, fFleet)
	if err != nil {
		s.backendFail(w, r, err)
		return
	}
	d := tokensData{View: "list", FFleet: fFleet, ShowFinished: q.Get("finished") == "1"}
	for _, f := range fleets {
		d.Fleets = append(d.Fleets, f.Name)
	}
	now := s.now()
	for _, t := range toks {
		text, kind, usable := tokenState(t, now)
		if !usable && !d.ShowFinished {
			continue
		}
		row := tokenListRow{ID: t.TokenID, Fleet: t.Fleet, Uses: strconv.Itoa(t.Uses) + " / " + strconv.Itoa(t.MaxUses),
			State: text, Kind: kind, Labels: chips(t.Labels), Auto: t.AutoApprove, Expires: s.timeOf(t.ExpiresAt)}
		if t.RevokedAt.IsZero() {
			row.RevokeURL = "/tokens/" + t.TokenID + "/revoke"
		}
		d.Rows = append(d.Rows, row)
	}
	d.NoMatch = len(toks) > 0 && len(d.Rows) == 0
	pg := s.screenPage(w, r, a, "Join tokens", "tokens")
	pg.Data = d
	s.Render(w, r, http.StatusOK, "tokens", "tokens/list", pg)
}

// newTokenForm builds the form model with a fresh signed nonce.
func (s *Server) newTokenForm(w http.ResponseWriter, r *http.Request, a adminReq, fleet, labels, ttl, uses string, approve bool, errs map[string]string) (tokensData, bool) {
	fleets, err := s.cfg.Backend.ListFleets(r.Context(), a.P, service.SourceGUI)
	if err != nil {
		s.backendFail(w, r, err)
		return tokensData{}, false
	}
	raw, err := NewToken()
	if err != nil {
		s.logf("gui: token nonce: %v", err)
		s.ErrorPage(w, r, http.StatusInternalServerError)
		return tokensData{}, false
	}
	if ttl == "" {
		ttl = defaultTokenTTLOpt
	}
	if uses == "" {
		uses = "1"
	}
	d := tokensData{View: "new", Nonce: SignNonce(a.Key, raw, s.now(), nonceTTL), Labels: labels, Approve: approve,
		Uses: uses, Errs: errs, NoFleets: len(fleets) == 0}
	for _, f := range fleets {
		d.FleetOpt = append(d.FleetOpt, selOpt{Value: f.Name, Label: f.Name, Sel: f.Name == fleet})
	}
	for _, o := range tokenTTLs {
		o.Sel = o.Value == ttl
		d.TTLOpt = append(d.TTLOpt, o)
	}
	return d, true
}

func (s *Server) renderTokenForm(w http.ResponseWriter, r *http.Request, a adminReq, status int, fleet, labels, ttl, uses string, approve bool, errs map[string]string) {
	d, ok := s.newTokenForm(w, r, a, fleet, labels, ttl, uses, approve, errs)
	if !ok {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	pg := s.screenPage(w, r, a, "New join token", "tokens", Crumb{Text: "Join tokens", URL: "/tokens"}, Crumb{Text: "New"})
	pg.Data = d
	s.Render(w, r, status, "tokens", "tokens/form", pg)
}

func (s *Server) tokenNewForm(w http.ResponseWriter, r *http.Request) {
	a, ok := s.screenAuth(w, r)
	if !ok {
		return
	}
	fleet := r.URL.Query().Get("fleet")
	s.renderTokenForm(w, r, a, http.StatusOK, fleet, "", "", "", false, map[string]string{})
}

// parseLabels reads "key=value" lines.
func parseLabels(text string) (map[string]string, string) {
	out := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, found := strings.Cut(line, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !found || k == "" {
			return nil, "Write each label as key=value, one per line."
		}
		if _, dup := out[k]; dup {
			return nil, "Label " + inertRunnerText(k, 64) + " appears twice."
		}
		out[k] = v
	}
	return out, ""
}

func (s *Server) tokenCreate(w http.ResponseWriter, r *http.Request) {
	a, ok := s.screenAuth(w, r)
	if !ok {
		return
	}
	form, ok := s.screenForm(w, r, a)
	if !ok {
		return
	}
	fleet := strings.TrimSpace(form.Get("fleet"))
	labelText, ttlText, usesText := form.Get("labels"), form.Get("ttl"), strings.TrimSpace(form.Get("uses"))
	approve := form.Get("approve") == "1"
	redo := func(status int, errs map[string]string) {
		s.renderTokenForm(w, r, a, status, fleet, labelText, ttlText, usesText, approve, errs)
	}

	raw, nonceOK := OpenNonce(a.Key, form.Get("nonce"), s.now())
	if nonceOK && !validToken(raw) {
		nonceOK = false
	}
	if !nonceOK {
		redo(http.StatusUnprocessableEntity, map[string]string{"form": "This form expired or is not valid. Check the values and create the token again."})
		return
	}

	errs := map[string]string{}
	if !store.ValidFleetName(fleet) {
		errs["fleet"] = "Choose a fleet."
	}
	labels, lerr := parseLabels(labelText)
	if lerr != "" {
		errs["labels"] = lerr
	}
	uses, uerr := strconv.Atoi(usesText)
	if uerr != nil || uses < 1 || uses > service.MaxTokenUses {
		errs["uses"] = "Uses must be a number from 1 to " + strconv.Itoa(service.MaxTokenUses) + "."
	}
	var ttl time.Duration
	for _, o := range tokenTTLs {
		if o.Value == ttlText {
			ttl, _ = time.ParseDuration(o.Value)
		}
	}
	if ttl == 0 {
		errs["ttl"] = "Choose how long the token stays valid."
	}
	if len(errs) > 0 {
		redo(http.StatusUnprocessableEntity, errs)
		return
	}

	// The nonce is consumed before the token is minted: a second submit of the same form (or a
	// concurrent one) finds it used and mints nothing.
	fresh, full := s.nonces.MarkUsed(raw, s.now())
	if full {
		redo(http.StatusTooManyRequests, map[string]string{"form": "Too many tokens were created recently. Reload the form and try again in a few minutes."})
		return
	}
	if !fresh {
		w.Header().Set("Cache-Control", "no-store")
		pg := s.screenPage(w, r, a, "Join token", "tokens", Crumb{Text: "Join tokens", URL: "/tokens"}, Crumb{Text: "Created"})
		pg.Data = tokensData{View: "result", Res: tokenResult{Replay: true}}
		s.Render(w, r, http.StatusOK, "tokens", "tokens/result", pg)
		return
	}
	ct, err := s.cfg.Backend.CreateToken(r.Context(), a.P, service.SourceGUI,
		service.TokenParams{Fleet: fleet, Labels: labels, Approve: approve, Uses: uses, TTL: ttl})
	if err != nil {
		switch errCode(err) {
		case api.CodeBadRequest, api.CodeNotFound:
			redo(http.StatusUnprocessableEntity, map[string]string{"form": "The token was not created: " + inertRunnerText(errMsg(err), 200)})
		default:
			s.backendFail(w, r, err)
		}
		return
	}
	adv := s.cfg.SSHAdvertise
	if adv == "" {
		adv = "CENTRAL:2222"
	}
	w.Header().Set("Cache-Control", "no-store")
	pg := s.screenPage(w, r, a, "Join token created", "tokens", Crumb{Text: "Join tokens", URL: "/tokens"}, Crumb{Text: "Created"})
	pg.Data = tokensData{View: "result", Res: tokenResult{ID: ct.ID, Copy: copyView{ID: "token", Value: ct.Token, Label: "Copy token"}, Fleet: fleet, Uses: ct.MaxUses,
		Expires: s.timeOf(ct.Expires), Command: "mirafleet runner join " + adv + " -name NAME -token -"}}
	s.Render(w, r, http.StatusOK, "tokens", "tokens/result", pg)
}

func validTokenIDParam(id string) bool {
	if len(id) != 16 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (s *Server) findToken(w http.ResponseWriter, r *http.Request, a adminReq) (store.JoinToken, bool) {
	id := r.PathValue("id")
	if !validTokenIDParam(id) {
		s.ErrorPage(w, r, http.StatusNotFound)
		return store.JoinToken{}, false
	}
	toks, err := s.cfg.Backend.ListTokens(r.Context(), a.P, service.SourceGUI, "")
	if err != nil {
		s.backendFail(w, r, err)
		return store.JoinToken{}, false
	}
	for _, t := range toks {
		if t.TokenID == id {
			return t, true
		}
	}
	s.ErrorPage(w, r, http.StatusNotFound)
	return store.JoinToken{}, false
}

func (s *Server) tokenRevokeForm(w http.ResponseWriter, r *http.Request) {
	a, ok := s.screenAuth(w, r)
	if !ok {
		return
	}
	t, ok := s.findToken(w, r, a)
	if !ok {
		return
	}
	pg := s.screenPage(w, r, a, "Revoke join token", "tokens", Crumb{Text: "Join tokens", URL: "/tokens"}, Crumb{Text: "Revoke"})
	pg.Data = tokensData{View: "revoke", TokenID: t.TokenID, Fleet: t.Fleet, ActionURL: "/tokens/" + t.TokenID + "/revoke"}
	s.Render(w, r, http.StatusOK, "tokens", "tokens/revoke", pg)
}

func (s *Server) tokenRevoke(w http.ResponseWriter, r *http.Request) {
	a, ok := s.screenAuth(w, r)
	if !ok {
		return
	}
	if _, ok := s.screenForm(w, r, a); !ok {
		return
	}
	t, ok := s.findToken(w, r, a)
	if !ok {
		return
	}
	if err := s.cfg.Backend.RevokeToken(r.Context(), a.P, service.SourceGUI, t.TokenID); err != nil {
		s.backendFail(w, r, err)
		return
	}
	s.Redirect(w, r, "/tokens", "token.revoke", t.TokenID)
}
