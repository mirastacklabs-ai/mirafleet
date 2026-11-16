//go:build unix

package central

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/orch"
	"github.com/mirastacklabs-ai/mirafleet/internal/policy"
	"github.com/mirastacklabs-ai/mirafleet/internal/secrets"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/wsconfig"
)

// TestM7Done is the end-to-end host-mode scenario, in process: a real central
// (SQLite, registry, service, SSH on an ephemeral port, admin socket), two host-mode runners
// (real runner code, credentials, per-session wiring) over the m7Host stand-in for the host
// executor, a guarded third runner, and MCP servers (mcpserver.Serve over io.Pipe, one orch each)
// for the operators "ann" and "bob". The clock of the service is injected; the secrets are decoys
// generated at test time. The subtests follow the numbered checks; what the fake
// cannot show (real processes, /proc, the broker on the wire) is in TestM7DoneIntegration.

const m7Pass = "m7-gui-password-1"

type m7Decoys struct {
	token, reg, rotOld, rotNew, rot2 string
	sshPEM, sshNeedle                string
	sshKnownHosts                    string
}

type m7Fix struct {
	t      *testing.T
	e      *env
	dbPath string
	snd    *m7Sender
	clk    *m4Clock
	key    secrets.Key
	sock   string
	d      m7Decoys

	hosts  map[string]*m7Host
	stops  map[string]func()
	wc     *wsconfig.Config
	rec    *m5Rec
	sink   *m5Sink
	dirs   map[string]string // operator -> orch config dir
	base   string
	proj   string // fleet vm
	projWD string // fleet vm, workdir shared
	projG  string // fleet guard

	gl       *httptest.Server
	glURL    atomic.Value // string: the GitLab base URL central rotates against
	glTokens atomic.Value // []string: tokens the fake GitLab accepts
	glCalls  atomic.Int64
}

func (f *m7Fix) id(name string) int64 { return f.e.ids[name] }

// adm sends an admin-socket call.
func (f *m7Fix) adm(method string, params, out any) error {
	f.t.Helper()
	return AdminCall(f.sock, method, params, out)
}

// credSet sets a credential through the admin socket (the secret as a body frame).
func (f *m7Fix) credSet(meta transport.CredMeta, value []byte, mod func(*api.CredentialSetParams)) (api.CredentialSetResult, error) {
	f.t.Helper()
	sum := sha256.Sum256(value)
	p := api.CredentialSetParams{CredMeta: meta, Length: len(value), SHA256: hex.EncodeToString(sum[:])}
	if mod != nil {
		mod(&p)
	}
	var res api.CredentialSetResult
	err := m7AdminBody(f.sock, api.MethodCredentialSet, p, value, &res)
	return res, err
}

func (f *m7Fix) assign(name, fleet, runner string) {
	f.t.Helper()
	var r api.CredentialResult
	must(f.t, f.adm(api.MethodCredentialAssign, api.CredentialAssignParams{Name: name, Fleet: fleet, Runner: runner}, &r))
}

func (f *m7Fix) unassign(name, fleet, runner string) {
	f.t.Helper()
	var r api.CredentialResult
	must(f.t, f.adm(api.MethodCredentialUnassign, api.CredentialUnassignParams{Name: name, Fleet: fleet, Runner: runner}, &r))
}

// waitHeld waits until the latest creds_sync to the runner carried exactly names.
func (f *m7Fix) waitHeld(runner string, names ...string) {
	f.t.Helper()
	sort.Strings(names)
	id := f.id(runner)
	waitFor(f.t, func() bool {
		h := f.snd.held(id)
		if len(h) != len(names) {
			return false
		}
		for i := range h {
			if h[i] != names[i] {
				return false
			}
		}
		return true
	})
}

// server starts an MCP server for operator op with a stable session id.
func (f *m7Fix) server(op, session, proj string) *m5Server {
	f.t.Helper()
	cfg := orch.Config{ConfigDir: f.dirs[op], StateDir: f.t.TempDir(), ProjectDir: proj, Home: f.base, Session: session}
	return m7Start(f.t, cfg, f.rec, f.sink)
}

