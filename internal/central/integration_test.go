package central

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/actions"
	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/audit"
	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/central/registry"
	"github.com/mirastacklabs-ai/mirafleet/internal/keys"
	"github.com/mirastacklabs-ai/mirafleet/internal/policy"
	"github.com/mirastacklabs-ai/mirafleet/internal/runner"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/store/sqlite"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

const testActions = `
actions:
  hello:
    run: ["/bin/echo", "hello", "{{name}}"]
    params: { name: { pattern: "[a-z]{1,8}" } }
  fail:
    run: ["/usr/bin/false"]
  slow:
    run: ["/bin/sleep", "30"]
    timeout: 300ms
  long:
    run: ["/bin/sleep", "30"]
    timeout: 60s
`

const testFleet = "f1"

type env struct {
	t          *testing.T
	dir        string
	hostSigner ssh.Signer
	knownHosts string
	acts       *actions.Set
	addr       string
	srv        *Server
	db         *sqlite.DB
	reg        *registry.Registry
	svc        *service.Service
	ops        *OperatorConns
	admin      auth.Principal
	ids        map[string]int64 // runner name -> store id (test bookkeeping only)
	cfgMod     func(*Config)
	tokenN     int

	logMu sync.Mutex
	logs  strings.Builder
}

func (e *env) logf(f string, a ...any) {
	e.logMu.Lock()
	defer e.logMu.Unlock()
	fmt.Fprintf(&e.logs, f+"\n", a...)
}

func (e *env) logText() string {
	e.logMu.Lock()
	defer e.logMu.Unlock()
	return e.logs.String()
}

func auditEv(action string) store.AuditEvent {
	return store.AuditEvent{Source: "system", ActorKind: "system", Actor: "test", Action: action, Outcome: "ok"}
}

// newEnv builds a temp SQLite store with fleet f1 and one active runner per name (keys generated
// now, in e.dir), a registry with the default heartbeat window, the service and a started central.
func newEnv(t *testing.T, runnerNames ...string) *env {
	return newEnvHB(t, 15*time.Second, runnerNames...)
}

// newEnvHB is newEnv with a custom heartbeat interval (the registry's stale window is 3x it).
func newEnvHB(t *testing.T, hb time.Duration, runnerNames ...string) *env {
	return newEnvOpts(t, hb, nil, runnerNames...)
}

