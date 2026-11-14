//go:build linux

package ctrd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	contribapparmor "github.com/containerd/containerd/v2/contrib/apparmor"
	"github.com/containerd/containerd/v2/pkg/apparmor"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"

	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/wsconfig"
)

// Container labels. Everything the runner needs to rebuild its view is on the container.
const (
	labelWorkspace  = "mirafleet.workspace"
	labelRunner     = "mirafleet.runner"
	labelSlot       = "mirafleet.slot"
	labelNetwork    = "mirafleet.network"
	labelUserPrefix = "mirafleet.label."

	appArmorProfileName = "mirafleet-workspace"
	metaDirName         = ".meta" // <root>/.meta/<id>/{hosts,resolv.conf}; never a valid workspace id

	killWait    = 15 * time.Second // bound for a task to die after SIGKILL
	startSettle = 300 * time.Millisecond
	cleanupTime = 60 * time.Second
)

var labelKeyRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)

// Options are the non-config inputs of New.
type Options struct {
	// Runner is stored in the mirafleet.runner label (the runner's name). Optional.
	Runner string
	// InitBinary is the executable bind-mounted as PID 1. Default: the running binary.
	InitBinary string
}

// Executor is the containerd workspace executor.
type Executor struct {
	cfg     *wsconfig.Config
	cc      *wsconfig.Containerd
	client  *containerd.Client
	ns      string
	root    string
	initBin string
	runner  string
	userns  bool
	apparm  string // AppArmor profile name, "" = none

	mu       sync.Mutex
	inflight map[workspace.ID]bool
	reserved map[int]bool

	egress egressMgr // egress_linux.go: per-workspace proxies
}

