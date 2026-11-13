//go:build linux

package ctrd

import (
	"slices"
	"testing"

	"github.com/opencontainers/runtime-spec/specs-go"
)

func testParams(userns bool, slot uint32) specParams {
	p := specParams{
		ID: "mf-0123", UserNS: userns, CtrUID: 1000, CtrGID: 1000,
		CPUMilli: 2500, MemoryBytes: 512 << 20, Pids: 256, TmpfsBytes: 64 << 20,
		WorkspaceDir: "/var/lib/mirafleet/ws/0123", HostsFile: "/var/lib/mirafleet/ws/.meta/0123/hosts",
		ResolvFile: "/var/lib/mirafleet/ws/.meta/0123/resolv.conf", InitBinary: "/usr/local/bin/mirafleet",
		AppArmorProfile: "mirafleet-workspace",
	}
	if userns {
		p.HostBase = 1000000 + slot*userNSStride
	}
	return p
}

func mustSpec(t *testing.T, p specParams) *specs.Spec {
	t.Helper()
	s, err := buildSpec(p)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSpecHardening(t *testing.T) {
	for _, userns := range []bool{true, false} {
		s := mustSpec(t, testParams(userns, 0))
		pr := s.Process
		if !pr.NoNewPrivileges {
			t.Error("NoNewPrivileges not set")
		}
		c := pr.Capabilities
		if c == nil {
			t.Fatal("capabilities must be explicitly empty, not nil")
		}
		for name, set := range map[string][]string{"bounding": c.Bounding, "effective": c.Effective,
			"inheritable": c.Inheritable, "permitted": c.Permitted, "ambient": c.Ambient} {
			if len(set) != 0 {
				t.Errorf("capability set %s not empty: %v", name, set)
			}
		}
		if pr.Terminal || pr.Cwd != "/workspace" {
			t.Errorf("process terminal=%v cwd=%q", pr.Terminal, pr.Cwd)
		}
		if !slices.Equal(pr.Args, []string{"/.mirafleet/init", "workspace-init"}) {
			t.Errorf("args = %v", pr.Args)
		}
		if pr.ApparmorProfile != "mirafleet-workspace" {
			t.Errorf("apparmor = %q", pr.ApparmorProfile)
		}
		if len(pr.Rlimits) != 1 || pr.Rlimits[0].Type != "RLIMIT_NOFILE" || pr.Rlimits[0].Hard == 0 {
			t.Errorf("rlimits = %+v", pr.Rlimits)
		}
		if s.Root == nil || !s.Root.Readonly {
			t.Error("rootfs not read-only")
		}
		sc := s.Linux.Seccomp
		if sc == nil || sc.DefaultAction == specs.ActAllow || len(sc.Syscalls) == 0 {
			t.Errorf("seccomp missing or unconfined: %+v", sc)
		}
		if len(s.Linux.MaskedPaths) == 0 || len(s.Linux.ReadonlyPaths) == 0 {
			t.Error("masked/readonly paths missing")
		}
		if len(s.Linux.Devices) != 0 {
			t.Errorf("extra devices: %v", s.Linux.Devices)
		}
		if d := s.Linux.Resources.Devices; len(d) != 1 || d[0].Allow || d[0].Access != "rwm" {
			t.Errorf("device cgroup must deny all: %+v", d)
		}
	}
}

func TestSpecMounts(t *testing.T) {
	p := testParams(true, 0)
	s := mustSpec(t, p)
	// Runtime-provided pseudo filesystems only; none has a host source.
	pseudo := map[string]string{
		"/proc": "proc", "/dev": "tmpfs", "/dev/pts": "devpts", "/dev/shm": "tmpfs",
		"/dev/mqueue": "mqueue", "/sys": "sysfs", "/sys/fs/cgroup": "cgroup", "/run": "tmpfs",
	}
	hostBinds := map[string]string{}
	for _, m := range s.Mounts {
		if typ, ok := pseudo[m.Destination]; ok {
			if m.Type != typ {
				t.Errorf("%s has type %s, want %s", m.Destination, m.Type, typ)
			}
			if (m.Destination == "/sys" || m.Destination == "/sys/fs/cgroup") && !slices.Contains(m.Options, "ro") {
				t.Errorf("%s must be read-only: %v", m.Destination, m.Options)
			}
			delete(pseudo, m.Destination)
			continue
		}
		hostBinds[m.Destination] = m.Source
		opts := m.Options
		has := func(o string) bool { return slices.Contains(opts, o) }
		switch m.Destination {
		case "/workspace":
			if m.Source != p.WorkspaceDir || !has("rw") || !has("nosuid") || !has("nodev") || has("ro") {
				t.Errorf("/workspace mount wrong: %+v", m)
			}
		case "/tmp":
			if m.Type != "tmpfs" || !has("nosuid") || !has("nodev") || !has("size=67108864") {
				t.Errorf("/tmp mount wrong: %+v", m)
			}
		case "/etc/hosts", "/etc/resolv.conf":
			if !has("ro") || !has("nosuid") || !has("nodev") || !has("noexec") || !has("bind") {
				t.Errorf("%s mount wrong: %+v", m.Destination, m)
			}
		case "/.mirafleet/init":
			if m.Source != p.InitBinary || !has("ro") || !has("nosuid") || !has("nodev") || has("noexec") {
				t.Errorf("init mount wrong: %+v", m)
			}
		default:
			t.Errorf("unexpected mount %+v", m)
		}
	}
	if len(pseudo) != 0 {
		t.Errorf("missing runtime mounts: %v", pseudo)
	}
	want := map[string]string{
		"/workspace": p.WorkspaceDir, "/tmp": "tmpfs", "/etc/hosts": p.HostsFile,
		"/etc/resolv.conf": p.ResolvFile, "/.mirafleet/init": p.InitBinary,
	}
	if len(hostBinds) != len(want) {
		t.Errorf("mounts beyond the fixed set: %v", hostBinds)
	}
	for d, src := range want {
		if hostBinds[d] != src {
			t.Errorf("mount %s source = %q, want %q", d, hostBinds[d], src)
		}
	}
}

func TestSpecNamespaces(t *testing.T) {
	for _, userns := range []bool{true, false} {
		s := mustSpec(t, testParams(userns, 0))
		got := map[specs.LinuxNamespaceType]bool{}
		for _, n := range s.Linux.Namespaces {
			if n.Path != "" {
				t.Errorf("namespace %s joins host path %q", n.Type, n.Path)
			}
			got[n.Type] = true
		}
		want := []specs.LinuxNamespaceType{specs.PIDNamespace, specs.IPCNamespace, specs.UTSNamespace,
			specs.MountNamespace, specs.CgroupNamespace, specs.NetworkNamespace}
		if userns {
			want = append(want, specs.UserNamespace)
		}
		for _, w := range want {
			if !got[w] {
				t.Errorf("userns=%v: namespace %s missing", userns, w)
			}
		}
		if len(got) != len(want) {
			t.Errorf("userns=%v: namespaces %v", userns, s.Linux.Namespaces)
		}
		if !userns && (len(s.Linux.UIDMappings) != 0 || len(s.Linux.GIDMappings) != 0) {
			t.Error("id mappings without a user namespace")
		}
	}
}

func TestSpecUserNSMappingDistinctPerSlot(t *testing.T) {
	seen := map[uint32]bool{}
	for slot := uint32(0); slot < 8; slot++ {
		s := mustSpec(t, testParams(true, slot))
		if len(s.Linux.UIDMappings) != 1 || len(s.Linux.GIDMappings) != 1 {
			t.Fatalf("mappings: %+v %+v", s.Linux.UIDMappings, s.Linux.GIDMappings)
		}
		m := s.Linux.UIDMappings[0]
		if m != s.Linux.GIDMappings[0] {
			t.Errorf("uid/gid mappings differ: %+v %+v", m, s.Linux.GIDMappings[0])
		}
		if m.ContainerID != 0 || m.Size != userNSStride || m.HostID != 1000000+slot*userNSStride {
			t.Errorf("slot %d mapping = %+v", slot, m)
		}
		if seen[m.HostID] {
			t.Errorf("slot %d reuses host range %d", slot, m.HostID)
		}
		seen[m.HostID] = true
		if s.Process.User.UID != 1000 || s.Process.User.GID != 1000 {
			t.Errorf("process user = %+v", s.Process.User)
		}
	}
}

func TestSpecCgroupLimits(t *testing.T) {
	r := mustSpec(t, testParams(true, 0)).Linux.Resources
	if r.CPU == nil || *r.CPU.Quota != 250000 || *r.CPU.Period != 100000 {
		t.Errorf("cpu = %+v", r.CPU)
	}
	if r.Memory == nil || *r.Memory.Limit != 512<<20 || *r.Memory.Swap != 512<<20 {
		t.Errorf("memory/swap = %+v", r.Memory)
	}
	if r.Pids == nil || r.Pids.Limit == nil || *r.Pids.Limit != 256 {
		t.Errorf("pids = %+v", r.Pids)
	}
}

func TestSpecRejectsBadInput(t *testing.T) {
	p := testParams(true, 0)
	p.Pids = 0
	if _, err := buildSpec(p); err == nil {
		t.Error("zero pids accepted")
	}
	p = testParams(true, 0)
	p.InitBinary = "relative/mirafleet"
	if _, err := buildSpec(p); err == nil {
		t.Error("relative host path accepted")
	}
}