// newEnvOpts is newEnvHB with a hook that tweaks the central Config before it starts.
func newEnvOpts(t *testing.T, hb time.Duration, mod func(*Config), runnerNames ...string) *env {
	t.Helper()
	ctx := context.Background()
	e := &env{t: t, dir: t.TempDir(), cfgMod: mod, ids: map[string]int64{}, admin: auth.LocalAdmin(os.Getuid())}
	if _, err := keys.Generate(e.dir, "host", "host"); err != nil {
		t.Fatal(err)
	}
	var err error
	if e.hostSigner, err = keys.LoadSigner(filepath.Join(e.dir, "host")); err != nil {
		t.Fatal(err)
	}
	e.knownHosts = filepath.Join(e.dir, "known_hosts")
	must(t, os.WriteFile(e.knownHosts, []byte("* "+keys.PubLine(e.hostSigner.PublicKey(), "")+"\n"), 0o600))
	e.acts, err = actions.Parse([]byte(testActions))
	must(t, err)

	e.db, err = sqlite.Open(ctx, filepath.Join(t.TempDir(), "mirafleet.db"))
	must(t, err)
	t.Cleanup(func() { e.db.Close() })
	_, err = e.db.CreateFleet(ctx, store.Fleet{Name: testFleet, Mode: store.ModeActions}, time.Now(), auditEv("fleet.create"))
	must(t, err)

	e.reg, err = registry.New(registry.Options{StaleAfter: 3 * hb, Logf: e.logf})
	must(t, err)
	e.ops = NewOperatorConns()
	e.svc, err = service.New(service.Config{Store: e.db, Policy: policy.TablePolicy{}, Live: e.reg,
		HostKey: e.hostSigner.PublicKey(), Now: time.Now, OperatorConns: e.ops, Logf: e.logf})
	must(t, err)

	for _, n := range runnerNames {
		e.addRunner(n, store.StatusActive)
	}
	e.startCentral("127.0.0.1:0")
	return e
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// tryAddRunner generates a key pair named name in e.dir and enrols a runner with that name in
// fleet f1 directly through the store (join token + Enroll, with audit), then moves it to
// status. A failed enrolment is returned, not fatal.
func (e *env) tryAddRunner(name string, status store.Status) (store.Runner, error) {
	return e.tryAddRunnerKey(name, name, status)
}

// tryAddRunnerKey is tryAddRunner with the key file named keyName (also the e.ids key).
func (e *env) tryAddRunnerKey(keyName, name string, status store.Status) (store.Runner, error) {
	e.t.Helper()
	ctx := context.Background()
	pub, err := keys.Generate(e.dir, keyName, name)
	must(e.t, err)
	pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pub))
	must(e.t, err)
	e.tokenN++
	tokID := fmt.Sprintf("%016x", e.tokenN)
	secret := sha256.Sum256([]byte("secret-" + tokID))
	now := time.Now()
	_, err = e.db.CreateToken(ctx, store.JoinToken{TokenID: tokID, SecretHash: secret[:], Fleet: testFleet,
		AutoApprove: status != store.StatusPending, MaxUses: 1, ExpiresAt: now.Add(time.Hour), CreatedBy: "test"},
		now, auditEv("token.create"))
	must(e.t, err)
	r, err := e.db.Enroll(ctx, tokID, secret[:], now,
		store.Runner{Name: name, PubKey: pk.Marshal(), Fingerprint: ssh.FingerprintSHA256(pk)}, auditEv("runner.enrol"))
	if err != nil {
		return store.Runner{}, err
	}
	switch status {
	case store.StatusDisabled:
		r, err = e.db.SetRunnerStatus(ctx, r.ID, []store.Status{store.StatusActive}, status, now, auditEv("runner.disable"))
	case store.StatusRevoked:
		r, err = e.db.SetRunnerStatus(ctx, r.ID, []store.Status{store.StatusPending, store.StatusActive}, status, now, auditEv("runner.revoke"))
	}
	must(e.t, err)
	e.ids[keyName] = r.ID
	return r, nil
}

func (e *env) addRunner(name string, status store.Status) store.Runner {
	e.t.Helper()
	r, err := e.tryAddRunner(name, status)
	must(e.t, err)
	return r
}

func (e *env) startCentral(listen string) {
	e.t.Helper()
	cfg := Config{Listen: listen, HostKey: e.hostSigner, Store: e.db, Service: e.svc,
		Registry: e.reg, Operators: e.ops, Logf: e.logf}
	if e.cfgMod != nil {
		e.cfgMod(&cfg)
	}
	srv, err := New(cfg)
	must(e.t, err)
	addr, err := srv.Start()
	must(e.t, err)
	e.srv, e.addr = srv, addr
	e.t.Cleanup(srv.Close)
}

func (e *env) signer(name string) ssh.Signer {
	e.t.Helper()
	s, err := keys.LoadSigner(filepath.Join(e.dir, name))
	must(e.t, err)
	return s
}

// startRunner runs a runner with the key named name (in e.dir) until the test ends.
func (e *env) startRunner(name string, mod ...func(*runner.Config)) {
	e.t.Helper()
	cfg := runner.Config{
		Central: e.addr, Name: name, Signer: e.signer(name), KnownHosts: e.knownHosts, Actions: e.acts,
		Labels: map[string]string{"role": name}, Version: "test",
		HeartbeatEvery: 50 * time.Millisecond, HeartbeatTimeout: 10 * time.Second, BackoffMin: 20 * time.Millisecond, BackoffMax: 100 * time.Millisecond,
	}
	for _, m := range mod {
		m(&cfg)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { runner.New(cfg).Run(ctx); close(done) }()
	e.t.Cleanup(func() { cancel(); <-done })
}

// dial opens a raw SSH connection to central as user with the named key.
func (e *env) dial(user, keyName string) (*ssh.Client, error) {
	return ssh.Dial("tcp", e.addr, &ssh.ClientConfig{
		User: user, Auth: []ssh.AuthMethod{ssh.PublicKeys(e.signer(keyName))},
		HostKeyCallback: ssh.FixedHostKey(e.hostSigner.PublicKey()), Timeout: 5 * time.Second,
	})
}

func (e *env) session(name string) *registry.Session { return e.reg.Get(e.ids[name]) }

func (e *env) waitOnline(names ...string) {
	e.t.Helper()
	waitFor(e.t, func() bool {
		for _, n := range names {
			if e.session(n) == nil {
				return false
			}
		}
		return true
	})
}

// online lists the names of online runners, from the registry snapshot.
func (e *env) online() []string {
	var out []string
	for _, si := range e.reg.Snapshot() {
		if si.Online {
			out = append(out, si.Name)
		}
	}
	return out
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for condition")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func params(kv []string) map[string]string {
	p := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		p[kv[i]] = kv[i+1]
	}
	return p
}

