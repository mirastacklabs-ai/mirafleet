//go:build linux && integration

package ctrd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"

	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/wsconfig"
)

const normalizedTestImage = "docker.io/mirafleet-test/init:1"

// testEnv is one executor over a fresh workspace root.
type testEnv struct {
	e    *Executor
	root string
	c    *containerd.Client
	ctx  context.Context
	mode string
}

func newConfig(t *testing.T, root, userns string, concurrent int) *wsconfig.Config {
	t.Helper()
	cfg, err := wsconfig.Parse([]byte(fmt.Sprintf(`
executor: containerd
concurrent: %d
images: ["mirafleet-test/init:*"]
limits: {cpus: 1, memory: 256MiB, pids: 128, tmpfs: 16MiB, disk: %s, max_file: 4MiB}
network: [none]
containerd: {address: %s, namespace: %s, root: %s, userns: %s, uid_base: 1000000, apparmor: auto}
`, concurrent, testDisk, os.Getenv("MIRAFLEET_TEST_CONTAINERD_SOCK"), testNamespace, root, userns)))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func newRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "ws")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

var (
	usernsOnce sync.Once
	usernsMode string
)

// usernsForTests probes once whether `userns: required` works on this kernel and returns the
// mode the remaining tests should use. If it does not work it proves New fails clearly under
// required and succeeds under off.
func usernsForTests(t *testing.T) string {
	t.Helper()
	testClient(t) // skips outside the harness
	usernsOnce.Do(func() {
		usernsMode = "required"
		e, err := New(newConfig(t, newRoot(t), "required", 1), Options{})
		if err == nil {
			_ = e.Close()
			return
		}
		usernsMode = "off"
		t.Logf("userns: required unsupported here: %v", err)
		if !strings.Contains(err.Error(), "userns: required") {
			t.Errorf("error does not name userns: required: %v", err)
		}
		e2, err := New(newConfig(t, newRoot(t), "off", 1), Options{})
		if err != nil {
			t.Errorf("userns: off must work: %v", err)
			return
		}
		_ = e2.Close()
	})
	return usernsMode
}

func newEnv(t *testing.T, concurrent int, opts Options) *testEnv {
	t.Helper()
	mode := usernsForTests(t)
	c, ctx := testClient(t)
	img := importTestImage(t, c, ctx)
	alias, err := c.ImageService().Create(ctx, images.Image{Name: normalizedTestImage, Target: img.Target()})
	if err != nil {
		t.Fatalf("alias image: %v", err)
	}
	t.Cleanup(func() { deleteImage(t, c, alias.Name) })

	root := newRoot(t)
	ex, err := New(newConfig(t, root, mode, concurrent), opts)
	if err != nil {
		t.Fatalf("New (userns %s): %v", mode, err)
	}
	e := ex.(*Executor)
	t.Cleanup(func() { destroyAll(t, e) }) // whatever a failed test left behind
	return &testEnv{e: e, root: root, c: c, ctx: ctx, mode: mode}
}

func testSpec() workspace.Spec {
	return workspace.Spec{
		ID: workspace.NewID(), Image: "mirafleet-test/init:1", Pull: workspace.PullNever,
		Labels: map[string]string{"session": "s1"},
	}
}

func (te *testEnv) container(t *testing.T, id workspace.ID) containerd.Container {
	t.Helper()
	c, err := te.c.LoadContainer(te.ctx, "mf-"+string(id))
	if err != nil {
		t.Fatalf("load container: %v", err)
	}
	return c
}