// open opens a workspace on the given runner zone; credentials narrows (nil: all held).
func (f *m7Fix) open(s *m5Server, zone string, creds []string) m5Res {
	f.t.Helper()
	args := map[string]any{}
	if zone != "" {
		args["labels"] = map[string]any{"zone": zone}
	}
	if creds != nil {
		args["credentials"] = creds
	}
	return s.call("workspace_open", args)
}

func (f *m7Fix) wsRunner(wsID string) string {
	f.t.Helper()
	w, err := f.e.db.GetWorkspace(context.Background(), wsID)
	must(f.t, err)
	return w.Runner
}

func (f *m7Fix) restartRunner(name string) {
	f.t.Helper()
	f.stops[name]()
	f.stops[name] = m7StartRunner(f.t, f.e, name, f.hosts[name], f.wc)
	f.waitHostOnline(name)
}

func (f *m7Fix) waitHostOnline(names ...string) {
	f.t.Helper()
	waitFor(f.t, func() bool {
		up := map[string]bool{}
		for _, si := range f.e.reg.Snapshot() {
			host := false
			for _, ft := range si.Info.Features {
				host = host || ft == transport.FeatureHost
			}
			up[si.Name] = si.Online && host && si.Slots > 0
		}
		for _, n := range names {
			if !up[n] {
				return false
			}
		}
		return true
	})
}

