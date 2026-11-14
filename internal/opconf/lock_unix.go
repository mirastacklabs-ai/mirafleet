//go:build unix

package opconf

import (
	"os"
	"syscall"
)

// lockFile takes an exclusive advisory lock on path (created 0600), blocking until it is held.
// Closing the returned file releases it. Locks on separate opens conflict, even in one process.
func lockFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
