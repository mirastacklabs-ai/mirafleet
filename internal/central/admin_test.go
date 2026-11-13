//go:build unix

package central

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
)

// adminSocket starts the admin socket of e in a short temp dir (unix socket paths are length
// limited) and returns its path.
func adminSocket(t *testing.T, e *env) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cf-adm")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "admin.sock")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	must(t, e.srv.ServeAdmin(ctx, sock))
	return sock
}

func TestAdminOversizedRequestRefusedBeforeDecoding(t *testing.T) {
	e := newEnv(t)
	sock := adminSocket(t, e)
	c, err := net.Dial("unix", sock)
	must(t, err)
	defer c.Close()
	// Only a header announcing too many bytes: the server must answer without waiting for (or
	// decoding) a payload.
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], api.MaxRequestBytes+1)
	_, err = c.Write(hdr[:])
	must(t, err)
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	b, err := api.ReadFrame(c, api.MaxResponseBytes)
	must(t, err)
	if !strings.Contains(string(b), api.CodeTooLarge) {
		t.Fatalf("want too_large, got %s", b)
	}
}

func TestAdminSilentClientDropped(t *testing.T) {
	old := api.RequestTimeout
	api.RequestTimeout = 150 * time.Millisecond
	t.Cleanup(func() { api.RequestTimeout = old }) // runs after Server.Close, which waits for admin calls
	e := newEnv(t)
	sock := adminSocket(t, e)
	c, err := net.Dial("unix", sock)
	must(t, err)
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	start := time.Now()
	buf := make([]byte, 1)
	_, err = c.Read(buf) // server error frame or EOF; never our own 5 s deadline
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("silent client was not dropped")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("dropped after %s", time.Since(start))
	}
}

