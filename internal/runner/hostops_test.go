package runner

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"encoding/pem"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/broker"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/fake"
)

const (
	decoyToken = "DECOY-token-7f3a9c1e5b"
	decoyReg   = "DECOY-registry-2d8e4a6f10"
)

// testKey is an ed25519 deploy key plus its decoy needle (a slice of the key body).
type testKey struct {
	pem    string
	needle string
	pub    ssh.PublicKey
	signer ssh.Signer
}

func newTestKey(t *testing.T) testKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	blk, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	pemData := string(pem.EncodeToMemory(blk))
	lines := strings.Split(strings.TrimSpace(pemData), "\n")
	signer, _ := ssh.NewSignerFromKey(priv)
	spub, _ := ssh.NewPublicKey(pub)
	return testKey{pem: pemData, needle: lines[2], pub: spub, signer: signer}
}

func hostKeyLine(t *testing.T, host string) (string, ssh.Signer) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	spub, _ := ssh.NewPublicKey(pub)
	return host + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(spub))), signer
}

func tokenCred(value string, hosts ...string) transport.CredSecret {
	return transport.CredSecret{CredMeta: transport.CredMeta{Name: "gl-token", Kind: transport.CredHTTPSToken, Hosts: hosts,
		Auth: transport.AuthBasic, User: "oauth2"}, Version: 1, Value: value}
}

func regCred() transport.CredSecret {
	return transport.CredSecret{CredMeta: transport.CredMeta{Name: "reg", Kind: transport.CredRegistry,
		Hosts: []string{"reg.test"}, User: "svc"}, Version: 1, Value: decoyReg}
}

func sshCred(k testKey, kh string, hosts ...string) transport.CredSecret {
	return transport.CredSecret{CredMeta: transport.CredMeta{Name: "deploy", Kind: transport.CredSSHKey, Hosts: hosts,
		KnownHosts: []string{kh}}, Version: 1, Value: k.pem}
}

func hostHarness(t *testing.T) (*wsHarness, *fake.Executor) {
	t.Helper()
	old := systemBundle
	systemBundle = func() ([]byte, error) {
		return []byte("-----BEGIN CERTIFICATE-----\nSYSTEM\n-----END CERTIFICATE-----\n"), nil
	}
	t.Cleanup(func() { systemBundle = old })
	ex := fake.New()
	// A short path: unix socket paths are limited to about 100 bytes.
	root, err := os.MkdirTemp("", "m7s")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	ex.SessionRoot = root
	ex.ExecFunc = func(ctx context.Context, id workspace.ID, r workspace.ExecRequest) (workspace.ExecResult, error) {
		io.WriteString(r.Stdout, "hello\nworld\n")
		return workspace.ExecResult{}, nil
	}
	return newHarness(t, wsYAML, ex, nil), ex
}

func (h *wsHarness) sync(gen int64, creds ...transport.CredSecret) {
	h.t.Helper()
	body, err := transport.EncodeCredsSync(&transport.CredsSyncBody{Generation: gen, Credentials: creds})
	if err != nil {
		h.t.Fatal(err)
	}
	res := h.want(transport.WorkspaceRequest{Op: transport.OpCredsSync, StdinLen: int64(len(body))}, body, transport.CodeOK)
	if res.Creds == nil || res.Creds.Count != len(creds) {
		h.t.Fatalf("creds result: %+v", res.Creds)
	}
}

func (h *wsHarness) session(id workspace.ID) *devSession {
	h.runnr.dev.mu.Lock()
	defer h.runnr.dev.mu.Unlock()
	return h.runnr.dev.sessions[id]
}

func envString(env map[string]string) string {
	var parts []string
	for k, v := range env {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return strings.Join(parts, "\n")
}

func noDecoy(t *testing.T, what, data string, decoys ...string) {
	t.Helper()
	for _, d := range decoys {
		if strings.Contains(data, d) {
			t.Fatalf("%s contains a decoy secret", what)
		}
	}
}

func dirText(t *testing.T, dir string) string {
	t.Helper()
	var sb strings.Builder
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			b, _ := os.ReadFile(p)
			sb.WriteString(p + "\n")
			sb.Write(b)
		}
		return nil
	})
	return sb.String()
}

func lastHeartbeat(h *wsHarness, ok func(transport.Heartbeat) bool) transport.Heartbeat {
	h.t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case hb := <-h.hbs:
			if ok(hb) {
				return hb
			}
		case <-deadline:
			h.t.Fatal("no matching heartbeat")
		}
	}
}

