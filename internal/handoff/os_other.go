//go:build !unix

package handoff

import "os"

const openFlags = os.O_RDONLY

// ownedByUs: no owner concept in os.FileInfo here; Windows relies on the owner-only DACL of the
// state directory (internal/keys).
func ownedByUs(os.FileInfo) bool { return true }
