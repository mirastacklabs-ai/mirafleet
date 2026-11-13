//go:build !unix

package central

import "net"

func listenUnix(path string) (net.Listener, error) { return net.Listen("unix", path) }

func checkPeer(net.Conn) error { return nil }
