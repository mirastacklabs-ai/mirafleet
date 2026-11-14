//go:build unix

package keys

import "os"

// SyncDir fsyncs a directory so a preceding rename or create in it is durable.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return err
	}
	return d.Close()
}
