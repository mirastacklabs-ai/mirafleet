package central

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/apiclient"
	"github.com/mirastacklabs-ai/mirafleet/internal/keys"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/tokens"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

func tctx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// addOperator registers an operator straight through the store (key named name in e.dir).
func (e *env) addOperator(name string) store.Operator {
	e.t.Helper()
	pub, err := keys.Generate(e.dir, name, name)
	must(e.t, err)
	pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pub))
	must(e.t, err)
	op, _, err := e.db.RegisterOperator(context.Background(), name, pk.Marshal(), ssh.FingerprintSHA256(pk), time.Now(), auditEv("operator.register"))
	must(e.t, err)
	return op
}

// opClient connects as operator with the key named name.
func (e *env) opClient(name string) *apiclient.Client {
	e.t.Helper()
	cl, err := apiclient.Dial(tctx(e.t), e.addr, transport.UserOperator, e.signer(name), ssh.FixedHostKey(e.hostSigner.PublicKey()))
	must(e.t, err)
	e.t.Cleanup(func() { cl.Close() })
	return cl
}

// rawCall sends one request on a fresh channel of chanType and returns the decoded response.
func rawCall(cl *ssh.Client, chanType, method string, params any) (*api.Response, error) {
	ch, reqs, err := cl.OpenChannel(chanType, nil)
	if err != nil {
		return nil, err
	}
	go ssh.DiscardRequests(reqs)
	defer ch.Close()
	req := api.Request{V: api.Version, ID: "t1", Method: method}
	if params != nil {
		req.Params, _ = json.Marshal(params)
	}
	b, _ := json.Marshal(req)
	if err := api.WriteFrame(ch, b); err != nil {
		return nil, err
	}
	raw, err := api.ReadFrame(ch, api.MaxResponseBytes)
	if err != nil {
		return nil, err
	}
	var resp api.Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Code != code {
		t.Fatalf("want api error %q, got %v", code, err)
	}
}

func waitSecondsLeft(min int) {
	if tokens.SecondsLeft(time.Now()) < min {
		time.Sleep(time.Duration(min+1) * time.Second)
	}
}