func TestCreateListDestroy(t *testing.T) {
	te := newEnv(t, 2, Options{Runner: "r1"})
	s := testSpec()
	if err := te.e.Create(te.ctx, s); err != nil {
		t.Fatalf("Create: %v", err)
	}

	infos, err := te.e.List(te.ctx)
	if err != nil || len(infos) != 1 {
		t.Fatalf("List = %v, %v", infos, err)
	}
	in := infos[0]
	if in.ID != s.ID || in.State != workspace.StateRunning || in.Labels["session"] != "s1" ||
		in.Image != normalizedTestImage || in.Created.IsZero() {
		t.Errorf("Info = %+v", in)
	}

	// Directory: 0700, owned by the mapped uid.
	dir := filepath.Join(te.root, string(s.ID))
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	wantOwner := uint32(1000000 + 1000)
	if te.mode == "off" {
		wantOwner = 1000000
	}
	if fi.Mode().Perm() != 0o700 || st.Uid != wantOwner || st.Gid != wantOwner {
		t.Errorf("dir mode %v uid %d gid %d, want 0700 and %d", fi.Mode().Perm(), st.Uid, st.Gid, wantOwner)
	}

	// The kernel honours the spec for PID 1 (visible from the harness, which shares its pid ns).
	ctr := te.container(t, s.ID)
	task, err := ctr.Task(te.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", task.Pid()))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"NoNewPrivs:\t1", "CapEff:\t0000000000000000", "CapBnd:\t0000000000000000", "Seccomp:\t2"} {
		if !strings.Contains(string(status), want) {
			t.Errorf("/proc/%d/status lacks %q:\n%s", task.Pid(), want, status)
		}
	}
	if te.mode == "required" {
		um, _ := os.ReadFile(fmt.Sprintf("/proc/%d/uid_map", task.Pid()))
		if f := strings.Fields(string(um)); len(f) != 3 || f[0] != "0" || f[1] != "1000000" || f[2] != "65536" {
			t.Errorf("uid_map = %q", um)
		}
	}
	cinfo, err := ctr.Info(te.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cinfo.Labels["mirafleet.workspace"] != string(s.ID) || cinfo.Labels["mirafleet.runner"] != "r1" ||
		cinfo.Labels["mirafleet.slot"] != "0" {
		t.Errorf("labels = %v", cinfo.Labels)
	}

	// Idempotent create: no-op, same container.
	if err := te.e.Create(te.ctx, s); err != nil {
		t.Fatalf("second Create: %v", err)
	}
	if infos, _ := te.e.List(te.ctx); len(infos) != 1 {
		t.Fatalf("List after idempotent create = %d", len(infos))
	}

	if err := te.e.Destroy(te.ctx, s.ID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if infos, _ := te.e.List(te.ctx); len(infos) != 0 {
		t.Fatalf("List after destroy = %v", infos)
	}
	for _, p := range []string{dir, filepath.Join(te.root, ".meta", string(s.ID))} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still exists after Destroy (%v)", p, err)
		}
	}
	if _, err := te.c.SnapshotService("").Stat(te.ctx, "mf-"+string(s.ID)); err == nil {
		t.Error("snapshot still present after Destroy")
	}
	// Idempotent destroy, and destroy of something that never existed.
	if err := te.e.Destroy(te.ctx, s.ID); err != nil {
		t.Errorf("second Destroy: %v", err)
	}
	if err := te.e.Destroy(te.ctx, workspace.NewID()); err != nil {
		t.Errorf("Destroy of unknown id: %v", err)
	}
}

func TestDestroyRemovesLeftoverDirectory(t *testing.T) {
	te := newEnv(t, 1, Options{})
	id := workspace.NewID()
	dir := filepath.Join(te.root, string(id))
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := te.e.Destroy(te.ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("leftover dir not removed: %v", err)
	}
	// A symlink in place of the directory is refused and left alone.
	id2 := workspace.NewID()
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(te.root, string(id2))); err != nil {
		t.Fatal(err)
	}
	if err := te.e.Destroy(te.ctx, id2); err == nil {
		t.Error("Destroy followed or removed a symlinked workspace dir")
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("symlink target damaged: %v", err)
	}
}