// selfSignedBundle writes a CA bundle for SSL_CERT_FILE (the runners need a system bundle).
func selfSignedBundle(t *testing.T) string {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "m7 test ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	must(t, err)
	p := filepath.Join(t.TempDir(), "bundle.pem")
	must(t, os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	return p
}

func TestM7Done(t *testing.T) {
	ctx := m3ctx(t)
	t.Setenv("SSL_CERT_FILE", selfSignedBundle(t))
	f := &m7Fix{t: t, clk: &m4Clock{}, rec: &m5Rec{}, sink: &m5Sink{}, hosts: map[string]*m7Host{},
		stops: map[string]func(){}, dirs: map[string]string{}}
	var err error
	f.key, _, err = secrets.GenerateKey()
	must(t, err)

	// ---- decoys (generated now; nothing real) ----
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	must(t, err)
	blk, err := ssh.MarshalPrivateKey(priv, "")
	must(t, err)
	f.d.sshPEM = string(pem.EncodeToMemory(blk))
	lines := strings.Split(strings.TrimSpace(f.d.sshPEM), "\n")
	f.d.sshNeedle = lines[2]
	spub, err := ssh.NewPublicKey(pub)
	must(t, err)
	hostPub, _, err := ed25519.GenerateKey(rand.Reader)
	must(t, err)
	hpk, err := ssh.NewPublicKey(hostPub)
	must(t, err)
	f.d.sshKnownHosts = "gitlab.test " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hpk)))
	_ = spub
	f.d.token, f.d.reg = "m7decoy-token-"+randHex(t, 12), "m7decoy-registry-"+randHex(t, 12)
	f.d.rotOld, f.d.rotNew, f.d.rot2 = "m7decoy-rot-old-"+randHex(t, 12), "m7decoy-rot-new-"+randHex(t, 12), "m7decoy-rot2-"+randHex(t, 12)

	// ---- a fake GitLab for central's self-rotation (check 12) ----
	f.glURL.Store("")
	f.gl = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.glCalls.Add(1)
		tok := r.Header.Get("PRIVATE-TOKEN")
		okTok, _ := f.glTokens.Load().([]string)
		good := false
		for _, t := range okTok {
			good = good || t == tok
		}
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/access_tokens/self/rotate") || !good {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"token": f.d.rotNew, "expires_at": f.clk.now().Add(30 * 24 * time.Hour).Format("2006-01-02")})
	}))
	t.Cleanup(f.gl.Close)
	f.glURL.Store(f.gl.URL)
	f.glTokens.Store([]string{f.d.rotOld})

	// ---- central ----
	f.e, f.dbPath, f.snd = newM7EnvDB(t, f.clk.now, f.key, func(cc *service.CredentialsConfig) {
		cc.RotateBaseURL = func(host string) string { return f.glURL.Load().(string) }
	})
	e := f.e
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("central log:\n%s", e.logText())
		}
	})
	f.sock = adminSocket(t, e)

	hostCfg := store.WorkspaceConfig{Executor: store.ExecutorHost, IdleTTL: time.Hour}
	_, err = e.svc.CreateFleet(ctx, e.admin, service.SourceCLI, "vm", store.ModeWorkspace, false, &hostCfg)
	must(t, err)
	_, err = e.svc.CreateFleet(ctx, e.admin, service.SourceCLI, "guard", store.ModeWorkspace, true, &hostCfg)
	must(t, err)
	m3Enrol(e, "vm", "vm1-dev", map[string]string{"zone": "dev"})
	m3Enrol(e, "vm", "vm1-qa", map[string]string{"zone": "qa"})
	m3Enrol(e, "guard", "g-1", map[string]string{"zone": "g"})
	f.wc, err = wsconfig.Parse([]byte("executor: host\nconcurrent: 4\nenv_allow: [LANG]\n"))
	must(t, err)
	for _, n := range []string{"vm1-dev", "vm1-qa", "g-1"} {
		f.hosts[n] = newM7Host(t)
		f.stops[n] = m7StartRunner(t, e, n, f.hosts[n], f.wc)
	}
	f.waitHostOnline("vm1-dev", "vm1-qa", "g-1")

	// ---- operators and their projects ----
	f.base = t.TempDir()
	mk := func(name, yaml string) string {
		p := filepath.Join(f.base, name)
		must(t, os.MkdirAll(p, 0o755))
		must(t, os.WriteFile(filepath.Join(p, ".mirafleet.yaml"), []byte(yaml), 0o644))
		return p
	}
	f.proj = mk("proj", "fleet: vm\n")
	f.projWD = mk("projwd", "fleet: vm\nremote: true\nworkdir: shared\n")
	f.projG = mk("projg", "fleet: guard\n")
	f.dirs["ann"], f.dirs["bob"] = m7Operator(t, e, "ann"), m7Operator(t, e, "bob")

	steps := []struct {
		name string
		fn   func(*testing.T, *m7Fix)
	}{
		{"01_credentials", m7Credentials},
		{"02_placement", m7Placement},
		{"03_tool_parity", m7Tools},
		{"04_bash", m7Bash},
		{"05_background_shells", m7Background},
		{"06_dirs", m7Dirs},
		{"07_transfer_confinement", m7Transfer},
		{"08_events", m7Events},
		{"10_one_operator_per_runner", m7OneOperator},
		{"11_guarded_host_fleet", m7Guarded},
		{"12_central_rotation", m7Rotation},
		{"09_hygiene", m7Hygiene}, // last: it scans everything the steps above produced
	}
	for _, s := range steps {
		// Steps 1 and 2 build the state the others use; the rest open their own workspaces and
		// leave none open, so one failure does not hide the next.
		if !t.Run(s.name, func(t *testing.T) { s.fn(t, f) }) && (strings.HasPrefix(s.name, "01") || strings.HasPrefix(s.name, "02")) {
			t.Logf("step %s failed; later steps depend on its state and are skipped", s.name)
			break
		}
	}
}

// ---- 1. Credentials ----