func TestEndToEndJoinRegisterAndRun(t *testing.T) {
	e := newEnv(t)
	ctx := tctx(t)
	must(t, e.svc.InitRegistrationSecret(ctx, e.admin, service.SourceCLI))

	// Runner joins with a token (host key pinned from the token), pending.
	tok, err := e.svc.CreateToken(ctx, e.admin, service.SourceCLI, service.TokenParams{Fleet: testFleet, Labels: map[string]string{"role": "newr"}})
	must(t, err)
	_, err = keys.Generate(e.dir, "newr", "newr")
	must(t, err)
	jr, hk, err := apiclient.Join(ctx, e.addr, tok.Token, "newr", nil, e.signer("newr"))
	must(t, err)
	if jr.Status != "pending" || jr.Fleet != testFleet || jr.Runner != "newr" {
		t.Fatalf("join: %+v", jr)
	}
	if string(hk.Marshal()) != string(e.hostSigner.PublicKey().Marshal()) {
		t.Fatal("central served another host key")
	}
	r, err := e.db.GetRunner(ctx, store.Ref{Fleet: testFleet, Runner: "newr"})
	must(t, err)
	if r.Status != store.StatusPending {
		t.Fatalf("status %s", r.Status)
	}
	e.ids["newr"] = r.ID
	e.startRunner("newr")
	e.waitOnline("newr")

	// Two operators register with the same code.
	waitSecondsLeft(5)
	code, err := e.svc.OperatorCode(ctx, e.admin, service.SourceCLI)
	must(t, err)
	var clients []*apiclient.Client
	for _, n := range []string{"opa", "opb"} {
		_, err := keys.Generate(e.dir, n, n)
		must(t, err)
		rr, _, err := apiclient.Register(ctx, e.addr, code.Code, n, e.signer(n))
		must(t, err)
		if rr.Operator != n || rr.Status != "active" {
			t.Fatalf("register: %+v", rr)
		}
		clients = append(clients, e.opClient(n))
	}

	for i, cl := range clients {
		h, err := cl.Hello(ctx, "test")
		must(t, err)
		if h.Operator != []string{"opa", "opb"}[i] || len(h.Versions) == 0 {
			t.Fatalf("hello: %+v", h)
		}
		fl, err := cl.FleetsList(ctx)
		must(t, err)
		if len(fl.Fleets) != 1 || fl.Fleets[0].Name != testFleet || fl.Fleets[0].Runners != 1 {
			t.Fatalf("fleets: %+v", fl)
		}
		rl, err := cl.RunnersList(ctx, testFleet)
		must(t, err)
		if len(rl.Runners) != 1 || rl.Runners[0].Name != "newr" || rl.Runners[0].Status != "pending" || !rl.Runners[0].Online {
			t.Fatalf("runners: %+v", rl)
		}
		b, _ := json.Marshal(rl)
		for _, bad := range []string{"pubkey", "PubKey", "token", "secret", "fingerprint", "ssh-ed25519", "join_token"} {
			if strings.Contains(strings.ToLower(string(b)), strings.ToLower(bad)) {
				t.Fatalf("runners.list leaks %q: %s", bad, b)
			}
		}
	}

	// Pending runner: action.run refused. Approve centrally, then it works.
	run := api.ActionRunParams{Fleet: testFleet, Runner: "newr", Action: "hello", Params: map[string]string{"name": "world"}}
	_, err = clients[0].ActionRun(ctx, run)
	wantCode(t, err, api.CodeConflict)
	_, err = e.svc.Approve(ctx, e.admin, service.SourceCLI, "f1/newr")
	must(t, err)
	for _, cl := range clients {
		res, err := cl.ActionRun(ctx, run)
		must(t, err)
		if res.ExitCode != 0 || strings.TrimSpace(res.Stdout) != "hello world" {
			t.Fatalf("run: %+v", res)
		}
	}
	// Audit rows carry source api.
	found := false
	for _, ev := range e.auditRows() {
		if ev.Action == api.MethodActionRun && ev.Source == "api" && ev.Actor == "opa" {
			found = true
		}
	}
	if !found {
		t.Fatal("no api-sourced action.run audit row")
	}
	// D-02: register wrote last_seen and info.
	r, err = e.db.GetRunnerByID(ctx, r.ID)
	must(t, err)
	if r.LastSeenAt.IsZero() || r.Info["version"] != "test" || r.Info["os"] == "" {
		t.Fatalf("runner last_seen/info not stored: %+v", r)
	}
	op, err := e.db.GetOperatorByKey(ctx, e.signer("opa").PublicKey().Marshal())
	must(t, err)
	if op.LastSeenAt.IsZero() {
		t.Fatal("operator last_seen not stored on connect")
	}
}

func TestAPIAdminAndEnrollMethodsForbidden(t *testing.T) {
	e := newEnv(t, "r1")
	e.addOperator("op")
	cl := e.opClient("op")
	ctx := tctx(t)
	methods := []string{
		api.MethodFleetCreate, api.MethodTokenCreate, api.MethodTokenList, api.MethodTokenRevoke,
		api.MethodRunnerLifecycle, api.MethodOperatorCode, api.MethodOperatorRotateSecret,
		api.MethodOperatorList, api.MethodOperatorRevoke, api.MethodAuditList,
		api.MethodEnrollJoin, api.MethodEnrollRegister,
	}
	before := len(e.auditRows())
	for _, m := range methods {
		err := cl.Call(ctx, m, map[string]string{"fleet": "x", "name": "x"}, nil)
		wantCode(t, err, api.CodeForbidden)
		if err := cl.Call(ctx, m, nil, nil); err == nil {
			t.Fatalf("%s succeeded on api@", m)
		}
	}
	wantCode(t, cl.Call(ctx, "no.such.method", nil, nil), api.CodeBadRequest)
	// Nothing was dispatched: no new rows, no fleet created.
	if n := len(e.auditRows()); n != before {
		t.Fatalf("audit grew by %d on forbidden methods", n-before)
	}
	fl, err := cl.FleetsList(ctx)
	must(t, err)
	if len(fl.Fleets) != 1 {
		t.Fatalf("fleets: %+v", fl)
	}
	// Strict params: unknown field is bad_request.
	wantCode(t, cl.Call(ctx, api.MethodRunnersList, map[string]string{"fleet": testFleet, "extra": "1"}, nil), api.CodeBadRequest)
}

