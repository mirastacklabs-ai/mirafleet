package central

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/actions"
	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/audit"
	"github.com/mirastacklabs-ai/mirafleet/internal/central/registry"
	"github.com/mirastacklabs-ai/mirafleet/internal/runner"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

const napActions = `
actions:
  hello:
    run: ["/bin/echo", "hello", "{{name}}"]
    params: { name: { pattern: "[a-z]{1,8}" } }
  long:
    run: ["/bin/sleep", "30"]
    timeout: 60s
  nap:
    run: ["/bin/sleep", "1"]
    timeout: 20s
`

// openRunnerAudit opens the runner-local JSONL audit under a fresh temp dir.
func openRunnerAudit(t *testing.T) (*audit.Writer, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "runner-audit.jsonl")
	w, err := audit.Open(p)
	must(t, err)
	t.Cleanup(func() { w.Close() })
	return w, p
}

func fileText(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

// auditRowsFor returns central audit rows with the given action.
func (e *env) auditRowsFor(action string) []store.AuditEvent {
	var out []store.AuditEvent
	for _, r := range e.auditRows() {
		if r.Action == action {
			out = append(out, r)
		}
	}
	return out
}

// runAsync starts action.run through apiclient and delivers its error on the returned channel.
func runAsync(cl interface {
	ActionRun(context.Context, api.ActionRunParams) (*api.ActionRunResult, error)
}, ctx context.Context, action string) <-chan error {
	ch := make(chan error, 1)
	go func() {
		_, err := cl.ActionRun(ctx, api.ActionRunParams{Fleet: testFleet, Runner: "r1", Action: action})
		ch <- err
	}()
	return ch
}

func waitErr(t *testing.T, ch <-chan error, within time.Duration) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(within):
		t.Fatalf("call did not return within %v", within)
		return nil
	}
}

// authRejected reports whether a raw dial as user with the named key fails authentication.
func (e *env) authRejected(user, key string) bool {
	cl, err := e.dial(user, key)
	if err == nil {
		cl.Close()
		return false
	}
	return strings.Contains(err.Error(), "unable to authenticate")
}

func TestRevokeRunnerDuringLongAction(t *testing.T) {
	e := newEnv(t, "r1")
	e.addOperator("op1")
	al, apath := openRunnerAudit(t)
	e.startRunner("r1", func(c *runner.Config) { c.Audit = al })
	e.waitOnline("r1")
	cl := e.opClient("op1")

	callCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res := runAsync(cl, callCtx, "long")
	// The action is in flight once the runner has logged its start row.
	waitFor(t, func() bool { return strings.Contains(fileText(apath), `"outcome":"start"`) })

	start := time.Now()
	if _, err := e.svc.Revoke(context.Background(), e.admin, service.SourceCLI, testFleet+"/r1"); err != nil {
		t.Fatal(err)
	}
	err := waitErr(t, res, 2*time.Second)
	if err == nil {
		t.Fatal("action.run succeeded despite revoke")
	}
	wantCode(t, err, api.CodeUnavailable)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("returned after %v", d)
	}

	// Runner side: the process (group) was killed, not left to run out its 30 s.
	waitFor(t, func() bool { return strings.Contains(fileText(apath), `"outcome":"failed"`) })
	txt := fileText(apath)
	if !strings.Contains(txt, `"exit_code":-1`) {
		t.Fatalf("runner audit lacks killed exit code:\n%s", txt)
	}

	// Central audit: cancelled outcome "runner revoked" and the lifecycle row.
	waitFor(t, func() bool {
		for _, r := range e.auditRowsFor(api.MethodActionRun) {
			if r.Outcome == service.OutcomeCancelled && r.Message == service.RevokeReason {
				return true
			}
		}
		return false
	})
	rows := e.auditRowsFor("runner." + api.OpRevoke)
	if len(rows) != 1 || rows[0].Source != "cli" || rows[0].ActorKind != "admin" || rows[0].Actor != e.admin.Name ||
		rows[0].Subject != testFleet+"/r1" || rows[0].Outcome != service.OutcomeOK {
		t.Fatalf("revoke audit rows: %+v", rows)
	}

	// Offline in the registry, and the reconnecting runner is turned away at auth.
	waitFor(t, func() bool {
		for _, si := range e.reg.Snapshot() {
			if si.ID == e.ids["r1"] && si.Online {
				return false
			}
		}
		return true
	})
	time.Sleep(500 * time.Millisecond) // several 20-100 ms reconnect cycles
	if e.session("r1") != nil || len(e.online()) != 0 {
		t.Fatalf("revoked runner is online: %v", e.online())
	}
	if !e.authRejected(transport.UserRunner, "r1") {
		t.Fatal("revoked runner key authenticated")
	}
	st, err := e.db.GetRunnerByID(context.Background(), e.ids["r1"])
	must(t, err)
	if st.Status != store.StatusRevoked {
		t.Fatalf("status %s", st.Status)
	}
}