func TestSessionCredentialWiringNeverExposesSecrets(t *testing.T) {
	h, ex := hostHarness(t)
	k := newTestKey(t)
	kh, _ := hostKeyLine(t, "ssh.test")
	decoys := []string{decoyToken, decoyReg, k.needle}
	h.sync(1, tokenCred(decoyToken, "git.test"), regCred(), sshCred(k, kh, "ssh.test"))

	id := h.create()
	env := ex.SessionEnv(id)
	for _, name := range []string{"HTTPS_PROXY", "HTTP_PROXY", "SSL_CERT_FILE", "GIT_SSL_CAINFO", "NODE_EXTRA_CA_CERTS",
		"REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE", "PIP_CERT", "JAVA_TOOL_OPTIONS", "SSH_AUTH_SOCK", "GIT_SSH_COMMAND"} {
		if env[name] == "" {
			t.Errorf("env %s missing", name)
		}
	}
	noDecoy(t, "session env", envString(env), decoys...)
	dir, _ := ex.SessionDir(id)
	if !strings.HasPrefix(env["SSL_CERT_FILE"], dir+"/") || !strings.HasPrefix(env["SSH_AUTH_SOCK"], dir+"/") {
		t.Fatalf("files are not under the session dir: %v", env)
	}
	bundle, err := os.ReadFile(env["SSL_CERT_FILE"])
	if err != nil || !strings.Contains(string(bundle), "SYSTEM") || strings.Count(string(bundle), "BEGIN CERTIFICATE") != 2 {
		t.Fatalf("ca bundle: %v %q", err, bundle)
	}
	noDecoy(t, "session dir files", dirText(t, dir), decoys...)
	if cfg, _ := os.ReadFile(filepath.Join(dir, "ssh_config")); !strings.Contains(string(cfg), "Host ssh.test") ||
		!strings.Contains(string(cfg), "IdentitiesOnly yes") {
		t.Fatalf("ssh_config: %s", cfg)
	}
	noDecoy(t, "audit log", h.log.String(), decoys...)

	// The agent lists the public key only.
	c, err := net.Dial("unix", env["SSH_AUTH_SOCK"])
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ids, err := agent.NewClient(c).List()
	if err != nil || len(ids) != 1 || string(ids[0].Blob) != string(k.pub.Marshal()) {
		t.Fatalf("agent list: %v %v", ids, err)
	}

	// Heartbeat: names, kinds, hosts, status; never a value.
	hb := lastHeartbeat(h, func(hb transport.Heartbeat) bool { return len(hb.Credentials) == 3 })
	raw, _ := json.Marshal(hb)
	noDecoy(t, "heartbeat", string(raw), decoys...)
	if err := hb.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, c := range hb.Credentials {
		if c.Status != transport.CredOK {
			t.Fatalf("status: %+v", c)
		}
	}
	if strings.Join(hb.Features, ",") != "fs,jobs,creds,events,host" {
		t.Fatalf("features: %v", hb.Features)
	}

	// Uses go to the runner-local audit log and the event table without any value.
	h.runnr.dev.onUse(string(id), broker.Use{Credential: "gl-token", Host: "git.test:443", Status: 200})
	h.runnr.dev.events.mu.Lock()
	n := len(h.runnr.dev.events.pending)
	h.runnr.dev.events.mu.Unlock()
	if n != 1 || !strings.Contains(h.log.String(), `"credential.use"`) {
		t.Fatalf("use not recorded: %d", n)
	}
	noDecoy(t, "audit log after use", h.log.String(), decoys...)
}

