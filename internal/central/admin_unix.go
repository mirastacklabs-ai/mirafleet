//go:build unix

package central

import (
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"syscall"
)

var umaskMu sync.Mutex

// listenUnix binds the socket under umask 077 so it is never created with wider permissions.
// The umask is process-wide; the lock only serialises our own callers.
func listenUnix(path string) (net.Listener, error) {
	umaskMu.Lock()
	old := syscall.Umask(0o077)
	defer func() { syscall.Umask(old); umaskMu.Unlock() }()
	return net.Listen("unix", path)
}

// checkPeer requires the connecting process to run as the same UID as this process.
func checkPeer(c net.Conn) error {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return errors.New("not a unix connection")
	}
	uid, err := peerUID(uc)
	if err != nil {
		return fmt.Errorf("peer credentials: %w", err)
	}
	if int(uid) != os.Getuid() {
		return fmt.Errorf("peer uid %d != %d", uid, os.Getuid())
	}
	return nil
}
