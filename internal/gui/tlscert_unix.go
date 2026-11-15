//go:build unix

package gui

import (
	"fmt"
	"os"
	"syscall"
)

// checkKeyFile refuses an operator key that group or others can access, or that another user owns.
func checkKeyFile(path string, st os.FileInfo) error {
	if st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("gui: -gui-key %s has mode %04o; it must not be accessible to group or others (chmod 600)", path, st.Mode().Perm())
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && int(sys.Uid) != os.Geteuid() {
		return fmt.Errorf("gui: -gui-key %s is owned by uid %d, not by central's uid %d", path, sys.Uid, os.Geteuid())
	}
	return nil
}

// ownedByUs reports whether st belongs to this process's effective user.
func ownedByUs(st os.FileInfo) bool {
	sys, ok := st.Sys().(*syscall.Stat_t)
	return ok && int(sys.Uid) == os.Geteuid()
}
