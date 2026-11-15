package service

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/secrets"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

const (
	decoyTok  = "glpat-DECOY-aaaa1111bbbb2222"
	decoyNew  = "glpat-DECOY-NEWNEW9999"
	decoyPass = "hunter2-DECOY-registry"
)

type sentSet struct {
	runner int64
	gen    int64
	creds  map[string]string // name -> value
	vers   map[string]int64
}

type fakeSender struct {
	mu   sync.Mutex
	sent []sentSet
	fail bool
}

func (f *fakeSender) SyncCredentials(_ context.Context, id int64, b *transport.CredsSyncBody) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return fmt.Errorf("offline")
	}
	s := sentSet{runner: id, gen: b.Generation, creds: map[string]string{}, vers: map[string]int64{}}
	for _, c := range b.Credentials {
		s.creds[c.Name] = c.Value
		s.vers[c.Name] = c.Version
	}
	f.sent = append(f.sent, s)
	return nil
}

func (f *fakeSender) last(id int64) (sentSet, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.sent) - 1; i >= 0; i-- {
		if f.sent[i].runner == id {
			return f.sent[i], true
		}
	}
	return sentSet{}, false
}

func (f *fakeSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

type credEnv struct {
	*env
	cs   *Service
	snd  *fakeSender
	key  secrets.Key
	gl   *httptest.Server
	hits int
	glFn func(w http.ResponseWriter, r *http.Request)
}

func newCredEnv(t *testing.T) *credEnv {
	t.Helper()
	e := newEnv(t)
	key, _, err := secrets.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	ce := &credEnv{env: e, snd: &fakeSender{}, key: key}
	ce.gl = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ce.hits++
		ce.glFn(w, r)
	}))
	t.Cleanup(ce.gl.Close)
	ce.cs = ce.service(key)
	return ce
}

func (ce *credEnv) service(key secrets.Key) *Service {
	ce.t.Helper()
	svc, err := ce.build(key)
	if err != nil {
		ce.t.Fatal(err)
	}
	return svc
}

type testRing struct{ k secrets.Key }

func (r testRing) Seal(pt, aad []byte) ([]byte, error) { return secrets.Seal(r.k, pt, aad) }
func (r testRing) Open(ct, aad []byte) ([]byte, error) { return secrets.Open(r.k, ct, aad) }
func (r testRing) Check() []byte                       { return r.k.Check() }
func (r testRing) VerifyCheck(stored []byte) bool      { return r.k.VerifyCheck(stored) }

func (ce *credEnv) build(key secrets.Key) (*Service, error) {
	var ring KeyRing
	if key != (secrets.Key{}) {
		ring = testRing{key}
	}
	return New(Config{Store: ce.db, Policy: ce.policy, Live: ce.live, HostKey: ce.host, OperatorConns: ce.conns,
		Now: func() time.Time { return t0 },
		Logf: func(f string, a ...any) {
			ce.logMu.Lock()
			fmt.Fprintf(ce.logs, f+"\n", a...)
			ce.logMu.Unlock()
		},
		Credentials: CredentialsConfig{Key: ring, Sender: ce.snd,
			RotateBaseURL: func(string) string { return ce.gl.URL }}})
}

func setParams(name, kind, value string) api.CredentialSetParams {
	sum := sha256.Sum256([]byte(value))
	p := api.CredentialSetParams{CredMeta: transport.CredMeta{Name: name, Kind: kind, Hosts: []string{"gitlab.example.com"}},
		Length: len(value), SHA256: hex.EncodeToString(sum[:])}
	switch kind {
	case transport.CredHTTPSToken:
		p.Auth = transport.AuthBearer
	case transport.CredRegistry:
		p.User = "ci"
	}
	return p
}

func (ce *credEnv) setTok(name, value string) api.CredentialSetResult {
	ce.t.Helper()
	res, err := ce.cs.SetCredential(ce.ctx, ce.admin, SourceCLI, setParams(name, transport.CredHTTPSToken, value), []byte(value))
	if err != nil {
		ce.t.Fatal(err)
	}
	return res
}

func (ce *credEnv) assign(name, fleet, runner string) {
	ce.t.Helper()
	if _, err := ce.cs.AssignCredential(ce.ctx, ce.admin, SourceCLI, api.CredentialAssignParams{Name: name, Fleet: fleet, Runner: runner}); err != nil {
		ce.t.Fatal(err)
	}
}

