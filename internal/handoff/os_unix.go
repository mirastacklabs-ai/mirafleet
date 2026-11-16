//go:build unix

package handoff

import (
	"os"
	"syscall"
)

// openFlags for reading a handoff: O_NONBLOCK so opening a FIFO planted in the directory cannot
// block.
const openFlags = os.O_RDONLY | syscall.O_NONBLOCK

// ownedByUs reports whether fi is owned by the current user. A root operator must not
// chmod or trust a directory another user owns.
func ownedByUs(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}
