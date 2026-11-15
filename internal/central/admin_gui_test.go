//go:build unix

package central

import (
	"context"
	"strings"
	"testing"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
)

// The M4 admin methods work on the admin socket and are refused on api@ for every principal.
func TestAdminGUIMethods(t *testing.T) {
	e := newEnv(t, "r1")
	sock := adminSocket(t, e)
	call := func(method string, params, result any) error { return AdminCall(sock, method, params, result) }

	var sc api.FleetSetConfirmResult
	must(t, call(api.MethodFleetSetConfirm, api.FleetSetConfirmParams{Fleet: testFleet, Confirm: true}, &sc))
	if sc.Fleet.Name != testFleet || !sc.Fleet.Confirm {
		t.Fatalf("set_confirm: %+v", sc)
	}
	wantCode(t, call(api.MethodFleetSetConfirm, api.FleetSetConfirmParams{Fleet: "nope", Confirm: true}, nil), api.CodeNotFound)
	wantCode(t, call(api.MethodFleetSetConfirm, map[string]any{"fleet": testFleet, "confirm": true, "bogus": 1}, nil), api.CodeBadRequest)

	var rs api.GUIResetAdminResult
	must(t, call(api.MethodGUIResetAdmin, struct{}{}, &rs))
	if !strings.HasPrefix(rs.SetupToken, service.SetupTokenPrefix) {
		t.Fatalf("reset_admin: %+v", rs)
	}
	wantCode(t, call(api.MethodGUIResetAdmin, map[string]any{"x": 1}, nil), api.CodeBadRequest)

	// audit.list through the service: new filters, source cli, and no token anywhere.
	var al api.AuditListResult
	must(t, call(api.MethodAuditList, api.AuditListParams{Action: "gui.", ActionPrefix: true}, &al))
	if len(al.Events) != 2 { // gui.reset_admin + gui.setup_token
		t.Fatalf("gui.* rows: %+v", al.Events)
	}
	for _, ev := range al.Events {
		if ev.Source != "cli" || strings.Contains(toJSON(t, ev), rs.SetupToken) {
			t.Fatalf("row: %+v", ev)
		}
	}
	must(t, call(api.MethodAuditList, api.AuditListParams{Action: api.MethodFleetSetConfirm, Fleet: testFleet, Actor: e.admin.Name}, &al))
	if len(al.Events) != 1 || al.Events[0].Params["to"] != "true" {
		t.Fatalf("set_confirm row: %+v", al.Events)
	}
	must(t, call(api.MethodAuditList, api.AuditListParams{Limit: 1}, &al))
	if len(al.Events) != 1 {
		t.Fatalf("limit: %+v", al.Events)
	}
	wantCode(t, call(api.MethodAuditList, api.AuditListParams{BeforeID: -1}, nil), api.CodeBadRequest)
}

func TestNewAdminMethodsRefusedOnAPIForEveryPrincipal(t *testing.T) {
	e := newEnv(t, "r1")
	ctx := context.Background()
	before := len(e.auditRows())
	for _, m := range []string{api.MethodGUIResetAdmin, api.MethodFleetSetConfirm} {
		if !api.IsAdminMethod(m) || api.IsOperatorMethod(m) || api.IsEnrollMethod(m) {
			t.Fatalf("%s misclassified", m)
		}
		if _, ok := operatorTable().handlers[m]; ok || operatorTable().allowed(m) {
			t.Fatalf("%s served on api@", m)
		}
		if _, ok := adminTable().handlers[m]; !ok || !adminTable().allowed(m) {
			t.Fatalf("%s not served on the admin socket", m)
		}
		for _, p := range []auth.Principal{
			{Kind: auth.KindOperator, ID: 1, Name: "op"}, {Kind: auth.KindAdmin, Name: "uid:0"},
			{Kind: auth.KindRunner, ID: 2, Name: "r"}, {Kind: auth.KindEnroll},
		} {
			c := &callCtx{p: p, src: service.SourceAPI, svc: e.svc, st: e.db, logf: func(string, ...any) {}, state: &connState{}}
			resp := operatorTable().serve(ctx, c, &api.Request{ID: "1", Method: m, Params: []byte(`{"fleet":"x"}`)})
			if resp.Error == nil || resp.Error.Code != api.CodeForbidden {
				t.Fatalf("%s as %s on api@: %+v", m, p.Kind, resp)
			}
		}
	}
	if n := len(e.auditRows()); n != before {
		t.Fatalf("audit grew by %d", n-before)
	}
}