// leakText gathers everything a decoy must never appear in.
func (ce *credEnv) leakText() string {
	var b strings.Builder
	for _, ev := range ce.audit() {
		fmt.Fprintf(&b, "%+v\n", ev)
	}
	b.WriteString(ce.logs.String())
	infos, _ := ce.cs.ListCredentials(ce.ctx, ce.admin, SourceCLI, "")
	j, _ := json.Marshal(infos)
	b.Write(j)
	cs, _ := ce.db.ListCredentials(ce.ctx)
	j, _ = json.Marshal(cs)
	b.Write(j)
	return b.String()
}

func TestWrongKeyRefusesStart(t *testing.T) {
	ce := newCredEnv(t) // stored the key check for ce.key
	ce.setTok("tok", decoyTok)
	other, otherB64, _ := secrets.GenerateKey()
	_, err := ce.build(other)
	if err == nil {
		t.Fatal("wrong key started")
	}
	if strings.Contains(err.Error(), otherB64) || strings.Contains(err.Error(), decoyTok) {
		t.Fatalf("error leaks: %v", err)
	}
	if _, err := ce.build(secrets.Key{}); err == nil {
		t.Fatal("missing key started with stored credentials")
	}
	if _, err := ce.build(ce.key); err != nil {
		t.Fatalf("right key refused: %v", err)
	}
}

func TestNoKeyDisablesSet(t *testing.T) {
	e := newEnv(t) // no credentials, no key
	_, err := e.svc.SetCredential(e.ctx, e.admin, SourceCLI, setParams("tok", transport.CredHTTPSToken, "x"), []byte("x"))
	if ToAPIError(err).Code != api.CodeUnavailable {
		t.Fatalf("want unavailable, got %v", err)
	}
}

func TestSetListDeleteNoLeak(t *testing.T) {
	ce := newCredEnv(t)
	ce.fleet("f")
	r := ce.enrollRunner("f", "r1", true)
	ce.setTok("tok", decoyTok)
	ce.assign("tok", "f", "r1")
	// replace bumps version and re-delivers the new value
	res := ce.setTok("tok", decoyTok+"-v2")
	if !res.Replaced || res.Credential.Version != 2 {
		t.Fatalf("replace: %+v", res)
	}
	if got, _ := ce.snd.last(r.ID); got.creds["tok"] != decoyTok+"-v2" || got.vers["tok"] != 2 {
		t.Fatalf("not redelivered: %+v", got)
	}
	// ciphertext at rest does not hold the value, and opens only with the right AAD
	sc, err := ce.db.GetSealedCredential(ce.ctx, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sc.Ciphertext), "DECOY") {
		t.Fatal("plaintext at rest")
	}
	if _, err := secrets.Open(ce.key, sc.Ciphertext, credAAD(sc.ID, sc.Version+1)); err == nil {
		t.Fatal("opened with wrong version AAD")
	}
	// a decoy-bearing bad request must not echo the value
	bad := setParams("tok2", transport.CredHTTPSToken, decoyTok)
	bad.SHA256 = strings.Repeat("0", 64)
	_, err = ce.cs.SetCredential(ce.ctx, ce.admin, SourceCLI, bad, []byte(decoyTok))
	if err == nil || strings.Contains(err.Error(), "DECOY") {
		t.Fatalf("bad sha: %v", err)
	}
	_, err = ce.cs.SetCredential(ce.ctx, ce.admin, SourceCLI, setParams("k", transport.CredSSHKey, decoyTok), []byte(decoyTok))
	if err == nil || strings.Contains(err.Error(), "DECOY") {
		t.Fatalf("bad ssh key: %v", err)
	}
	if txt := ce.leakText(); strings.Contains(txt, "DECOY") || strings.Contains(txt, "BEGIN") {
		t.Fatalf("leak:\n%s", txt)
	}
	// delete: audited, runner gets an empty set
	if err := ce.cs.DeleteCredential(ce.ctx, ce.admin, SourceCLI, "tok"); err != nil {
		t.Fatal(err)
	}
	if got, _ := ce.snd.last(r.ID); len(got.creds) != 0 {
		t.Fatalf("delete not synced: %+v", got)
	}
	if !hasAction(ce.audit(), api.MethodCredentialDelete) || !hasAction(ce.audit(), api.MethodCredentialSet) {
		t.Fatal("missing audit rows")
	}
}