func TestRevokeOperatorDuringAction(t *testing.T) {
	e := newEnv(t, "r1")
	op := e.addOperator("op1")
	al, apath := openRunnerAudit(t)
	e.startRunner("r1", func(c *runner.Config) { c.Audit = al })
	e.waitOnline("r1")
	cl := e.opClient("op1")
	if _, err := cl.Hello(tctx(t), "test"); err != nil {
		t.Fatal(err)
	}

	callCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res := runAsync(cl, callCtx, "long")
	waitFor(t, func() bool { return strings.Contains(fileText(apath), `"outcome":"start"`) })

	start := time.Now()
	if _, err := e.svc.RevokeOperator(context.Background(), e.admin, service.SourceCLI, "op1"); err != nil {
		t.Fatal(err)
	}
	if err := waitErr(t, res, 2*time.Second); err == nil {
		t.Fatal("action.run succeeded despite operator revoke")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("returned after %v", d)
	}

	// Outcome row for the action is cancelled, on the operator's behalf.
	waitFor(t, func() bool {
		for _, r := range e.auditRowsFor(api.MethodActionRun) {
			if r.Outcome == service.OutcomeCancelled && r.ActorKind == "operator" && r.Actor == op.Name {
				return true
			}
		}
		return false
	})
	// Runner killed the process too.
	waitFor(t, func() bool { return strings.Contains(fileText(apath), `"outcome":"failed"`) })

	// The connection is closed, and the same key cannot connect again.
	if _, err := cl.Hello(tctx(t), "test"); err == nil {
		t.Fatal("revoked operator's connection still serves calls")
	}
	if !e.authRejected(transport.UserOperator, "op1") {
		t.Fatal("revoked operator key authenticated")
	}
	rows := e.auditRowsFor(api.MethodOperatorRevoke)
	if len(rows) != 1 || rows[0].Source != "cli" || rows[0].Subject != "op1" {
		t.Fatalf("operator revoke audit: %+v", rows)
	}
}

