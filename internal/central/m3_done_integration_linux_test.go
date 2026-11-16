//go:build linux && integration

package central

// The end-to-end workspace scenario against a real containerd, run by
// `scripts/dev.sh integration` in the privileged dind harness: central, two runners and the
// operator clients are in this process (as in TestM3Done); each runner owns a real ctrd executor
// over its own root and containerd namespace. Every isolation claim is checked by
// behaviour: the process runs as a non-root uid, the root filesystem is read-only, a host symlink
// cannot be read, and a TCP dial fails. Nothing is pulled: the image is built at test time from
// this test binary, which also acts as the in-container init and as a tiny tool.

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
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/namespaces"

	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/ctrd"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/initproc"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/wsconfig"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "workspace-init" {
		// This test binary is bind-mounted as /.mirafleet/init by the executor under test.
		if err := initproc.Run(); err != nil {
			fmt.Fprintln(os.Stderr, "workspace-init:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(os.Args) > 1 && os.Args[1] == "mf-tool" {
		os.Exit(m3Tool(os.Args[2:]))
	}
	os.Exit(m.Run())
}

// m3Tool is the in-container userland: /bin/init mf-tool <cmd> ...
func m3Tool(a []string) int {
	if len(a) == 0 {
		return 2
	}
	fail := func(err error) int { fmt.Println("error:", err); return 1 }
	switch a[0] {
	case "id":
		fmt.Printf("uid=%d gid=%d\n", os.Getuid(), os.Getgid())
	case "touch":
		if err := os.WriteFile(a[1], []byte("x"), 0o644); err != nil {
			return fail(err)
		}
	case "symlink":
		if err := os.Symlink(a[1], a[2]); err != nil {
			return fail(err)
		}
	case "dial":
		c, err := net.DialTimeout("tcp", a[1], 2*time.Second)
		if err == nil {
			c.Close()
			fmt.Println("dial-ok")
			return 0
		}
		fmt.Println("dial-failed:", err)
	default:
		return 2
	}
	return 0
}

const m3RealImageRef = "docker.io/mirafleet-test/init:1" // normalised form of m3Image