func hasAction(evs []store.AuditEvent, a string) bool {
	for _, e := range evs {
		if e.Action == a && e.Outcome == OutcomeOK {
			return true
		}
	}
	return false
}

func TestSSHKeyAndRegistryKinds(t *testing.T) {
	ce := newCredEnv(t)
	ce.fleet("f")
	r := ce.enrollRunner("f", "r1", true)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	blk, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	pemKey := string(pem.EncodeToMemory(blk))
	signer, _ := ssh.NewSignerFromKey(priv)
	p := setParams("deploy", transport.CredSSHKey, pemKey)
	p.KnownHosts = []string{"gitlab.example.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDECOYKNOWNHOST"}
	res, err := ce.cs.SetCredential(ce.ctx, ce.admin, SourceCLI, p, []byte(pemKey))
	if err != nil {
		t.Fatal(err)
	}
	if res.Credential.Fingerprint != ssh.FingerprintSHA256(signer.PublicKey()) {
		t.Fatalf("fingerprint %q", res.Credential.Fingerprint)
	}
	if _, err := ce.cs.SetCredential(ce.ctx, ce.admin, SourceCLI, setParams("reg", transport.CredRegistry, decoyPass), []byte(decoyPass)); err != nil {
		t.Fatal(err)
	}
	ce.assign("deploy", "f", "")
	ce.assign("reg", "f", "r1")
	got, _ := ce.snd.last(r.ID)
	if got.creds["deploy"] != pemKey || got.creds["reg"] != decoyPass {
		t.Fatalf("delivery: %v", got)
	}
	if txt := ce.leakText(); strings.Contains(txt, "OPENSSH") || strings.Contains(txt, "DECOY-registry") {
		t.Fatalf("leak:\n%s", txt)
	}
}

func TestAssignmentUnionAndChangeDelivery(t *testing.T) {
	ce := newCredEnv(t)
	ce.fleet("f")
	ce.fleet("g")
	r1 := ce.enrollRunner("f", "r1", true)
	r2 := ce.enrollRunner("f", "r2", true)
	r3 := ce.enrollRunner("g", "r3", true)
	ce.setTok("fleetwide", "v-fleet")
	ce.setTok("direct", "v-direct")
	ce.assign("fleetwide", "f", "")
	ce.assign("direct", "f", "r1")
	ce.assign("fleetwide", "f", "r1") // duplicate via direct: still once
	if got, _ := ce.snd.last(r1.ID); len(got.creds) != 2 || got.creds["fleetwide"] == "" || got.creds["direct"] == "" {
		t.Fatalf("r1 union: %+v", got)
	}
	if got, _ := ce.snd.last(r2.ID); len(got.creds) != 1 || got.creds["fleetwide"] == "" {
		t.Fatalf("r2: %+v", got)
	}
	if _, ok := ce.snd.last(r3.ID); ok {
		t.Fatal("unrelated runner received a sync")
	}
	// unassign the direct one: r1 loses it at once
	n := ce.snd.count()
	if _, err := ce.cs.UnassignCredential(ce.ctx, ce.admin, SourceCLI, api.CredentialUnassignParams{Name: "direct", Fleet: "f", Runner: "r1"}); err != nil {
		t.Fatal(err)
	}
	if ce.snd.count() == n {
		t.Fatal("unassign triggered no delivery")
	}
	if got, _ := ce.snd.last(r1.ID); len(got.creds) != 1 || got.creds["direct"] != "" {
		t.Fatalf("r1 after unassign: %+v", got)
	}
	// reconnect resends the full set
	n = ce.snd.count()
	if err := ce.cs.RunnerConnected(ce.ctx, r2.ID); err != nil || ce.snd.count() != n+1 {
		t.Fatalf("connect: %v", err)
	}
	// delivery status: pending until confirmed, then ok
	infos, _ := ce.cs.ListCredentials(ce.ctx, ce.admin, SourceCLI, "fleetwide")
	for _, d := range infos[0].Delivery {
		if d.Status != transport.CredOK {
			t.Fatalf("delivery %+v", d)
		}
	}
	ce.snd.fail = true
	ce.setTok("fleetwide", "v-fleet-2")
	infos, _ = ce.cs.ListCredentials(ce.ctx, ce.admin, SourceCLI, "fleetwide")
	for _, d := range infos[0].Delivery {
		if d.Status != "pending" {
			t.Fatalf("want pending, got %+v", d)
		}
	}
}

