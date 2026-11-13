//go:build unix && !linux && !darwin

package central

import (
	"errors"
	"net"
)

// Peer credentials are only implemented for linux and darwin; fail closed elsewhere.
func peerUID(*net.UnixConn) (uint32, error) {
	return 0, errors.New("peer credential check unsupported on this OS")
}
