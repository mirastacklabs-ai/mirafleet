//go:build linux && integration

package ctrd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/namespaces"

	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/egress"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/wsconfig"
)

// How these tests reach a "remote" host without a network: the harness container (the runner's
// netns, the proxy's side) has an eth0 address on the default bridge (172.17.x.x). The test HTTP
// server listens on that address; it is a private address, so it is reachable only through an
// explicit IP rule (egress_cidr_rules: true). Workspaces have a netns with loopback only, so
// the only way to that server is the proxy listening on 127.0.0.1:3128 inside the workspace.
//
// The test binary is its own HTTP client: run inside a workspace with MF_TEST_GET=<url> (it
// honours HTTP_PROXY like any Go client) or MF_TEST_DIAL=<host:port> (a direct dial).
func init() {
	if u := os.Getenv("MF_TEST_GET"); u != "" {
		cl := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, DisableKeepAlives: true}}
		resp, err := cl.Get(u)
		if err != nil {
			fmt.Println("ERR", err)
			os.Exit(2)
		}
		b, _ := io.ReadAll(resp.Body)
		fmt.Printf("STATUS %d BODY %s\n", resp.StatusCode, strings.TrimSpace(string(b)))
		os.Exit(0)
	}
	if a := os.Getenv("MF_TEST_DIAL"); a != "" {
		c, err := net.DialTimeout("tcp", a, 5*time.Second)
		if err != nil {
			fmt.Println("ERR", err)
			os.Exit(2)
		}
		c.Close()
		fmt.Println("CONNECTED")
		os.Exit(0)
	}
}

const egressImageRef = "mirafleet-test/init:1"

func egressConfig(t *testing.T, root, userns string) *wsconfig.Config {
	t.Helper()
	cfg, err := wsconfig.Parse([]byte(fmt.Sprintf(`
executor: containerd
concurrent: 4
images: ["mirafleet-test/init:*"]
limits: {cpus: 1, memory: 256MiB, pids: 128, tmpfs: 16MiB, disk: 1GiB, max_file: 1MiB}
network: [none, egress]
egress_cidr_rules: true
containerd: {address: %s, namespace: %s, root: %s, userns: %s, uid_base: 1000000, apparmor: off} # the harness has no apparmor_parser; "auto" would refuse to start
`, os.Getenv("MIRAFLEET_TEST_CONTAINERD_SOCK"), testNamespace, root, userns)))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// bridgeIP returns a non-loopback IPv4 address of this (harness) container.
func bridgeIP(t *testing.T) string {
	t.Helper()
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() {
			return ipn.IP.String()
		}
	}
	t.Skip("harness container has no non-loopback IPv4 address")
	return ""
}

type egressEnv struct {
	e    *Executor
	cfg  *wsconfig.Config
	ctx  context.Context
	host string // server IP
	port string
}

