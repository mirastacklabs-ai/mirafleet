//go:build linux && integration

package main

// Built-binary end-to-end test (docs/M3.md S19), run by `scripts/dev.sh integration` inside the
// privileged dind harness with a real containerd. It starts the real `mirafleet` binary (path in
// MIRAFLEET_TEST_BIN) as central and as a `runner run -workspace` runner, and drives everything
// through the CLI: fleet create/set, workspace list/kill, mcp api workspace.open / exec /
// write_file / read_file / close with -in/-out, and the local `runner workspace list|prune`.
// The workspace image is built at test time from the mirafleet binary itself (no network).

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/namespaces"

	"github.com/mirastacklabs-ai/mirafleet/internal/opconf"
)

const (
	e2eNamespace = "mirafleet-e2e"
	e2eImage     = "docker.io/mirafleet-test/e2e:1"
	e2eSession   = "e2e-session"
)

// proc is a background mirafleet process with captured output.
type proc struct {
	t    *testing.T
	cmd  *exec.Cmd
	out  *lockedBuf
	done chan struct{}
	err  error
}

func startProc(t *testing.T, bin string, env []string, args ...string) *proc {
	t.Helper()
	p := &proc{t: t, out: &lockedBuf{}, done: make(chan struct{})}
	p.cmd = exec.Command(bin, args...)
	p.cmd.Env = append(os.Environ(), env...)
	p.cmd.Stdout, p.cmd.Stderr = p.out, p.out
	if err := p.cmd.Start(); err != nil {
		t.Fatalf("start %v: %v", args, err)
	}
	go func() { p.err = p.cmd.Wait(); close(p.done) }()
	t.Cleanup(func() { p.stop() })
	return p
}

func (p *proc) running() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// stop sends SIGTERM and waits (SIGKILL after 20 s). Safe to call twice.
func (p *proc) stop() {
	if !p.running() {
		return
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(20 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.done
	}
}

type e2eEnv struct {
	t       *testing.T
	bin     string
	dir     string
	central string // data dir
	runner  string // data dir
	cfgDir  string // operator config dir
	addr    string
	sock    string // containerd socket
	wsFile  string
	wsRoot  string
	env     []string
}

// cli runs `mirafleet args...` to completion and returns stdout, stderr and the error.
func (e *e2eEnv) cli(stdin string, args ...string) (string, string, error) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.bin, args...)
	cmd.Env = append(os.Environ(), e.env...)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.String(), errb.String(), err
}

func (e *e2eEnv) must(stdin string, args ...string) string {
	e.t.Helper()
	out, errs, err := e.cli(stdin, args...)
	if err != nil {
		e.t.Fatalf("mirafleet %s: %v\nstdout: %s\nstderr: %s", strings.Join(args, " "), err, out, errs)
	}
	return out
}