func TestCredsSyncReplaceVersusHostSetChange(t *testing.T) {
	h, ex := hostHarness(t)
	h.sync(1, tokenCred(decoyToken, "git.test"))
	id := h.create()
	s := h.session(id)
	dir, _ := ex.SessionDir(id)
	ca1, _ := os.ReadFile(filepath.Join(dir, "ca.pem"))
	b1, env1 := s.brk, ex.SessionEnv(id)

	// Same hosts, new value: the broker, CA and proxy stay; only the value changes.
	h.sync(2, tokenCred("DECOY-token-rotated-99", "git.test"))
	s.mu.Lock()
	same := s.brk == b1
	s.mu.Unlock()
	ca2, _ := os.ReadFile(filepath.Join(dir, "ca.pem"))
	if !same || string(ca1) != string(ca2) || ex.SessionEnv(id)["HTTPS_PROXY"] != env1["HTTPS_PROXY"] {
		t.Fatal("a value-only change must not rebuild the broker")
	}
	if c, ok := s.prov.Lookup("git.test", "443"); !ok || c.Secret != "DECOY-token-rotated-99" {
		t.Fatalf("value not replaced: %v", ok)
	}

	// A new host set rebuilds: new CA, new bundle, new proxy URL; the old proxy is gone.
	oldAddr := b1 // keep for the closed check below
	_ = oldAddr
	oldPort := strings.TrimPrefix(strings.TrimPrefix(env1["HTTPS_PROXY"], "http://"), "")
	oldPort = oldPort[strings.LastIndexByte(oldPort, '@')+1:]
	h.sync(3, tokenCred(decoyToken, "git.test", "other.test"))
	s.mu.Lock()
	rebuilt := s.brk != b1
	s.mu.Unlock()
	ca3, _ := os.ReadFile(filepath.Join(dir, "ca.pem"))
	env3 := ex.SessionEnv(id)
	if !rebuilt || string(ca3) == string(ca1) || env3["HTTPS_PROXY"] == env1["HTTPS_PROXY"] {
		t.Fatal("a changed host set must rebuild the broker, CA and bundle")
	}
	if c, err := net.DialTimeout("tcp", oldPort, time.Second); err == nil {
		c.Close()
		t.Fatal("old proxy still listening")
	}

	// A stale generation is ignored.
	h.sync(2, tokenCred("DECOY-stale", "git.test"))
	if c, _ := s.prov.Lookup("git.test", "443"); c.Secret != decoyToken {
		t.Fatal("stale sync applied")
	}

	// Removing everything stops the proxy and agent and clears the env.
	h.sync(4)
	env4 := ex.SessionEnv(id)
	if len(env4) != 0 {
		t.Fatalf("env after empty sync: %v", env4)
	}
	if _, err := os.Stat(filepath.Join(dir, "ca.pem")); err == nil {
		t.Fatal("ca.pem left")
	}
}

func TestSessionTeardownRemovesSocketsAndProxy(t *testing.T) {
	h, ex := hostHarness(t)
	k := newTestKey(t)
	kh, _ := hostKeyLine(t, "ssh.test")
	h.sync(1, tokenCred(decoyToken, "git.test"), sshCred(k, kh, "ssh.test"))
	id := h.create()
	env := ex.SessionEnv(id)
	dir, _ := ex.SessionDir(id)
	proxy := env["HTTPS_PROXY"]
	addr := proxy[strings.LastIndexByte(proxy, '@')+1:]
	if c, err := net.Dial("tcp", addr); err != nil {
		t.Fatal(err)
	} else {
		c.Close()
	}
	h.want(transport.WorkspaceRequest{Op: transport.OpDestroy, ID: string(id)}, nil, transport.CodeOK)
	if _, err := os.Stat(env["SSH_AUTH_SOCK"]); err == nil {
		t.Fatal("agent socket left")
	}
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		c.Close()
		t.Fatal("proxy left")
	}
	for _, f := range []string{"ca.pem", "ssh_config", "known_hosts"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			t.Fatalf("%s left", f)
		}
	}
	if h.session(id) != nil {
		t.Fatal("session left in the manager")
	}
}

func TestOpenNarrowingAndUnknownCredential(t *testing.T) {
	h, ex := hostHarness(t)
	k := newTestKey(t)
	kh, _ := hostKeyLine(t, "ssh.test")
	h.sync(1, tokenCred(decoyToken, "git.test"), sshCred(k, kh, "ssh.test"))

	req := createReq(workspace.NewID())
	req.Spec.Labels = map[string]string{LabelCredentials: "nope"}
	h.want(req, nil, transport.CodeRefused)

	req = createReq(workspace.NewID())
	req.Spec.Labels = map[string]string{LabelCredentials: "deploy"}
	h.want(req, nil, transport.CodeOK)
	env := ex.SessionEnv(workspace.ID(req.ID))
	if env["SSH_AUTH_SOCK"] == "" || env["HTTPS_PROXY"] != "" {
		t.Fatalf("narrowed to the ssh key only: %v", env)
	}
	// The label never reaches the executor.
	infos, _ := ex.List(context.Background())
	for _, in := range infos {
		if _, ok := in.Labels[LabelCredentials]; ok {
			t.Fatal("credentials label leaked to the executor")
		}
	}

	req = createReq(workspace.NewID())
	req.Spec.Labels = map[string]string{LabelCredentials: ""}
	h.want(req, nil, transport.CodeOK)
	if env := ex.SessionEnv(workspace.ID(req.ID)); len(env) != 0 {
		t.Fatalf("empty list means no credentials: %v", env)
	}
}