func TestAdminMethodsEndToEnd(t *testing.T) {
	e := newEnv(t, "r1")
	must(t, e.svc.InitRegistrationSecret(context.Background(), e.admin, service.SourceCLI))
	e.addRunner("pend", store.StatusPending)
	e.addOperator("opx")
	sock := adminSocket(t, e)
	call := func(method string, params, result any) error { return AdminCall(sock, method, params, result) }

	// fleet.create, then it shows in fleets.list.
	var fc api.FleetCreateResult
	must(t, call(api.MethodFleetCreate, api.FleetCreateParams{Name: "f2", Confirm: true}, &fc))
	if fc.Fleet.Name != "f2" || fc.Fleet.Mode != "actions" || !fc.Fleet.Confirm {
		t.Fatalf("fleet.create: %+v", fc)
	}
	wantCode(t, call(api.MethodFleetCreate, api.FleetCreateParams{Name: "f2"}, nil), api.CodeConflict)
	var fl api.FleetsListResult
	must(t, call(api.MethodFleetsList, nil, &fl))
	if len(fl.Fleets) != 2 {
		t.Fatalf("fleets.list: %+v", fl)
	}

	// token.create returns the token once; token.list never shows a secret.
	var tc api.TokenCreateResult
	must(t, call(api.MethodTokenCreate, api.TokenCreateParams{Fleet: "f2", Uses: 2, TTLS: 3600, Labels: map[string]string{"k": "v"}}, &tc))
	if !strings.HasPrefix(tc.Token, "mfj1.") || len(tc.ID) != 16 {
		t.Fatalf("token.create: %+v", tc)
	}
	var tl api.TokenListResult
	must(t, call(api.MethodTokenList, api.TokenListParams{Fleet: "f2"}, &tl))
	if len(tl.Tokens) != 1 || tl.Tokens[0].ID != tc.ID || tl.Tokens[0].MaxUses != 2 || tl.Tokens[0].Revoked {
		t.Fatalf("token.list: %+v", tl)
	}
	// Check the raw response too: the secret part of the token must not appear anywhere.
	secretPart := tc.Token[strings.LastIndex(tc.Token, ".")+1:]
	var raw map[string]any
	must(t, call(api.MethodTokenList, nil, &raw))
	if s := toJSON(t, raw); strings.Contains(s, secretPart) || strings.Contains(s, "mfj1.") {
		t.Fatalf("token.list leaks the secret: %s", s)
	}
	must(t, call(api.MethodTokenRevoke, api.TokenRevokeParams{ID: tc.ID}, nil))
	must(t, call(api.MethodTokenList, nil, &tl))
	if !tl.Tokens[len(tl.Tokens)-1].Revoked {
		t.Fatalf("token not revoked: %+v", tl)
	}
	wantCode(t, call(api.MethodTokenRevoke, api.TokenRevokeParams{ID: "zz"}, nil), api.CodeBadRequest)

	// runner.lifecycle: approve, disable, enable, revoke.
	for _, step := range []struct{ op, status string }{
		{api.OpApprove, "active"}, {api.OpDisable, "disabled"}, {api.OpEnable, "active"}, {api.OpRevoke, "revoked"},
	} {
		var ri api.RunnerInfo
		must(t, call(api.MethodRunnerLifecycle, api.RunnerLifecycleParams{Fleet: testFleet, Runner: "pend", Op: step.op}, &ri))
		if ri.Name != "pend" || ri.Status != step.status {
			t.Fatalf("%s: %+v", step.op, ri)
		}
	}
	wantCode(t, call(api.MethodRunnerLifecycle, api.RunnerLifecycleParams{Fleet: testFleet, Runner: "pend", Op: api.OpApprove}, nil), api.CodeNotFound)
	wantCode(t, call(api.MethodRunnerLifecycle, api.RunnerLifecycleParams{Fleet: testFleet, Runner: "r1", Op: "explode"}, nil), api.CodeBadRequest)
	var rl api.RunnersListResult
	must(t, call(api.MethodRunnersList, api.RunnersListParams{Fleet: testFleet}, &rl))
	if len(rl.Runners) != 1 || rl.Runners[0].Name != "r1" {
		t.Fatalf("runners.list: %+v", rl)
	}

	// operator.code and operator.rotate_secret.
	var c1, c2 api.OperatorCodeResult
	must(t, call(api.MethodOperatorCode, nil, &c1))
	if !strings.HasPrefix(c1.Code, "mfr1.") || c1.SecondsLeft < 1 || c1.SecondsLeft > 60 || c1.HostFingerprint == "" {
		t.Fatalf("operator.code: %+v", c1)
	}
	must(t, call(api.MethodOperatorRotateSecret, nil, nil))
	must(t, call(api.MethodOperatorCode, nil, &c2))
	if c1.Code == c2.Code {
		t.Fatal("rotating the secret did not change the code")
	}

	// operator.list / operator.revoke.
	var ol api.OperatorListResult
	must(t, call(api.MethodOperatorList, nil, &ol))
	if len(ol.Operators) != 1 || ol.Operators[0].Name != "opx" || ol.Operators[0].Status != "active" {
		t.Fatalf("operator.list: %+v", ol)
	}
	must(t, call(api.MethodOperatorRevoke, api.OperatorRevokeParams{Name: "opx"}, nil))
	must(t, call(api.MethodOperatorList, nil, &ol))
	if ol.Operators[0].Status != "revoked" {
		t.Fatalf("operator not revoked: %+v", ol)
	}
	wantCode(t, call(api.MethodOperatorRevoke, api.OperatorRevokeParams{Name: "nobody"}, nil), api.CodeNotFound)

	// audit.list: paging, source cli and the local admin actor.
	var all api.AuditListResult
	must(t, call(api.MethodAuditList, nil, &all))
	if len(all.Events) < 10 {
		t.Fatalf("audit.list: %d events", len(all.Events))
	}
	var page api.AuditListResult
	must(t, call(api.MethodAuditList, api.AuditListParams{SinceID: all.Events[2].ID, Limit: 3}, &page))
	if len(page.Events) != 3 || page.Events[0].ID != all.Events[3].ID {
		t.Fatalf("audit paging: %+v", page.Events)
	}
	wantCode(t, call(api.MethodAuditList, api.AuditListParams{Limit: -1}, nil), api.CodeBadRequest)
	var cli int
	for _, ev := range all.Events {
		if ev.Source != "system" && (ev.Action == api.MethodFleetCreate || ev.Action == "runner.approve" || ev.Action == api.MethodOperatorCode) {
			if ev.Source != "cli" || ev.ActorKind != "admin" || ev.Actor != e.admin.Name {
				t.Fatalf("audit row not attributed to the local admin: %+v", ev)
			}
			cli++
		}
		if strings.Contains(toJSON(t, ev), c1.Code) || strings.Contains(toJSON(t, ev), tc.Token) {
			t.Fatalf("secret in audit row: %+v", ev)
		}
	}
	if cli != 4 { // fleet.create, runner.approve, operator.code x2
		t.Fatalf("want 4 cli rows, got %d", cli)
	}
}

func TestAdminSocketMethodSet(t *testing.T) {
	e := newEnv(t, "r1")
	sock := adminSocket(t, e)
	for _, m := range []string{api.MethodEnrollJoin, api.MethodEnrollRegister, api.MethodHello} {
		wantCode(t, AdminCall(sock, m, map[string]string{}, nil), api.CodeForbidden)
	}
	wantCode(t, AdminCall(sock, "no.such", nil, nil), api.CodeBadRequest)
	// Unknown params are refused by the strict decoder.
	wantCode(t, AdminCall(sock, api.MethodFleetCreate, map[string]any{"name": "x", "bogus": 1}, nil), api.CodeBadRequest)
}

func toJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	must(t, err)
	return string(b)
}