func newEgressEnv(t *testing.T) *egressEnv {
	t.Helper()
	mode := usernsForTests(t)
	c, ctx := testClient(t)
	img := importTestImage(t, c, ctx)
	alias, err := c.ImageService().Create(ctx, images.Image{Name: normalizedTestImage, Target: img.Target()})
	if err != nil {
		t.Fatalf("alias image: %v", err)
	}
	t.Cleanup(func() { deleteImage(t, c, alias.Name) })

	ip := bridgeIP(t)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello from host")
	})}
	l, err := net.Listen("tcp", ip+":0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(l)
	t.Cleanup(func() { _ = srv.Close() })
	_, port, _ := net.SplitHostPort(l.Addr().String())

	cfg := egressConfig(t, newRoot(t), mode)
	ex, err := New(cfg, Options{StageInit: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	te := &egressEnv{e: ex.(*Executor), cfg: cfg, ctx: ctx, host: ip, port: port}
	t.Cleanup(func() { destroyAll(t, te.e) })
	return te
}

func (te *egressEnv) create(t *testing.T, mode workspace.NetworkMode, allow ...workspace.EgressRule) workspace.ID {
	t.Helper()
	s := testSpec()
	s.Network = workspace.Network{Mode: mode, Allow: allow}
	if err := te.e.Create(te.ctx, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	return s.ID
}

// run executes the test binary inside the workspace with extra env.
func (te *egressEnv) run(t *testing.T, id workspace.ID, env map[string]string) (int, string) {
	t.Helper()
	var out bytes.Buffer
	res, err := te.e.Exec(te.ctx, id, workspace.ExecRequest{Argv: []string{initMount}, Env: env, Stdout: &out, Stderr: &out})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	return res.ExitCode, strings.TrimSpace(out.String())
}

func portAsInt(t *testing.T, p string) int {
	t.Helper()
	var n int
	if _, err := fmt.Sscanf(p, "%d", &n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestEgressProxyInNetns(t *testing.T) {
	te := newEgressEnv(t)
	url := fmt.Sprintf("http://%s:%s/", te.host, te.port)
	pn := portAsInt(t, te.port)

	id := te.create(t, workspace.NetEgress, workspace.EgressRule{Host: te.host, Port: pn})
	if env := te.e.EgressEnv(id); env["HTTP_PROXY"] != "http://127.0.0.1:3128" || env["NO_PROXY"] != "" {
		t.Fatalf("EgressEnv = %v", env)
	}

	// Allowlisted: reaches the host server through the proxy (Exec injects the proxy env).
	code, out := te.run(t, id, map[string]string{"MF_TEST_GET": url})
	if code != 0 || !strings.Contains(out, "STATUS 200 BODY hello from host") {
		t.Fatalf("allowlisted GET: exit %d, %q", code, out)
	}
	// Not allowlisted: other port on the same host, an unknown name, the metadata IP.
	for _, u := range []string{
		fmt.Sprintf("http://%s:%d/", te.host, pn+1),
		"http://example.invalid/",
		"http://169.254.169.254/latest/meta-data/",
	} {
		code, out := te.run(t, id, map[string]string{"MF_TEST_GET": u})
		if code != 0 || !strings.Contains(out, "STATUS 403") {
			t.Errorf("GET %s: want proxy 403, got exit %d %q", u, code, out)
		}
	}
	// No route around the proxy: a direct dial from the workspace fails.
	code, out = te.run(t, id, map[string]string{"MF_TEST_DIAL": te.host + ":" + te.port})
	if code == 0 || !strings.Contains(out, "ERR") {
		t.Errorf("direct dial succeeded: exit %d %q", code, out)
	}

	// Destroy closes the listener: keep the netns alive through an fd, then the proxy port must
	// be free in it (binding it again succeeds only if the runner's listener is gone).
	ctx := namespaces.WithNamespace(te.ctx, te.e.ns)
	ctr, err := te.e.client.LoadContainer(ctx, ctrName(id))
	if err != nil {
		t.Fatal(err)
	}
	task, err := ctr.Task(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	nsf, err := os.Open(fmt.Sprintf("/proc/%d/ns/net", task.Pid()))
	if err != nil {
		t.Fatal(err)
	}
	defer nsf.Close()
	nsPath := fmt.Sprintf("/proc/self/fd/%d", nsf.Fd())
	if l, err := egress.ListenInNetns(nsPath, "127.0.0.1:3128"); err == nil {
		l.Close()
		t.Fatal("proxy port is free while the workspace is alive")
	}
	if err := te.e.Destroy(te.ctx, id); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if te.e.EgressEnv(id) != nil {
		t.Error("EgressEnv still set after Destroy")
	}
	l, err := egress.ListenInNetns(nsPath, "127.0.0.1:3128")
	if err != nil {
		t.Fatalf("proxy listener still open in the netns after Destroy: %v", err)
	}
	l.Close()
}

func TestEgressNetworkNone(t *testing.T) {
	te := newEgressEnv(t)
	id := te.create(t, workspace.NetNone)
	if env := te.e.EgressEnv(id); env != nil {
		t.Fatalf("EgressEnv for network=none = %v", env)
	}
	// Even with the proxy variables set by hand there is nothing to talk to.
	code, out := te.run(t, id, map[string]string{
		"MF_TEST_GET": fmt.Sprintf("http://%s:%s/", te.host, te.port),
		"HTTP_PROXY":  "http://127.0.0.1:3128",
	})
	if code == 0 || !strings.Contains(out, "ERR") {
		t.Errorf("GET via a nonexistent proxy: exit %d %q", code, out)
	}
	code, out = te.run(t, id, map[string]string{"MF_TEST_DIAL": te.host + ":" + te.port})
	if code == 0 || !strings.Contains(out, "ERR") {
		t.Errorf("direct dial succeeded: exit %d %q", code, out)
	}
}

func TestEgressRefusals(t *testing.T) {
	te := newEgressEnv(t)
	s := testSpec()
	s.Network = workspace.Network{Mode: workspace.NetEgress, Allow: []workspace.EgressRule{{Host: "169.254.169.254", Port: 80}}}
	if err := te.e.Create(te.ctx, s); err == nil {
		t.Error("rule for the metadata IP was accepted")
	}
	s = testSpec()
	s.Network = workspace.Network{Mode: workspace.NetNone, Allow: []workspace.EgressRule{{Host: "example.com", Port: 443}}}
	if err := te.e.Create(te.ctx, s); err == nil {
		t.Error("egress rules with network=none were accepted")
	}
}

// A restarted runner re-attaches listeners for running egress workspaces.
func TestEgressReattachAfterRestart(t *testing.T) {
	te := newEgressEnv(t)
	url := fmt.Sprintf("http://%s:%s/", te.host, te.port)
	id := te.create(t, workspace.NetEgress, workspace.EgressRule{Host: te.host, Port: portAsInt(t, te.port)})

	// "Crash": the first executor goes away, the container keeps running.
	te.e.closeEgress()
	if code, out := te.run(t, id, map[string]string{"MF_TEST_GET": url}); code == 0 {
		t.Fatalf("GET worked with the runner's proxy gone: %q", out)
	}
	ex, err := New(te.cfg, Options{})
	if err != nil {
		t.Fatalf("New (restart): %v", err)
	}
	te.e = ex.(*Executor)
	if te.e.EgressEnv(id) == nil {
		t.Fatal("listener not re-attached")
	}
	code, out := te.run(t, id, map[string]string{"MF_TEST_GET": url})
	if code != 0 || !strings.Contains(out, "STATUS 200") {
		t.Fatalf("after re-attach: exit %d %q", code, out)
	}
}