func m3RealImage() ([]byte, error) {
	self, err := os.ReadFile("/proc/self/exe")
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
		{"bin/init", 0o555, self},
		{"etc/passwd", 0o644, []byte("root:x:0:0:root:/root:/bin/init\nworker:x:1000:1000:worker:/tmp:/bin/init\n")},
		{"etc/group", 0o644, []byte("root:x:0:\nworker:x:1000:\n")},
	} {
		if err := add(&tar.Header{Typeflag: tar.TypeReg, Name: f.name, Mode: f.mode}, f.body); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	sum := func(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
	layerBytes := layer.Bytes()
	layerDigest := sum(layerBytes)
	cfg, _ := json.Marshal(map[string]any{
		"architecture": runtime.GOARCH, "os": "linux",
		"config": map[string]any{"User": "1000:1000", "Entrypoint": []string{"/bin/init"}, "WorkingDir": "/", "Env": []string{"PATH=/bin"}},
		"rootfs": map[string]any{"type": "layers", "diff_ids": []string{"sha256:" + layerDigest}},
	})
	cfgDigest := sum(cfg)
	manifest, _ := json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": "sha256:" + cfgDigest, "size": len(cfg)},
		"layers": []any{map[string]any{"mediaType": "application/vnd.oci.image.layer.v1.tar", "digest": "sha256:" + layerDigest, "size": len(layerBytes)}},
	})
	manifestDigest := sum(manifest)
	index, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"manifests": []any{map[string]any{
			"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": "sha256:" + manifestDigest, "size": len(manifest),
			"platform":    map[string]any{"architecture": runtime.GOARCH, "os": "linux"},
			"annotations": map[string]any{"io.containerd.image.name": m3RealImageRef, "org.opencontainers.image.ref.name": "1"},
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
	return out.Bytes(), ow.Close()
}

func TestM3DoneIntegration(t *testing.T) {
	sock := os.Getenv("MIRAFLEET_TEST_CONTAINERD_SOCK")
	if sock == "" {
		t.Skip("MIRAFLEET_TEST_CONTAINERD_SOCK not set; run scripts/dev.sh integration")
	}
	img, err := m3RealImage()
	must(t, err)

	type host struct {
		ns, root string
		client   *containerd.Client
		ctx      context.Context
		mu       sync.Mutex
		ex       workspace.Executor
	}
	hosts := map[string]*host{}
	userns := "required"
	yamlFor := func(name string) string {
		h := hosts[name]
		base := 1000000
		if name == "rb" {
			base = 2000000
		}
		return fmt.Sprintf(`executor: containerd
concurrent: 1
max_age: 24h
images: ["mirafleet-test/init:*"]
limits: {cpus: 1, memory: 256MiB, pids: 128, tmpfs: 16MiB, disk: 256MiB, max_file: 4MiB, exec_max: 2m}
network: [none]
env_allow: [LANG]
containerd: {address: %s, namespace: %s, root: %s, userns: %s, uid_base: %d, apparmor: auto}
`, sock, h.ns, h.root, userns, base)
	}
	newExec := func(name string) (workspace.Executor, error) {
		cfg, err := wsconfig.Parse([]byte(yamlFor(name)))
		if err != nil {
			return nil, err
		}
		return ctrd.New(cfg, ctrd.Options{Runner: name})
	}
	for _, name := range []string{"ra", "rb"} {
		h := &host{ns: "mirafleet-m3-" + name}
		h.root = filepath.Join(t.TempDir(), "ws")
		must(t, os.Mkdir(h.root, 0o700))
		c, err := containerd.New(sock, containerd.WithDefaultNamespace(h.ns))
		must(t, err)
		h.client = c
		h.ctx = namespaces.WithNamespace(context.Background(), h.ns)
		t.Cleanup(func() { _ = c.Close() })
		imgs, err := c.Import(h.ctx, bytes.NewReader(img), containerd.WithAllPlatforms(true))
		must(t, err)
		t.Cleanup(func() {
			for _, i := range imgs {
				_ = c.ImageService().Delete(context.Background(), i.Name, images.SynchronousDelete())
			}
		})
		im, err := c.GetImage(h.ctx, m3RealImageRef)
		must(t, err)
		must(t, im.Unpack(h.ctx, ""))
		hosts[name] = h
	}
	// userns: required where the kernel allows it, off otherwise (as the executor tests do).
	if ex, err := newExec("ra"); err != nil {
		if !strings.Contains(err.Error(), "userns: required") {
			t.Fatalf("executor: %v", err)
		}
		userns = "off"
		t.Logf("userns: required unsupported here, using off")
	} else {
		_ = ex.Close()
	}
	for name, h := range hosts {
		name, h := name, h
		t.Cleanup(func() { // whatever a failed run left behind
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.ex == nil {
				ex, err := newExec(name)
				if err != nil {
					return
				}
				h.ex = ex
			}
			infos, _ := h.ex.List(context.Background())
			for _, i := range infos {
				_ = h.ex.Destroy(context.Background(), i.ID)
			}
			_ = h.ex.Close()
		})
	}

	secretDir := t.TempDir()
	secret := filepath.Join(secretDir, "host-secret")
	must(t, os.WriteFile(secret, []byte("HOST-SECRET-"+fmt.Sprint(time.Now().UnixNano())), 0o644))

	tool := func(a ...string) []string { return append([]string{"/bin/init", "mf-tool"}, a...) }
	b := m3Backend{
		real: true,
		yaml: yamlFor,
		exec: func(t *testing.T, name string) workspace.Executor {
			h := hosts[name]
			h.mu.Lock()
			defer h.mu.Unlock()
			ex, err := newExec(name)
			must(t, err)
			h.ex = ex
			return ex
		},
		stop: func(t *testing.T, name string) {
			h := hosts[name]
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.ex != nil {
				must(t, h.ex.Close())
				h.ex = nil
			}
		},
		idArgv:   tool("id"),
		etcArgv:  tool("touch", "/etc/m3"),
		dialArgv: tool("dial", "1.1.1.1:80"),
		symlinkArgv: func(target, link string) []string {
			return tool("symlink", target, link)
		},
		hostSecret: secret,
		containers: func(t *testing.T, name string) int {
			h := hosts[name]
			cs, err := h.client.Containers(h.ctx)
			must(t, err)
			return len(cs)
		},
		dirs: func(t *testing.T, name string) []string {
			ents, err := os.ReadDir(hosts[name].root)
			must(t, err)
			var out []string
			for _, d := range ents {
				if d.Name() != ".meta" { // the executor's own state, not a workspace
					out = append(out, d.Name())
				}
			}
			return out
		},
		after: func(t *testing.T) {
			// ra's workspace was closed by alice's revoke: nothing is left there. rb's was
			// orphaned by the runner revoke (marked lost); its host keeps it, by design, until
			// `runner workspace prune`; the cleanup above removes it.
			if n := hostsLeft(t, hosts["ra"].client, hosts["ra"].ctx); n != 0 {
				t.Errorf("ra: %d containers left", n)
			}
		},
	}
	runM3Done(t, b)
}

func hostsLeft(t *testing.T, c *containerd.Client, ctx context.Context) int {
	t.Helper()
	cs, err := c.Containers(ctx)
	must(t, err)
	return len(cs)
}
