//go:build !unix && !windows

package opconf

import "os"

// lockFile has no locking primitive here; the lock is skipped (writes stay atomic).
func lockFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
}