func TestEventAggregation(t *testing.T) {
	now := time.UnixMilli(1_000_000_000)
	a := newEventAgg()
	a.now = func() time.Time { return now }
	for i := 0; i < 5; i++ {
		a.record("s1", "c", "git.test:443", transport.EventOK)
	}
	a.record("s1", "c", "git.test:443", transport.EventDenied)
	if b := a.take(false); len(b) != 0 {
		t.Fatalf("open window sent early: %v", b)
	}
	now = now.Add(a.window)
	b := a.take(false)
	if len(b) != 1 || len(b[0].Events) != 2 {
		t.Fatalf("batches: %+v", b)
	}
	total := int64(0)
	for _, ev := range b[0].Events {
		total += ev.Count
		if ev.WindowS != 60 || ev.Type != transport.EventCredentialUse {
			t.Fatalf("event: %+v", ev)
		}
	}
	if total != 6 {
		t.Fatalf("count %d", total)
	}
	if err := b[0].Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestEventBatchingAndDropCount(t *testing.T) {
	a := newEventAgg()
	a.maxKeys = 300
	for i := 0; i < 400; i++ {
		a.record("s", "c", fmt.Sprintf("h%03d.test:443", i), transport.EventOK)
	}
	// An existing key still counts when the table is full.
	a.record("s", "c", "h000.test:443", transport.EventOK)
	batches := a.take(true)
	events, dropped := 0, int64(0)
	for _, b := range batches {
		if len(b.Events) > transport.MaxBatchEvents {
			t.Fatalf("batch of %d events", len(b.Events))
		}
		if err := b.Validate(); err != nil {
			t.Fatal(err)
		}
		events += len(b.Events)
		dropped += b.Dropped
	}
	if events != 300 || dropped != 100 || len(batches) != 3 || batches[0].Dropped != 100 {
		t.Fatalf("events %d dropped %d batches %d", events, dropped, len(batches))
	}
	// Unsent batches go back; what no longer fits is counted.
	a2 := newEventAgg()
	a2.maxKeys = 1
	a2.record("s", "c", "a.test:443", transport.EventOK)
	a2.requeue(batches[:1])
	if a2.dropped < 100 || len(a2.pending) != 1 {
		t.Fatalf("requeue: dropped %d pending %d", a2.dropped, len(a2.pending))
	}
	// A drop count alone is a valid batch.
	a3 := newEventAgg()
	a3.dropped = 7
	if b := a3.take(false); len(b) != 1 || b[0].Dropped != 7 || b[0].Validate() != nil {
		t.Fatalf("dropped-only batch: %+v", b)
	}
}

func TestEventsSentOverTheChannel(t *testing.T) {
	oldW, oldF := eventWindow, eventFlushEvery
	eventWindow, eventFlushEvery = 50*time.Millisecond, 20*time.Millisecond
	got := make(chan *transport.EventBatch, 16)
	var mu sync.Mutex
	chanHook = func(c ssh.NewChannel) bool {
		if c.ChannelType() != transport.ChanEvents {
			return false
		}
		ch, reqs, err := c.Accept()
		if err != nil {
			return true
		}
		go ssh.DiscardRequests(reqs)
		go func() {
			for {
				b, err := transport.ReadEventBatch(ch)
				if err != nil {
					return
				}
				mu.Lock()
				got <- b
				mu.Unlock()
			}
		}()
		return true
	}
	t.Cleanup(func() { chanHook = nil; eventWindow, eventFlushEvery = oldW, oldF })
	h, _ := hostHarness(t)
	h.sync(1, tokenCred(decoyToken, "git.test"))
	id := h.create()
	for i := 0; i < 3; i++ {
		h.runnr.dev.onUse(string(id), broker.Use{Credential: "gl-token", Host: "git.test:443", Status: 200})
	}
	h.runnr.dev.onUse(string(id), broker.Use{Credential: "gl-token", Host: "git.test:443", Status: 401})
	ok, denied := int64(0), int64(0)
	deadline := time.After(10 * time.Second)
	for ok+denied < 4 {
		select {
		case b := <-got:
			raw, _ := json.Marshal(b)
			noDecoy(t, "event batch", string(raw), decoyToken)
			for _, ev := range b.Events {
				if ev.Session != string(id) || ev.Credential != "gl-token" || ev.Host != "git.test:443" {
					t.Fatalf("event: %+v", ev)
				}
				if ev.Status == transport.EventOK {
					ok += ev.Count
				} else if ev.Status == transport.EventDenied {
					denied += ev.Count
				}
			}
		case <-deadline:
			t.Fatalf("events: ok %d denied %d", ok, denied)
		}
	}
	if ok != 3 || denied != 1 {
		t.Fatalf("ok %d denied %d", ok, denied)
	}
}

func fsWrite(path, content string) (transport.WorkspaceRequest, []byte) {
	sum := sha256.Sum256([]byte(content))
	return transport.WorkspaceRequest{Op: transport.OpFS, StdinLen: int64(len(content)), ID: "",
		FS: &transport.FSOp{Op: transport.FSWrite, Path: path, Length: int64(len(content)), SHA256: hex.EncodeToString(sum[:])}}, []byte(content)
}

func TestDevOpsDispatch(t *testing.T) {
	h, ex := hostHarness(t)
	id := h.create()
	sid := string(id)

	req, body := fsWrite("x.txt", "a\nb\nc\n")
	req.ID = sid
	if res := h.want(req, body, transport.CodeOK); res.FS == nil || !res.FS.NewFile {
		t.Fatalf("fs write: %+v", res.FS)
	}
	res := h.want(transport.WorkspaceRequest{Op: transport.OpFS, ID: sid, FS: &transport.FSOp{Op: transport.FSRead, Path: "x.txt"}}, nil, transport.CodeOK)
	if res.FS.Kind != transport.KindText || !strings.Contains(res.FS.Content, "b") {
		t.Fatalf("fs read: %+v", res.FS)
	}
	h.want(transport.WorkspaceRequest{Op: transport.OpFS, ID: sid, FS: &transport.FSOp{Op: transport.FSEdit, Path: "x.txt", OldString: "b", NewString: "B"}}, nil, transport.CodeOK)
	// Upload/download keep working on the same tree (workspace.read_file / write_file).
	_, data := h.do(transport.WorkspaceRequest{Op: transport.OpRead, ID: sid, Path: "/workspace/x.txt"}, nil)
	if string(data) != "a\nB\nc\n" {
		t.Fatalf("read_file: %q", data)
	}
	h.want(transport.WorkspaceRequest{Op: transport.OpWrite, ID: sid, Path: "up/y.txt", Length: 2, Truncate: true}, []byte("hi"), transport.CodeOK)
	res = h.want(transport.WorkspaceRequest{Op: transport.OpFS, ID: sid, FS: &transport.FSOp{Op: transport.FSGrep, Pattern: "hi", OutputMode: "content", LineNumbers: true}}, nil, transport.CodeOK)
	if !strings.Contains(res.FS.Content, "up/y.txt:1:hi") {
		t.Fatalf("grep: %q", res.FS.Content)
	}
	// A failing fs op is a refusal, not an internal error.
	h.want(transport.WorkspaceRequest{Op: transport.OpFS, ID: sid, FS: &transport.FSOp{Op: transport.FSEdit, Path: "x.txt", OldString: "zzz", NewString: "y"}}, nil, transport.CodeRefused)

	// Jobs.
	res = h.want(transport.WorkspaceRequest{Op: transport.OpJobStart, ID: sid, Job: &transport.JobOp{Command: "make", Description: "build"}}, nil, transport.CodeOK)
	job := res.Job.Job
	ex.FinishJobs(context.Background())
	res = h.want(transport.WorkspaceRequest{Op: transport.OpJobTail, ID: sid, Job: &transport.JobOp{Job: job}}, nil, transport.CodeOK)
	if res.Job.Output != "hello\nworld\n" || res.Job.State != transport.JobExited {
		t.Fatalf("tail: %+v", res.Job)
	}
	res = h.want(transport.WorkspaceRequest{Op: transport.OpJobTail, ID: sid, Job: &transport.JobOp{Job: job}}, nil, transport.CodeOK)
	if res.Job.Output != "" {
		t.Fatalf("second tail must be incremental: %q", res.Job.Output)
	}
	res = h.want(transport.WorkspaceRequest{Op: transport.OpJobStatus, ID: sid, Job: &transport.JobOp{}}, nil, transport.CodeOK)
	if len(res.Jobs) != 1 {
		t.Fatalf("jobs: %+v", res.Jobs)
	}
	res = h.want(transport.WorkspaceRequest{Op: transport.OpOutputRead, ID: sid, Out: &transport.OutputOp{Job: job, TailLines: 1}}, nil, transport.CodeOK)
	if res.Output.Content != "world\n" || !res.Output.EOF {
		t.Fatalf("output: %+v", res.Output)
	}
	h.want(transport.WorkspaceRequest{Op: transport.OpJobCancel, ID: sid, Job: &transport.JobOp{Job: "nope"}}, nil, transport.CodeNotFound)
	h.want(transport.WorkspaceRequest{Op: transport.OpFS, ID: string(workspace.NewID()), FS: &transport.FSOp{Op: transport.FSStat, Path: "x"}}, nil, transport.CodeNotFound)

	// close with delete removes the session directory; plain close keeps it.
	dir, _ := ex.SessionDir(id)
	id2 := h.create()
	dir2, _ := ex.SessionDir(id2)
	h.want(transport.WorkspaceRequest{Op: transport.OpDestroy, ID: string(id2)}, nil, transport.CodeOK)
	if _, err := os.Stat(dir2); err != nil {
		t.Fatalf("plain destroy must keep the dir: %v", err)
	}
	h.want(transport.WorkspaceRequest{Op: transport.OpDestroy, ID: sid, Delete: true}, nil, transport.CodeOK)
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("destroy delete kept the dir")
	}
}

