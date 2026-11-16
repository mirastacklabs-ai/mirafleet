package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/keys"
	"github.com/mirastacklabs-ai/mirafleet/internal/opconf"
	"github.com/mirastacklabs-ai/mirafleet/internal/runner"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/fake"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/osb"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/wsconfig"
)

// namedFake is the fake executor reporting the name a real containerd executor has, so the
// heartbeat matches the fleet's executor.
type namedFake struct{ *fake.Executor }

func (namedFake) Name() string { return wsconfig.ExecContainerd }

const e2eYAML = `
executor: containerd
concurrent: 2
max_age: 720h
images: ["docker.io/library/alpine:*"]
limits: {cpus: 4, memory: 8GiB, pids: 2048, tmpfs: 1GiB, disk: 20GiB, max_file: 4MiB, exec_max: 1m}
network: [none]
env_allow: [LANG]
containerd:
  namespace: mirafleet
  root: /var/lib/mirafleet/ws
`

func TestRunnerTLSInitDirChecks(t *testing.T) {
	var out bytes.Buffer
	parent := t.TempDir()

	// Happy path: a missing leaf is created 0700; a pre-created empty 0700 dir of ours is fine.
	fresh := filepath.Join(parent, "fresh")
	if err := runnerTLSInit([]string{"-dir", fresh}, &out); err != nil {
		t.Fatal(err)
	}
	if mode(t, fresh) != 0o700 {
		t.Errorf("fresh dir mode %v", mode(t, fresh))
	}
	pre := filepath.Join(parent, "pre")
	if err := os.Mkdir(pre, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := runnerTLSInit([]string{"-dir", pre}, &out); err != nil {
		t.Fatalf("pre-created 0700 dir: %v", err)
	}

	// The parent must exist: nothing is created recursively.
	if err := runnerTLSInit([]string{"-dir", filepath.Join(parent, "no", "leaf")}, &out); err == nil {
		t.Error("missing parent accepted")
	}

	// A symlinked leaf is refused and nothing is written through it.
	target := filepath.Join(parent, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := runnerTLSInit([]string{"-dir", link}, &out); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("symlinked leaf: %v", err)
	}
	if ents, _ := os.ReadDir(target); len(ents) != 0 {
		t.Errorf("wrote through the symlink: %v", ents)
	}

	// A pre-created dir with a looser mode is refused and left as it was (no chmod).
	loose := filepath.Join(parent, "loose")
	if err := os.Mkdir(loose, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runnerTLSInit([]string{"-dir", loose}, &out); err == nil || !strings.Contains(err.Error(), "0700") {
		t.Errorf("0755 dir: %v", err)
	}
	if mode(t, loose) != 0o755 {
		t.Errorf("refused dir was chmodded to %v", mode(t, loose))
	}
	if ents, _ := os.ReadDir(loose); len(ents) != 0 {
		t.Errorf("wrote into the refused dir: %v", ents)
	}

	// A regular file at the leaf is refused.
	file := filepath.Join(parent, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runnerTLSInit([]string{"-dir", file}, &out); err == nil {
		t.Error("regular file accepted")
	}

	// A dir owned by another uid: only root can chown in a test; otherwise skip the case.
	t.Run("other owner", func(t *testing.T) {
		if os.Geteuid() != 0 {
			t.Skip("needs root to chown a directory to another uid")
		}
		d := filepath.Join(parent, "foreign")
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(d, 12345, 12345); err != nil {
			t.Skipf("chown unavailable: %v", err)
		}
		if err := runnerTLSInit([]string{"-dir", d}, &out); err == nil || !strings.Contains(err.Error(), "owned by uid") {
			t.Errorf("foreign-owned dir: %v", err)
		}
	})
}

func TestRunnerTLSInitAndFront(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	var out bytes.Buffer
	if err := runnerTLSInit([]string{"-dir", dir, "-days", "30"}, &out); err != nil {
		t.Fatal(err)
	}
	if mode(t, dir) != 0o700 {
		t.Errorf("dir mode %v", mode(t, dir))
	}
	for _, f := range []string{"ca.pem", "front.pem", "front-key.pem", "client.pem", "client-key.pem"} {
		if mode(t, filepath.Join(dir, f)) != 0o600 {
			t.Errorf("%s mode %v", f, mode(t, filepath.Join(dir, f)))
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "ca-key.pem")); err == nil {
		t.Error("CA key kept")
	}
	pem, _ := os.ReadFile(filepath.Join(dir, "front.pem"))
	cs := string(out.String())
	pinLine := ""
	for _, l := range strings.Split(cs, "\n") {
		if strings.Contains(l, "pin_sha256:") {
			pinLine = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "pin_sha256:"))
		}
	}
	if len(pinLine) != 64 || !strings.Contains(cs, "ca_file: "+filepath.Join(dir, "ca.pem")) || len(pem) == 0 {
		t.Errorf("output:\n%s", cs)
	}
	// Never overwrites; never uses a populated directory.
	if err := runnerTLSInit([]string{"-dir", dir}, &out); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Errorf("second tls-init: %v", err)
	}
	// A symlink is refused before anything is chmodded through it.
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := runnerTLSInit([]string{"-dir", link}, &out); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("symlink dir: %v", err)
	}
	if ents, _ := os.ReadDir(target); len(ents) != 0 {
		t.Errorf("tls-init wrote through the symlink: %v", ents)
	}
	for _, a := range [][]string{{}, {"-dir", filepath.Join(t.TempDir(), "x"), "-days", "0"}, {"-dir", dir, "extra"}} {
		if err := runnerTLSInit(a, &out); err == nil {
			t.Errorf("%v accepted", a)
		}
	}

	// tls-front: serve in front of a plain-http upstream; talk to it with a stdlib mTLS client.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "up:"+r.URL.Path) }))
	defer up.Close()
	for _, bad := range [][]string{
		{"-dir", dir, "-listen", "0.0.0.0:8443"},
		{"-dir", dir, "-upstream", "10.1.2.3:8080"},
		{"-dir", dir, "-upstream", "https://127.0.0.1:8080"},
		{"-dir", filepath.Join(dir, "none")},
		{},
	} {
		if _, err := newTLSFront(bad); err == nil {
			t.Errorf("tls-front %v accepted", bad)
		}
	}
	f, err := newTLSFront([]string{"-dir", dir, "-listen", "127.0.0.1:0", "-upstream", up.Listener.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}
	addr, err := f.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	caPEM, _ := os.ReadFile(filepath.Join(dir, "ca.pem"))
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "client.pem"), filepath.Join(dir, "client-key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	client := func(certs []tls.Certificate) *http.Client {
		return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs: pool, MinVersion: tls.VersionTLS13, Certificates: certs,
			VerifyConnection: func(s tls.ConnectionState) error { // the printed pin must match the served leaf
				if got := osb.SPKIPin(s.PeerCertificates[0]); got != pinLine {
					t.Errorf("served pin %s != printed %s", got, pinLine)
				}
				return nil
			}}}}
	}
	resp, err := client([]tls.Certificate{cert}).Get("https://" + addr + "/v1/sandboxes")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != "up:/v1/sandboxes" {
		t.Errorf("proxied: %d %q", resp.StatusCode, b)
	}
	if resp, err = client([]tls.Certificate{cert}).Get("https://" + addr + "/health"); err != nil || resp.StatusCode != 404 {
		t.Errorf("non-route: %v %v", resp, err)
	}
	// mTLS is required: no client certificate, no service.
	if resp, err := client(nil).Get("https://" + addr + "/v1/sandboxes"); err == nil {
		resp.Body.Close()
		t.Errorf("served a client without a certificate: %d", resp.StatusCode)
	}
}

