//go:build !unix

package fsops

// openFlags: no non-blocking flag; the regular-file checks before and after the open still apply.
const openFlags = 0