func TestAllowlistRefusesBeforePull(t *testing.T) {
	te := newEnv(t, 1, Options{})
	before, _ := te.c.ListImages(te.ctx)
	for _, img := range []string{
		"evil.example/x/y:1", // not allowlisted; must never be contacted
		"mirafleet-test/other:1",
		"mirafleet-test/init@sha256:" + strings.Repeat("0", 64), // digest ref vs tag-only pattern
		"mirafleet-test/init:*",
		"",
	} {
		s := testSpec()
		s.Image = img
		s.Pull = workspace.PullAlways
		err := te.e.Create(te.ctx, s)
		if !errors.Is(err, workspace.ErrRefused) {
			t.Errorf("Create(%q) = %v, want ErrRefused", img, err)
		}
		if _, serr := os.Lstat(filepath.Join(te.root, string(s.ID))); !errors.Is(serr, os.ErrNotExist) {
			t.Errorf("Create(%q) left a directory", img)
		}
	}
	after, _ := te.c.ListImages(te.ctx)
	if len(after) != len(before) {
		t.Errorf("images changed: %d -> %d (something was pulled)", len(before), len(after))
	}

	// Allowlisted but absent locally with pull: never.
	s := testSpec()
	s.Image = "mirafleet-test/absent:1"
	if err := te.e.Create(te.ctx, s); !errors.Is(err, workspace.ErrRefused) {
		t.Errorf("pull never, image absent: %v, want ErrRefused", err)
	}
	// Other refusals: network mode not enabled on this runner, limits above the caps, secure
	// runtime not configured, bad label.
	s = testSpec()
	s.Network.Mode = workspace.NetEgress
	if err := te.e.Create(te.ctx, s); !errors.Is(err, workspace.ErrRefused) {
		t.Errorf("egress: %v", err)
	}
	s = testSpec()
	s.Limits.MemoryBytes = 1 << 40
	if err := te.e.Create(te.ctx, s); !errors.Is(err, workspace.ErrRefused) {
		t.Errorf("over-cap memory: %v", err)
	}
	s = testSpec()
	s.RequireSecureRuntime = true
	if err := te.e.Create(te.ctx, s); !errors.Is(err, workspace.ErrRefused) {
		t.Errorf("secure runtime without config: %v", err)
	}
	s = testSpec()
	s.Labels = map[string]string{"bad key!": "x"}
	if err := te.e.Create(te.ctx, s); !errors.Is(err, workspace.ErrRefused) {
		t.Errorf("bad label: %v", err)
	}
	if infos, _ := te.e.List(te.ctx); len(infos) != 0 {
		t.Errorf("refused creates left workspaces: %v", infos)
	}
}