// New connects to containerd and checks that the host can honour the configuration: the
// socket answers, user namespaces work when required, the secure runtime's shim exists, the
// AppArmor policy holds and the init binary is usable. Any failure is an error; nothing is
// silently weakened.
func New(cfg *wsconfig.Config, opts Options) (workspace.Executor, error) {
	if cfg == nil || cfg.Executor != wsconfig.ExecContainerd || cfg.Containerd == nil {
		return nil, errors.New("ctrd: config is not a containerd workspace config")
	}
	cc := cfg.Containerd
	if cc.Namespace == "k8s.io" || cc.Namespace == "moby" || cc.Namespace == "" {
		return nil, fmt.Errorf("ctrd: namespace %q is refused", cc.Namespace)
	}
	if err := cfg.CheckFiles(); err != nil {
		return nil, fmt.Errorf("ctrd: %w", err)
	}
	bin := opts.InitBinary
	if bin == "" {
		self, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("ctrd: locate own binary: %w", err)
		}
		bin = self
	}
	bin, err := filepath.EvalSymlinks(bin)
	if err != nil {
		return nil, fmt.Errorf("ctrd: init binary: %w", err)
	}
	if err := checkInitBinary(bin); err != nil {
		return nil, err
	}

	// Go probes IPv4/IPv6 support once (ipStackCaps) on the first listen or dial. Do that now in the
	// host netns: the first listen may otherwise happen on a thread that has setns'd into a
	// workspace netns (egress), and the probe would cache that namespace's capabilities.
	if l, lerr := net.Listen("tcp", "127.0.0.1:0"); lerr == nil {
		_ = l.Close()
	}

	c, err := containerd.New(cc.Address, containerd.WithDefaultNamespace(cc.Namespace))
	if err != nil {
		return nil, fmt.Errorf("ctrd: connect to containerd at %s: %w", cc.Address, err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = c.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(namespaces.WithNamespace(context.Background(), cc.Namespace), 30*time.Second)
	defer cancel()
	if _, err := c.Version(ctx); err != nil {
		return nil, fmt.Errorf("ctrd: containerd at %s not answering: %w", cc.Address, err)
	}

	e := &Executor{
		cfg: cfg, cc: cc, client: c, ns: cc.Namespace, root: cc.Root, initBin: bin,
		runner: opts.Runner, userns: cc.UserNS == "required",
		inflight: map[workspace.ID]bool{}, reserved: map[int]bool{},
	}
	if e.userns {
		if err := probeUserNS(ctx, c, cc.Snapshotter); err != nil {
			return nil, fmt.Errorf("ctrd: userns: required is not supported on this host: %w (fix the host or set userns: off, which weakens isolation between workspaces)", err)
		}
	}
	if cc.SecureRuntime != "" {
		if err := checkRuntimeShim(cc.SecureRuntime); err != nil {
			return nil, fmt.Errorf("ctrd: secure_runtime: %w", err)
		}
	}
	switch cc.AppArmor {
	case "off":
	case "auto", "required":
		if !apparmor.HostSupports() {
			if cc.AppArmor == "required" {
				return nil, errors.New("ctrd: apparmor: required but AppArmor is not available on this host")
			}
			break // auto: no AppArmor on this host
		}
		if err := contribapparmor.LoadDefaultProfile(appArmorProfileName); err != nil {
			return nil, fmt.Errorf("ctrd: load AppArmor profile (set apparmor: off to run without it): %w", err)
		}
		e.apparm = appArmorProfileName
	}
	e.reattachEgress(ctx)
	ok = true
	return e, nil
}

func checkInitBinary(p string) error {
	fi, err := os.Stat(p)
	if err != nil {
		return fmt.Errorf("ctrd: init binary: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("ctrd: init binary %s is not a regular file", p)
	}
	// Inside the container the process is neither the owner nor in the group.
	if fi.Mode().Perm()&0o005 != 0o005 {
		return fmt.Errorf("ctrd: init binary %s must be world readable and executable (mode %04o)", p, fi.Mode().Perm())
	}
	return nil
}

// checkRuntimeShim verifies that containerd can find the shim binary for a runtime name such
// as io.containerd.runsc.v1 (containerd-shim-runsc-v1 on PATH).
func checkRuntimeShim(name string) error {
	parts := strings.Split(name, ".")
	if len(parts) < 3 || parts[0] != "io" || parts[1] != "containerd" {
		return fmt.Errorf("runtime %q: expected a name like io.containerd.runsc.v1", name)
	}
	bin := "containerd-shim-" + strings.Join(parts[2:], "-")
	if _, err := exec.LookPath(bin); err != nil {
		return fmt.Errorf("runtime %q: shim %s not found on PATH", name, bin)
	}
	return nil
}

// Name implements workspace.Executor.
func (e *Executor) Name() string { return wsconfig.ExecContainerd }

// Close releases the containerd connection.
func (e *Executor) Close() error {
	e.closeEgress()
	return e.client.Close()
}

// Exec, ReadFile and WriteFile live in exec_linux.go and files_linux.go; disk accounting in
// disk_linux.go.

func ctrName(id workspace.ID) string { return "mf-" + string(id) }

func refuse(format string, a ...any) error {
	return fmt.Errorf("%w: %s", workspace.ErrRefused, fmt.Sprintf(format, a...))
}

func unavailable(what string, err error) error {
	return fmt.Errorf("%w: %s: %v", workspace.ErrUnavailable, what, err)
}

// resolveLimits applies the runner's caps: zero means the cap, above the cap is refused.
func (e *Executor) resolveLimits(l workspace.Limits) (cpuMilli, mem, pids, tmpfs int64, err error) {
	lim := e.cfg.Limits
	pick := func(name string, v, max int64) (int64, error) {
		if v < 0 {
			return 0, refuse("limit %s is negative", name)
		}
		if v == 0 {
			return max, nil
		}
		if v > max {
			return 0, refuse("limit %s %d exceeds the runner cap %d", name, v, max)
		}
		return v, nil
	}
	if cpuMilli, err = pick("cpu", l.CPUMilli, int64(lim.CPUs*1000)); err != nil {
		return
	}
	if mem, err = pick("memory", l.MemoryBytes, int64(lim.Memory)); err != nil {
		return
	}
	if pids, err = pick("pids", l.Pids, lim.Pids); err != nil {
		return
	}
	if tmpfs, err = pick("tmpfs", l.TmpfsBytes, int64(lim.Tmpfs)); err != nil {
		return
	}
	_, err = pick("disk", l.DiskBytes, int64(lim.Disk))
	return
}

// identity returns the in-container ids, the host start of the slot's range and the host uid
// that owns the workspace directory.
func (e *Executor) identity(slot int) (ctrID, hostBase, hostOwner uint32) {
	base := uint32(e.cc.UIDBase)
	if e.userns {
		hostBase = base + uint32(slot)*userNSStride
		return ctrUser, hostBase, hostBase + ctrUser
	}
	id := base + uint32(slot)
	return id, 0, id
}

// Create implements workspace.Executor.
func (e *Executor) Create(ctx context.Context, s workspace.Spec) (err error) {
	if !workspace.ValidID(s.ID) {
		return refuse("invalid workspace id")
	}
	// Policy first, from the runner's own config. Nothing below talks to a registry until the
	// normalised reference passed the allowlist.
	norm, nerr := workspace.NormalizeImage(s.Image)
	if nerr != nil {
		return refuse("invalid image reference: %v", nerr)
	}
	if !workspace.MatchImage(e.cfg.Images, norm) {
		return refuse("image %q is not on the runner's allowlist", norm)
	}
	if !s.Pull.Valid() {
		return refuse("unknown pull policy %q", s.Pull)
	}
	mode := string(s.Network.Mode)
	if mode == "" {
		mode = string(workspace.NetNone)
	}
	if !e.cfg.AllowsNetwork(mode) {
		return refuse("network mode %q is not allowed on this runner", mode)
	}
	if mode == string(workspace.NetEgress) && s.RequireSecureRuntime {
		return refuse("egress is not supported with secure runtimes yet")
	}
	var egressRules []string
	if mode == string(workspace.NetEgress) {
		r, rerr := e.egressRules(s.Network.Allow)
		if rerr != nil {
			return rerr
		}
		egressRules = r
	} else if len(s.Network.Allow) > 0 {
		return refuse("egress rules need network mode egress")
	}
	cpu, mem, pids, tmpfs, lerr := e.resolveLimits(s.Limits)
	if lerr != nil {
		return lerr
	}
	runtime := e.cc.Runtime
	if s.RequireSecureRuntime {
		if e.cc.SecureRuntime == "" {
			return refuse("a secure runtime is required but none is configured")
		}
		runtime = e.cc.SecureRuntime
	}
	userLabels := map[string]string{}
	for k, v := range s.Labels {
		if !labelKeyRE.MatchString(k) || len(v) > 256 {
			return refuse("invalid label %q", k)
		}
		userLabels[labelUserPrefix+k] = v
	}

	ctx = namespaces.WithNamespace(ctx, e.ns)
	name := ctrName(s.ID)

	if !e.lockID(s.ID) {
		return fmt.Errorf("%w: workspace %s is being created or destroyed", workspace.ErrBusy, s.ID)
	}
	defer e.unlockID(s.ID)

	if _, lerr := e.client.LoadContainer(ctx, name); lerr == nil {
		return nil // idempotent
	} else if !errdefs.IsNotFound(lerr) {
		return unavailable("look up container", lerr)
	}

	slot, rerr := e.reserveSlot(ctx)
	if rerr != nil {
		return rerr
	}
	defer e.releaseSlot(slot)

	// From here on, anything that fails removes everything this call made.
	defer func() {
		if err != nil {
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTime)
			defer cancel()
			if terr := e.teardown(cctx, s.ID); terr != nil {
				err = fmt.Errorf("%w (cleanup also failed: %v)", err, terr)
			}
		}
	}()

	img, ierr := e.ensureImage(ctx, norm, s.Pull)
	if ierr != nil {
		return ierr
	}

	// A directory without a container is leftover garbage from a crash; start clean.
	if rerr := e.removeDirs(s.ID); rerr != nil {
		return unavailable("remove stale workspace directory", rerr)
	}
	ctrID, hostBase, owner := e.identity(slot)
	wsDir := filepath.Join(e.root, string(s.ID))
	metaDir := filepath.Join(e.root, metaDirName, string(s.ID))
	if merr := os.MkdirAll(filepath.Dir(metaDir), 0o755); merr != nil {
		return unavailable("create meta directory", merr)
	}
	if merr := os.Mkdir(wsDir, 0o700); merr != nil {
		return unavailable("create workspace directory", merr)
	}
	if merr := os.Chown(wsDir, int(owner), int(owner)); merr != nil {
		return unavailable("chown workspace directory", merr)
	}
	if merr := os.Mkdir(metaDir, 0o755); merr != nil {
		return unavailable("create meta directory", merr)
	}
	hosts := filepath.Join(metaDir, "hosts")
	resolv := filepath.Join(metaDir, "resolv.conf")
	if merr := os.WriteFile(hosts, []byte("127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n"), 0o644); merr != nil {
		return unavailable("write hosts", merr)
	}
	// No resolver in the container: name resolution for egress happens in the runner's proxy.
	if merr := os.WriteFile(resolv, []byte("# mirafleet: no resolver in workspaces\n"), 0o644); merr != nil {
		return unavailable("write resolv.conf", merr)
	}

	diskQuota := int64(e.cfg.Limits.Disk)
	if d := s.Limits.DiskBytes; d > 0 && d < diskQuota {
		diskQuota = d
	}
	spec, serr := buildSpec(specParams{
		DiskBytes: diskQuota,
		ID:        name, UserNS: e.userns, CtrUID: ctrID, CtrGID: ctrID, HostBase: hostBase,
		CPUMilli: cpu, MemoryBytes: mem, Pids: pids, TmpfsBytes: tmpfs,
		WorkspaceDir: wsDir, HostsFile: hosts, ResolvFile: resolv, InitBinary: e.initBin,
		AppArmorProfile: e.apparm,
	})
	if serr != nil {
		return serr
	}

	labels := map[string]string{
		labelWorkspace: string(s.ID),
		labelSlot:      strconv.Itoa(slot),
		labelNetwork:   mode,
	}
	if mode == string(workspace.NetEgress) {
		labels[labelEgress] = strings.Join(egressRules, ",")
	}
	// The per-workspace disk quota can only lower the runner cap (Usage reads this label).
	if d := s.Limits.DiskBytes; d > 0 && d < int64(e.cfg.Limits.Disk) {
		labels[labelDisk] = strconv.FormatInt(d, 10)
	}
	if e.runner != "" {
		labels[labelRunner] = e.runner
	}
	for k, v := range userLabels {
		labels[k] = v
	}
	copts := []containerd.NewContainerOpts{
		containerd.WithImage(img),
		containerd.WithSnapshotter(e.cc.Snapshotter),
	}
	if e.userns {
		copts = append(copts, containerd.WithNewSnapshot(name, img,
			containerd.WithRemapperLabels(0, hostBase, 0, hostBase, userNSStride)))
	} else {
		copts = append(copts, containerd.WithNewSnapshot(name, img))
	}
	copts = append(copts,
		containerd.WithRuntime(runtime, nil),
		containerd.WithContainerLabels(labels),
		containerd.WithSpec(spec),
	)
	ctr, cerr := e.client.NewContainer(ctx, name, copts...)
	if cerr != nil {
		return unavailable("create container", cerr)
	}

	task, terr := ctr.NewTask(ctx, cio.NullIO)
	if terr != nil {
		return unavailable("create task", terr)
	}
	exit, werr := task.Wait(ctx)
	if werr != nil {
		return unavailable("wait on task", werr)
	}
	if serr := task.Start(ctx); serr != nil {
		return unavailable("start init", serr)
	}
	// Init must stay up; if it dies at once (bad binary, bad spec) report it now.
	select {
	case st := <-exit:
		return unavailable("init exited immediately", fmt.Errorf("exit code %d, err %v", st.ExitCode(), st.Error()))
	case <-time.After(startSettle):
	case <-ctx.Done():
		return ctx.Err()
	}
	if mode == string(workspace.NetEgress) {
		if eerr := e.startEgress(s.ID, task.Pid(), egressRules); eerr != nil {
			return eerr
		}
	}
	return nil
}

func (e *Executor) lockID(id workspace.ID) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.inflight[id] {
		return false
	}
	e.inflight[id] = true
	return true
}