func m7Credentials(t *testing.T, f *m7Fix) {
	ctx := m3ctx(t)
	e := f.e
	tokenMeta := transport.CredMeta{Name: "gl-token", Kind: transport.CredHTTPSToken, Hosts: []string{"gitlab.test"}, Auth: transport.AuthBasic, User: "oauth2"}
	sshMeta := transport.CredMeta{Name: "deploy", Kind: transport.CredSSHKey, Hosts: []string{"gitlab.test"}, KnownHosts: []string{f.d.sshKnownHosts}}
	regMeta := transport.CredMeta{Name: "reg", Kind: transport.CredRegistry, Hosts: []string{"reg.test"}, User: "svc"}

	// Set through the admin socket: the value is never in a result, a list or an error.
	var leaks []string
	for _, c := range []struct {
		meta  transport.CredMeta
		value string
	}{{tokenMeta, f.d.token}, {sshMeta, f.d.sshPEM}, {regMeta, f.d.reg}} {
		res, err := f.credSet(c.meta, []byte(c.value), nil)
		must(t, err)
		if res.Replaced || res.Credential.Name != c.meta.Name || res.Credential.Version != 1 || res.Credential.Fingerprint == "" {
			t.Fatalf("set %s: %+v", c.meta.Name, res)
		}
		raw, _ := json.Marshal(res)
		leaks = append(leaks, string(raw))
	}
	// A wrong digest and a missing body are refused; nothing was stored by them.
	bad, err := f.credSet(tokenMeta, []byte("other-value"), func(p *api.CredentialSetParams) { p.SHA256 = strings.Repeat("0", 64) })
	if err == nil {
		t.Fatalf("a credential with a wrong digest was accepted: %+v", bad)
	}
	var list api.CredentialListResult
	must(t, f.adm(api.MethodCredentialList, api.CredentialListParams{}, &list))
	raw, _ := json.Marshal(list)
	leaks = append(leaks, string(raw))
	if len(list.Credentials) != 3 {
		t.Fatalf("list: %s", raw)
	}
	for _, c := range list.Credentials {
		if c.Version != 1 || c.Fingerprint == "" {
			t.Errorf("admin list lacks version or fingerprint: %+v", c)
		}
	}
	for _, s := range []string{f.d.token, f.d.reg, f.d.sshNeedle} {
		for _, enc := range m4Encodings([]byte(s)) {
			for _, l := range leaks {
				if strings.Contains(l, enc) {
					t.Errorf("a secret value appears in a credential method result")
				}
			}
		}
	}

	// Assign: gl-token to one runner, deploy to the whole fleet, reg to the other runner. Each
	// runner receives exactly its effective set (the union), with the right values.
	f.assign("gl-token", "vm", "vm1-dev")
	f.assign("deploy", "vm", "")
	f.assign("reg", "vm", "vm1-qa")
	f.waitHeld("vm1-dev", "deploy", "gl-token")
	f.waitHeld("vm1-qa", "deploy", "reg")
	if f.snd.value(f.id("vm1-dev"), "gl-token") != f.d.token || f.snd.value(f.id("vm1-qa"), "reg") != f.d.reg ||
		f.snd.value(f.id("vm1-qa"), "deploy") != f.d.sshPEM {
		t.Errorf("a runner received a different value than the one stored")
	}
	if got := f.snd.held(f.id("g-1")); len(got) != 0 {
		t.Errorf("the guarded fleet's runner received %v", got)
	}

	// Operators see names, kinds, hosts and status only.
	cl := m7OperatorClient(t, e, f.dirs["ann"])
	waitFor(t, func() bool {
		rl, err := cl.RunnersList(ctx, "vm")
		if err != nil {
			return false
		}
		for _, r := range rl.Runners {
			if r.Name == "vm1-dev" && len(r.Credentials) == 2 {
				return true
			}
		}
		return false
	})
	rl, err := cl.RunnersList(ctx, "vm")
	must(t, err)
	for _, r := range rl.Runners {
		var names []string
		for _, c := range r.Credentials {
			names = append(names, c.Name)
			if c.Kind == "" || len(c.Hosts) == 0 || c.Status != "ok" {
				t.Errorf("runner %s credential view: %+v", r.Name, c)
			}
		}
		sort.Strings(names)
		want := map[string]string{"vm1-dev": "deploy,gl-token", "vm1-qa": "deploy,reg"}[r.Name]
		if strings.Join(names, ",") != want {
			t.Errorf("runners.list %s credentials = %v, want %s", r.Name, names, want)
		}
		if !contains(r.Features, transport.FeatureHost) {
			t.Errorf("runners.list %s features = %v", r.Name, r.Features)
		}
	}
	raw, _ = json.Marshal(rl)
	for _, bad := range []string{"fingerprint", "expires", "SHA256:", "version"} {
		if strings.Contains(string(raw), bad) {
			t.Errorf("runners.list shows %q: %s", bad, raw)
		}
	}

	// Unassign takes effect at once: the next sync no longer carries it.
	f.unassign("gl-token", "vm", "vm1-dev")
	f.waitHeld("vm1-dev", "deploy")
	if _, err := e.svc.ResolveCredentials(ctx, f.id("vm1-dev"), []string{"gl-token"}); !errors.Is(err, service.ErrCredentialNotHeld) {
		t.Errorf("a removed credential can still be requested: %v", err)
	}
	f.assign("gl-token", "vm", "vm1-dev")
	f.waitHeld("vm1-dev", "deploy", "gl-token")

	// Restart central with the wrong key: refused. Rekey: the new key opens, the old one no longer.
	m7KeyRestartAndRekey(t)
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// m7KeyRestartAndRekey runs on a separate central so the running one keeps its key.
func m7KeyRestartAndRekey(t *testing.T) {
	ctx := m3ctx(t)
	oldKey, _, err := secrets.GenerateKey()
	must(t, err)
	newKey, _, err := secrets.GenerateKey()
	must(t, err)
	wrong, _, err := secrets.GenerateKey()
	must(t, err)
	e2, _, snd2 := newM7EnvDB(t, time.Now, oldKey, nil)
	value := []byte("m7decoy-rekey-" + randHex(t, 8))
	sum := sha256.Sum256(value)
	_, err = e2.svc.SetCredential(ctx, e2.admin, service.SourceCLI, api.CredentialSetParams{
		CredMeta: transport.CredMeta{Name: "rk", Kind: transport.CredHTTPSToken, Hosts: []string{"gitlab.test"}, Auth: transport.AuthBasic, User: "u"},
		Length:   len(value), SHA256: hex.EncodeToString(sum[:])}, value)
	must(t, err)
	r := e2.addRunner("rk-1", store.StatusActive)
	_, err = e2.svc.AssignCredential(ctx, e2.admin, service.SourceCLI, api.CredentialAssignParams{Name: "rk", Fleet: testFleet, Runner: "rk-1"})
	must(t, err)

	restart := func(k secrets.Key, s service.CredentialSender) error {
		cfg := service.Config{Store: e2.db, Policy: policy.TablePolicy{}, Live: e2.reg, HostKey: e2.hostSigner.PublicKey(),
			Now: time.Now, OperatorConns: e2.ops, Logf: e2.logf, ReaperInterval: time.Hour}
		if k != (secrets.Key{}) {
			cfg.Credentials = service.CredentialsConfig{Key: m7Ring{k}, Sender: s}
		}
		_, err := service.New(cfg)
		return err
	}
	if err := restart(wrong, nil); err == nil {
		t.Error("central started with the wrong key")
	}
	if err := restart(secrets.Key{}, nil); err == nil {
		t.Error("central started with stored credentials and no key")
	}
	if err := restart(oldKey, nil); err != nil {
		t.Errorf("central did not start with the right key: %v", err)
	}

	// Rekey: the transaction of `central secrets rekey` (cmd/mirafleet rekeyCentral).
	rekey := func(id, version int64, _, ct []byte) ([]byte, []byte, error) {
		aad := []byte(fmt.Sprintf("mirafleet credential v1|%d|%d", id, version))
		sealed, err := secrets.Rekey(oldKey, newKey, ct, aad, aad)
		if err != nil {
			return nil, nil, err
		}
		const nonceAt, nonceLen = 1 + 4, 12
		return append([]byte(nil), sealed[nonceAt:nonceAt+nonceLen]...), sealed, nil
	}
	n, err := e2.db.RekeyCredentials(ctx, rekey, newKey.Check(), time.Now(), store.AuditEvent{Time: time.Now(), Source: "cli",
		ActorKind: "admin", Actor: "test", Action: "secrets.rekey", Outcome: "ok"})
	must(t, err)
	if n != 1 {
		t.Fatalf("rekey re-encrypted %d credentials", n)
	}
	if err := restart(oldKey, nil); err == nil {
		t.Error("central started with the old key after a rekey")
	}
	// The new key starts and opens the credential: delivery carries the original value.
	cfg := service.Config{Store: e2.db, Policy: policy.TablePolicy{}, Live: e2.reg, HostKey: e2.hostSigner.PublicKey(),
		Now: time.Now, OperatorConns: e2.ops, Logf: e2.logf, ReaperInterval: time.Hour,
		Credentials: service.CredentialsConfig{Key: m7Ring{newKey}, Sender: snd2}}
	snd2.inner = noopSender{}
	svc3, err := service.New(cfg)
	must(t, err)
	must(t, svc3.DeliverRunner(ctx, r.ID))
	if got := snd2.value(r.ID, "rk"); got != string(value) {
		t.Errorf("after rekey the delivered value differs from the stored one")
	}
}

type noopSender struct{}

func (noopSender) SyncCredentials(context.Context, int64, *transport.CredsSyncBody) error { return nil }

// ---- 2. Placement ----

func m7Placement(t *testing.T, f *m7Fix) {
	ctx := m3ctx(t)
	e := f.e
	f.waitHeld("vm1-dev", "deploy", "gl-token")

	// Open needs every requested credential: gl-token is held only by vm1-dev.
	a1 := f.server("ann", "ann-1", f.proj)
	r := f.open(a1, "", []string{"gl-token"})
	if r.IsErr || f.wsRunner(r.str("workspace", "id")) != "vm1-dev" {
		t.Fatalf("open with gl-token: %s", r.Text)
	}
	// Nobody holds reg and gl-token together; dev lacks reg.
	a2 := f.server("ann", "ann-2", f.proj)
	if r := f.open(a2, "", []string{"gl-token", "reg"}); !r.IsErr {
		t.Errorf("open with credentials no runner holds together: %s", r.Text)
	}
	if r := f.open(a2, "dev", []string{"reg"}); !r.IsErr {
		t.Errorf("open on dev with a credential it lacks: %s", r.Text)
	}
	if r := f.open(a2, "", []string{"no-such-credential"}); !r.IsErr {
		t.Errorf("open with an unknown credential: %s", r.Text)
	}

	// Narrowing works: this session uses only deploy, so the runner wires an agent but no proxy.
	r = f.open(a2, "dev", []string{"deploy"})
	if r.IsErr {
		t.Fatalf("narrowed open: %s", r.Text)
	}
	envNarrow := f.hosts["vm1-dev"].SessionEnv(workspace.ID(r.str("workspace", "id")))
	env1 := f.hosts["vm1-dev"].SessionEnv(workspace.ID(f.wsOf(a1)))
	if envNarrow["SSH_AUTH_SOCK"] == "" || envNarrow["HTTPS_PROXY"] != "" {
		t.Errorf("narrowed to deploy: env has agent=%v proxy=%v", envNarrow["SSH_AUTH_SOCK"] != "", envNarrow["HTTPS_PROXY"] != "")
	}
	if env1["HTTPS_PROXY"] == "" || env1["SSH_AUTH_SOCK"] != "" {
		t.Errorf("narrowed to gl-token: env has agent=%v proxy=%v", env1["SSH_AUTH_SOCK"] != "", env1["HTTPS_PROXY"] != "")
	}
	// Default: everything the runner holds.
	a3 := f.server("ann", "ann-3", f.proj)
	r = f.open(a3, "dev", nil)
	if r.IsErr {
		t.Fatalf("default open: %s", r.Text)
	}
	env3 := f.hosts["vm1-dev"].SessionEnv(workspace.ID(r.str("workspace", "id")))
	if env3["SSH_AUTH_SOCK"] == "" || env3["HTTPS_PROXY"] == "" {
		t.Errorf("default session lacks agent or proxy: %v", env3)
	}

	// Widening is refused: the held set is the ceiling, whatever the session asks.
	if _, err := e.svc.ResolveCredentials(ctx, f.id("vm1-dev"), []string{"deploy", "reg"}); !errors.Is(err, service.ErrCredentialNotHeld) {
		t.Errorf("widening beyond the held set: %v", err)
	}
	got, err := e.svc.ResolveCredentials(ctx, f.id("vm1-dev"), []string{"deploy"})
	if err != nil || len(got) != 1 || got[0] != "deploy" {
		t.Errorf("narrowing: %v %v", got, err)
	}
	for _, s := range []*m5Server{a1, a2, a3} {
		if r := s.call("workspace_close", nil); r.IsErr {
			t.Errorf("close: %s", r.Text)
		}
	}
}

// wsOf returns the workspace id the server is bound to (via workspace_list).
func (f *m7Fix) wsOf(s *m5Server) string {
	f.t.Helper()
	r := s.call("workspace_open", nil)
	if r.IsErr {
		f.t.Fatalf("workspace_open: %s", r.Text)
	}
	return r.str("workspace", "id")
}

var _ = time.Second
