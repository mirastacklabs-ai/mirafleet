//go:build linux

package hostx

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestCheckIdentity(t *testing.T) {
	zero := "Name:\tx\nCapEff:\t0000000000000000\n"
	if err := CheckIdentity(1000, zero); err != nil {
		t.Fatal(err)
	}
	if CheckIdentity(0, zero) == nil {
		t.Error("uid 0 accepted")
	}
	if CheckIdentity(1000, "CapEff:\t0000000000000400\n") == nil {
		t.Error("capability accepted")
	}
	if CheckIdentity(1000, "Name:\tx\n") == nil {
		t.Error("missing CapEff accepted")
	}
}

func TestCheckDir(t *testing.T) {
	uid := os.Geteuid()
	base := t.TempDir()
	mk := func(name string, mode os.FileMode) string {
		p := filepath.Join(base, name)
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if err := CheckDir(mk("ok", 0o700), uid); err != nil {
		t.Errorf("0700: %v", err)
	}
	if err := CheckDir(mk("ok2", 0o755), uid); err != nil {
		t.Errorf("0755: %v", err)
	}
	for _, m := range []os.FileMode{0o770, 0o707, 0o777, 0o720} {
		if CheckDir(mk("bad"+m.String(), m), uid) == nil {
			t.Errorf("mode %v accepted", m)
		}
	}
	if CheckDir(mk("other", 0o700), uid+1) == nil {
		t.Error("foreign owner accepted")
	}
	if CheckDir(filepath.Join(base, "missing"), uid) == nil {
		t.Error("missing accepted")
	}
	f := filepath.Join(base, "file")
	_ = os.WriteFile(f, nil, 0o600)
	if CheckDir(f, uid) == nil {
		t.Error("file accepted")
	}
	ln := filepath.Join(base, "ln")
	_ = os.Symlink(filepath.Join(base, "ok"), ln)
	if CheckDir(ln, uid) == nil {
		t.Error("symlink accepted")
	}
}

func TestHardenSetsNotDumpable(t *testing.T) {
	st, _ := os.ReadFile("/proc/self/status")
	if os.Geteuid() == 0 || CheckIdentity(os.Geteuid(), string(st)) != nil {
		t.Skip("test process is root or holds capabilities; Harden correctly refuses (see TestCheckIdentity)")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Harden(dir); err != nil {
		t.Fatal(err)
	}
	d, _, _ := syscall.RawSyscall(syscall.SYS_PRCTL, 3, 0, 0) // PR_GET_DUMPABLE
	if d != 0 {
		t.Errorf("dumpable = %d", d)
	}
	var rl syscall.Rlimit
	_ = syscall.Getrlimit(syscall.RLIMIT_CORE, &rl)
	if rl.Cur != 0 || rl.Max != 0 {
		t.Errorf("RLIMIT_CORE = %+v", rl)
	}
	loose := t.TempDir()
	_ = os.Chmod(loose, 0o777)
	if Harden(loose) == nil {
		t.Error("loose dir accepted")
	}
}
