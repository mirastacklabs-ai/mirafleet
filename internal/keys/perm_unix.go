//go:build unix

package keys

import (
	"fmt"
	"os"
)

// checkPrivateKeyPerm refuses keys that are accessible to group or others (any 0o077 mode bit).
func checkPrivateKeyPerm(path string, st os.FileInfo) error {
	if st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("private key %s has mode %04o; it must not be accessible to group or others (chmod 600)", path, st.Mode().Perm())
	}
	return nil
}
