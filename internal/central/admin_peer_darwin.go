//go:build darwin

package central

import (
	"net"

	"golang.org/x/sys/unix"
)

func peerUID(c *net.UnixConn) (uint32, error) {
	rc, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var uid uint32
	var serr error
	if err := rc.Control(func(fd uintptr) {
		var cred *unix.Xucred
		cred, serr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if serr == nil {
			uid = cred.Uid
		}
	}); err != nil {
		return 0, err
	}
	return uid, serr
}

// peerClosed reports whether the peer has fully closed c (POLLHUP). A peer that only shut down
// its write side (half-close) does not raise POLLHUP, so it is not reported.
func peerClosed(c *net.UnixConn) bool {
	rc, err := c.SyscallConn()
	if err != nil {
		return true
	}
	closed := false
	if err := rc.Control(func(fd uintptr) {
		fds := []unix.PollFd{{Fd: int32(fd), Events: 0}}
		n, perr := unix.Poll(fds, 0)
		closed = perr == nil && n > 0 && fds[0].Revents&(unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0
	}); err != nil {
		return true
	}
	return closed
}
