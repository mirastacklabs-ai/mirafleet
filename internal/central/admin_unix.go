//go:build unix

package central

import (
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
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

// Seam for tests: the peer-credential getter (atomic: accept loops of earlier servers may still
// be reading it). Production code never changes it.
var peerUIDFunc atomic.Pointer[func(*net.UnixConn) (uint32, error)]

func init() { f := peerUID; peerUIDFunc.Store(&f) }

// setPeerUIDFunc swaps the getter and returns the previous one (tests only).
func setPeerUIDFunc(f func(*net.UnixConn) (uint32, error)) (prev func(*net.UnixConn) (uint32, error)) {
	return *peerUIDFunc.Swap(&f)
}

// checkPeer requires the connecting process to run as the same UID as this process.
func checkPeer(c net.Conn) error {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return errors.New("not a unix connection")
	}
	uid, err := (*peerUIDFunc.Load())(uc)
	if err != nil {
		return fmt.Errorf("peer credentials: %w", err)
	}
	if int(uid) != os.Getuid() {
		return fmt.Errorf("peer uid %d != %d", uid, os.Getuid())
	}
	return nil
}