func TestRoleChannelIsolation(t *testing.T) {
	e := newEnv(t, "r1")
	e.addOperator("op")
	e.startRunner("r1")
	e.waitOnline("r1")
	ctx := tctx(t)

	// A revoked operator key cannot authenticate.
	e.addOperator("gone")
	_, err := e.db.RevokeOperator(ctx, "gone", time.Now(), auditEv("operator.revoke"))
	must(t, err)
	if _, err := e.dial(transport.UserOperator, "gone"); err == nil {
		t.Fatal("revoked operator key authenticated")
	}

	// A runner key cannot open api@ (nor authenticate as operator).
	rc, err := e.dial(transport.UserRunner, "r1")
	must(t, err)
	defer rc.Close()
	for _, typ := range []string{transport.ChanAPI, transport.ChanJoin, transport.ChanRegister} {
		_, _, err := rc.OpenChannel(typ, nil)
		var oe *ssh.OpenChannelError
		if !errors.As(err, &oe) {
			t.Fatalf("runner opened %s: %v", typ, err)
		}
	}
	if _, err := e.dial(transport.UserOperator, "r1"); err == nil {
		t.Fatal("runner key authenticated as operator")
	}

	// An operator cannot open join@/register@/exec@ and heartbeats are ignored.
	oc, err := e.dial(transport.UserOperator, "op")
	must(t, err)
	defer oc.Close()
	for _, typ := range []string{transport.ChanJoin, transport.ChanRegister, transport.ChanExec, "session"} {
		_, _, err := oc.OpenChannel(typ, nil)
		var oe *ssh.OpenChannelError
		if !errors.As(err, &oe) || oe.Reason != ssh.UnknownChannelType {
			t.Fatalf("operator opened %s: %v", typ, err)
		}
	}
	hb, _ := json.Marshal(transport.Heartbeat{Hostname: "x"})
	if ok, _, err := oc.SendRequest(transport.ReqHeartbeat, true, hb); err != nil || ok {
		t.Fatalf("operator heartbeat: %v %v", ok, err)
	}
	// Operator key cannot be an enroll-less runner: no registry session appeared.
	if len(e.online()) != 1 {
		t.Fatalf("online: %v", e.online())
	}

	// An enroll connection cannot open api@.
	ec, err := e.dial(transport.UserEnroll, "op")
	must(t, err)
	defer ec.Close()
	_, _, err = ec.OpenChannel(transport.ChanAPI, nil)
	var oe *ssh.OpenChannelError
	if !errors.As(err, &oe) {
		t.Fatalf("enroll opened api@: %v", err)
	}
}

