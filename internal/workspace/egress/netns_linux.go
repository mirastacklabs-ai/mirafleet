//go:build linux

package egress

import (
	"errors"
	"net"
)

// ListenInNetns opens a TCP listener on addr (e.g. "127.0.0.1:3128") inside the network
// namespace at nsPath (e.g. /proc/<pid>/ns/net), then returns to the caller's namespace.
// S14 implements this with LockOSThread + unix.Setns.
func ListenInNetns(nsPath, addr string) (net.Listener, error) {
	return nil, errors.New("egress: ListenInNetns not implemented (S14)")
}
