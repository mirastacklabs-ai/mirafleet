//go:build unix

package central

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/keys"
	"github.com/mirastacklabs-ai/mirafleet/internal/opconf"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// rawRunnerEvents connects as the runner named name (its key), registers with one heartbeat and
// opens events@. The caller closes the client.
func (f *m7Fix) rawRunnerEvents(name string) (*ssh.Client, ssh.Channel) {
	f.t.Helper()
	cl, err := f.e.dial(transport.UserRunner, name)
	must(f.t, err)
	hb, _ := json.Marshal(transport.Heartbeat{Hostname: "raw", Proto: testProto, Features: []string{transport.FeatureCreds, transport.FeatureEvents}})
	if ok, _, err := cl.SendRequest(transport.ReqHeartbeat, true, hb); err != nil || !ok {
		f.t.Fatalf("heartbeat: %v %v", ok, err)
	}
	ch, reqs, err := cl.OpenChannel(transport.ChanEvents, nil)
	must(f.t, err)
	go ssh.DiscardRequests(reqs)
	return cl, ch
}

// ---- 8. Events ----

func m7Events(t *testing.T, f *m7Fix) {
	f.stops["vm1-qa"]() // the raw connection below plays the runner
	cl, ch := f.rawRunnerEvents("vm1-qa")
	sess := "0123456789abcdef0123456789abcdef"
	ev := func(cred, host, status string, n int64) transport.Event {
		return transport.Event{Type: transport.EventCredentialUse, Session: sess, Credential: cred, Host: host, Status: status,
			Count: n, WindowMs: time.Now().UnixMilli(), WindowS: 60}
	}
	must(t, transport.WriteEventBatch(ch, &transport.EventBatch{Events: []transport.Event{
		ev("deploy", "gitlab.test", transport.EventOK, 3), ev("reg", "reg.test", transport.EventDenied, 1)}}))
	rows := func(action string) []store.AuditEvent {
		var out []store.AuditEvent
		for _, r := range f.e.auditRowsFor(action) {
			if r.Source == "runner" && r.Runner == "vm1-qa" {
				out = append(out, r)
			}
		}
		return out
	}
	waitFor(t, func() bool { return len(rows("credential.use")) == 2 })
	got := rows("credential.use")
	if got[0].Params["credential"] != "deploy" || got[0].Params["count"] != "3" || got[0].Params["host"] != "gitlab.test" ||
		got[0].Outcome != "ok" || got[0].Actor != "vm1-qa" || got[0].Fleet != "vm" {
		t.Errorf("first event row: %+v", got[0])
	}
	if got[1].Params["status"] != "denied" || got[1].Outcome != "denied" {
		t.Errorf("second event row: %+v", got[1])
	}

	// A flood: a full batch plus the runner's own count of what it dropped locally. Every event is
	// recorded, and the drop becomes one events.dropped row.
	var many []transport.Event
	for i := 0; i < transport.MaxBatchEvents; i++ {
		many = append(many, ev("deploy", fmt.Sprintf("h%03d.test", i), transport.EventOK, 1))
	}
	must(t, transport.WriteEventBatch(ch, &transport.EventBatch{Events: many, Dropped: 5000}))
	waitFor(t, func() bool {
		return len(rows("credential.use")) == 2+transport.MaxBatchEvents && len(rows("events.dropped")) == 1
	})
	if d := rows("events.dropped")[0]; d.Params["dropped"] != "5000" {
		t.Errorf("events.dropped row: %+v", d)
	}
	// The wire refuses what the contract forbids: more than a batch is not sent.
	if err := transport.WriteEventBatch(ch, &transport.EventBatch{Events: append(many, ev("deploy", "x.test", transport.EventOK, 1))}); err == nil {
		t.Error("an oversized batch was written")
	}
	ch.Close()
	cl.Close()
	f.stops["vm1-qa"] = m7StartRunner(f.t, f.e, "vm1-qa", f.hosts["vm1-qa"], f.wc) // lives past this subtest
	f.waitHostOnline("vm1-qa")
}

// ---- 10. One operator per runner ----

