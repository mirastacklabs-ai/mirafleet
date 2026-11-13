//go:build linux

package ctrd

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/containerd/containerd/v2/contrib/seccomp"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/opencontainers/runtime-spec/specs-go"
)

// Fixed layout of every workspace container.
const (
	workspaceMount = "/workspace"
	tmpMount       = "/tmp"
	initMount      = "/.mirafleet/init"
	hostsMount     = "/etc/hosts"
	resolvMount    = "/etc/resolv.conf"

	// userNSStride is the size of one slot's host uid/gid range with user namespaces.
	userNSStride = 65536
	// ctrUser is the uid/gid workspaces run as inside a user namespace.
	ctrUser = 1000

	cpuPeriod = 100000
	noFile    = 4096
)

// specParams is everything the OCI spec depends on. The runner builds the spec from a fixed
// template; central never influences anything beyond these (already validated) numbers.
type specParams struct {
	ID       string
	UserNS   bool
	CtrUID   uint32 // uid/gid of the init process inside the container
	CtrGID   uint32
	HostBase uint32 // start of this slot's host id range (user namespace only)

	CPUMilli    int64
	MemoryBytes int64
	Pids        int64
	TmpfsBytes  int64

	WorkspaceDir string // host paths of the only host mounts
	HostsFile    string
	ResolvFile   string
	InitBinary   string

	AppArmorProfile string // "" = none
}

// buildSpec returns the hardened OCI spec. It starts from containerd's default spec (default
// /proc, /dev, /sys mounts, masked and readonly paths) and then pins every security-relevant
// field explicitly; spec_linux_test.go asserts the result.
func buildSpec(p specParams) (*specs.Spec, error) {
	if p.CPUMilli <= 0 || p.MemoryBytes <= 0 || p.Pids <= 0 || p.TmpfsBytes <= 0 {
		return nil, errors.New("ctrd: spec needs positive cpu, memory, pids and tmpfs limits")
	}
	for _, f := range []string{p.WorkspaceDir, p.HostsFile, p.ResolvFile, p.InitBinary} {
		if !filepath.IsAbs(f) {
			return nil, fmt.Errorf("ctrd: host path %q must be absolute", f)
		}
	}
	// The namespace only feeds the default cgroup path, which is dropped below.
	ctx := namespaces.WithNamespace(context.Background(), "mirafleet")
	def, err := oci.GenerateSpec(ctx, nil, &containers.Container{ID: p.ID})
	if err != nil {
		return nil, err
	}
	s := *def

	s.Hostname = "workspace"
	s.Root = &specs.Root{Path: "rootfs", Readonly: true}
	s.Process = &specs.Process{
		Terminal:        false,
		User:            specs.User{UID: p.CtrUID, GID: p.CtrGID},
		Args:            []string{initMount, "workspace-init"},
		Env:             []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + workspaceMount},
		Cwd:             workspaceMount,
		NoNewPrivileges: true,
		// Empty, non-nil sets: every capability dropped from all five sets.
		Capabilities: &specs.LinuxCapabilities{
			Bounding: []string{}, Effective: []string{}, Inheritable: []string{},
			Permitted: []string{}, Ambient: []string{},
		},
		Rlimits:         []specs.POSIXRlimit{{Type: "RLIMIT_NOFILE", Hard: noFile, Soft: noFile}},
		ApparmorProfile: p.AppArmorProfile,
	}

	// Default runtime mounts (proc, dev, devpts, shm, mqueue, sysfs, cgroup) stay; add ours.
	ro := []string{"bind", "ro", "nosuid", "nodev", "noexec"}
	s.Mounts = append(s.Mounts,
		// Read-only view of the container's own cgroup (the cgroup namespace scopes it).
		specs.Mount{Destination: "/sys/fs/cgroup", Type: "cgroup", Source: "cgroup",
			Options: []string{"nosuid", "noexec", "nodev", "relatime", "ro"}},
		specs.Mount{Destination: workspaceMount, Type: "bind", Source: p.WorkspaceDir,
			Options: []string{"bind", "rw", "nosuid", "nodev"}},
		specs.Mount{Destination: tmpMount, Type: "tmpfs", Source: "tmpfs",
			Options: []string{"nosuid", "nodev", "mode=1777", fmt.Sprintf("size=%d", p.TmpfsBytes)}},
		specs.Mount{Destination: hostsMount, Type: "bind", Source: p.HostsFile, Options: ro},
		specs.Mount{Destination: resolvMount, Type: "bind", Source: p.ResolvFile, Options: ro},
		// The mirafleet binary, so PID 1 works in any image (distroless included). Executable.
		specs.Mount{Destination: initMount, Type: "bind", Source: p.InitBinary,
			Options: []string{"bind", "ro", "nosuid", "nodev"}},
	)

	ns := []specs.LinuxNamespace{
		{Type: specs.PIDNamespace}, {Type: specs.IPCNamespace}, {Type: specs.UTSNamespace},
		{Type: specs.MountNamespace}, {Type: specs.CgroupNamespace},
		{Type: specs.NetworkNamespace}, // new, empty: loopback only
	}
	l := &specs.Linux{
		Namespaces:    ns,
		MaskedPaths:   s.Linux.MaskedPaths,
		ReadonlyPaths: s.Linux.ReadonlyPaths,
		// Deny every device; runc re-adds the standard set (null, zero, full, random, ...).
		Resources: &specs.LinuxResources{
			Devices: []specs.LinuxDeviceCgroup{{Allow: false, Access: "rwm"}},
			CPU:     &specs.LinuxCPU{Quota: i64(p.CPUMilli * cpuPeriod / 1000), Period: u64(cpuPeriod)},
			Memory:  &specs.LinuxMemory{Limit: i64(p.MemoryBytes), Swap: i64(p.MemoryBytes)}, // swap == memory: no swap
			Pids:    &specs.LinuxPids{Limit: &p.Pids},
		},
		RootfsPropagation: "private",
	}
	if p.UserNS {
		l.Namespaces = append(l.Namespaces, specs.LinuxNamespace{Type: specs.UserNamespace})
		m := []specs.LinuxIDMapping{{ContainerID: 0, HostID: p.HostBase, Size: userNSStride}}
		l.UIDMappings, l.GIDMappings = m, m
	}
	s.Linux = l

	// After the capabilities are final (the profile gates syscalls on them).
	s.Linux.Seccomp = seccomp.DefaultProfile(&s)
	if s.Linux.Seccomp == nil || s.Linux.Seccomp.DefaultAction == specs.ActAllow {
		return nil, errors.New("ctrd: default seccomp profile unavailable")
	}
	return &s, nil
}

func i64(v int64) *int64   { return &v }
func u64(v uint64) *uint64 { return &v }