func TestDevOpsUnsupportedOnContainerExecutor(t *testing.T) {
	h := newHarness(t, wsYAML, fake.New(), nil) // no SessionRoot: not host mode
	id := h.create()
	h.want(transport.WorkspaceRequest{Op: transport.OpFS, ID: string(id), FS: &transport.FSOp{Op: transport.FSStat, Path: "x"}}, nil, transport.CodeUnsupported)
	hb := lastHeartbeat(h, func(transport.Heartbeat) bool { return true })
	if len(hb.Features) != 0 || len(hb.Credentials) != 0 {
		t.Fatalf("container runner heartbeat: %+v", hb)
	}
}

// A real OpenSSH client reaches an x/crypto/ssh server through the session agent and ssh_config:
// key never on disk, host binding on, host key pinned, one sign counted as a usage event.
func TestOpenSSHThroughSessionAgent(t *testing.T) {
	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("openssh-client is not installed in this image")
	}
	if _, err := user.Current(); err != nil {
		t.Skip("ssh needs a passwd entry for the uid (the dev container runs as the host uid); runs in the S16 harness")
	}
	k := newTestKey(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	hostLine, hostSigner := hostKeyLine(t, fmt.Sprintf("[127.0.0.1]:%d", port))
	sc := &ssh.ServerConfig{PublicKeyCallback: func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if c.User() == "git" && string(key.Marshal()) == string(k.pub.Marshal()) {
			return nil, nil
		}
		return nil, fmt.Errorf("denied")
	}}
	sc.AddHostKey(hostSigner)
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, chans, reqs, err := ssh.NewServerConn(nc, sc)
				if err != nil {
					nc.Close()
					return
				}
				go ssh.DiscardRequests(reqs)
				for nch := range chans {
					ch, creqs, _ := nch.Accept()
					go func() {
						for r := range creqs {
							if r.Type == "exec" {
								r.Reply(true, nil)
								io.WriteString(ch, "git-over-agent-ok\n")
								ch.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
								ch.Close()
							} else {
								r.Reply(false, nil)
							}
						}
					}()
				}
			}()
		}
	}()

	h, ex := hostHarness(t)
	h.sync(1, sshCred(k, hostLine, fmt.Sprintf("127.0.0.1:%d", port)))
	id := h.create()
	env := ex.SessionEnv(id)
	dir, _ := ex.SessionDir(id)
	noDecoy(t, "session dir", dirText(t, dir), k.needle)

	cmd := exec.Command(sshBin, "-F", filepath.Join(dir, "ssh_config"), "127.0.0.1", "echo")
	cmd.Env = []string{"SSH_AUTH_SOCK=" + env["SSH_AUTH_SOCK"], "HOME=" + t.TempDir(), "PATH=" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "git-over-agent-ok") {
		t.Fatalf("ssh: %v\n%s", err, out)
	}
	h.runnr.dev.events.mu.Lock()
	var signs int64
	for k2, n := range h.runnr.dev.events.pending {
		if k2.cred == "deploy" && k2.status == transport.EventOK {
			signs += n
		}
	}
	h.runnr.dev.events.mu.Unlock()
	if signs < 1 {
		t.Fatal("agent sign not counted as a credential.use event")
	}

	// An unpinned host key is refused (the server presents another key than the pinned one).
	other, _ := hostKeyLine(t, "x")
	_ = other
	badLine, _ := hostKeyLine(t, fmt.Sprintf("[127.0.0.1]:%d", port))
	h.sync(2, sshCred(k, badLine, fmt.Sprintf("127.0.0.1:%d", port)))
	cmd = exec.Command(sshBin, "-F", filepath.Join(dir, "ssh_config"), "127.0.0.1", "echo")
	cmd.Env = []string{"SSH_AUTH_SOCK=" + env["SSH_AUTH_SOCK"], "HOME=" + t.TempDir(), "PATH=" + os.Getenv("PATH")}
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("an unpinned host key must be refused:\n%s", out)
	}
}