func m7OneOperator(t *testing.T, f *m7Fix) {
	ctx := m3ctx(t)
	if live, err := f.e.db.ListWorkspaces(ctx, store.WorkspaceFilter{Limit: 100, States: []store.WorkspaceState{store.WorkspaceOpening, store.WorkspaceOpen, store.WorkspaceClosing}}); err != nil || len(live) != 0 {
		for _, w := range live {
			t.Logf("live workspace left by an earlier step: %s %s %s session %s", w.Operator, w.Runner, w.State, w.Session)
		}
		t.Fatalf("earlier steps left %d live workspaces (%v)", len(live), err)
	}
	ann, bob := f.server("ann", "o-ann", f.proj), f.server("bob", "o-bob", f.proj)
	if r := f.open(ann, "dev", nil); r.IsErr {
		t.Fatalf("ann open: %s", r.Text)
	}
	// bob asks for the runner ann holds: busy.
	r := f.open(bob, "dev", nil)
	if !r.IsErr || !strings.Contains(r.Text, "busy") || !strings.Contains(r.Text, "in use by ann") {
		t.Fatalf("bob on ann's runner: %s", r.Text)
	}
	// Without a preference, placement skips the held runner.
	bob2 := f.server("bob", "o-bob2", f.proj)
	r = f.open(bob2, "", nil)
	if r.IsErr || f.wsRunner(r.str("workspace", "id")) != "vm1-qa" {
		t.Fatalf("placement did not skip the held runner: %s", r.Text)
	}
	// ann may have several sessions on her runner.
	ann2 := f.server("ann", "o-ann2", f.proj)
	if r := f.open(ann2, "dev", nil); r.IsErr {
		t.Fatalf("ann's second session: %s", r.Text)
	}
	// Released when her last live workspace on it closes.
	ann.call("workspace_close", nil)
	if r := f.open(bob, "dev", nil); !r.IsErr {
		t.Fatalf("released with a workspace still open: %s", r.Text)
	}
	ann2.call("workspace_close", nil)
	r = f.open(bob, "dev", nil)
	if r.IsErr {
		t.Fatalf("bob after ann's last close: %s", r.Text)
	}
	// Separately: released by the idle reaper.
	ann3 := f.server("ann", "o-ann3", f.proj)
	if r := f.open(ann3, "dev", nil); !r.IsErr || !strings.Contains(r.Text, "in use by bob") {
		t.Fatalf("ann on bob's runner: %s", r.Text)
	}
	f.clk.advance(61 * time.Minute)
	f.e.svc.ReapOnce(ctx)
	if r := f.open(ann3, "dev", nil); r.IsErr {
		t.Fatalf("ann after the idle reap: %s", r.Text)
	}
	ann3.call("workspace_close", nil)
	bob2.call("workspace_close", nil)

	// A replacing runner connection writes runner.replaced (and only that: no refusal).
	before := len(f.e.auditRowsFor("runner.replaced"))
	cl, err := f.e.dial(transport.UserRunner, "vm1-qa")
	must(t, err)
	hb, _ := json.Marshal(transport.Heartbeat{Hostname: "raw", Proto: testProto})
	// The reply is not the signal: central registers this connection (closing the real runner,
	// which reconnects within ~20ms) and writes the audit row before it replies, so under load the
	// real runner can replace this connection first and the reply is lost (io.EOF). Either
	// outcome is correct; the audit row is what is under test.
	if ok, _, err := cl.SendRequest(transport.ReqHeartbeat, true, hb); err != nil && !errors.Is(err, io.EOF) || err == nil && !ok {
		t.Fatalf("replacing heartbeat: %v %v", ok, err)
	}
	waitFor(t, func() bool { return len(f.e.auditRowsFor("runner.replaced")) > before })
	row := f.e.auditRowsFor("runner.replaced")[before]
	if row.Source != "runner" || row.Runner != "vm1-qa" || row.Fleet != "vm" {
		t.Errorf("runner.replaced row: %+v", row)
	}
	cl.Close()
	f.waitHostOnline("vm1-qa") // the real runner reconnects
}

// ---- 11. Guarded host fleet ----

// m7Guard names the guarded fleet in every call (a guarded fleet is never taken from the project
// file, so a harness must say it explicitly).
type m7Guard struct{ *m5Server }

func (g m7Guard) call(tool string, args map[string]any) m5Res {
	a := map[string]any{"fleet": "guard"}
	for k, v := range args {
		a[k] = v
	}
	return g.m5Server.call(tool, a)
}