// exec runs an action straight through the registry (the runner's guard is what is under test).
func (e *env) exec(runnerName, action string, kv ...string) (*transport.ExecResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return e.reg.Exec(ctx, e.ids[runnerName], transport.ExecRequest{Action: action, Params: params(kv)})
}

// run runs an action through the service as the local admin (policy, audit rows in the store).
func (e *env) run(runnerName, action string, kv ...string) (service.RunActionResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return e.svc.RunAction(ctx, e.admin, service.SourceCLI, testFleet, runnerName, action, params(kv), 0, "")
}

func (e *env) auditRows() []store.AuditEvent {
	e.t.Helper()
	rows, err := e.db.ListAudit(context.Background(), 0, 10000)
	must(e.t, err)
	return rows
}

func TestTwoRunnersActions(t *testing.T) {
	e := newEnv(t, "r1", "r2")
	e.startRunner("r1")
	e.startRunner("r2")
	e.waitOnline("r1", "r2")

	// Registry carries heartbeat data, keyed by store id.
	snap := e.reg.Snapshot()
	if len(snap) != 2 || snap[0].ID != e.ids["r1"] || snap[0].Name != "r1" || snap[0].Fleet != testFleet ||
		snap[0].Status != registry.StatusActive || snap[0].Info.Version != "test" ||
		snap[0].Info.Labels["role"] != "r1" || snap[0].Info.OS == "" {
		t.Fatalf("registry: %+v", snap)
	}

	for _, n := range []string{"r1", "r2"} {
		res, err := e.exec(n, "hello", "name", "world")
		if err != nil || res.Refused || res.ExitCode != 0 || strings.TrimSpace(res.Stdout) != "hello world" {
			t.Fatalf("%s: %+v %v", n, res, err)
		}
	}

	t.Run("nonzero exit", func(t *testing.T) {
		res, err := e.exec("r1", "fail")
		if err != nil || res.Refused || res.ExitCode != 1 {
			t.Fatalf("%+v %v", res, err)
		}
	})
	t.Run("unknown action refused", func(t *testing.T) {
		res, err := e.exec("r1", "rm-everything")
		if err != nil || !res.Refused {
			t.Fatalf("%+v %v", res, err)
		}
	})
	t.Run("bad param refused", func(t *testing.T) {
		for _, bad := range []string{"200; rm -rf /", "UPPER", "bob\n", ""} {
			res, err := e.exec("r2", "hello", "name", bad)
			if err != nil || !res.Refused || res.Stdout != "" {
				t.Fatalf("%q: %+v %v", bad, res, err)
			}
		}
	})
	t.Run("timeout kills", func(t *testing.T) {
		start := time.Now()
		res, err := e.exec("r1", "slow")
		if err != nil || !res.TimedOut || res.ExitCode != -1 || time.Since(start) > 5*time.Second {
			t.Fatalf("%+v %v after %v", res, err, time.Since(start))
		}
	})
	t.Run("unknown runner", func(t *testing.T) {
		if _, err := e.reg.Exec(context.Background(), 9999, transport.ExecRequest{Action: "hello"}); err == nil {
			t.Fatal("want error")
		}
	})
}

