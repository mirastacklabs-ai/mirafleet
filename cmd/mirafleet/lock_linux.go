package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockDataDir takes an exclusive, non-blocking flock on <dir>/central.lock (docs/M2.md §1.8,
// Q13). A second central on the same directory fails instead of sharing the database and admin
// socket. The lock lives as long as the returned release func has not run (or the process
// lives); it is also what makes stale admin-socket removal safe (central.Server.ServeAdmin).
// dir must exist; call after keys.EnsureDir and umask 077.
func lockDataDir(dir string) (release func(), err error) {
	path := filepath.Join(dir, "central.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another central is already using %s (lock %s is held)", dir, path)
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