func m7Guarded(t *testing.T, f *m7Fix) {
	must(t, os.WriteFile(filepath.Join(f.projG, "ok.txt"), []byte("fine"), 0o644))
	s := m7Guard{f.server("ann", "g1", f.projG)}
	if r := s.call("workspace_open", map[string]any{"fleet": "guard"}); !r.IsErr {
		t.Fatalf("open of a guarded fleet without confirm: %s", r.Text)
	}
	if r := s.call("workspace_open", map[string]any{"fleet": "guard", "confirm": "guard"}); r.IsErr {
		t.Fatalf("open with the fleet name: %s", r.Text)
	}
	mut := []struct {
		tool string
		args map[string]any
	}{
		{"write", map[string]any{"path": "g.txt", "content": "x"}},
		{"bash", map[string]any{"command": "echo hi"}},
		{"bash", map[string]any{"command": "echo hi", "run_in_background": true}},
		{"upload", map[string]any{"local": "ok.txt"}},
	}
	for _, m := range mut {
		if r := s.call(m.tool, m.args); !r.IsErr || !strings.Contains(r.Text, "is guarded") {
			t.Errorf("%s without confirm: %s", m.tool, r.Text)
		}
		wrong := map[string]any{"confirm": "vm"}
		for k, v := range m.args {
			wrong[k] = v
		}
		if r := s.call(m.tool, wrong); !r.IsErr {
			t.Errorf("%s with the wrong fleet name: %s", m.tool, r.Text)
		}
	}
	// Reading needs no confirmation; mutating with the fleet name works.
	if r := s.call("ls", nil); r.IsErr {
		t.Errorf("ls on a guarded fleet: %s", r.Text)
	}
	if r := s.call("write", map[string]any{"path": "g.txt", "content": "x", "confirm": "guard"}); r.IsErr {
		t.Errorf("write with confirm: %s", r.Text)
	}
	if r := s.call("bash", map[string]any{"command": "echo hi", "confirm": "guard"}); r.IsErr || r.str("stdout") != "echo hi\n" {
		t.Errorf("bash with confirm: %s", r.Text)
	}
	if r := s.call("bash", map[string]any{"command": "m7:build", "run_in_background": true, "confirm": "guard"}); r.IsErr {
		t.Errorf("background bash with confirm: %s", r.Text)
	} else {
		s.call("kill_shell", map[string]any{"shell_id": r.str("shell_id"), "confirm": "guard"})
	}
	s.call("workspace_close", map[string]any{"confirm": "guard"})
}

// ---- 12. Central rotation ----

