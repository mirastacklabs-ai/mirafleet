package central

// Local admin mechanism (M1 framing, kept until S9 re-bases it on internal/api): `mirafleet
// central` listens on a unix socket in its data dir (admin.sock). `mirafleet central exec`
// connects, writes ONE JSON AdminRequest line and reads ONE JSON AdminResponse line. Access
// control (unix only; central is linux-only): socket mode 0600, created under umask 077, plus a
// peer-UID check on every connection. The 0700 data dir is enforced by keys.EnsureDir in the
// server wiring, not here. On non-unix OSes admin_other.go fails closed (listen errors, peers are
// rejected); on unix OSes other than linux/darwin admin_peer_other.go rejects every peer.
//
// Since S7a the exec goes through service.RunAction as the local admin principal (policy, fleet
// mode, confirm, start/outcome audit rows in the store), so the runner is addressed as
// "fleet/runner".

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"syscall"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
)

// adminTimeoutS is the action.run timeout the admin socket asks for (the service caps it).
const adminTimeoutS = 600

type AdminRequest struct {
	Runner  string            `json:"runner"` // fleet/runner
	Action  string            `json:"action"`
	Params  map[string]string `json:"params,omitempty"`
	Confirm string            `json:"confirm,omitempty"` // must equal the fleet name on guarded fleets
}

type AdminResponse struct {
	Result *api.ActionRunResult `json:"result,omitempty"`
	Error  string               `json:"error,omitempty"`
}

// ServeAdmin serves the admin socket until ctx is done.
func (s *Server) ServeAdmin(ctx context.Context, path string) error {
	ln, err := listenUnix(path) // created under umask 077: never briefly world-accessible
	if errors.Is(err, syscall.EADDRINUSE) {
		// Stale socket from a previous run. Removed only after a real bind attempt, so the
		// fail-closed (non-unix) path never deletes anything.
		os.Remove(path)
		ln, err = listenUnix(path)
	}
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return err
	}
	go func() { <-ctx.Done(); ln.Close(); os.Remove(path) }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			if err := checkPeer(c); err != nil {
				s.cfg.Logf("admin socket: rejected connection: %v", err)
				c.Close()
				continue
			}
			go s.adminConn(ctx, c)
		}
	}()
	return nil
}

func (s *Server) adminConn(ctx context.Context, c net.Conn) {
	defer c.Close()
	var req AdminRequest
	line, err := bufio.NewReader(c).ReadBytes('\n')
	var resp AdminResponse
	if err == nil {
		err = json.Unmarshal(line, &req)
	}
	if err != nil {
		resp.Error = "bad request: " + err.Error()
	} else if ref, rerr := store.ParseRef(req.Runner); rerr != nil {
		resp.Error = "bad request: " + rerr.Error()
	} else {
		xctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		// checkPeer required the peer to run as our own UID.
		res, err := s.cfg.Service.RunAction(xctx, auth.LocalAdmin(os.Getuid()), service.SourceCLI,
			ref.Fleet, ref.Runner, req.Action, req.Params, adminTimeoutS, req.Confirm)
		if err != nil {
			resp.Error = err.Error()
		} else {
			resp.Result = &res
		}
	}
	json.NewEncoder(c).Encode(resp)
}

// AdminExec is the client side used by `mirafleet central exec`.
func AdminExec(sock string, req AdminRequest) (*api.ActionRunResult, error) {
	c, err := net.DialTimeout("unix", sock, 5*time.Second)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if err := json.NewEncoder(c).Encode(req); err != nil {
		return nil, err
	}
	var resp AdminResponse
	if err := json.NewDecoder(c).Decode(&resp); err != nil {
		return nil, err
	}
	if resp.Error != "" {
		return resp.Result, errors.New(resp.Error)
	}
	return resp.Result, nil
}