// wsRunnerEnv writes a runner data dir, an osb workspace.yaml (with a 0600 API key owned by the
// test user, so wsconfig.Load's file checks pass) and swaps the executor seam for a fake.
func wsRunnerEnv(t *testing.T, maxAge string) (data, wsFile string, ex *fake.Executor) {
	t.Helper()
	dir := t.TempDir()
	data = filepath.Join(dir, "data")
	if err := os.Mkdir(data, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runner.ConfigPath(data), []byte("central: 127.0.0.1:1\nname: r1\nfleet: dev\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "api.key")
	os.WriteFile(key, []byte("secret\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "c-key.pem"), []byte("k\n"), 0o600)
	wsFile = filepath.Join(dir, "workspace.yaml")
	yaml := `
executor: opensandbox
concurrent: 2
max_age: ` + maxAge + `
images: ["docker.io/library/alpine:*"]
limits: {cpus: 2, memory: 1GiB, pids: 256, tmpfs: 64MiB, disk: 1GiB, max_file: 4MiB}
network: [none]
opensandbox:
  server: https://127.0.0.1:8443
  ca_file: ` + filepath.Join(dir, "ca.pem") + `
  client_cert: ` + filepath.Join(dir, "c.pem") + `
  client_key: ` + filepath.Join(dir, "c-key.pem") + `
  api_key_file: ` + key + `
`
	if err := os.WriteFile(wsFile, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	ex = fake.New()
	var gotOwner string
	old := newExecutor
	newExecutor = func(cfg *wsconfig.Config, owner string) (workspace.Executor, error) {
		gotOwner = owner
		return keepOpen{ex}, nil
	}
	t.Cleanup(func() {
		newExecutor = old
		if gotOwner != "" && gotOwner != "dev/r1" {
			t.Errorf("executor ownership name %q, want <fleet>/<runner> = dev/r1", gotOwner)
		}
	})
	return data, wsFile, ex
}

func TestRunnerWorkspaceListPrune(t *testing.T) {
	data, wsFile, ex := wsRunnerEnv(t, "24h")
	ctx := context.Background()
	var ids []workspace.ID
	for i := 0; i < 3; i++ {
		id := workspace.NewID()
		if err := ex.Create(ctx, workspace.Spec{ID: id, Image: "docker.io/library/alpine:3"}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	now := time.Now()
	run := func(now time.Time, args ...string) (string, error) {
		var out bytes.Buffer
		err := runnerWorkspace(append(args, "-data", data, "-workspace", wsFile), &out, now)
		return out.String(), err
	}
	out, err := run(now, "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if !strings.Contains(out, string(id)) {
			t.Errorf("list lacks %s:\n%s", id, out)
		}
	}
	// Young workspaces are not pruned by max_age.
	if out, err = run(now, "prune", "-yes"); err != nil || !strings.Contains(out, "nothing to prune") {
		t.Errorf("young prune: %v %s", err, out)
	}
	// Old ones are listed but kept without -yes ...
	later := now.Add(48 * time.Hour)
	if out, err = run(later, "prune"); err != nil || !strings.Contains(out, "re-run with -yes") {
		t.Errorf("dry run: %v %s", err, out)
	}
	if l, _ := ex.List(ctx); len(l) != 3 {
		t.Fatalf("dry run destroyed workspaces: %d left", len(l))
	}
	// ... and destroyed with it.
	if out, err = run(later, "prune", "-yes"); err != nil {
		t.Fatalf("prune: %v %s", err, out)
	}
	if l, _ := ex.List(ctx); len(l) != 0 {
		t.Errorf("%d workspaces left after prune", len(l))
	}
	// -all needs -yes too, and ignores age.
	ex.Create(ctx, workspace.Spec{ID: workspace.NewID(), Image: "docker.io/library/alpine:3"})
	if _, err = run(now, "prune", "-all"); err != nil {
		t.Fatal(err)
	}
	if l, _ := ex.List(ctx); len(l) != 1 {
		t.Errorf("-all without -yes destroyed: %d", len(l))
	}
	if _, err = run(now, "prune", "-all", "-yes"); err != nil {
		t.Fatal(err)
	}
	if l, _ := ex.List(ctx); len(l) != 0 {
		t.Errorf("%d left after -all -yes", len(l))
	}
	for _, a := range [][]string{{}, {"nuke"}, {"list", "extra"}} {
		if err := runnerWorkspace(a, io.Discard, now); err == nil {
			t.Errorf("%v accepted", a)
		}
	}
}

func TestRunnerWorkspacePruneNeedsMaxAge(t *testing.T) {
	data, wsFile, _ := wsRunnerEnv(t, "0s")
	var out bytes.Buffer
	err := runnerWorkspace([]string{"prune", "-yes", "-data", data, "-workspace", wsFile}, &out, time.Now())
	if err == nil || !strings.Contains(err.Error(), "-all") {
		t.Errorf("err = %v", err)
	}
}

func TestRunnerWorkspaceConfigRefused(t *testing.T) {
	data, wsFile, _ := wsRunnerEnv(t, "24h")
	// A loosened API key file fails wsconfig.Load before any executor is built.
	b, _ := os.ReadFile(wsFile)
	key := ""
	for _, l := range strings.Split(string(b), "\n") {
		if strings.Contains(l, "api_key_file:") {
			key = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "api_key_file:"))
		}
	}
	os.Chmod(key, 0o644)
	err := runnerWorkspace([]string{"list", "-data", data, "-workspace", wsFile}, io.Discard, time.Now())
	if err == nil || !strings.Contains(err.Error(), "mode") {
		t.Errorf("err = %v", err)
	}
}

func TestRunnerRunModeFlags(t *testing.T) {
	data := t.TempDir()
	dir := t.TempDir()
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"both modes":           {[]string{"-data", data, "-actions", dir + "/a.yaml", "-workspace", dir + "/w.yaml"}, "mutually exclusive"},
		"empty workspace":      {[]string{"-data", data, "-workspace", ""}, "needs a file"},
		"not joined (ws)":      {[]string{"-data", data, "-workspace", dir + "/w.yaml"}, "not joined"},
		"not joined (acts)":    {[]string{"-data", data, "-actions", dir + "/a.yaml"}, "not joined"},
		"not joined (neither)": {[]string{"-data", data}, "not joined"},
	} {
		err := runnerRun(tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Joined, but the workspace file is missing: workspace mode never falls back to actions.
	wsRunnerEnv(t, "24h")
	d2 := filepath.Join(t.TempDir(), "d")
	os.Mkdir(d2, 0o700)
	os.WriteFile(runner.ConfigPath(d2), []byte("central: 127.0.0.1:1\nname: r1\nfleet: dev\n"), 0o600)
	if _, err := keys.Generate(d2, runner.KeyName, "r1"); err != nil {
		t.Fatal(err)
	}
	err := runnerRun([]string{"-data", d2, "-workspace", filepath.Join(dir, "missing.yaml")})
	if err == nil || !strings.Contains(err.Error(), "workspace config") {
		t.Errorf("missing workspace file: %v", err)
	}
}

// TestWorkspaceCLIEndToEnd drives `mcp api workspace.*` (with -in and -out) against an in-process
// central and a runner serving the fake executor, and checks the additive list fields.
func TestWorkspaceCLIEndToEnd(t *testing.T) {
	e := newCenv(t)
	ctx := context.Background()
	cfg := store.WorkspaceConfig{DefaultImage: "docker.io/library/alpine:3.20", Images: []string{"docker.io/library/alpine:*"}}
	if _, err := e.svc.CreateFleet(ctx, e.admin, service.SourceCLI, "dev", store.ModeWorkspace, false, &cfg); err != nil {
		t.Fatal(err)
	}
	tok, err := e.svc.CreateToken(ctx, e.admin, service.SourceCLI, service.TokenParams{Fleet: "dev", Approve: true})
	if err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(t.TempDir(), "runner")
	if err := runnerJoin([]string{e.addr, "-name", "r1", "-data", data, "-token", "-"}, strings.NewReader(tok.Token), io.Discard); err != nil {
		t.Fatal(err)
	}
	signer, err := keys.LoadSigner(runner.KeyPath(data))
	if err != nil {
		t.Fatal(err)
	}
	ex := fake.New()
	ex.ExecFunc = func(ctx context.Context, id workspace.ID, r workspace.ExecRequest) (workspace.ExecResult, error) {
		if r.Stdin != nil {
			io.Copy(r.Stdout, r.Stdin) // cat
		}
		return workspace.ExecResult{}, nil
	}
	wc, err := wsconfig.Parse([]byte(e2eYAML))
	if err != nil {
		t.Fatal(err)
	}
	rctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		runner.New(runner.Config{Central: e.addr, Name: "r1", Signer: signer, KnownHosts: runner.KnownHostsPath(data),
			Workspace: namedFake{ex}, WorkspaceConfig: wc, Version: "test", Logf: t.Logf,
			HeartbeatEvery: 100 * time.Millisecond, HeartbeatTimeout: 10 * time.Second}).Run(rctx)
	}()
	t.Cleanup(func() { cancel(); wg.Wait() })

	t.Setenv(opconf.EnvDir, filepath.Join(t.TempDir(), "cfg"))
	if err := mcpMain([]string{"login", e.addr, "-name", "alice", "-code", "-"}, strings.NewReader(e.code()), io.Discard); err != nil {
		t.Fatal(err)
	}
	api := func(stdin string, args ...string) (map[string]any, error) {
		var out bytes.Buffer
		if err := mcpMain(append([]string{"api"}, args...), strings.NewReader(stdin), &out); err != nil {
			return nil, err
		}
		var m map[string]any
		if err := json.Unmarshal(out.Bytes(), &m); err != nil {
			t.Fatalf("%v: %q", err, out.String())
		}
		return m, nil
	}

	var rl map[string]any
	deadline := time.Now().Add(15 * time.Second)
	for {
		rl, err = api("", "runners.list", `{"fleet":"dev"}`)
		if rs, _ := rl["runners"].([]any); err == nil && len(rs) == 1 && rs[0].(map[string]any)["free_slots"] != nil {
			r := rs[0].(map[string]any)
			if r["slots"] != float64(2) || r["free_slots"] != float64(2) || r["executor"] != "containerd" || r["online"] != true {
				t.Fatalf("runner: %v", r)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("runner never showed slots: %v %v", rl, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	fl, err := api("", "fleets.list")
	if err != nil {
		t.Fatal(err)
	}
	var dev map[string]any
	for _, f := range fl["fleets"].([]any) {
		if f.(map[string]any)["name"] == "dev" {
			dev = f.(map[string]any)
		}
	}
	w, _ := dev["workspace"].(map[string]any)
	if w == nil || w["default_image"] != "docker.io/library/alpine:3.20" || w["network"] != "none" || w["config"] != nil {
		t.Fatalf("fleets.list workspace: %v", dev)
	}

	op, err := api("", "workspace.open", `{"fleet":"dev","session":"s1"}`)
	if err != nil {
		t.Fatal(err)
	}
	id := op["workspace"].(map[string]any)["id"].(string)
	ws := `{"workspace":"` + id + `"`

	// write_file -in: length and sha256 are computed; a 700 KiB body; then read_file -out.
	tmp := t.TempDir()
	data700 := bytes.Repeat([]byte("0123456789abcdef"), 700*1024/16)
	in, outp := filepath.Join(tmp, "in.bin"), filepath.Join(tmp, "out.bin")
	os.WriteFile(in, data700, 0o600)
	wr, err := api("", "workspace.write_file", ws+`,"path":"big.bin"}`, "-in", in)
	if err != nil || wr["size"] != float64(len(data700)) {
		t.Fatalf("write_file: %v %v", wr, err)
	}
	rf, err := api("", "workspace.read_file", ws+`,"path":"big.bin"}`, "-out", outp)
	if err != nil || rf["length"] != float64(len(data700)) || rf["eof"] != true {
		t.Fatalf("read_file: %v %v", rf, err)
	}
	if got, _ := os.ReadFile(outp); !bytes.Equal(got, data700) {
		t.Errorf("round trip differs: %d bytes", len(got))
	}
	if mode(t, outp) != 0o600 {
		t.Errorf("-out mode %v", mode(t, outp))
	}
	// stdin through -in -, exec echoes it (the fake runs `cat`).
	ex1, err := api("piped input", "workspace.exec", ws+`,"argv":["cat"]}`, "-in", "-")
	if err != nil || ex1["stdout"] != "piped input" || ex1["exit_code"] != float64(0) {
		t.Fatalf("exec: %v %v", ex1, err)
	}
	// Refusals surface the server's code; oversize bodies are refused client-side.
	big := filepath.Join(tmp, "toobig")
	os.WriteFile(big, make([]byte, 1<<20+1), 0o600)
	if _, err := api("", "workspace.write_file", ws+`,"path":"x"}`, "-in", big); err == nil || !strings.Contains(err.Error(), "chunks") {
		t.Errorf("oversize -in: %v", err)
	}
	if _, err := api("", "workspace.read_file", ws+`,"path":"missing"}`, "-out", filepath.Join(tmp, "nope")); err == nil {
		t.Error("read of a missing file succeeded")
	}
	if _, err := os.Stat(filepath.Join(tmp, "nope")); err == nil {
		t.Error("-out file created after a failed read")
	}
	rl, _ = api("", "runners.list", `{"fleet":"dev"}`)
	if r := rl["runners"].([]any)[0].(map[string]any); r["free_slots"] != float64(1) {
		t.Errorf("free_slots after open: %v", r)
	}
	if _, err := api("", "workspace.close", ws+`}`); err != nil {
		t.Fatal(err)
	}
	if _, err := api("", "workspace.exec", ws+`,"argv":["id"]}`); err == nil {
		t.Error("exec on a closed workspace succeeded")
	}
}

// keepOpen ignores Close: the local admin commands close their executor, a fake would then
// refuse every later call in the same test.
type keepOpen struct{ *fake.Executor }

func (keepOpen) Close() error { return nil }

// A trailing slash must not defeat O_NOFOLLOW on a symlinked leaf.
func TestRunnerTLSInitTrailingSlashSymlink(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runnerTLSInit([]string{"-dir", link + "/"}, &out); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("trailing-slash symlink: %v", err)
	}
	if ents, _ := os.ReadDir(target); len(ents) != 0 {
		t.Errorf("wrote through the symlink: %v", ents)
	}
	if _, err := openPrivateDir(link + "///"); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("openPrivateDir trailing slashes: %v", err)
	}
}

// A leaf created under a setgid parent inherits 02700; since tls-init created it, it is
// normalised to 0700. A pre-existing setgid leaf is refused with a message naming the bit.
func TestRunnerTLSInitSetgidParent(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o2755); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(parent); err != nil || st.Mode()&os.ModeSetgid == 0 {
		t.Skip("setgid not honoured on this filesystem")
	}
	var out bytes.Buffer
	fresh := filepath.Join(parent, "fresh")
	if err := runnerTLSInit([]string{"-dir", fresh}, &out); err != nil {
		t.Fatalf("fresh leaf under setgid parent: %v", err)
	}
	if st, _ := os.Stat(fresh); st.Mode()&os.ModeSetgid != 0 || st.Mode().Perm() != 0o700 {
		t.Errorf("leaf mode %v, want plain 0700", st.Mode())
	}
	pre := filepath.Join(parent, "pre")
	if err := os.Mkdir(pre, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(pre, 0o2700); err != nil {
		t.Fatal(err)
	}
	err := runnerTLSInit([]string{"-dir", pre}, &out)
	if err == nil || !strings.Contains(err.Error(), "setgid") {
		t.Errorf("pre-existing setgid leaf: %v", err)
	}
	if st, _ := os.Stat(pre); st.Mode()&os.ModeSetgid == 0 {
		t.Error("refused leaf was chmodded")
	}
}