func m7Rotation(t *testing.T, f *m7Fix) {
	ctx := m3ctx(t)
	e := f.e
	soon := f.clk.now().Add(3 * 24 * time.Hour)
	rot := transport.CredMeta{Name: "rot", Kind: transport.CredHTTPSToken, Hosts: []string{"gitlab.test"}, Auth: transport.AuthBasic, User: "oauth2"}
	// A https-token with self-rotation shares the host with gl-token: use another host name.
	rot.Hosts = []string{"rot.gitlab.test"}
	if _, err := f.credSet(rot, []byte(f.d.rotOld), func(p *api.CredentialSetParams) {
		p.Expires, p.Rotate, p.RotateProject = &soon, api.RotateGitLabSelf, "42"
	}); err != nil {
		t.Fatalf("set rot: %v", err)
	}
	f.assign("rot", "vm", "")
	f.waitHeld("vm1-dev", "deploy", "gl-token", "rot")
	f.waitHeld("vm1-qa", "deploy", "reg", "rot")

	calls := f.glCalls.Load()
	if n := e.svc.RotateDue(ctx); n != 1 {
		t.Fatalf("RotateDue rotated %d credentials, want 1", n)
	}
	if f.glCalls.Load() != calls+1 {
		t.Errorf("GitLab was called %d times", f.glCalls.Load()-calls)
	}
	// The new value reaches every assigned runner; the old one is gone from them.
	waitFor(t, func() bool {
		return f.snd.value(f.id("vm1-dev"), "rot") == f.d.rotNew && f.snd.value(f.id("vm1-qa"), "rot") == f.d.rotNew
	})
	var list api.CredentialListResult
	must(t, f.adm(api.MethodCredentialList, api.CredentialListParams{Name: "rot"}, &list))
	c := list.Credentials[0]
	if c.Version != 2 || c.Expires == nil || c.Expires.Sub(f.clk.now()) < 20*24*time.Hour || c.RotateError != "" {
		t.Errorf("after rotation: version %d expires %v error %q", c.Version, c.Expires, c.RotateError)
	}
	var rotRows int
	for _, r := range e.auditRowsFor(api.MethodCredentialRotate) {
		if r.Subject == "rot" && r.Outcome == "ok" && r.Source == "system" {
			rotRows++
		}
	}
	if rotRows != 1 {
		t.Errorf("rotation audit rows: %d", rotRows)
	}
	if n := e.svc.RotateDue(ctx); n != 0 {
		t.Errorf("a freshly rotated credential was rotated again (%d)", n)
	}

	// An unreachable GitLab: audited, shown, and the old value keeps working.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	dead := "http://" + l.Addr().String()
	l.Close()
	f.glURL.Store(dead)
	rot2 := transport.CredMeta{Name: "rot2", Kind: transport.CredHTTPSToken, Hosts: []string{"rot2.gitlab.test"}, Auth: transport.AuthBasic, User: "oauth2"}
	soon2 := f.clk.now().Add(2 * 24 * time.Hour)
	if _, err := f.credSet(rot2, []byte(f.d.rot2), func(p *api.CredentialSetParams) {
		p.Expires, p.Rotate, p.RotateProject = &soon2, api.RotateGitLabSelf, "7"
	}); err != nil {
		t.Fatalf("set rot2: %v", err)
	}
	f.assign("rot2", "vm", "vm1-dev")
	f.waitHeld("vm1-dev", "deploy", "gl-token", "rot", "rot2")
	if n := e.svc.RotateDue(ctx); n != 0 {
		t.Fatalf("RotateDue rotated %d credentials with GitLab unreachable", n)
	}
	var failed int
	for _, r := range e.auditRowsFor(api.MethodCredentialRotate) {
		if r.Subject == "rot2" && r.Outcome == "error" {
			failed++
		}
	}
	if failed != 1 {
		t.Errorf("failed-rotation audit rows: %d", failed)
	}
	must(t, f.adm(api.MethodCredentialList, api.CredentialListParams{Name: "rot2"}, &list))
	if c := list.Credentials[0]; c.RotateError == "" || c.Version != 1 {
		t.Errorf("rot2 after the failed rotation: version %d error %q", c.Version, c.RotateError)
	}
	if f.snd.value(f.id("vm1-dev"), "rot2") != f.d.rot2 {
		t.Error("the old value of rot2 stopped working")
	}
	raw, _ := json.Marshal(list)
	for _, s := range []string{f.d.rot2, f.d.rotNew, f.d.rotOld} {
		if strings.Contains(string(raw), s) {
			t.Error("a rotation result shows a token value")
		}
	}
}

// ---- 9. Hygiene (runs last: it scans everything the other steps produced) ----

