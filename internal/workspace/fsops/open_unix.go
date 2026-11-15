//go:build unix

package fsops

import "syscall"

// openFlags keeps an open of a FIFO swapped in after the regular-file check from blocking.
const openFlags = syscall.O_NONBLOCK
