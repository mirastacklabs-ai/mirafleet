//go:build !linux

package sshagent

import (
	"errors"
	"net"
)

// peerUID fails closed: without SO_PEERCRED no peer is trusted. The agent runs on the
// Linux runner only.
func peerUID(net.Conn) (int, error) {
	return 0, errors.New("sshagent: peer credentials are not supported on this platform")
}