func TestPlacementAndNarrowing(t *testing.T) {
	ce := newCredEnv(t)
	ce.fleet("f")
	r1 := ce.enrollRunner("f", "r1", true)
	r2 := ce.enrollRunner("f", "r2", true)
	ce.setTok("a", "va")
	ce.setTok("b", "vb")
	ce.assign("a", "f", "")
	ce.assign("b", "f", "r1")
	rs := []store.Runner{r1, r2}
	names := func(rs []store.Runner) string {
		var n []string
		for _, r := range rs {
			n = append(n, r.Name)
		}
		return strings.Join(n, ",")
	}
	for _, c := range []struct {
		req  []string
		want string
	}{{nil, "r1,r2"}, {[]string{"a"}, "r1,r2"}, {[]string{"b"}, "r1"}, {[]string{"a", "b"}, "r1"}, {[]string{"nope"}, ""}} {
		got, err := ce.cs.FilterRunnersByCredentials(ce.ctx, rs, c.req)
		if err != nil || names(got) != c.want {
			t.Errorf("filter %v = %q, %v; want %q", c.req, names(got), err, c.want)
		}
	}
	all, err := ce.cs.ResolveCredentials(ce.ctx, r1.ID, nil)
	if err != nil || strings.Join(all, ",") != "a,b" {
		t.Fatalf("default = all held: %v %v", all, err)
	}
	if sub, err := ce.cs.ResolveCredentials(ce.ctx, r1.ID, []string{"b"}); err != nil || len(sub) != 1 || sub[0] != "b" {
		t.Fatalf("narrow: %v %v", sub, err)
	}
	if _, err := ce.cs.ResolveCredentials(ce.ctx, r2.ID, []string{"b"}); err != ErrCredentialNotHeld {
		t.Fatalf("narrowing must never widen: %v", err)
	}
}

func TestAdminOnlyAndOperatorView(t *testing.T) {
	ce := newCredEnv(t)
	ce.fleet("f")
	r := ce.enrollRunner("f", "r1", true)
	op := ce.operator("alice")
	ce.setTok("tok", decoyTok)
	ce.assign("tok", "f", "")
	if _, err := ce.cs.SetCredential(ce.ctx, op, SourceAPI, setParams("x", transport.CredHTTPSToken, "v"), []byte("v")); ToAPIError(err).Code != api.CodeForbidden {
		t.Fatalf("operator set: %v", err)
	}
	if _, err := ce.cs.ListCredentials(ce.ctx, op, SourceAPI, ""); ToAPIError(err).Code != api.CodeForbidden {
		t.Fatalf("operator list: %v", err)
	}
	if err := ce.cs.DeleteCredential(ce.ctx, op, SourceAPI, "tok"); ToAPIError(err).Code != api.CodeForbidden {
		t.Fatalf("operator delete: %v", err)
	}
	if _, err := ce.cs.AssignCredential(ce.ctx, op, SourceAPI, api.CredentialAssignParams{Name: "tok", Fleet: "f"}); ToAPIError(err).Code != api.CodeForbidden {
		t.Fatalf("operator assign: %v", err)
	}
	if _, err := ce.cs.RotateCredential(ce.ctx, op, SourceAPI, "tok"); ToAPIError(err).Code != api.CodeForbidden {
		t.Fatalf("operator rotate: %v", err)
	}
	ce.cs.ReportCredentialStatus(r.ID, []transport.HeartbeatCredential{{Name: "tok", Kind: "https-token", Hosts: []string{"gitlab.example.com"}, Status: transport.CredInvalid}})
	views, err := ce.cs.RunnerCredentials(ce.ctx, op, SourceAPI, r.Ref())
	if err != nil || len(views) != 1 || views[0].Name != "tok" || views[0].Status != transport.CredInvalid {
		t.Fatalf("operator view: %+v %v", views, err)
	}
	j, _ := json.Marshal(views)
	if strings.Contains(string(j), "DECOY") || strings.Contains(string(j), "SHA256") {
		t.Fatalf("operator view leaks: %s", j)
	}
}

func (ce *credEnv) rotating(expires time.Time) {
	ce.t.Helper()
	p := setParams("gl", transport.CredHTTPSToken, decoyTok)
	p.Rotate, p.RotateProject, p.Expires = api.RotateGitLabSelf, "grp/proj", &expires
	if _, err := ce.cs.SetCredential(ce.ctx, ce.admin, SourceCLI, p, []byte(decoyTok)); err != nil {
		ce.t.Fatal(err)
	}
}