// Central audit through the service: every exec has a start row followed by its outcome row
// with the same request id (the audit is in the store, not a file).
func TestCentralAuditOutcomes(t *testing.T) {
	e := newEnv(t, "r1")
	e.startRunner("r1")
	e.waitOnline("r1")

	if res, err := e.run("r1", "hello", "name", "audit"); err != nil || strings.TrimSpace(res.Stdout) != "hello audit" {
		t.Fatalf("%+v %v", res, err)
	}
	if res, err := e.run("r1", "fail"); err != nil || res.ExitCode != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := e.run("r1", "rm-everything"); err == nil {
		t.Fatal("refused action must be an error")
	}
	if res, err := e.run("r1", "slow"); err != nil || res.ExitCode != -1 {
		t.Fatalf("%+v %v", res, err)
	}

	start := map[string]int64{}
	outcomes := map[string]bool{}
	for _, row := range e.auditRows() {
		if row.Action != api.MethodActionRun {
			continue
		}
		if row.Fleet != testFleet || row.Runner != "r1" || row.ActorKind != "admin" || row.Source != "cli" || row.RequestID == "" {
			t.Fatalf("attribution: %+v", row)
		}
		if row.Outcome == service.OutcomeStart {
			start[row.RequestID] = row.ID
			continue
		}
		if s, ok := start[row.RequestID]; !ok || s >= row.ID {
			t.Errorf("outcome %q row %d without an earlier start row (%d)", row.Outcome, row.ID, s)
		}
		outcomes[row.Outcome] = true
	}
	if len(start) != 4 {
		t.Errorf("want 4 start rows, got %d", len(start))
	}
	for _, o := range []string{service.OutcomeOK, service.OutcomeError, service.OutcomeTimeout} {
		if !outcomes[o] {
			t.Errorf("no %q audit outcome", o)
		}
	}
}

// An unknown key cannot authenticate as runner (or operator); as a full runner it never registers.
func TestUnknownKeyCannotAuthenticateAsRunner(t *testing.T) {
	e := newEnv(t, "r1")
	_, err := keys.Generate(e.dir, "mallory", "mallory")
	must(t, err)
	for _, user := range []string{transport.UserRunner, transport.UserOperator, "mallory"} {
		_, err = e.dial(user, "mallory")
		if err == nil || !strings.Contains(err.Error(), "unable to authenticate") {
			t.Fatalf("user %q: want auth failure, got %v", user, err)
		}
	}
	e.startRunner("mallory")
	time.Sleep(400 * time.Millisecond)
	if n := e.online(); len(n) != 0 {
		t.Fatalf("unauthorized runner registered: %v", n)
	}
}

// A runner key authenticates only as user runner, never as operator.
func TestRunnerKeyCannotAuthenticateAsOperator(t *testing.T) {
	e := newEnv(t, "r1")
	if _, err := e.dial(transport.UserOperator, "r1"); err == nil || !strings.Contains(err.Error(), "unable to authenticate") {
		t.Fatalf("runner key as operator: %v", err)
	}
	cl, err := e.dial(transport.UserRunner, "r1")
	must(t, err)
	cl.Close()
}

// A revoked runner key is rejected at authentication and never registers.
func TestRevokedRunnerKeyRejectedAtAuth(t *testing.T) {
	e := newEnv(t)
	e.addRunner("gone", store.StatusRevoked)
	if _, err := e.dial(transport.UserRunner, "gone"); err == nil || !strings.Contains(err.Error(), "unable to authenticate") {
		t.Fatalf("revoked key authenticated: %v", err)
	}
	e.startRunner("gone")
	time.Sleep(400 * time.Millisecond)
	if n := e.online(); len(n) != 0 {
		t.Fatalf("revoked runner registered: %v", n)
	}
}

// A pending runner heartbeats (visible in the snapshot) but exec is refused until approval;
// approving through the service reaches the live session.
func TestPendingRunnerHeartbeatsButCannotExec(t *testing.T) {
	e := newEnv(t)
	e.addRunner("p1", store.StatusPending)
	e.startRunner("p1")
	e.waitOnline("p1")
	snap := e.reg.Snapshot()
	if len(snap) != 1 || !snap[0].Online || snap[0].Status != registry.StatusPending || snap[0].Info.Version != "test" {
		t.Fatalf("snapshot: %+v", snap)
	}
	if _, err := e.exec("p1", "hello", "name", "x"); !errors.Is(err, registry.ErrPending) {
		t.Fatalf("pending runner exec: %v", err)
	}
	if _, err := e.run("p1", "hello", "name", "x"); err == nil {
		t.Fatal("service ran an action on a pending runner")
	}
	if _, err := e.svc.Approve(context.Background(), e.admin, service.SourceCLI, testFleet+"/p1"); err != nil {
		t.Fatal(err)
	}
	res, err := e.exec("p1", "hello", "name", "ok")
	if err != nil || strings.TrimSpace(res.Stdout) != "hello ok" {
		t.Fatalf("after approve: %+v %v", res, err)
	}
}

