//go:build windows

package keys

import (
	"errors"
	"os"
)

// checkPrivateKeyPerm fails closed: a DACL check lands in M5.
func checkPrivateKeyPerm(_ string, _ os.FileInfo) error {
	return errors.New("private key permissions cannot be verified on windows yet (M5)")
}
