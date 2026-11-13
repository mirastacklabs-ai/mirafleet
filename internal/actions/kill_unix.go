//go:build unix

package actions

import (
	"os/exec"
	"syscall"
)

// prepareKill puts the child in its own process group so the whole group can be killed.
func prepareKill(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killTree SIGKILLs the child's process group (pgid == pid because of Setpgid). It is safe to
// call after Wait: the group id stays reserved while any member (e.g. a grandchild) lives.
func killTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