func TestEnrollConnectionRules(t *testing.T) {
	e := newEnv(t)
	_, err := keys.Generate(e.dir, "anon", "anon")
	must(t, err)

	t.Run("non-enroll method rejected, mismatch rejected", func(t *testing.T) {
		cl, err := e.dial(transport.UserEnroll, "anon")
		must(t, err)
		defer cl.Close()
		resp, err := rawCall(cl, transport.ChanRegister, api.MethodFleetsList, nil)
		must(t, err)
		if resp.Error == nil || resp.Error.Code != api.CodeForbidden {
			t.Fatalf("resp: %+v", resp)
		}
		cl2, err := e.dial(transport.UserEnroll, "anon")
		must(t, err)
		defer cl2.Close()
		resp, err = rawCall(cl2, transport.ChanRegister, api.MethodEnrollJoin, api.EnrollJoinParams{Token: "x", Name: "x"})
		must(t, err)
		if resp.Error == nil || resp.Error.Code != api.CodeBadRequest {
			t.Fatalf("resp: %+v", resp)
		}
	})

	t.Run("one channel only, connection closed after response", func(t *testing.T) {
		cl, err := e.dial(transport.UserEnroll, "anon")
		must(t, err)
		defer cl.Close()
		ch, reqs, err := cl.OpenChannel(transport.ChanJoin, nil)
		must(t, err)
		go ssh.DiscardRequests(reqs)
		defer ch.Close()
		_, _, err = cl.OpenChannel(transport.ChanJoin, nil)
		var oe *ssh.OpenChannelError
		if !errors.As(err, &oe) {
			t.Fatalf("second channel accepted: %v", err)
		}
		// Finish the first: a bad token gets the identical refusal, then the connection ends.
		b, _ := json.Marshal(api.Request{V: 1, ID: "a", Method: api.MethodEnrollJoin,
			Params: json.RawMessage(`{"token":"junk","name":"x"}`)})
		must(t, api.WriteFrame(ch, b))
		raw, err := api.ReadFrame(ch, api.MaxResponseBytes)
		must(t, err)
		var resp api.Response
		must(t, json.Unmarshal(raw, &resp))
		if resp.Error == nil || resp.Error.Code != api.CodeUnauthenticated {
			t.Fatalf("resp: %+v", resp)
		}
		done := make(chan struct{})
		go func() { cl.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("connection not closed after the response")
		}
	})

	t.Run("bad channel type closes", func(t *testing.T) {
		cl, err := e.dial(transport.UserEnroll, "anon")
		must(t, err)
		defer cl.Close()
		if _, _, err := cl.OpenChannel(transport.ChanAPI, nil); err == nil {
			t.Fatal("api@ accepted")
		}
		done := make(chan struct{})
		go func() { cl.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("connection not closed")
		}
	})

	t.Run("join and register failures are not audited", func(t *testing.T) {
		before := len(e.auditRows())
		cl, err := e.dial(transport.UserEnroll, "anon")
		must(t, err)
		defer cl.Close()
		_, err = rawCall(cl, transport.ChanJoin, api.MethodEnrollJoin, api.EnrollJoinParams{Token: "mfj1.bad", Name: "x"})
		must(t, err)
		if n := len(e.auditRows()); n != before {
			t.Fatalf("failed join audited (%d new rows)", n-before)
		}
	})
}

func TestEnrollDeadline(t *testing.T) {
	e := newEnvOpts(t, 15*time.Second, func(c *Config) { c.EnrollDeadline = 300 * time.Millisecond })
	_, err := keys.Generate(e.dir, "anon", "anon")
	must(t, err)
	start := time.Now()
	cl, err := e.dial(transport.UserEnroll, "anon")
	must(t, err)
	defer cl.Close()
	done := make(chan struct{})
	go func() { cl.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("idle enroll connection not closed by deadline")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("closed after %s", time.Since(start))
	}
}

func TestEnrollRateLimit(t *testing.T) {
	e := newEnvOpts(t, 15*time.Second, func(c *Config) { c.EnrollRatePerMin = 3 })
	_, err := keys.Generate(e.dir, "anon", "anon")
	must(t, err)
	for i := 0; i < 3; i++ {
		cl, err := e.dial(transport.UserEnroll, "anon")
		must(t, err)
		cl.Close()
	}
	// 4th: handshake may complete but the connection is closed right after.
	cl, err := e.dial(transport.UserEnroll, "anon")
	if err == nil {
		defer cl.Close()
		done := make(chan struct{})
		go func() { cl.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("rate-limited connection stayed open")
		}
	}
	waitFor(t, func() bool { return strings.Contains(e.logText(), "rate limit exceeded") })
}

func TestIPLimiterRefill(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newIPLimiter(2, func() time.Time { return now })
	if !l.allow("1.2.3.4:1") || !l.allow("1.2.3.4:2") || l.allow("1.2.3.4:3") {
		t.Fatal("burst of 2 expected")
	}
	if !l.allow("5.6.7.8:1") {
		t.Fatal("other IP must have its own bucket")
	}
	now = now.Add(31 * time.Second) // 1 token back
	if !l.allow("1.2.3.4:4") || l.allow("1.2.3.4:5") {
		t.Fatal("refill of one token expected")
	}
}

