//go:build !unix

package keys

// SyncDir is a no-op where directories cannot be fsynced (Windows has no directory fsync; NTFS
// journals the rename).
func SyncDir(string) error { return nil }
