//go:build !unix

package opconf

import "os"

// openNoFollow: no O_NOFOLLOW here; callers Lstat first and compare with os.SameFile after
// opening, which detects (not prevents) a swap.
func openNoFollow(path string) (*os.File, error) { return os.Open(path) }
