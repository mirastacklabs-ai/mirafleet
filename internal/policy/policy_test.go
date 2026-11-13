package policy

import (
	"context"
	"errors"
	"testing"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
)

func TestTruthTable(t *testing.T) {
	actions := []string{
		api.MethodHello, api.MethodFleetsList, api.MethodRunnersList, api.MethodActionRun,
		api.MethodFleetCreate, api.MethodTokenCreate, api.MethodTokenList, api.MethodTokenRevoke,
		api.MethodRunnerLifecycle, api.MethodOperatorCode, api.MethodOperatorRotateSecret,
		api.MethodOperatorList, api.MethodOperatorRevoke, api.MethodAuditList,
		api.MethodEnrollJoin, api.MethodEnrollRegister, "", "bogus",
	}
	opAllowed := map[string]bool{api.MethodHello: true, api.MethodFleetsList: true, api.MethodRunnersList: true, api.MethodActionRun: true}
	enrollAllowed := map[string]bool{api.MethodEnrollJoin: true, api.MethodEnrollRegister: true}
	type who struct {
		name   string
		p      auth.Principal
		status string
		allow  func(string) bool
	}
	all := func(string) bool { return true }
	none := func(string) bool { return false }
	whos := []who{
		{"admin", auth.LocalAdmin(1), "", all},
		{"active operator", auth.Principal{Kind: auth.KindOperator, ID: 1}, "active", func(a string) bool { return opAllowed[a] }},
		{"revoked operator", auth.Principal{Kind: auth.KindOperator, ID: 1}, "revoked", none},
		{"unknown-status operator", auth.Principal{Kind: auth.KindOperator, ID: 1}, "", none},
		{"enroll", auth.Principal{Kind: auth.KindEnroll}, "", func(a string) bool { return enrollAllowed[a] }},
		{"runner", auth.Principal{Kind: auth.KindRunner, ID: 1}, "active", none},
		{"zero principal", auth.Principal{}, "active", none},
		{"junk kind", auth.Principal{Kind: "root"}, "active", none},
	}
	var pe PolicyEngine = TablePolicy{}
	for _, w := range whos {
		for _, a := range actions {
			err := pe.Authorize(context.Background(), w.p, a, Resource{ActorStatus: w.status})
			if w.allow(a) != (err == nil) {
				t.Errorf("%s / %q: err=%v", w.name, a, err)
			}
			if err != nil && !errors.Is(err, ErrForbidden) {
				t.Errorf("%s / %q: not ErrForbidden: %v", w.name, a, err)
			}
		}
	}
}
