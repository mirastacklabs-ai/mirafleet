//go:build !linux

package egress

import (
	"errors"
	"net"
)

// ListenInNetns is Linux-only; elsewhere it fails closed.
func ListenInNetns(nsPath, addr string) (net.Listener, error) {
	return nil, errors.New("egress: network namespaces are only supported on Linux")
}
