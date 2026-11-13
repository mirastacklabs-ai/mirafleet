//go:build !unix

package sqlite

import "os"

// openNoFollow is the non-unix fallback: no O_NOFOLLOW, so Open's Lstat checks before and after
// the open (os.SameFile) are the only symlink defence.
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
}
