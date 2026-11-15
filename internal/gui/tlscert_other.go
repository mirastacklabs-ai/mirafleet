//go:build !unix

package gui

import (
	"fmt"
	"os"
)

// checkKeyFile fails closed: central runs on Linux only, and an ownership check cannot be made here.
func checkKeyFile(path string, _ os.FileInfo) error {
	return fmt.Errorf("gui: -gui-key %s cannot be checked on this platform", path)
}

// ownedByUs fails closed: ownership cannot be checked here.
func ownedByUs(os.FileInfo) bool { return false }
