//go:build !unix && !windows

package keys

import (
	"errors"
	"os"
)

// checkPrivateKeyPerm fails closed on platforms with no permission check.
func checkPrivateKeyPerm(_ string, _ os.FileInfo) error {
	return errors.New("private key permissions cannot be verified on this platform")
}