// A runner restart keeps the executor's workspaces: the adopted sessions are wired again once
// central's creds_sync arrives, with the same narrowed set.
func TestRestartRewiresAdoptedSessions(t *testing.T) {
	h, ex := hostHarness(t)
	k := newTestKey(t)
	kh, _ := hostKeyLine(t, "ssh.test")
	creds := []transport.CredSecret{tokenCred(decoyToken, "git.test"), sshCred(k, kh, "ssh.test")}
	h.sync(1, creds...)
	narrow := createReq(workspace.NewID())
	narrow.Spec.Labels = map[string]string{LabelCredentials: "deploy"}
	h.want(narrow, nil, transport.CodeOK)
	all := h.create()
	nid := workspace.ID(narrow.ID)

	// "Restart": a new runner on the same executor; the process lost its wiring and its env.
	ex.SetSessionEnv(nid, nil)
	ex.SetSessionEnv(all, nil)
	h2 := newHarness(t, wsYAML, ex, nil)
	h2.sync(1, creds...)

	env := ex.SessionEnv(nid)
	if env["SSH_AUTH_SOCK"] == "" || env["HTTPS_PROXY"] != "" {
		t.Fatalf("adopted narrowed session: %v", env)
	}
	env = ex.SessionEnv(all)
	if env["SSH_AUTH_SOCK"] == "" || env["HTTPS_PROXY"] == "" {
		t.Fatalf("adopted unrestricted session: %v", env)
	}
}

