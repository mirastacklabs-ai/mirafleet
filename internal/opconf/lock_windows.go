//go:build windows

package opconf

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockFile takes an exclusive lock on the first byte of path, blocking until it is held. Closing
// the returned file releases it. The file inherits the config dir's protected DACL.
func lockFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	ol := new(windows.Overlapped)
	if err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, ol); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