func (e *Executor) unlockID(id workspace.ID) {
	e.mu.Lock()
	delete(e.inflight, id)
	e.mu.Unlock()
}

// reserveSlot picks the lowest slot not used by an existing container or a running Create.
func (e *Executor) reserveSlot(ctx context.Context) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	cs, err := e.client.Containers(ctx, `labels."`+labelWorkspace+`"`)
	if err != nil {
		return 0, unavailable("list containers", err)
	}
	used := map[int]bool{}
	for k := range e.reserved {
		used[k] = true
	}
	for _, c := range cs {
		info, err := c.Info(ctx, containerd.WithoutRefreshedMetadata)
		if err != nil {
			if errdefs.IsNotFound(err) {
				continue
			}
			return 0, unavailable("container info", err)
		}
		if n, err := strconv.Atoi(info.Labels[labelSlot]); err == nil {
			used[n] = true
		}
	}
	for slot := 0; slot < e.cfg.Concurrent; slot++ {
		if !used[slot] {
			e.reserved[slot] = true
			return slot, nil
		}
	}
	return 0, fmt.Errorf("%w: all %d workspace slots are in use", workspace.ErrBusy, e.cfg.Concurrent)
}

func (e *Executor) releaseSlot(slot int) {
	e.mu.Lock()
	delete(e.reserved, slot)
	e.mu.Unlock()
}

