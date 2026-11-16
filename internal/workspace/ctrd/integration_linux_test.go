//go:build linux && integration

package ctrd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"

	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/initproc"
)

// Run via `scripts/dev.sh integration`, which starts containerd inside a throwaway privileged
// container and exports its socket path in MIRAFLEET_TEST_CONTAINERD_SOCK. Outside that harness
// these tests skip.

const testNamespace = "mirafleet-test"

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "workspace-init" {
		// This test binary is bind-mounted as /.mirafleet/init by the executor under test.
		if err := initproc.Run(); err != nil {
			fmt.Fprintln(os.Stderr, "workspace-init:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if os.Getenv(testInitEnv) == "1" {
		// Running as the container's init: prove we started, then exit.
		fmt.Println("test-init ok uid", os.Getuid())
		if os.Getuid() != 1000 {
			os.Exit(3)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// testClient connects to the harness containerd or skips.
func testClient(t *testing.T) (*containerd.Client, context.Context) {
	t.Helper()
	sock := os.Getenv("MIRAFLEET_TEST_CONTAINERD_SOCK")
	if sock == "" {
		t.Skip("MIRAFLEET_TEST_CONTAINERD_SOCK not set; run scripts/dev.sh integration")
	}
	c, err := containerd.New(sock, containerd.WithDefaultNamespace(testNamespace))
	if err != nil {
		t.Fatalf("connect %s: %v", sock, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ctx, cancel := context.WithTimeout(namespaces.WithNamespace(context.Background(), testNamespace), 3*time.Minute)
	t.Cleanup(cancel)
	return c, ctx
}

// importTestImage imports the hermetic image and unpacks it; the image is removed on cleanup.
func importTestImage(t *testing.T, c *containerd.Client, ctx context.Context) containerd.Image {
	t.Helper()
	data, ref, err := buildTestImage()
	if err != nil {
		t.Fatal(err)
	}
	imgs, err := c.Import(ctx, bytes.NewReader(data), containerd.WithAllPlatforms(true))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	t.Cleanup(func() {
		for _, i := range imgs {
			deleteImage(t, c, i.Name, images.SynchronousDelete())
		}
	})
	img, err := c.GetImage(ctx, ref)
	if err != nil {
		t.Fatalf("get image %q (imported %v): %v", ref, imgs, err)
	}
	if err := img.Unpack(ctx, ""); err != nil {
		t.Fatalf("unpack: %v", err)
	}
	return img
}

func TestSmokeCreateStartDelete(t *testing.T) {
	c, ctx := testClient(t)
	img := importTestImage(t, c, ctx)

	const id = "smoke-1"
	ctr, err := c.NewContainer(ctx, id,
		containerd.WithImage(img),
		containerd.WithNewSnapshot(id+"-snap", img),
		containerd.WithNewSpec(
			oci.WithImageConfig(img),
			oci.WithEnv([]string{testInitEnv + "=1"}),
			oci.WithNoNewPrivileges,
		),
	)
	if err != nil {
		t.Fatalf("new container: %v", err)
	}
	deleted := false
	t.Cleanup(func() {
		if !deleted {
			cctx, cancel := cleanupCtx()
			defer cancel()
			cleanupErr(t, "delete container", ctr.Delete(cctx, containerd.WithSnapshotCleanup))
		}
	})

	task, err := ctr.NewTask(ctx, cio.NullIO)
	if err != nil {
		t.Fatalf("new task: %v", err)
	}
	exitCh, err := task.Wait(ctx)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if err := task.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	select {
	case st := <-exitCh:
		if st.Error() != nil || st.ExitCode() != 0 {
			t.Fatalf("task exit code=%d err=%v (want 0)", st.ExitCode(), st.Error())
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for task exit")
	}
	if _, err := task.Delete(ctx); err != nil {
		t.Fatalf("delete task: %v", err)
	}
	if err := ctr.Delete(ctx, containerd.WithSnapshotCleanup); err != nil {
		t.Fatalf("delete container: %v", err)
	}
	deleted = true

	// Cleanup assertions: no containers, no leftover snapshot for the container.
	list, err := c.Containers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("containers left behind: %d", len(list))
	}
	if _, err := c.SnapshotService("").Stat(ctx, id+"-snap"); err == nil {
		t.Fatal("container snapshot still present after delete")
	}
}

func TestDaemonVersionAndIdmap(t *testing.T) {
	c, ctx := testClient(t)
	v, err := c.Version(ctx)
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	t.Logf("containerd %s (%s)", v.Version, v.Revision)
	if !strings.HasPrefix(strings.TrimPrefix(v.Version, "v"), "2.4.") {
		t.Fatalf("daemon version %q, want 2.4.x (harness must run the go.mod containerd)", v.Version)
	}

	// Idmapped mounts need Linux >= 5.12 (>= 5.19 for overlayfs). Informational only: the
	// executor decides what to do when unsupported.
	var u syscall.Utsname
	if err := syscall.Uname(&u); err != nil {
		t.Logf("uname: %v", err)
		return
	}
	rel := make([]byte, 0, len(u.Release))
	for _, b := range u.Release {
		if b == 0 {
			break
		}
		rel = append(rel, byte(b))
	}
	var major, minor int
	_, _ = fmt.Sscanf(string(rel), "%d.%d", &major, &minor)
	t.Logf("kernel %s: idmapped mounts=%v, idmapped overlayfs=%v",
		rel, major > 5 || (major == 5 && minor >= 12), major > 5 || (major == 5 && minor >= 19))
}

// cleanupCtx is the context for t.Cleanup work: the test namespace and its own deadline, so
// cleanup neither depends on the (possibly cancelled) test context nor falls back to a default
// namespace.
func cleanupCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(namespaces.WithNamespace(context.Background(), testNamespace), 2*time.Minute)
}

// cleanupErr reports a failed cleanup step; "already gone" is fine.
func cleanupErr(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil && !errdefs.IsNotFound(err) && !errors.Is(err, workspace.ErrNotFound) {
		t.Errorf("cleanup: %s: %v", what, err)
	}
}

// destroyAll removes every workspace the executor knows and closes it.
func destroyAll(t *testing.T, e *Executor) {
	t.Helper()
	ctx, cancel := cleanupCtx()
	defer cancel()
	infos, err := e.List(ctx)
	cleanupErr(t, "list workspaces", err)
	for _, i := range infos {
		cleanupErr(t, "destroy "+string(i.ID), e.Destroy(ctx, i.ID))
	}
	cleanupErr(t, "close executor", e.Close())
}

// deleteImage removes an image on cleanup.
func deleteImage(t *testing.T, c *containerd.Client, name string, opts ...images.DeleteOpt) {
	t.Helper()
	ctx, cancel := cleanupCtx()
	defer cancel()
	cleanupErr(t, "delete image "+name, c.ImageService().Delete(ctx, name, opts...))
}
