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
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	// Go would unlink the path on Close, even when it is by then a newer server's socket.
	// ServeAdmin removes it itself, only if it still is this listener's file.
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	return ln, nil
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

// sockID identifies a socket file (device and inode) so cleanup removes only the file this
// server created, never a newer server's socket at the same path.
type sockID struct{ dev, ino uint64 }

func statSocket(path string) (sockID, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return sockID{}, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return sockID{}, errors.New("no inode information for " + path)
	}
	return sockID{dev: uint64(st.Dev), ino: uint64(st.Ino)}, nil //nolint:unconvert // width differs per OS
}

// removeSocketIfSame removes path only while it is still the socket identified by id. (The
// stat-then-remove window is not atomic; the caller holds the data-dir lock, so no other
// central is racing it, and an in-process successor binds only after the old file is gone.)
func removeSocketIfSame(path string, id sockID) {
	if cur, err := statSocket(path); err == nil && cur == id {
		os.Remove(path)
	}
}