// H-21: the unsent table is spooled to disk and a new runner process sends it; a poisoned entry
// is dropped, and a sent table removes the file.
func TestEventSpoolSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.spool")
	a := newEventAgg()
	a.loadSpool(path)
	a.record("s1", "c", "git.test:443", transport.EventOK)
	a.record("s1", "c", "git.test:443", transport.EventOK)
	a.persist()
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("spool: %v %v", fi, err)
	}
	b := newEventAgg() // the "restarted" runner
	b.loadSpool(path)
	batches := b.take(true)
	if len(batches) != 1 || len(batches[0].Events) != 1 || batches[0].Events[0].Count != 2 || batches[0].Validate() != nil {
		t.Fatalf("after restart: %+v", batches)
	}
	b.persist() // everything was taken: the file goes away
	if _, err := os.Stat(path); err == nil {
		t.Fatal("spool kept after the table was drained")
	}
	// A hand-edited entry central would refuse is dropped, not retried forever.
	os.WriteFile(path, []byte(`{"events":[{"type":"credential_use","session":"s","credential":"c","host":"h","status":"ok","count":0,"window_ms":1,"window_s":60}]}`), 0o600)
	c := newEventAgg()
	c.loadSpool(path)
	if c.hasPending() {
		t.Fatal("invalid spool entry loaded")
	}
	c.persist()
	if _, err := os.Stat(path); err == nil {
		t.Fatal("unusable spool kept")
	}
}