func TestSlotsDistinctAndBounded(t *testing.T) {
	te := newEnv(t, 2, Options{})
	a, b := testSpec(), testSpec()
	for _, s := range []workspace.Spec{a, b} {
		if err := te.e.Create(te.ctx, s); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	slot := func(id workspace.ID) string {
		info, err := te.container(t, id).Info(te.ctx)
		if err != nil {
			t.Fatal(err)
		}
		return info.Labels["mirafleet.slot"]
	}
	if slot(a.ID) == slot(b.ID) {
		t.Errorf("both workspaces got slot %s", slot(a.ID))
	}
	if te.mode == "required" {
		// Different host ranges: the two init processes' uid_maps differ.
		maps := map[string]bool{}
		for _, id := range []workspace.ID{a.ID, b.ID} {
			task, _ := te.container(t, id).Task(te.ctx, nil)
			um, _ := os.ReadFile(fmt.Sprintf("/proc/%d/uid_map", task.Pid()))
			maps[strings.Join(strings.Fields(string(um)), " ")] = true
		}
		if len(maps) != 2 {
			t.Errorf("uid maps not distinct: %v", maps)
		}
	}
	if err := te.e.Create(te.ctx, testSpec()); !errors.Is(err, workspace.ErrBusy) {
		t.Errorf("third create = %v, want ErrBusy", err)
	}
	freed := slot(a.ID)
	if err := te.e.Destroy(te.ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	c := testSpec()
	if err := te.e.Create(te.ctx, c); err != nil {
		t.Fatalf("create after free: %v", err)
	}
	if slot(c.ID) != freed {
		t.Errorf("freed slot %s not reused (got %s)", freed, slot(c.ID))
	}
}

func TestCreateFailureCleansUp(t *testing.T) {
	// An init "binary" that cannot execute: the task fails to start after the container, the
	// snapshot and the directories exist, so Create must undo all of it.
	bad := filepath.Join(t.TempDir(), "badinit")
	if err := os.WriteFile(bad, []byte("not an executable\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	te := newEnv(t, 1, Options{InitBinary: bad})
	s := testSpec()
	err := te.e.Create(te.ctx, s)
	if err == nil {
		t.Fatal("Create with a broken init succeeded")
	}
	t.Logf("expected failure: %v", err)
	if infos, _ := te.e.List(te.ctx); len(infos) != 0 {
		t.Errorf("containers left behind: %v", infos)
	}
	for _, p := range []string{filepath.Join(te.root, string(s.ID)), filepath.Join(te.root, ".meta", string(s.ID))} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s left behind (%v)", p, err)
		}
	}
	if _, err := te.c.SnapshotService("").Stat(te.ctx, "mf-"+string(s.ID)); err == nil {
		t.Error("snapshot left behind")
	}
	// The slot is free again.
	if te.e.reserved[0] {
		t.Error("slot reservation leaked")
	}
}

func TestNewRefusesBadNamespaceAndSocket(t *testing.T) {
	testClient(t)
	root := newRoot(t)
	cfg := newConfig(t, root, "off", 1)
	cfg.Containerd.Namespace = "moby"
	if _, err := New(cfg, Options{}); err == nil {
		t.Error("namespace moby accepted")
	}
	cfg = newConfig(t, root, "off", 1)
	cfg.Containerd.Address = filepath.Join(t.TempDir(), "nope.sock")
	if _, err := New(cfg, Options{}); err == nil {
		t.Error("missing socket accepted")
	}
	cfg = newConfig(t, root, "off", 1)
	cfg.Containerd.SecureRuntime = "io.containerd.runsc.v1"
	if _, err := New(cfg, Options{}); err == nil || !strings.Contains(err.Error(), "secure_runtime") {
		t.Errorf("missing secure runtime shim: %v", err)
	}
}

// A per-workspace disk limit below the runner cap is stored as a label that Usage and List use.
func TestCreateDiskLabelAndListDiskBytes(t *testing.T) {
	te := newEnv(t, 2, Options{Runner: "r1"})
	capDisk := int64(te.e.cfg.Limits.Disk)
	low, same := testSpec(), testSpec()
	low.Limits.DiskBytes = capDisk / 2
	same.Limits.DiskBytes = capDisk // not lower: no label
	for _, s := range []workspace.Spec{low, same} {
		if err := te.e.Create(te.ctx, s); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	li, err := te.container(t, low.ID).Info(te.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := li.Labels[labelDisk]; got != fmt.Sprint(capDisk/2) {
		t.Errorf("label = %q, want %d", got, capDisk/2)
	}
	si, err := te.container(t, same.ID).Info(te.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := si.Labels[labelDisk]; ok {
		t.Errorf("label set although not below the cap: %v", si.Labels)
	}
	u, err := te.e.Usage(te.ctx, low.ID)
	if err != nil || u.Quota != capDisk/2 {
		t.Fatalf("Usage = %+v, %v; want quota %d", u, err, capDisk/2)
	}
	infos, err := te.e.List(te.ctx)
	if err != nil || len(infos) != 2 {
		t.Fatalf("List = %v, %v", infos, err)
	}
	for _, in := range infos {
		if in.DiskBytes < 0 {
			t.Errorf("DiskBytes = %d", in.DiskBytes)
		}
		if in.ID == low.ID && in.DiskBytes != u.Used {
			t.Errorf("DiskBytes = %d, want last measurement %d", in.DiskBytes, u.Used)
		}
	}
}
