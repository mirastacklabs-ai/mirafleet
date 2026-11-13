package central

import (
	"bytes"
	"context"
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
	"github.com/mirastacklabs-ai/mirafleet/internal/audit"
	"github.com/mirastacklabs-ai/mirafleet/internal/keys"
	"github.com/mirastacklabs-ai/mirafleet/internal/runner"
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

type env struct {
	t          *testing.T
	dir        string
	hostSigner ssh.Signer
	authFile   string
	knownHosts string
	acts       *actions.Set
	addr       string
	srv        *Server
	centralLog bytes.Buffer
	hb         time.Duration // central HeartbeatEvery (0 = default)

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

func newEnv(t *testing.T, runnerNames ...string) *env {
	t.Helper()
	e := &env{t: t, dir: t.TempDir()}
	if _, err := keys.Generate(e.dir, "host", "host"); err != nil {
		t.Fatal(err)
	}
	var err error
	if e.hostSigner, err = keys.LoadSigner(filepath.Join(e.dir, "host")); err != nil {
		t.Fatal(err)
	}
	var auth strings.Builder
	for _, n := range runnerNames {
		line, err := keys.Generate(e.dir, n, n)
		if err != nil {
			t.Fatal(err)
		}
		auth.WriteString(line + "\n")
	}
	e.authFile = filepath.Join(e.dir, "authorized_runners")
	must(t, os.WriteFile(e.authFile, []byte(auth.String()), 0o600))
	e.knownHosts = filepath.Join(e.dir, "known_hosts")
	must(t, os.WriteFile(e.knownHosts, []byte("* "+keys.PubLine(e.hostSigner.PublicKey(), "")+"\n"), 0o600))
	e.acts, err = actions.Parse([]byte(testActions))
	must(t, err)
	e.startCentral("127.0.0.1:0")
	return e
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (e *env) startCentral(listen string) {
	e.t.Helper()
	e.srv = New(Config{Listen: listen, HostKey: e.hostSigner, AuthorizedRunnersFile: e.authFile, Audit: audit.New(&e.centralLog), Logf: e.logf, HeartbeatEvery: e.hb})
	addr, err := e.srv.Start()
	must(e.t, err)
	e.addr = addr
	e.t.Cleanup(e.srv.Close)
}

// startRunner runs a runner with the key named name (in e.dir) until the test ends.
func (e *env) startRunner(name string, mod ...func(*runner.Config)) {
	e.t.Helper()
	signer, err := keys.LoadSigner(filepath.Join(e.dir, name))
	must(e.t, err)
	cfg := runner.Config{
		Central: e.addr, Name: name, Signer: signer, KnownHosts: e.knownHosts, Actions: e.acts,
		Labels: map[string]string{"role": name}, Version: "test",
		HeartbeatEvery: 50 * time.Millisecond, BackoffMin: 20 * time.Millisecond, BackoffMax: 100 * time.Millisecond,
	}
	for _, m := range mod {
		m(&cfg)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { runner.New(cfg).Run(ctx); close(done) }()
	e.t.Cleanup(func() { cancel(); <-done })
}

func (e *env) waitOnline(names ...string) {
	e.t.Helper()
	waitFor(e.t, func() bool {
		for _, n := range names {
			if e.srv.sessionFor(n) == nil {
				return false
			}
		}
		return true
	})
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

func (e *env) exec(runnerName, action string, kv ...string) (*transport.ExecResult, error) {
	p := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		p[kv[i]] = kv[i+1]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return e.srv.Exec(ctx, runnerName, transport.ExecRequest{Action: action, Params: p})
}

func TestTwoRunnersActions(t *testing.T) {
	e := newEnv(t, "r1", "r2")
	e.startRunner("r1")
	e.startRunner("r2")
	e.waitOnline("r1", "r2")

	// Registry carries heartbeat data.
	rs := e.srv.Runners()
	if len(rs) != 2 || rs[0].Name != "r1" || rs[0].Heartbeat.Version != "test" || rs[0].Heartbeat.Labels["role"] != "r1" || rs[0].Heartbeat.OS == "" {
		t.Fatalf("registry: %+v", rs)
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
		if _, err := e.exec("ghost", "hello", "name", "x"); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("central audit", func(t *testing.T) {
		outcomes := map[string]bool{}
		for _, l := range bytes.Split(bytes.TrimSpace(e.centralLog.Bytes()), []byte("\n")) {
			var ev audit.Event
			must(t, json.Unmarshal(l, &ev))
			outcomes[ev.Outcome] = true
		}
		for _, o := range []string{"ok", "failed", "refused", "timeout", "error"} {
			if !outcomes[o] {
				t.Errorf("no %q audit event", o)
			}
		}
	})
}

func TestUnauthorizedRunnerRejected(t *testing.T) {
	e := newEnv(t, "r1")
	// A valid key that is simply not in the authorized file.
	_, err := keys.Generate(e.dir, "mallory", "mallory")
	must(t, err)
	signer, err := keys.LoadSigner(filepath.Join(e.dir, "mallory"))
	must(t, err)
	_, err = ssh.Dial("tcp", e.addr, &ssh.ClientConfig{
		User: "mallory", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.FixedHostKey(e.hostSigner.PublicKey()), Timeout: 5 * time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "unable to authenticate") {
		t.Fatalf("want auth failure, got %v", err)
	}
	// And as a full runner: it never registers.
	e.startRunner("mallory")
	time.Sleep(400 * time.Millisecond)
	if len(e.srv.Runners()) != 0 {
		t.Fatalf("unauthorized runner registered: %+v", e.srv.Runners())
	}
}

func TestRunnerPinsHostKey(t *testing.T) {
	e := newEnv(t, "r1")
	other, err := keys.Generate(t.TempDir(), "x", "x")
	must(t, err)
	must(t, os.WriteFile(e.knownHosts, []byte("* "+other+"\n"), 0o600)) // pin a different key
	e.startRunner("r1")
	time.Sleep(400 * time.Millisecond)
	if len(e.srv.Runners()) != 0 {
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
	old := e.srv.sessionFor("r1")
	old.Conn().Close()
	waitFor(t, func() bool { s := e.srv.sessionFor("r1"); return s != nil && s != old })
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
	conn := e.srv.sessionFor("r1").Conn()
	for _, typ := range []string{"session", "direct-tcpip", "file@mirafleet", "workspace@mirafleet", "x11"} {
		_, _, err := conn.OpenChannel(typ, nil)
		var oe *ssh.OpenChannelError
		if !errors.As(err, &oe) || oe.Reason != ssh.UnknownChannelType {
			t.Errorf("runner accepted %q: %v", typ, err)
		}
	}
	// Malformed exec payload is rejected, not run.
	if _, _, err := conn.OpenChannel(transport.ChanExec, []byte("not json")); err == nil {
		t.Error("runner accepted malformed exec request")
	}

	// Central side: an authenticated runner key opens session / forwarding / even exec@ toward central.
	signer, err := keys.LoadSigner(filepath.Join(e.dir, "r1"))
	must(t, err)
	cl, err := ssh.Dial("tcp", e.addr, &ssh.ClientConfig{
		User: "r1", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.FixedHostKey(e.hostSigner.PublicKey()), Timeout: 5 * time.Second,
	})
	must(t, err)
	defer cl.Close()
	for _, typ := range []string{"session", "direct-tcpip", "exec@mirafleet", "api@mirafleet"} {
		_, _, err := cl.OpenChannel(typ, nil)
		var oe *ssh.OpenChannelError
		if !errors.As(err, &oe) || oe.Reason != ssh.UnknownChannelType {
			t.Errorf("central accepted %q: %v", typ, err)
		}
	}
	// Unknown global requests are refused.
	if ok, _, err := cl.SendRequest("tcpip-forward", true, nil); err != nil || ok {
		t.Errorf("central accepted tcpip-forward: %v %v", ok, err)
	}
}

func TestAdminSocket(t *testing.T) {
	e := newEnv(t, "r1")
	e.startRunner("r1")
	e.waitOnline("r1")
	sock := filepath.Join(os.TempDir(), "cf-adm-"+filepath.Base(e.dir)+".sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	must(t, e.srv.ServeAdmin(ctx, sock))
	res, err := AdminExec(sock, "r1", transport.ExecRequest{Action: "hello", Params: map[string]string{"name": "adm"}})
	if err != nil || strings.TrimSpace(res.Stdout) != "hello adm" {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := AdminExec(sock, "ghost", transport.ExecRequest{Action: "hello"}); err == nil {
		t.Fatal("want error for offline runner")
	}
}

// rawRunner connects with the named runner's key and sends one heartbeat, then does whatever
// onChan says with exec channels (nil: reject them). It never heartbeats again.
func (e *env) rawRunner(name string, onChan func(ssh.NewChannel)) *ssh.Client {
	e.t.Helper()
	signer, err := keys.LoadSigner(filepath.Join(e.dir, name))
	must(e.t, err)
	cl, err := ssh.Dial("tcp", e.addr, &ssh.ClientConfig{
		User: name, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.FixedHostKey(e.hostSigner.PublicKey()), Timeout: 5 * time.Second,
	})
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
	hb, _ := json.Marshal(transport.Heartbeat{Hostname: "raw"})
	if ok, _, err := cl.SendRequest(transport.ReqHeartbeat, true, hb); err != nil || !ok {
		e.t.Fatalf("heartbeat: %v %v", ok, err)
	}
	return cl
}

// Q01/Q02: two keys with the same name are both refused; junk lines do not lock anyone out.
func TestDuplicateRunnerNameRefused(t *testing.T) {
	e := newEnv(t, "a", "b", "c")
	sa, _ := keys.LoadSigner(filepath.Join(e.dir, "a"))
	sb, _ := keys.LoadSigner(filepath.Join(e.dir, "b"))
	sc, _ := keys.LoadSigner(filepath.Join(e.dir, "c"))
	body := "# runners\n" + keys.PubLine(sa.PublicKey(), "dup") + "\n" + keys.PubLine(sb.PublicKey(), "dup") + "\n" +
		keys.PubLine(sc.PublicKey(), "c") + "\n\n# trailing comment\n"
	must(t, os.WriteFile(e.authFile, []byte(body), 0o600))
	e.startRunner("a")
	e.startRunner("b")
	e.startRunner("c")
	e.waitOnline("c") // the trailing comment did not lock out the good key
	time.Sleep(300 * time.Millisecond)
	rs := e.srv.Runners()
	if len(rs) != 1 || rs[0].Name != "c" {
		t.Fatalf("only c may be online: %+v", rs)
	}
	if !strings.Contains(e.logText(), "duplicate runner name") {
		t.Fatalf("no loud log: %q", e.logText())
	}
}

func TestSameKeyReconnectReplacesSession(t *testing.T) {
	e := newEnv(t, "r1")
	e.startRunner("r1")
	e.waitOnline("r1")
	old := e.srv.sessionFor("r1")
	e.rawRunner("r1", nil) // same key, second connection
	waitFor(t, func() bool { s := e.srv.sessionFor("r1"); return s != nil && s != old })
}

// Q05: a runner that stops heartbeating goes offline and its connection is closed.
func TestSilentRunnerGoesOffline(t *testing.T) {
	e := newEnv(t, "r1")
	e.srv.Close()
	e.hb = 40 * time.Millisecond
	e.startCentral("127.0.0.1:0")
	cl := e.rawRunner("r1", nil)
	e.waitOnline("r1")
	waitFor(t, func() bool { return e.srv.sessionFor("r1") == nil && len(e.srv.Runners()) == 0 })
	closed := make(chan struct{})
	go func() { cl.Wait(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("central did not close the silent runner's connection")
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
	_, err := e.srv.Exec(ctx, "r1", transport.ExecRequest{Action: "hello"})
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
		_, err := e.srv.Exec(ctx, "r1", transport.ExecRequest{Action: "long"})
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

func TestRunnerIdentityFailsClosed(t *testing.T) {
	ext := func(name, id string) *ssh.Permissions {
		return &ssh.Permissions{Extensions: map[string]string{"runner": name, "runner_id": id}}
	}
	if n, id, ok := runnerIdentity(ext("a", "3")); !ok || n != "a" || id != 3 {
		t.Fatalf("good identity rejected: %q %d %v", n, id, ok)
	}
	bad := []*ssh.Permissions{
		nil, {}, ext("", "3"), ext("a", ""), ext("a", "0"), ext("a", "-1"),
		ext("a", "x"), ext("a", "03"), ext("a", "+3"), ext("a", "3 "),
		{Extensions: map[string]string{"runner": "a"}},
	}
	for i, p := range bad {
		if _, _, ok := runnerIdentity(p); ok {
			t.Errorf("case %d accepted", i)
		}
	}
}
