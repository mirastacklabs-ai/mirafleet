//go:build linux && integration

package gitlab

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os/exec"
	"path"
	"strings"

	"golang.org/x/crypto/ssh"
)

func (s *Server) serveSSH(l net.Listener) {
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(m ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			good := m.User() == "git" && bytes.Equal(key.Marshal(), s.DeployKey.Public.Marshal())
			s.record(Event{Transport: "ssh", Identity: m.User(), Credential: ssh.FingerprintSHA256(key), OK: good})
			if !good {
				return nil, fmt.Errorf("unauthorized key")
			}
			return nil, nil
		},
	}
	cfg.AddHostKey(s.hostKey)
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go s.sshConn(c, cfg)
	}
}

func (s *Server) sshConn(c net.Conn, cfg *ssh.ServerConfig) {
	defer c.Close()
	conn, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	defer conn.Close()
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			nc.Reject(ssh.UnknownChannelType, "session only")
			continue
		}
		ch, creqs, err := nc.Accept()
		if err != nil {
			continue
		}
		go s.sshSession(ch, creqs)
	}
}

// parseGitCommand accepts only `git-upload-pack 'p'` / `git-receive-pack 'p'` and returns the
// git subcommand and the repository directory under Root.
func (s *Server) parseGitCommand(cmd string) (sub, dir string, ok bool) {
	verb, arg, found := strings.Cut(cmd, " ")
	if !found || (verb != "git-upload-pack" && verb != "git-receive-pack") {
		return "", "", false
	}
	arg = strings.Trim(strings.TrimSpace(arg), `'"`)
	if strings.Contains(arg, "..") {
		return "", "", false
	}
	arg = strings.TrimPrefix(path.Clean("/"+arg), "/")
	if arg == "" {
		return "", "", false
	}
	return strings.TrimPrefix(verb, "git-"), s.RepoPath(arg), true
}

func (s *Server) sshSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	for req := range reqs {
		if req.Type != "exec" {
			req.Reply(false, nil)
			continue
		}
		var p struct{ Command string }
		if ssh.Unmarshal(req.Payload, &p) != nil {
			req.Reply(false, nil)
			continue
		}
		sub, dir, ok := s.parseGitCommand(p.Command)
		if !ok {
			req.Reply(false, nil)
			fmt.Fprintf(ch.Stderr(), "fatal: unsupported command\n")
			return
		}
		req.Reply(true, nil)
		cmd := exec.Command("git", sub, dir)
		cmd.Stdout = ch
		cmd.Stderr = ch.Stderr()
		in, err := cmd.StdinPipe()
		if err != nil {
			return
		}
		go func() { io.Copy(in, ch); in.Close() }()
		status := uint32(0)
		if err := cmd.Run(); err != nil {
			status = 1
			if ee, ok := err.(*exec.ExitError); ok {
				status = uint32(ee.ExitCode())
			}
		}
		ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
		return
	}
}