// ensureImage returns the unpacked image for norm, pulling per policy. norm already passed the
// allowlist. There are no registry credentials: pulls are anonymous.
func (e *Executor) ensureImage(ctx context.Context, norm string, pull workspace.PullPolicy) (containerd.Image, error) {
	sn := e.cc.Snapshotter
	var img containerd.Image
	if pull != workspace.PullAlways {
		i, err := e.client.GetImage(ctx, norm)
		switch {
		case err == nil:
			img = i
		case !errdefs.IsNotFound(err):
			return nil, unavailable("look up image", err)
		}
	}
	if img == nil {
		if pull == workspace.PullNever {
			return nil, refuse("image %q is not present locally and pull policy is never", norm)
		}
		i, err := e.client.Pull(ctx, norm, containerd.WithPullUnpack, containerd.WithPullSnapshotter(sn))
		if err != nil {
			return nil, unavailable("pull image "+norm, err)
		}
		img = i
	}
	ok, err := img.IsUnpacked(ctx, sn)
	if err != nil {
		return nil, unavailable("check image unpack", err)
	}
	if !ok {
		if err := img.Unpack(ctx, sn); err != nil {
			return nil, unavailable("unpack image", err)
		}
	}
	return img, nil
}

// Destroy implements workspace.Executor.
func (e *Executor) Destroy(ctx context.Context, id workspace.ID) error {
	if !workspace.ValidID(id) {
		return refuse("invalid workspace id")
	}
	if !e.lockID(id) {
		return fmt.Errorf("%w: workspace %s is being created or destroyed", workspace.ErrBusy, id)
	}
	defer e.unlockID(id)
	return e.teardown(namespaces.WithNamespace(ctx, e.ns), id)
}