func TestAPIChannelCap(t *testing.T) {
	e := newEnv(t)
	e.addOperator("op")
	cl, err := e.dial(transport.UserOperator, "op")
	must(t, err)
	defer cl.Close()
	var chans []ssh.Channel
	for i := 0; i < api.MaxChannelsPerConn; i++ {
		ch, reqs, err := cl.OpenChannel(transport.ChanAPI, nil)
		if err != nil {
			t.Fatalf("channel %d: %v", i+1, err)
		}
		go ssh.DiscardRequests(reqs)
		chans = append(chans, ch)
	}
	_, _, err = cl.OpenChannel(transport.ChanAPI, nil)
	var oe *ssh.OpenChannelError
	if !errors.As(err, &oe) || oe.Reason != ssh.ResourceShortage {
		t.Fatalf("9th channel: %v", err)
	}
	// Closing one frees a slot.
	chans[0].Close()
	waitFor(t, func() bool {
		ch, reqs, err := cl.OpenChannel(transport.ChanAPI, nil)
		if err != nil {
			return false
		}
		go ssh.DiscardRequests(reqs)
		chans = append(chans, ch)
		return true
	})
	for _, ch := range chans {
		ch.Close()
	}
}

func TestRevokeOperatorKillsInflightAction(t *testing.T) {
	e := newEnv(t, "r1")
	e.addOperator("op")
	e.startRunner("r1")
	e.waitOnline("r1")
	cl := e.opClient("op")
	ctx := tctx(t)

	errc := make(chan error, 1)
	go func() {
		_, err := cl.ActionRun(ctx, api.ActionRunParams{Fleet: testFleet, Runner: "r1", Action: "long"})
		errc <- err
	}()
	waitFor(t, func() bool {
		for _, ev := range e.auditRows() {
			if ev.Action == api.MethodActionRun && ev.Outcome == service.OutcomeStart {
				return true
			}
		}
		return false
	})
	_, err := e.svc.RevokeOperator(ctx, e.admin, service.SourceCLI, "op")
	must(t, err)
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("action survived operator revoke")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("in-flight action not cancelled")
	}
	waitFor(t, func() bool {
		for _, ev := range e.auditRows() {
			if ev.Action == api.MethodActionRun && ev.Outcome == service.OutcomeCancelled {
				return true
			}
		}
		return false
	})
	if _, err := cl.Hello(ctx, ""); err == nil {
		t.Fatal("revoked operator connection still works")
	}
	if _, err := e.dial(transport.UserOperator, "op"); err == nil {
		t.Fatal("revoked operator reconnected")
	}
}

func TestRunnerLastSeenOnHeartbeatInterval(t *testing.T) {
	e := newEnvOpts(t, 15*time.Second, func(c *Config) { c.TouchInterval = 60 * time.Millisecond }, "r1")
	e.startRunner("r1")
	e.waitOnline("r1")
	ctx := context.Background()
	var first time.Time
	waitFor(t, func() bool {
		r, err := e.db.GetRunnerByID(ctx, e.ids["r1"])
		if err != nil || r.LastSeenAt.IsZero() {
			return false
		}
		first = r.LastSeenAt
		return true
	})
	waitFor(t, func() bool {
		r, err := e.db.GetRunnerByID(ctx, e.ids["r1"])
		return err == nil && r.LastSeenAt.After(first)
	})
	// Disconnect writes again.
	r, _ := e.db.GetRunnerByID(ctx, e.ids["r1"])
	before := r.LastSeenAt
	e.reg.Kick(e.ids["r1"], "test")
	waitFor(t, func() bool {
		r, err := e.db.GetRunnerByID(ctx, e.ids["r1"])
		return err == nil && r.LastSeenAt.After(before)
	})
}
