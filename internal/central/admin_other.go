//go:build !unix

package central

import (
	"errors"
	"net"
)

// errAdminUnsupported is returned wherever the admin socket would be used on a non-unix OS.
// Central is Linux-only, so this is defense in depth: the socket intentionally fails closed
// (no peer-credential check or owner-only permission model exists here) and no non-unix
// admin transport is planned.
var errAdminUnsupported = errors.New("admin socket is unix-only; mirafleet central runs on linux")

func listenUnix(string) (net.Listener, error) { return nil, errAdminUnsupported }

func checkPeer(net.Conn) error { return errAdminUnsupported }

func peerClosed(*net.UnixConn) bool { return false }

type sockID struct{}

func statSocket(string) (sockID, error) { return sockID{}, errAdminUnsupported }

func removeSocketIfSame(string, sockID) {}