// teardown removes the container (task killed first, snapshot cleaned) and only then the
// directories, so nothing can still be writing to them. Missing pieces are fine.
func (e *Executor) teardown(ctx context.Context, id workspace.ID) error {
	e.stopEgress(id)
	c, err := e.client.LoadContainer(ctx, ctrName(id))
	switch {
	case err == nil:
		if err := e.removeContainer(ctx, c); err != nil {
			return err
		}
	case errdefs.IsNotFound(err):
	default:
		return unavailable("look up container", err)
	}
	if err := e.removeDirs(id); err != nil {
		return unavailable("remove workspace directory", err)
	}
	return nil
}

func (e *Executor) removeContainer(ctx context.Context, c containerd.Container) error {
	t, err := c.Task(ctx, nil)
	switch {
	case err == nil:
		exit, werr := t.Wait(ctx)
		if kerr := t.Kill(ctx, syscall.SIGKILL, containerd.WithKillAll); kerr != nil && !errdefs.IsNotFound(kerr) {
			return unavailable("kill task", kerr)
		}
		if werr == nil {
			select {
			case <-exit:
			case <-time.After(killWait):
				return fmt.Errorf("%w: task did not exit within %s after SIGKILL", workspace.ErrUnavailable, killWait)
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if _, derr := t.Delete(ctx, containerd.WithProcessKill); derr != nil && !errdefs.IsNotFound(derr) {
			return unavailable("delete task", derr)
		}
	case errdefs.IsNotFound(err):
	default:
		return unavailable("load task", err)
	}
	if err := c.Delete(ctx, containerd.WithSnapshotCleanup); err != nil && !errdefs.IsNotFound(err) {
		return unavailable("delete container", err)
	}
	return nil
}

// removeDirs deletes <root>/<id> and <root>/.meta/<id>. Each must be a real directory directly
// under the root (never a symlink); os.RemoveAll does not follow symlinks inside it.
func (e *Executor) removeDirs(id workspace.ID) error {
	if !workspace.ValidID(id) {
		return errors.New("invalid id")
	}
	for _, p := range []string{
		filepath.Join(e.root, string(id)),
		filepath.Join(e.root, metaDirName, string(id)),
	} {
		fi, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
			return fmt.Errorf("%s is not a plain directory; refusing to remove it", p)
		}
		if rel, err := filepath.Rel(e.root, p); err != nil || strings.HasPrefix(rel, "..") {
			return fmt.Errorf("%s escapes the workspace root", p)
		}
		if err := os.RemoveAll(p); err != nil {
			return err
		}
	}
	return nil
}
