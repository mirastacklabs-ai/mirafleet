package central

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/apiclient"
	"github.com/mirastacklabs-ai/mirafleet/internal/keys"
	"github.com/mirastacklabs-ai/mirafleet/internal/policy"
	"github.com/mirastacklabs-ai/mirafleet/internal/runner"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/tokens"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// TestM2Done is the "M2 done when" scenario from docs/M2.md, in process:
// fleet -> token -> join (pinned from the token) -> approve (central) -> two operators register
// with the same current code (pinned from the code) -> list and run over api@ -> lifecycle refused
// on api@ -> revoke on central during the long action (cancelled, reconnect rejected, audited) ->
// join-token reuse refused, expired registration code refused.
func TestM2Done(t *testing.T) {
	e := newEnv(t)
	ctx := tctx(t)

	// Service with an injected clock (fixed inside a registration step, moved by tests).
	var offset atomic.Int64 // seconds
	base := time.Unix(tokens.Step(time.Now())*tokens.StepSeconds+10, 0)
	clock := func() time.Time { return base.Add(time.Duration(offset.Load()) * time.Second) }
	svc, err := service.New(service.Config{Store: e.db, Policy: policy.TablePolicy{}, Live: e.reg,
		HostKey: e.hostSigner.PublicKey(), Now: clock, OperatorConns: e.ops, Logf: e.logf})
	must(t, err)
	e.svc = svc
	e.startCentral("127.0.0.1:0") // serves the clocked service; the first server is unused
	must(t, e.svc.InitRegistrationSecret(ctx, e.admin, service.SourceCLI))

	// Fleet and join token.
	const fleet = "m2"
	if _, err := e.svc.CreateFleet(ctx, e.admin, service.SourceCLI, fleet, store.ModeActions, false, nil); err != nil {
		t.Fatal(err)
	}
	tok, err := e.svc.CreateToken(ctx, e.admin, service.SourceCLI, service.TokenParams{Fleet: fleet})
	must(t, err)

	// Runner joins; central's host key is pinned from the token alone.
	_, err = keys.Generate(e.dir, "r1", "r1")
	must(t, err)
	jr, hk, err := apiclient.Join(ctx, e.addr, tok.Token, "r1", nil, e.signer("r1"))
	must(t, err)
	if jr.Status != "pending" || jr.Fleet != fleet {
		t.Fatalf("join: %+v", jr)
	}
	if string(hk.Marshal()) != string(e.hostSigner.PublicKey().Marshal()) {
		t.Fatal("join pinned another host key")
	}
	r, err := e.db.GetRunner(ctx, store.Ref{Fleet: fleet, Runner: "r1"})
	must(t, err)
	e.ids["r1"] = r.ID

	// Approve on central, then the runner comes online (audited for the long action below).
	if _, err := e.svc.Approve(ctx, e.admin, service.SourceCLI, fleet+"/r1"); err != nil {
		t.Fatal(err)
	}
	al, apath := openRunnerAudit(t)
	e.startRunner("r1", func(c *runner.Config) { c.Audit = al })
	e.waitOnline("r1")

	// Two operators register with the same current code (pinned from the code).
	code, err := e.svc.OperatorCode(ctx, e.admin, service.SourceCLI)
	must(t, err)
	var clients []*apiclient.Client
	for _, n := range []string{"opa", "opb"} {
		_, err := keys.Generate(e.dir, n, n)
		must(t, err)
		rr, rhk, err := apiclient.Register(ctx, e.addr, code.Code, n, e.signer(n))
		must(t, err)
		if rr.Operator != n || rr.Status != "active" {
			t.Fatalf("register %s: %+v", n, rr)
		}
		if string(rhk.Marshal()) != string(e.hostSigner.PublicKey().Marshal()) {
			t.Fatal("register pinned another host key")
		}
		clients = append(clients, e.opClient(n))
	}

	// List and run over api@.
	for _, cl := range clients {
		fl, err := cl.FleetsList(ctx)
		must(t, err)
		if len(fl.Fleets) < 1 {
			t.Fatalf("fleets: %+v", fl)
		}
		rl, err := cl.RunnersList(ctx, fleet)
		must(t, err)
		if len(rl.Runners) != 1 || rl.Runners[0].Name != "r1" || rl.Runners[0].Status != "active" || !rl.Runners[0].Online {
			t.Fatalf("runners: %+v", rl)
		}
		res, err := cl.ActionRun(ctx, api.ActionRunParams{Fleet: fleet, Runner: "r1", Action: "hello", Params: map[string]string{"name": "world"}})
		must(t, err)
		if res.ExitCode != 0 || strings.TrimSpace(res.Stdout) != "hello world" {
			t.Fatalf("run: %+v", res)
		}
	}

	// Lifecycle (and other admin/enrol) methods are refused on api@.
	for _, m := range []string{api.MethodRunnerLifecycle, api.MethodOperatorRevoke, api.MethodTokenCreate, api.MethodFleetCreate} {
		wantCode(t, clients[0].Call(ctx, m, map[string]string{"fleet": fleet, "runner": "r1", "op": api.OpRevoke}, nil), api.CodeForbidden)
	}
	if r, err = e.db.GetRunnerByID(ctx, r.ID); err != nil || r.Status != store.StatusActive {
		t.Fatalf("runner changed by api@: %+v %v", r, err)
	}

	// Revoke on central during the long action.
	callCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	starts := strings.Count(fileText(apath), `"outcome":"start"`) // earlier hello runs logged theirs
	res := make(chan error, 1)
	go func() {
		_, err := clients[0].ActionRun(callCtx, api.ActionRunParams{Fleet: fleet, Runner: "r1", Action: "long"})
		res <- err
	}()
	waitFor(t, func() bool { return strings.Count(fileText(apath), `"outcome":"start"`) > starts })
	if _, err := e.svc.Revoke(ctx, e.admin, service.SourceCLI, fleet+"/r1"); err != nil {
		t.Fatal(err)
	}
	err = waitErr(t, res, 2*time.Second)
	if err == nil {
		t.Fatal("long action survived revoke")
	}
	wantCode(t, err, api.CodeUnavailable)
	waitFor(t, func() bool {
		for _, row := range e.auditRowsFor(api.MethodActionRun) {
			if row.Outcome == service.OutcomeCancelled && row.Message == service.RevokeReason {
				return true
			}
		}
		return false
	})
	if rows := e.auditRowsFor("runner." + api.OpRevoke); len(rows) != 1 || rows[0].Source != "cli" || rows[0].Subject != fleet+"/r1" {
		t.Fatalf("revoke audit: %+v", rows)
	}
	time.Sleep(300 * time.Millisecond) // several reconnect attempts
	if len(e.online()) != 0 {
		t.Fatalf("revoked runner is online: %v", e.online())
	}
	if !e.authRejected(transport.UserRunner, "r1") {
		t.Fatal("revoked runner key authenticated")
	}

	// Join-token reuse is refused (single use, already consumed).
	_, err = keys.Generate(e.dir, "r2", "r2")
	must(t, err)
	_, _, err = apiclient.Join(ctx, e.addr, tok.Token, "r2", nil, e.signer("r2"))
	wantCode(t, err, api.CodeUnauthenticated)

	// An expired registration code is refused: move the clock past the 60 s step.
	offset.Add(tokens.StepSeconds + 1)
	_, err = keys.Generate(e.dir, "opc", "opc")
	must(t, err)
	_, _, err = apiclient.Register(ctx, e.addr, code.Code, "opc", e.signer("opc"))
	wantCode(t, err, api.CodeUnauthenticated)
	if _, err := e.db.GetOperatorByKey(ctx, e.signer("opc").PublicKey().Marshal()); err == nil {
		t.Fatal("expired code registered an operator")
	}
}