// A runner whose stored status changed before its first heartbeat registers with the CURRENT
// status (reloaded by id), not the one it authenticated with.
func TestRegisterUsesCurrentStatus(t *testing.T) {
	e := newEnv(t)
	e.addRunner("d1", store.StatusDisabled)
	e.startRunner("d1")
	e.waitOnline("d1")
	if _, err := e.exec("d1", "hello", "name", "x"); !errors.Is(err, registry.ErrDisabled) {
		t.Fatalf("disabled runner exec: %v", err)
	}
}

func TestRunnerPinsHostKey(t *testing.T) {
	e := newEnv(t, "r1")
	other, err := keys.Generate(t.TempDir(), "x", "x")
	must(t, err)
	must(t, os.WriteFile(e.knownHosts, []byte("* "+other+"\n"), 0o600)) // pin a different key
	e.startRunner("r1")
	time.Sleep(400 * time.Millisecond)
	if len(e.online()) != 0 {
		t.Fatal("runner connected to a central with an unpinned host key")
	}
}

func TestRunnerReconnectsAfterCentralRestart(t *testing.T) {
	e := newEnv(t, "r1")
	e.startRunner("r1")
	e.waitOnline("r1")
	if res, err := e.exec("r1", "hello", "name", "a"); err != nil || res.ExitCode != 0 {
		t.Fatalf("%+v %v", res, err)
	}

	addr := e.addr
	e.srv.Close()
	if _, err := e.exec("r1", "hello", "name", "a"); err == nil {
		t.Fatal("exec should fail while central is down")
	}
	e.startCentral(addr) // same address, same host key
	e.waitOnline("r1")
	res, err := e.exec("r1", "hello", "name", "again")
	if err != nil || strings.TrimSpace(res.Stdout) != "hello again" {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestRunnerReconnectsAfterDroppedConnection(t *testing.T) {
	e := newEnv(t, "r1")
	e.startRunner("r1")
	e.waitOnline("r1")
	old := e.session("r1")
	old.Conn().Close()
	waitFor(t, func() bool { s := e.session("r1"); return s != nil && s != old })
	res, err := e.exec("r1", "hello", "name", "back")
	if err != nil || strings.TrimSpace(res.Stdout) != "hello back" {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestChannelRestrictions(t *testing.T) {
	e := newEnv(t, "r1")
	e.startRunner("r1")
	e.waitOnline("r1")

	// Runner side: central tries other channel types over the runner's connection.
	conn := e.session("r1").Conn()
	for _, typ := range []string{"session", "direct-tcpip", "file@mirafleet", "x11"} {
		_, _, err := conn.OpenChannel(typ, nil)
		var oe *ssh.OpenChannelError
		if !errors.As(err, &oe) || oe.Reason != ssh.UnknownChannelType {
			t.Errorf("runner accepted %q: %v", typ, err)
		}
	}
	// workspace@mirafleet is a registered channel type: a runner without a workspace config
	// refuses it as Prohibited (not unknown, not accepted).
	_, _, werr := conn.OpenChannel(transport.ChanWorkspace, nil)
	var woe *ssh.OpenChannelError
	if !errors.As(werr, &woe) || woe.Reason != ssh.Prohibited {
		t.Errorf("runner without workspace config on %q: %v", transport.ChanWorkspace, werr)
	}
	// Malformed exec payload is rejected, not run.
	if _, _, err := conn.OpenChannel(transport.ChanExec, []byte("not json")); err == nil {
		t.Error("runner accepted malformed exec request")
	}

	// Central side: an authenticated runner key may open nothing toward central: not session or
	// forwarding, not exec@, and not the operator/enroll channels.
	cl, err := e.dial(transport.UserRunner, "r1")
	must(t, err)
	defer cl.Close()
	for _, typ := range []string{"session", "direct-tcpip", transport.ChanExec, transport.ChanAPI, transport.ChanJoin, transport.ChanRegister} {
		_, _, err := cl.OpenChannel(typ, nil)
		var oe *ssh.OpenChannelError
		if !errors.As(err, &oe) || oe.Reason != ssh.UnknownChannelType {
			t.Errorf("central accepted %q from a runner: %v", typ, err)
		}
	}
	// Unknown global requests are refused.
	if ok, _, err := cl.SendRequest("tcpip-forward", true, nil); err != nil || ok {
		t.Errorf("central accepted tcpip-forward: %v %v", ok, err)
	}
}

func TestChannelAllowList(t *testing.T) {
	cases := []struct {
		kind auth.Kind
		typ  string
		want bool
	}{
		{auth.KindRunner, transport.ChanAPI, false},
		{auth.KindRunner, transport.ChanJoin, false},
		{auth.KindRunner, transport.ChanRegister, false},
		{auth.KindRunner, transport.ChanExec, false},
		{auth.KindRunner, "session", false},
		{auth.KindOperator, transport.ChanAPI, true},
		{auth.KindOperator, transport.ChanJoin, false},
		{auth.KindOperator, transport.ChanRegister, false},
		{auth.KindOperator, transport.ChanExec, false},
		{auth.KindEnroll, transport.ChanJoin, true},
		{auth.KindEnroll, transport.ChanRegister, true},
		{auth.KindEnroll, transport.ChanAPI, false},
		{auth.KindEnroll, "session", false},
		{auth.KindAdmin, transport.ChanAPI, false},
		{auth.Kind("bogus"), transport.ChanAPI, false},
	}
	for _, c := range cases {
		if got := channelAllowed(c.kind, c.typ); got != c.want {
			t.Errorf("channelAllowed(%s, %s) = %v, want %v", c.kind, c.typ, got, c.want)
		}
	}
}

// MaxAuthTries is 3, and a valid key offered after many rejected ones never gets a chance (the
// x/crypto server counts rejected key probes toward the limit; with 8 it has cut us off).
func TestMaxAuthTries(t *testing.T) {
	e := newEnv(t, "r1")
	if got := e.srv.ssh.MaxAuthTries; got != 3 {
		t.Fatalf("MaxAuthTries = %d, want 3", got)
	}
	var bad []ssh.Signer
	for i := 0; i < 8; i++ {
		n := fmt.Sprintf("m%d", i)
		_, err := keys.Generate(e.dir, n, n)
		must(t, err)
		bad = append(bad, e.signer(n))
	}
	cfg := func(s ...ssh.Signer) *ssh.ClientConfig {
		return &ssh.ClientConfig{User: transport.UserRunner, Auth: []ssh.AuthMethod{ssh.PublicKeys(s...)},
			HostKeyCallback: ssh.FixedHostKey(e.hostSigner.PublicKey()), Timeout: 5 * time.Second}
	}
	if cl, err := ssh.Dial("tcp", e.addr, cfg(append(bad, e.signer("r1"))...)); err == nil {
		cl.Close()
		t.Fatal("valid key accepted after eight rejected keys")
	}
	cl, err := ssh.Dial("tcp", e.addr, cfg(bad[0], e.signer("r1"))) // one rejected key, then the valid one
	if err != nil {
		t.Fatalf("valid key within the limit: %v", err)
	}
	cl.Close()
}

func TestAdminSocket(t *testing.T) {
	e := newEnv(t, "r1")
	e.startRunner("r1")
	e.waitOnline("r1")
	sock := filepath.Join(os.TempDir(), "cf-adm-"+filepath.Base(e.dir)+".sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	must(t, e.srv.ServeAdmin(ctx, sock))
	run := func(fleet, runner string, params map[string]string) (api.ActionRunResult, error) {
		var res api.ActionRunResult
		err := AdminCall(sock, api.MethodActionRun, api.ActionRunParams{Fleet: fleet, Runner: runner, Action: "hello", Params: params}, &res)
		return res, err
	}
	res, err := run(testFleet, "r1", map[string]string{"name": "adm"})
	if err != nil || strings.TrimSpace(res.Stdout) != "hello adm" {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := run(testFleet, "ghost", nil); err == nil {
		t.Fatal("want error for unknown runner")
	}
	if _, err := run("", "ghost", nil); err == nil {
		t.Fatal("want error for a malformed runner reference")
	}
	// The exec went through the service: start + outcome rows attributed to the admin.
	var n int
	for _, row := range e.auditRows() {
		if row.Action == api.MethodActionRun && row.Actor != "" && row.ActorKind == "admin" && row.Source == "cli" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("want 2 admin audit rows, got %d", n)
	}
}

// rawRunner connects with the named runner's key and sends one heartbeat, then does whatever
// onChan says with exec channels (nil: reject them). It never heartbeats again.
func (e *env) rawRunner(name string, onChan func(ssh.NewChannel)) *ssh.Client {
	e.t.Helper()
	cl, err := e.dial(transport.UserRunner, name)
	must(e.t, err)
	e.t.Cleanup(func() { cl.Close() })
	chans := cl.HandleChannelOpen(transport.ChanExec)
	go func() {
		for nch := range chans {
			if onChan == nil {
				nch.Reject(ssh.Prohibited, "no")
			} else {
				go onChan(nch)
			}
		}
	}()
	hb, _ := json.Marshal(transport.Heartbeat{Hostname: "raw", Proto: testProto})
	if ok, _, err := cl.SendRequest(transport.ReqHeartbeat, true, hb); err != nil || !ok {
		e.t.Fatalf("heartbeat: %v %v", ok, err)
	}
	return cl
}

// Q01/Q02: a second key enrolling under a live name is refused by the store (partial unique
// index), so it can never authenticate; a revoked name is reusable by a new key.
func TestDuplicateRunnerNameRefused(t *testing.T) {
	e := newEnv(t, "a", "c")
	// Second key, same name "a": enrolment is refused.
	if _, err := e.tryAddRunnerKey("a2", "a", store.StatusActive); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate name: want ErrConflict, got %v", err)
	}
	e.startRunner("a")
	e.startRunner("c")
	e.waitOnline("a", "c")
	// The refused key is unknown to the store: it never registers.
	e.startRunner("a2")
	time.Sleep(300 * time.Millisecond)
	if n := e.online(); len(n) != 2 {
		t.Fatalf("only a and c may be online: %v", n)
	}

	// Revoking "a" frees the name for a new key (revoked keys never re-enrol).
	if _, err := e.svc.Revoke(context.Background(), e.admin, service.SourceCLI, testFleet+"/a"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.tryAddRunnerKey("a3", "a", store.StatusActive); err != nil {
		t.Fatalf("revoked name must be reusable: %v", err)
	}
}

func TestSameKeyReconnectReplacesSession(t *testing.T) {
	e := newEnv(t, "r1")
	// Two raw connections, not startRunner: a real runner reconnects when it is replaced and
	// could replace the second connection before its heartbeat reply (EOF flake).
	e.rawRunner("r1", nil)
	e.waitOnline("r1")
	old := e.session("r1")
	e.rawRunner("r1", nil) // same key, second connection
	waitFor(t, func() bool { s := e.session("r1"); return s != nil && s != old })
}

// Q05: a runner that stops heartbeating goes offline and its connection is closed.
func TestSilentRunnerGoesOffline(t *testing.T) {
	e := newEnvHB(t, 40*time.Millisecond, "r1")
	cl := e.rawRunner("r1", nil)
	e.waitOnline("r1")
	waitFor(t, func() bool { return e.session("r1") == nil && len(e.online()) == 0 })
	closed := make(chan struct{})
	go func() { cl.Wait(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("central did not close the silent runner's connection")
	}
}

// A connection that authenticates but never heartbeats is closed after the stale window.
func TestNeverHeartbeatingConnectionClosed(t *testing.T) {
	e := newEnvHB(t, 40*time.Millisecond, "r1")
	cl, err := e.dial(transport.UserRunner, "r1")
	must(t, err)
	defer cl.Close()
	closed := make(chan struct{})
	go func() { cl.Wait(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("silent connection was not closed")
	}
}

// Q05: OpenChannel that never completes is abandoned when ctx ends.
func TestExecCancelledWhileOpening(t *testing.T) {
	e := newEnv(t, "r1")
	gate := make(chan struct{})
	e.rawRunner("r1", func(nch ssh.NewChannel) { <-gate; nch.Reject(ssh.Prohibited, "late") })
	defer close(gate)
	e.waitOnline("r1")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := e.reg.Exec(ctx, e.ids["r1"], transport.ExecRequest{Action: "hello"})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 3*time.Second {
		t.Fatalf("%v after %v", err, time.Since(start))
	}
}

// Q08: an oversized result is an explicit error, not a silent failure.
func TestResultTooLarge(t *testing.T) {
	e := newEnv(t, "r1")
	e.rawRunner("r1", func(nch ssh.NewChannel) {
		ch, _, err := nch.Accept()
		if err != nil {
			return
		}
		defer ch.Close()
		junk := bytes.Repeat([]byte("x"), 1<<20)
		for i := 0; i < transport.MaxResultBytes/len(junk)+2; i++ {
			if _, err := ch.Write(junk); err != nil {
				return
			}
		}
	})
	e.waitOnline("r1")
	_, err := e.exec("r1", "hello", "name", "x")
	if err == nil || !strings.Contains(err.Error(), "result too large") {
		t.Fatalf("got %v", err)
	}
}

// Q07: concurrency cap, and an action dies with its channel (freeing the slot).
func TestRunnerExecCapAndCancel(t *testing.T) {
	e := newEnv(t, "r1")
	auditPath := filepath.Join(t.TempDir(), "runner-audit.jsonl")
	al, err := audit.Open(auditPath)
	must(t, err)
	defer al.Close()
	e.startRunner("r1", func(c *runner.Config) { c.MaxExec = 1; c.Audit = al })
	e.waitOnline("r1")

	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() {
		_, err := e.reg.Exec(ctx, e.ids["r1"], transport.ExecRequest{Action: "long"})
		first <- err
	}()
	time.Sleep(300 * time.Millisecond)
	_, err = e.exec("r1", "hello", "name", "x")
	if err == nil || !strings.Contains(err.Error(), "runner busy") {
		t.Fatalf("want busy refusal, got %v", err)
	}
	cancel() // central gives up -> channel closes -> runner kills the sleep and frees the slot
	<-first
	start := time.Now()
	waitFor(t, func() bool {
		res, err := e.exec("r1", "hello", "name", "x")
		return err == nil && res.ExitCode == 0
	})
	if time.Since(start) > 5*time.Second {
		t.Fatal("slot not freed promptly")
	}
	// Q18: the audit shows start before its result.
	b, _ := os.ReadFile(auditPath)
	text := string(b)
	si, ri := strings.Index(text, `"outcome":"start"`), strings.Index(text, `"outcome":"failed"`)
	if si < 0 || ri < 0 || si > ri {
		t.Fatalf("audit order wrong:\n%s", text)
	}
}

func TestAdminSocketPermissionsAndPeer(t *testing.T) {
	e := newEnv(t, "r1")
	dir, err := os.MkdirTemp("", "cf-adm")
	must(t, err)
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "admin.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	must(t, e.srv.ServeAdmin(ctx, sock))
	st, err := os.Stat(sock)
	must(t, err)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %v", st.Mode())
	}
}

func TestNewValidatesConfig(t *testing.T) {
	e := newEnv(t)
	good := Config{Listen: "127.0.0.1:0", HostKey: e.hostSigner, Store: e.db, Service: e.svc, Registry: e.reg, Operators: e.ops}
	if _, err := New(good); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(*Config){
		"listen":    func(c *Config) { c.Listen = "" },
		"hostkey":   func(c *Config) { c.HostKey = nil },
		"store":     func(c *Config) { c.Store = nil },
		"service":   func(c *Config) { c.Service = nil },
		"registry":  func(c *Config) { c.Registry = nil },
		"operators": func(c *Config) { c.Operators = nil },
	} {
		c := good
		mod(&c)
		if s, err := New(c); err == nil || s != nil {
			t.Errorf("%s: want error, got %v", name, err)
		}
	}
}
