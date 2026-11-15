package sshagent

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/crypto/ssh/agent"
)

var randReader io.Reader = rand.Reader

// SocketName is the agent socket's file name inside the session directory.
const SocketName = "agent.sock"

// Server serves the restricted agent on a Unix socket.
type Server struct {
	path string
	ln   net.Listener
	p    Provider
	uid  int

	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
	wg     sync.WaitGroup
}

// Listen creates dir (0700) and serves <dir>/agent.sock (0600). Only connections from
// allowedUID are served; pass os.Getuid() for "the user running this process". Peer
// credentials are checked with SO_PEERCRED, so on platforms without it every connection
// is refused.
func Listen(dir string, p Provider, allowedUID int) (*Server, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() || fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("sshagent: %s must be a directory with mode 0700", dir)
	}
	path := filepath.Join(dir, SocketName)
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	s := &Server{path: path, ln: ln, p: p, uid: allowedUID, conns: map[net.Conn]struct{}{}}
	s.wg.Add(1)
	go s.accept()
	return s, nil
}

// Path is the socket path, for SSH_AUTH_SOCK.
func (s *Server) Path() string { return s.path }

func (s *Server) accept() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			c.Close()
			return
		}
		s.conns[c] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()
		go s.serve(c)
	}
}

func (s *Server) serve(c net.Conn) {
	defer s.wg.Done()
	defer func() {
		c.Close()
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
	}()
	uid, err := peerUID(c)
	if err != nil || uid != s.uid {
		return
	}
	_ = agent.ServeAgent(newConnAgent(s.p), c)
}

// Close stops the server, drops all connections and removes the socket.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	err := s.ln.Close()
	for c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
	if rerr := os.Remove(s.path); rerr != nil && !errors.Is(rerr, os.ErrNotExist) && err == nil {
		err = rerr
	}
	return err
}