func m7Hygiene(t *testing.T, f *m7Fix) {
	ctx := m3ctx(t)
	e := f.e
	secretsList := []string{f.d.token, f.d.reg, f.d.rotOld, f.d.rotNew, f.d.rot2, f.d.sshNeedle}
	var needles []string
	for _, s := range secretsList {
		needles = append(needles, m4Encodings([]byte(s))...)
	}
	scan := func(what string, data []byte) {
		t.Helper()
		for _, n := range needles {
			if strings.Contains(string(data), n) {
				t.Errorf("a secret value (%.12s...) appears in %s", n, what)
			}
		}
	}

	// MCP results of every tool call, audit rows, log lines, heartbeats, operator and admin views.
	scan("an MCP result", []byte(strings.Join(f.sink.all(), "\n")))
	rows, _ := json.Marshal(e.auditRows())
	scan("the audit rows", rows)
	scan("the central and runner log", []byte(e.logText()))
	snap, _ := json.Marshal(e.reg.Snapshot())
	scan("a heartbeat", snap)
	cl := m7OperatorClient(t, e, f.dirs["ann"])
	rl, err := cl.RunnersList(ctx, "vm")
	must(t, err)
	b, _ := json.Marshal(rl)
	scan("runners.list", b)
	var list api.CredentialListResult
	must(t, f.adm(api.MethodCredentialList, api.CredentialListParams{}, &list))
	b, _ = json.Marshal(list)
	scan("credential.list", b)

	// The SQLite file (and its write-ahead log) never holds a value.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if data, err := os.ReadFile(f.dbPath + suffix); err == nil {
			scan("the SQLite file "+suffix, data)
		}
	}
	// Files the agent user could read on the hosts: session directories, ssh config, CA bundle.
	for n, h := range f.hosts {
		filepath.WalkDir(h.root, func(p string, d os.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				if data, rerr := os.ReadFile(p); rerr == nil && len(data) < 4<<20 {
					scan("a file on "+n, data)
				}
			}
			return nil
		})
	}

	// The admin GUI: every page it serves while crawled from its overview.
	f4 := &m4Fixture{rootT: t, e: e, clk: f.clk, guiDir: t.TempDir()}
	srv, ts := f4.startGUI(e.svc)
	w := newM4World(srv.TLSConfig().Certificates[0].Certificate[0])
	w.setBase(ts.URL)
	_, _, err = srv.PrepareSetup(ctx)
	must(t, err)
	tok, err := os.ReadFile(srv.SetupTokenPath())
	must(t, err)
	br := w.newBrowser(t)
	pw := m7Pass
	if p := br.post("/setup", "/setup", url.Values{"token": {strings.TrimSpace(string(tok))}, "username": {"admin"}, "password": {pw}, "password2": {pw}}); p.Status != 303 {
		t.Fatalf("gui setup: status %d", p.Status)
	}
	href := regexp.MustCompile(`href="(/[^"#]*)"`)
	seen := map[string]bool{}
	queue := []string{"/", "/fleets", "/runners", "/credentials", "/audit", "/workspaces", "/operators", "/tokens", "/account"}
	credPage := false
	for len(queue) > 0 && len(seen) < 150 {
		p := queue[0]
		queue = queue[1:]
		if seen[p] || strings.HasPrefix(p, "/logout") || strings.HasPrefix(p, "/static") || strings.HasPrefix(p, "/assets") {
			continue
		}
		seen[p] = true
		page := br.get(p)
		if page.Status != 200 {
			continue
		}
		if p == "/credentials" && strings.Contains(page.Body, "gl-token") && strings.Contains(page.Body, "rot2") {
			credPage = true
		}
		for _, m := range href.FindAllStringSubmatch(page.Body, -1) {
			queue = append(queue, strings.ReplaceAll(m[1], "&amp;", "&"))
		}
	}
	if !credPage {
		t.Error("the Credentials page did not list the credentials")
	}
	for _, r := range w.responses() {
		scan(fmt.Sprintf("GUI page %s %s", r.Method, r.Path), []byte(r.Text))
	}
	if len(seen) < 8 {
		t.Errorf("the crawl saw only %d GUI pages", len(seen))
	}

	// No admin method is reachable on api@, whatever the operator.
	signer, err := keys.LoadSigner(opconf.KeyPath(f.dirs["ann"]))
	must(t, err)
	sc, err := ssh.Dial("tcp", e.addr, &ssh.ClientConfig{User: transport.UserOperator, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.FixedHostKey(e.hostSigner.PublicKey()), Timeout: 5 * time.Second})
	must(t, err)
	defer sc.Close()
	for _, m := range []string{api.MethodFleetCreate, api.MethodTokenCreate, api.MethodTokenList, api.MethodTokenRevoke,
		api.MethodRunnerLifecycle, api.MethodOperatorCode, api.MethodOperatorRotateSecret, api.MethodOperatorList,
		api.MethodOperatorRevoke, api.MethodAuditList, api.MethodFleetWorkspaceSet, api.MethodWorkspaceAdminList,
		api.MethodWorkspaceKill, api.MethodGUIResetAdmin, api.MethodFleetSetConfirm, api.MethodCredentialSet,
		api.MethodCredentialList, api.MethodCredentialDelete, api.MethodCredentialAssign, api.MethodCredentialUnassign,
		api.MethodCredentialRotate} {
		if !api.IsAdminMethod(m) {
			t.Errorf("%s is not an admin method", m)
		}
		resp, err := rawCall(sc, transport.ChanAPI, m, map[string]string{"name": "gl-token"})
		if err != nil || resp.Error == nil || resp.Error.Code != api.CodeForbidden {
			t.Errorf("admin method %s on api@: %+v %v", m, resp, err)
		}
	}
}

var _ context.Context
