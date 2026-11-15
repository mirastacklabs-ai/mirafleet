//go:build linux

package hostx

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
)

const prSetDumpable = 4 // PR_SET_DUMPABLE

// Harden applies the runner start-up rules of docs/M7.md section 3.2. Call it first thing in the
// runner, before any secret is loaded: PR_SET_DUMPABLE 0 (same-uid processes can no longer ptrace
// the runner or open /proc/<pid>/{mem,environ,fd,maps,root}), RLIMIT_CORE 0, then refuse uid 0, any
// effective capability, and any state dir that is not an own, non-group/world-writable directory.
// Children exec'd later are normal dumpable processes (execve resets the flag).
func Harden(stateDirs ...string) error {
	if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, prSetDumpable, 0, 0); e != 0 {
		return fmt.Errorf("hostx: prctl(PR_SET_DUMPABLE, 0): %w", e)
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{Cur: 0, Max: 0}); err != nil {
		return fmt.Errorf("hostx: setrlimit(RLIMIT_CORE, 0): %w", err)
	}
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return fmt.Errorf("hostx: read /proc/self/status: %w", err)
	}
	if err := CheckIdentity(os.Geteuid(), string(status)); err != nil {
		return err
	}
	for _, d := range stateDirs {
		if err := CheckDir(d, os.Geteuid()); err != nil {
			return err
		}
	}
	return nil
}

// CheckIdentity refuses uid 0 and any non-empty effective capability set (status is the text of
// /proc/self/status).
func CheckIdentity(uid int, status string) error {
	if uid == 0 {
		return errors.New("hostx: refusing to run as root (uid 0); use an ordinary user")
	}
	found := false
	for _, line := range strings.Split(status, "\n") {
		v, ok := strings.CutPrefix(line, "CapEff:")
		if !ok {
			continue
		}
		found = true
		if strings.Trim(strings.TrimSpace(v), "0") != "" {
			return errors.New("hostx: refusing to run with effective capabilities (CapEff=" + strings.TrimSpace(v) + ")")
		}
	}
	if !found {
		return errors.New("hostx: cannot read CapEff; refusing to run")
	}
	return nil
}

// CheckDir refuses a path that is not a directory owned by uid, or is group- or world-writable.
// A symlink is not followed.
func CheckDir(path string, uid int) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("hostx: state dir: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("hostx: %s is not a directory", path)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("hostx: %s is group- or world-writable (mode %04o); chmod go-w", path, fi.Mode().Perm())
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != uid {
		return fmt.Errorf("hostx: %s is owned by uid %d, not %d", path, st.Uid, uid)
	}
	return nil
}