func TestDisableLetsInFlightFinish(t *testing.T) {
	e := newEnv(t)
	var err error
	e.acts, err = actions.Parse([]byte(napActions))
	must(t, err)
	e.addRunner("r1", store.StatusActive)
	al, apath := openRunnerAudit(t)
	e.startRunner("r1", func(c *runner.Config) { c.Audit = al })
	e.waitOnline("r1")

	type out struct {
		res service.RunActionResult
		err error
	}
	done := make(chan out, 1)
	go func() {
		r, err := e.run("r1", "nap")
		done <- out{r, err}
	}()
	waitFor(t, func() bool { return strings.Contains(fileText(apath), `"outcome":"start"`) })

	if _, err := e.svc.Disable(context.Background(), e.admin, service.SourceCLI, testFleet+"/r1"); err != nil {
		t.Fatal(err)
	}
	// Connection stays up, status disabled; new work is refused while the nap is in flight.
	snap := e.reg.Snapshot()
	if len(snap) != 1 || !snap[0].Online || snap[0].Status != registry.StatusDisabled {
		t.Fatalf("snapshot after disable: %+v", snap)
	}
	if _, err := e.run("r1", "hello", "name", "x"); err == nil {
		t.Fatal("disabled runner accepted a new action")
	}
	if _, err := e.exec("r1", "hello", "name", "x"); !errors.Is(err, registry.ErrDisabled) {
		t.Fatalf("registry exec on disabled runner: %v", err)
	}

	select {
	case o := <-done:
		if o.err != nil || o.res.ExitCode != 0 {
			t.Fatalf("in-flight action: %+v %v", o.res, o.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("in-flight action did not finish")
	}
	snap = e.reg.Snapshot()
	if len(snap) != 1 || !snap[0].Online {
		t.Fatalf("connection dropped after in-flight finish: %+v", snap)
	}
	for _, r := range e.auditRowsFor(api.MethodActionRun) {
		if r.Outcome == service.OutcomeCancelled {
			t.Fatalf("in-flight action was cancelled: %+v", r)
		}
	}

	if _, err := e.svc.Enable(context.Background(), e.admin, service.SourceCLI, testFleet+"/r1"); err != nil {
		t.Fatal(err)
	}
	res, err := e.run("r1", "hello", "name", "again")
	if err != nil || strings.TrimSpace(res.Stdout) != "hello again" {
		t.Fatalf("after enable: %+v %v", res, err)
	}
}

func TestApproveWhileOfflineThenConnect(t *testing.T) {
	e := newEnv(t)
	e.addRunner("p1", store.StatusPending)
	if len(e.reg.Snapshot()) != 0 {
		t.Fatal("pending runner should be offline")
	}
	if _, err := e.svc.Approve(context.Background(), e.admin, service.SourceCLI, testFleet+"/p1"); err != nil {
		t.Fatal(err)
	}
	e.startRunner("p1")
	e.waitOnline("p1")
	snap := e.reg.Snapshot()
	if len(snap) != 1 || snap[0].Status != registry.StatusActive {
		t.Fatalf("snapshot: %+v", snap)
	}
	res, err := e.run("p1", "hello", "name", "now")
	if err != nil || strings.TrimSpace(res.Stdout) != "hello now" {
		t.Fatalf("exec right after connect: %+v %v", res, err)
	}
}

// fakeConn satisfies registry.Conn for a direct Register attempt.
type fakeConn struct{ closed bool }

func (f *fakeConn) OpenChannel(string, []byte) (ssh.Channel, <-chan *ssh.Request, error) {
	return nil, nil, errors.New("fake")
}
func (f *fakeConn) Close() error { f.closed = true; return nil }

func TestRevokeOfflineRunnerNeverConnects(t *testing.T) {
	e := newEnv(t, "r1")
	if e.session("r1") != nil {
		t.Fatal("runner should be offline")
	}
	if _, err := e.svc.Revoke(context.Background(), e.admin, service.SourceCLI, testFleet+"/r1"); err != nil {
		t.Fatal(err)
	}
	if rows := e.auditRowsFor("runner." + api.OpRevoke); len(rows) != 1 || rows[0].Source != "cli" {
		t.Fatalf("revoke audit: %+v", rows)
	}

	// Gate 1: the tombstone refuses registration even with a valid-looking session.
	_, err := e.reg.Register(e.ids["r1"], "r1", testFleet, &fakeConn{}, registry.StatusActive, transport.Heartbeat{})
	if !errors.Is(err, registry.ErrTombstoned) {
		t.Fatalf("register after revoke: %v", err)
	}
	// Gate 2: the DB status refuses authentication.
	st, err := e.db.GetRunnerByID(context.Background(), e.ids["r1"])
	must(t, err)
	if st.Status != store.StatusRevoked {
		t.Fatalf("status %s", st.Status)
	}
	if !e.authRejected(transport.UserRunner, "r1") {
		t.Fatal("revoked key authenticated")
	}
	e.startRunner("r1")
	time.Sleep(500 * time.Millisecond)
	if n := e.online(); len(n) != 0 || e.session("r1") != nil {
		t.Fatalf("revoked runner connected: %v", n)
	}
	// Revoke is terminal: no way back through the lifecycle.
	if _, err := e.svc.Approve(context.Background(), e.admin, service.SourceCLI, testFleet+"/r1"); err == nil {
		t.Fatal("revoked runner was approved")
	}
	if _, err := e.svc.Enable(context.Background(), e.admin, service.SourceCLI, testFleet+"/r1"); err == nil {
		t.Fatal("revoked runner was enabled")
	}
}
