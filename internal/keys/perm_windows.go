//go:build windows

package keys

import "os"

// checkPrivateKeyPerm verifies the key file's owner and DACL (see acl_windows.go).
func checkPrivateKeyPerm(path string, _ os.FileInfo) error {
	return checkPathACL(path)
}