func TestGitLabRotation(t *testing.T) {
	ce := newCredEnv(t)
	ce.fleet("f")
	r := ce.enrollRunner("f", "r1", true)
	ce.rotating(t0.Add(30 * 24 * time.Hour))
	ce.assign("gl", "f", "r1")
	ce.glFn = func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost || req.URL.EscapedPath() != "/api/v4/projects/grp%2Fproj/access_tokens/self/rotate" ||
			req.Header.Get("PRIVATE-TOKEN") != decoyTok {
			t.Errorf("bad request %s %s", req.Method, req.URL)
			http.Error(w, "no", 400)
			return
		}
		fmt.Fprintf(w, `{"token":%q,"expires_at":"2026-12-13"}`, decoyNew)
	}
	if n := ce.cs.RotateDue(ce.ctx); n != 0 || ce.hits != 0 {
		t.Fatalf("rotated early: %d", n)
	}
	// 5 days left: due
	ce.rotating(t0.Add(5 * 24 * time.Hour))
	sendsBefore := ce.snd.count()
	if n := ce.cs.RotateDue(ce.ctx); n != 1 || ce.hits != 1 {
		t.Fatalf("rotate due: n=%d hits=%d", n, ce.hits)
	}
	got, _ := ce.snd.last(r.ID)
	if got.creds["gl"] != decoyNew || ce.snd.count() <= sendsBefore {
		t.Fatalf("new value not pushed: %+v", got)
	}
	infos, _ := ce.cs.ListCredentials(ce.ctx, ce.admin, SourceCLI, "gl")
	if infos[0].Version < 3 || infos[0].Expires == nil || infos[0].Expires.Before(t0.Add(29*24*time.Hour)) {
		t.Fatalf("after rotation: %+v", infos[0])
	}
	if !hasAction(ce.audit(), api.MethodCredentialRotate) {
		t.Fatal("rotation not audited")
	}
	if txt := ce.leakText(); strings.Contains(txt, "DECOY") {
		t.Fatalf("leak:\n%s", txt)
	}
}

func TestGitLabRotationFailure(t *testing.T) {
	ce := newCredEnv(t)
	ce.fleet("f")
	r := ce.enrollRunner("f", "r1", true)
	ce.rotating(t0.Add(2 * 24 * time.Hour))
	ce.assign("gl", "f", "r1")
	ce.glFn = func(w http.ResponseWriter, req *http.Request) {
		http.Error(w, `{"message":"401 Unauthorized `+decoyTok+`"}`, http.StatusUnauthorized)
	}
	before, _ := ce.snd.last(r.ID)
	if n := ce.cs.RotateDue(ce.ctx); n != 0 || ce.hits != 1 {
		t.Fatalf("n=%d hits=%d", n, ce.hits)
	}
	// throttled: a second sweep inside the hour does not hit GitLab again
	ce.cs.RotateDue(ce.ctx)
	if ce.hits != 1 {
		t.Fatalf("not throttled: %d hits", ce.hits)
	}
	// admin rotate-now surfaces a generic error
	_, err := ce.cs.RotateCredential(ce.ctx, ce.admin, SourceCLI, "gl")
	if err == nil || strings.Contains(err.Error(), "DECOY") || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err: %v", err)
	}
	var failed bool
	for _, ev := range ce.audit() {
		if ev.Action == api.MethodCredentialRotate && ev.Outcome == "error" && ev.Subject == "gl" {
			failed = true
		}
	}
	if !failed {
		t.Fatal("failure not audited")
	}
	infos, _ := ce.cs.ListCredentials(ce.ctx, ce.admin, SourceCLI, "gl")
	if infos[0].RotateError == "" || infos[0].Version != 1 {
		t.Fatalf("surfaced: %+v", infos[0])
	}
	if after, _ := ce.snd.last(r.ID); after.creds["gl"] != before.creds["gl"] {
		t.Fatal("value changed on failure")
	}
	if txt := ce.leakText(); strings.Contains(txt, "DECOY") {
		t.Fatalf("leak:\n%s", txt)
	}
	// a replace clears the surfaced error
	ce.rotating(t0.Add(40 * 24 * time.Hour))
	if infos, _ = ce.cs.ListCredentials(ce.ctx, ce.admin, SourceCLI, "gl"); infos[0].RotateError != "" {
		t.Fatal("error not cleared by replace")
	}
}