func (e *e2eEnv) api(stdin string, args ...string) (map[string]any, error) {
	e.t.Helper()
	out, errs, err := e.cli(stdin, append([]string{"mcp", "api"}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("%v: %s", err, strings.TrimSpace(errs))
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		e.t.Fatalf("mcp api %v: bad JSON %q: %v", args, out, err)
	}
	return m, nil
}

func (e *e2eEnv) apiOK(stdin string, args ...string) map[string]any {
	e.t.Helper()
	m, err := e.api(stdin, args...)
	if err != nil {
		e.t.Fatalf("mcp api %v: %v", args, err)
	}
	return m
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func waitFor(t *testing.T, what string, timeout time.Duration, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestIntegrationWorkspaceCLI(t *testing.T) {
	bin := os.Getenv("MIRAFLEET_TEST_BIN")
	sock := os.Getenv("MIRAFLEET_TEST_CONTAINERD_SOCK")
	if bin == "" || sock == "" {
		t.Skip("MIRAFLEET_TEST_BIN / MIRAFLEET_TEST_CONTAINERD_SOCK not set; run scripts/dev.sh integration")
	}
	dir, err := os.MkdirTemp("", "mfe2e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	e := &e2eEnv{t: t, bin: bin, dir: dir, sock: sock,
		central: filepath.Join(dir, "central"), runner: filepath.Join(dir, "runner"),
		cfgDir: filepath.Join(dir, "operator"), wsFile: filepath.Join(dir, "workspace.yaml"),
		wsRoot: filepath.Join(dir, "ws")}
	e.env = []string{opconf.EnvDir + "=" + e.cfgDir}
	if err := os.Mkdir(e.wsRoot, 0o700); err != nil {
		t.Fatal(err)
	}

	cc, cctx := ctrdClient(t, sock)
	importE2EImage(t, cc, cctx, bin)

	// ---- central + fleet ----
	e.addr = freeAddr(t)
	e.must("", "central", "init", "-data", e.central)
	central := startProc(t, bin, nil, "central", "run", "-data", e.central, "-listen", e.addr)
	waitFor(t, "central admin socket", 20*time.Second, func() bool {
		if !central.running() {
			t.Fatalf("central exited: %v\n%s", central.err, central.out)
		}
		_, err := os.Stat(filepath.Join(e.central, "admin.sock"))
		return err == nil
	})

	e.must("", "central", "fleet", "create", "dev", "-data", e.central, "-mode", "workspace", "-executor", "containerd",
		"-image", e2eImage, "-images", "docker.io/mirafleet-test/e2e:*", "-pull", "never", "-network", "none",
		"-cpus", "1", "-memory", "512MiB", "-pids", "128", "-tmpfs", "16MiB", "-disk", "128MiB",
		"-idle-ttl", "1h", "-max-ttl", "2h", "-exec-timeout", "30s", "-exec-max", "2m")
	// fleet workspace set reads the current config and sends a full replace: change one field.
	e.must("", "central", "fleet", "workspace", "set", "dev", "-data", e.central, "-memory", "256MiB")
	var fl struct {
		Fleets []struct {
			Name      string `json:"name"`
			Workspace *struct {
				DefaultImage string `json:"default_image"`
				Network      string `json:"network"`
				Config       *struct {
					MemoryMB     int    `json:"memory_mb"`
					CPUMilli     int    `json:"cpu_milli"`
					Pids         int    `json:"pids"`
					ExecMaxS     int    `json:"exec_max_s"`
					ExecDefaultS int    `json:"exec_default_s"`
					IdleTTLS     int    `json:"idle_ttl_s"`
					Pull         string `json:"pull"`
				} `json:"config"`
			} `json:"workspace"`
		} `json:"fleets"`
	}
	if err := json.Unmarshal([]byte(e.must("", "central", "fleet", "list", "-data", e.central, "-json")), &fl); err != nil {
		t.Fatal(err)
	}
	if len(fl.Fleets) != 1 || fl.Fleets[0].Workspace == nil || fl.Fleets[0].Workspace.Config == nil {
		t.Fatalf("fleet list: %+v", fl)
	}
	if c := fl.Fleets[0].Workspace.Config; fl.Fleets[0].Workspace.DefaultImage != e2eImage || c.MemoryMB != 256 || c.CPUMilli != 1000 ||
		c.Pids != 128 || c.ExecMaxS != 120 || c.ExecDefaultS != 30 || c.IdleTTLS != 3600 || c.Pull != "never" {
		t.Fatalf("fleet config after set: %+v", c)
	}

	// ---- runner (real containerd) ----
	tok := strings.TrimSpace(e.must("", "central", "token", "create", "-data", e.central, "-fleet", "dev", "-approve"))
	e.must(tok+"\n", "runner", "join", e.addr, "-name", "r1", "-data", e.runner, "-token", "-")
	rp := e.startRunner(t, "required")
	if !rp.waitOnlineOrExit(e) {
		// Hosts without user namespace support: fall back to off, as the executor tests do.
		if !strings.Contains(rp.out.String(), "userns: required") {
			t.Fatalf("runner exited:\n%s", rp.out)
		}
		t.Logf("userns: required unsupported here, using off")
		rp = e.startRunner(t, "off")
		if !rp.waitOnlineOrExit(e) {
			t.Fatalf("runner exited:\n%s", rp.out)
		}
	}
	var rl struct {
		Runners []struct {
			Name      string `json:"name"`
			Online    bool   `json:"online"`
			Slots     int    `json:"slots"`
			FreeSlots int    `json:"free_slots"`
			Executor  string `json:"executor"`
		} `json:"runners"`
	}
	if err := json.Unmarshal([]byte(e.must("", "central", "runner", "list", "-data", e.central, "-fleet", "dev", "-json")), &rl); err != nil {
		t.Fatal(err)
	}
	if len(rl.Runners) != 1 || rl.Runners[0].Slots != 2 || rl.Runners[0].Executor != "containerd" {
		t.Fatalf("runner list: %+v", rl)
	}

	// ---- operator ----
	code := e.must("", "central", "operator", "code", "-data", e.central)
	var otp string
	for _, l := range strings.Split(code, "\n") {
		if strings.HasPrefix(l, "code:") {
			otp = strings.TrimSpace(strings.TrimPrefix(l, "code:"))
		}
	}
	if otp == "" {
		t.Fatalf("no code in %q", code)
	}
	e.must(otp+"\n", "mcp", "login", e.addr, "-name", "alice", "-code", "-")

	// ---- workspace life cycle through `mcp api` ----
	op := e.apiOK("", "workspace.open", `{"fleet":"dev","session":"`+e2eSession+`"}`)
	w := op["workspace"].(map[string]any)
	id := w["id"].(string)
	if op["reused"] == true || w["state"] != "open" || w["runner"] != "r1" || w["image"] != e2eImage {
		t.Fatalf("open: %v", op)
	}
	if again := e.apiOK("", "workspace.open", `{"fleet":"dev","session":"`+e2eSession+`"}`); again["reused"] != true {
		t.Fatalf("sticky open: %v", again)
	}
	if cs, _ := cc.Containers(cctx); len(cs) != 1 {
		t.Fatalf("containers in %s: %d", e2eNamespace, len(cs))
	}
	if r := e.apiOK("", "runners.list", `{"fleet":"dev"}`)["runners"].([]any)[0].(map[string]any); r["free_slots"] != float64(1) {
		t.Errorf("free_slots after open: %v", r)
	}
	ws := `{"workspace":"` + id + `"`

	ok := e.apiOK("", "workspace.exec", ws+`,"argv":["/bin/mirafleet","version"]}`)
	if ok["exit_code"] != float64(0) || ok["stdout"] != "dev\n" {
		t.Fatalf("exec version: %v", ok)
	}
	bad := e.apiOK("", "workspace.exec", ws+`,"argv":["/bin/mirafleet","no-such-command"]}`)
	if bad["exit_code"] != float64(2) || !strings.Contains(bad["stderr"].(string), "usage:") {
		t.Fatalf("exec nonzero: %v", bad)
	}
	if sin := e.apiOK("some stdin\n", "workspace.exec", ws+`,"argv":["/bin/mirafleet","version"]}`, "-in", "-"); sin["exit_code"] != float64(0) {
		t.Fatalf("exec with stdin: %v", sin)
	}

	// write_file -in / read_file -out round trip: 700 KiB in one chunk, then a second chunk by offset.
	tmp := t.TempDir()
	part1 := bytes.Repeat([]byte("0123456789abcdef"), 700*1024/16)
	part2 := []byte("tail-chunk\n")
	f1, f2, out := filepath.Join(tmp, "p1"), filepath.Join(tmp, "p2"), filepath.Join(tmp, "out")
	os.WriteFile(f1, part1, 0o600)
	os.WriteFile(f2, part2, 0o600)
	if wr := e.apiOK("", "workspace.write_file", ws+`,"path":"data/big.bin","truncate":true}`, "-in", f1); wr["size"] != float64(len(part1)) {
		t.Fatalf("write 1: %v", wr)
	}
	if wr := e.apiOK("", "workspace.write_file", ws+fmt.Sprintf(`,"path":"data/big.bin","offset":%d}`, len(part1)), "-in", f2); wr["size"] != float64(len(part1)+len(part2)) {
		t.Fatalf("write 2: %v", wr)
	}
	rf := e.apiOK("", "workspace.read_file", ws+`,"path":"data/big.bin","limit":1048576}`, "-out", out)
	got, _ := os.ReadFile(out)
	want := append(append([]byte{}, part1...), part2...)
	if !bytes.Equal(got, want) || rf["size"] != float64(len(want)) || rf["eof"] != true {
		t.Fatalf("round trip: %d bytes (want %d), meta %v", len(got), len(want), rf)
	}
	if sum := sha256.Sum256(got); hex.EncodeToString(sum[:]) != fmt.Sprintf("%x", sha256.Sum256(want)) {
		t.Error("checksum mismatch")
	}
	// A write that escapes /workspace and a read of a missing file are refused.
	if _, err := e.api("", "workspace.write_file", ws+`,"path":"../etc/evil"}`, "-in", f2); err == nil {
		t.Error("write outside /workspace accepted")
	}
	if _, err := e.api("", "workspace.read_file", ws+`,"path":"missing"}`, "-out", filepath.Join(tmp, "nope")); err == nil {
		t.Error("read of a missing file succeeded")
	}

	// ---- central workspace list / kill ----
	var al struct {
		Workspaces []struct {
			ID, Fleet, Runner, Operator, State string
		} `json:"workspaces"`
	}
	if err := json.Unmarshal([]byte(e.must("", "central", "workspace", "list", "-data", e.central, "-fleet", "dev", "-json")), &al); err != nil {
		t.Fatal(err)
	}
	if len(al.Workspaces) != 1 || al.Workspaces[0].ID != id || al.Workspaces[0].Operator != "alice" || al.Workspaces[0].State != "open" {
		t.Fatalf("central workspace list: %+v", al)
	}
	if tbl := e.must("", "central", "workspace", "list", "-data", e.central); !strings.Contains(tbl, id) {
		t.Errorf("table lacks the id:\n%s", tbl)
	}

	// ---- local runner admin while the runner is up: list sees it ----
	if lo := e.must("", "runner", "workspace", "list", "-data", e.runner, "-workspace", e.wsFile); !strings.Contains(lo, id) {
		t.Errorf("runner workspace list lacks %s:\n%s", id, lo)
	}

	// close is the normal path; the container and its directory go away.
	e.apiOK("", "workspace.close", ws+`}`)
	waitFor(t, "container removal after close", 30*time.Second, func() bool { cs, _ := cc.Containers(cctx); return len(cs) == 0 })
	if ents := e.wsDirs(); len(ents) != 0 {
		t.Errorf("workspace root not empty after close: %v", ents)
	}
	if _, err := e.api("", "workspace.exec", ws+`,"argv":["/bin/mirafleet","version"]}`); err == nil {
		t.Error("exec on a closed workspace succeeded")
	}

	// kill: a second workspace is destroyed from central.
	id2 := e.apiOK("", "workspace.open", `{"fleet":"dev","session":"second"}`)["workspace"].(map[string]any)["id"].(string)
	e.must("", "central", "workspace", "kill", "-data", e.central, id2)
	waitFor(t, "container removal after kill", 30*time.Second, func() bool { cs, _ := cc.Containers(cctx); return len(cs) == 0 })
	if _, err := e.api("", "workspace.exec", `{"workspace":"`+id2+`","argv":["/bin/mirafleet","version"]}`); err == nil {
		t.Error("exec on a killed workspace succeeded")
	}

	// ---- local prune with the runner stopped: a workspace survives a runner stop and is pruned ----
	id3 := e.apiOK("", "workspace.open", `{"fleet":"dev","session":"third"}`)["workspace"].(map[string]any)["id"].(string)
	rp.stop()
	if out := e.must("", "runner", "workspace", "prune", "-all", "-data", e.runner, "-workspace", e.wsFile); !strings.Contains(out, id3) || !strings.Contains(out, "-yes") {
		t.Errorf("prune dry run:\n%s", out)
	}
	if cs, _ := cc.Containers(cctx); len(cs) != 1 {
		t.Fatalf("dry-run prune removed the container: %d left", len(cs))
	}
	if out := e.must("", "runner", "workspace", "prune", "-all", "-yes", "-data", e.runner, "-workspace", e.wsFile); !strings.Contains(out, "destroyed "+id3) {
		t.Errorf("prune:\n%s", out)
	}
	if cs, _ := cc.Containers(cctx); len(cs) != 0 {
		t.Errorf("%d containers left after prune", len(cs))
	}
	if ents := e.wsDirs(); len(ents) != 0 {
		t.Errorf("workspace root not empty after prune: %v", ents)
	}
}

// startRunner writes workspace.yaml for the given userns mode and starts `runner run -workspace`.
func (e *e2eEnv) startRunner(t *testing.T, userns string) *proc {
	t.Helper()
	y := fmt.Sprintf(`executor: containerd
concurrent: 2
max_age: 24h
images: ["mirafleet-test/e2e:*"]
limits: {cpus: 2, memory: 1GiB, pids: 256, tmpfs: 64MiB, disk: 256MiB, max_file: 8MiB, exec_max: 2m}
network: [none]
env_allow: [LANG]
containerd: {address: %s, namespace: %s, root: %s, userns: %s, uid_base: 1000000, apparmor: auto}
`, e.sock, e2eNamespace, e.wsRoot, userns)
	if err := os.WriteFile(e.wsFile, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	return startProc(t, e.bin, e.env, "runner", "run", "-data", e.runner, "-workspace", e.wsFile)
}

// waitOnlineOrExit returns true once central lists the runner online, false if the process exits first.
func (p *proc) waitOnlineOrExit(e *e2eEnv) bool {
	p.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if !p.running() {
			return false
		}
		out, _, err := e.cli("", "central", "runner", "list", "-data", e.central, "-fleet", "dev", "-json")
		if err == nil && strings.Contains(out, `"online": true`) && strings.Contains(out, `"slots"`) {
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	p.t.Fatalf("runner never came online:\n%s", p.out)
	return false
}

func ctrdClient(t *testing.T, sock string) (*containerd.Client, context.Context) {
	t.Helper()
	c, err := containerd.New(sock, containerd.WithDefaultNamespace(e2eNamespace))
	if err != nil {
		t.Fatalf("connect %s: %v", sock, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ctx, cancel := context.WithTimeout(namespaces.WithNamespace(context.Background(), e2eNamespace), 10*time.Minute)
	t.Cleanup(cancel)
	return c, ctx
}

// importE2EImage imports a hermetic image holding the mirafleet binary and names it e2eImage.
func importE2EImage(t *testing.T, c *containerd.Client, ctx context.Context, bin string) {
	t.Helper()
	data, err := buildE2EImage(bin)
	if err != nil {
		t.Fatal(err)
	}
	imgs, err := c.Import(ctx, bytes.NewReader(data), containerd.WithAllPlatforms(true))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	t.Cleanup(func() {
		for _, i := range imgs {
			_ = c.ImageService().Delete(context.Background(), i.Name, images.SynchronousDelete())
		}
		_ = c.ImageService().Delete(context.Background(), e2eImage, images.SynchronousDelete())
	})
	img, err := c.GetImage(ctx, "mirafleet-test/e2e:1")
	if err != nil {
		t.Fatalf("get image (imported %v): %v", imgs, err)
	}
	if _, err := c.ImageService().Create(ctx, images.Image{Name: e2eImage, Target: img.Target()}); err != nil {
		t.Fatalf("alias image: %v", err)
	}
	if err := img.Unpack(ctx, ""); err != nil {
		t.Fatalf("unpack: %v", err)
	}
}

func sha256Hex(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

// buildE2EImage returns an OCI image layout tarball: /bin/mirafleet (the binary under test), a
// minimal /etc/passwd and /etc/group for uid/gid 1000, /tmp. The image runs as 1000:1000.
func buildE2EImage(bin string) ([]byte, error) {
	self, err := os.ReadFile(bin)
	if err != nil {
		return nil, err
	}
	var layer bytes.Buffer
	tw := tar.NewWriter(&layer)
	add := func(h *tar.Header, body []byte) error {
		h.Format = tar.FormatPAX
		h.Size = int64(len(body))
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		_, err := tw.Write(body)
		return err
	}
	for _, d := range []string{"bin/", "etc/", "tmp/"} {
		mode := int64(0o755)
		if d == "tmp/" {
			mode = 0o1777
		}
		if err := add(&tar.Header{Typeflag: tar.TypeDir, Name: d, Mode: mode}, nil); err != nil {
			return nil, err
		}
	}
	for _, f := range []struct {
		name string
		mode int64
		body []byte
	}{
		{"bin/mirafleet", 0o555, self},
		{"etc/passwd", 0o644, []byte("root:x:0:0:root:/root:/bin/mirafleet\nworker:x:1000:1000:worker:/tmp:/bin/mirafleet\n")},
		{"etc/group", 0o644, []byte("root:x:0:\nworker:x:1000:\n")},
	} {
		if err := add(&tar.Header{Typeflag: tar.TypeReg, Name: f.name, Mode: f.mode}, f.body); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	layerBytes := layer.Bytes()
	layerDigest := sha256Hex(layerBytes)
	cfg, _ := json.Marshal(map[string]any{
		"architecture": runtime.GOARCH, "os": "linux",
		"config": map[string]any{"User": "1000:1000", "Entrypoint": []string{"/bin/mirafleet"}, "WorkingDir": "/", "Env": []string{"PATH=/bin"}},
		"rootfs": map[string]any{"type": "layers", "diff_ids": []string{"sha256:" + layerDigest}},
	})
	cfgDigest := sha256Hex(cfg)
	manifest, _ := json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": "sha256:" + cfgDigest, "size": len(cfg)},
		"layers": []any{map[string]any{"mediaType": "application/vnd.oci.image.layer.v1.tar", "digest": "sha256:" + layerDigest, "size": len(layerBytes)}},
	})
	manifestDigest := sha256Hex(manifest)
	index, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"manifests": []any{map[string]any{
			"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": "sha256:" + manifestDigest, "size": len(manifest),
			"platform":    map[string]any{"architecture": runtime.GOARCH, "os": "linux"},
			"annotations": map[string]any{"io.containerd.image.name": "mirafleet-test/e2e:1", "org.opencontainers.image.ref.name": "1"},
		}},
	})
	var out bytes.Buffer
	ow := tar.NewWriter(&out)
	for _, f := range []struct {
		name string
		body []byte
	}{
		{"oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`)},
		{"index.json", index},
		{"blobs/sha256/" + layerDigest, layerBytes},
		{"blobs/sha256/" + cfgDigest, cfg},
		{"blobs/sha256/" + manifestDigest, manifest},
	} {
		if err := ow.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: f.name, Mode: 0o644, Size: int64(len(f.body))}); err != nil {
			return nil, err
		}
		if _, err := io.Copy(ow, bytes.NewReader(f.body)); err != nil {
			return nil, err
		}
	}
	if err := ow.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// wsDirs lists workspace directories under the runner's root (the executor's own .meta is state,
// not a workspace).
func (e *e2eEnv) wsDirs() []string {
	ents, _ := os.ReadDir(e.wsRoot)
	var out []string
	for _, d := range ents {
		if d.Name() != ".meta" {
			out = append(out, d.Name())
		}
	}
	return out
}
