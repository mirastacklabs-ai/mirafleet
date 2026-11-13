//go:build unix

package sqlite

import (
	"os"
	"syscall"
)

// openNoFollow opens (creating 0600 if absent) path read-write, refusing a symlink as the final
// path element atomically (O_NOFOLLOW).
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
}
