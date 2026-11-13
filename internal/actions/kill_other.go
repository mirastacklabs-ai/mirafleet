//go:build !unix

package actions

import "os/exec"

// Fallback is unreachable in shipped runners: the runner is Linux-only.
func prepareKill(cmd *exec.Cmd) {}

func killTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
