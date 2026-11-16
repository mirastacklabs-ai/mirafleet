//go:build unix

package opconf

import (
	"os"
	"syscall"
)

// openNoFollow opens path read-only without following a final symlink (ELOOP) and without
// blocking on a FIFO.
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}
