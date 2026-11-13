package central

// Local admin socket: `mirafleet central` listens on a unix socket in its data dir (admin.sock)
// and speaks the internal/api framing: one framed request, one framed response per connection
// (docs/M2.md §1.5). Access control (unix only; central is linux-only): socket mode 0600,
// created under umask 077, plus a peer-UID check on every connection BEFORE anything is read.
// The 0700 data dir is enforced by keys.EnsureDir in the server wiring, not here. On non-unix
// OSes admin_other.go fails closed (listen errors, peers are rejected); on unix OSes other than
// linux/darwin admin_peer_other.go rejects every peer.
//
// The principal is auth.LocalAdmin(uid) with source cli; every call goes through the service
// (policy, validation, audit). The request size limit is enforced by api.ReadRequestTimeout
// before any decoding.

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"syscall"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
)

// AdminActionTimeoutS is the action.run timeout the CLI asks for (the service caps it).
const AdminActionTimeoutS = 600

// adminTable is the admin-socket table: every admin method plus the operator methods useful to
// an admin. Enroll methods and hello are never served here.
func adminTable() *methodTable {
	return &methodTable{
		src: service.SourceCLI,
		allowed: func(m string) bool {
			switch m {
			case api.MethodFleetsList, api.MethodRunnersList, api.MethodActionRun:
				return true
			}
			return api.IsAdminMethod(m)
		},
		handlers: map[string]handler{
			api.MethodFleetsList:           handleFleetsList,
			api.MethodRunnersList:          handleRunnersList,
			api.MethodActionRun:            handleActionRun,
			api.MethodFleetCreate:          handleFleetCreate,
			api.MethodTokenCreate:          handleTokenCreate,
			api.MethodTokenList:            handleTokenList,
			api.MethodTokenRevoke:          handleTokenRevoke,
			api.MethodRunnerLifecycle:      handleRunnerLifecycle,
			api.MethodOperatorCode:         handleOperatorCode,
			api.MethodOperatorRotateSecret: handleOperatorRotateSecret,
			api.MethodOperatorList:         handleOperatorList,
			api.MethodOperatorRevoke:       handleOperatorRevoke,
			api.MethodAuditList:            handleAuditList,
		},
		timeout: func(m string) time.Duration {
			if m == api.MethodActionRun {
				return 0
			}
			return api.ListTimeout
		},
	}
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
	table := adminTable()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Peer check first: nothing is read from a foreign peer.
			if err := checkPeer(c); err != nil {
				s.cfg.Logf("admin socket: rejected connection: %v", err)
				c.Close()
				continue
			}
			s.wg.Add(1) // Server.Close waits for in-flight admin calls
			go func() { defer s.wg.Done(); s.adminConn(ctx, c, table) }()
		}
	}()
	return nil
}

func (s *Server) adminConn(ctx context.Context, c net.Conn, table *methodTable) {
	defer c.Close()
	req, id, err := api.ReadRequestTimeout(c, c)
	if err != nil {
		if !errors.Is(err, io.EOF) {
			api.WriteResponse(c, api.NewError(id, api.AsError(err)))
		}
		return
	}
	// The peer closing the connection cancels the request (e.g. Ctrl-C of `central exec`).
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { io.Copy(io.Discard, c); cancel() }()

	// checkPeer required the peer to run as our own UID.
	cc := &callCtx{p: auth.LocalAdmin(os.Getuid()), src: table.src, svc: s.cfg.Service, st: s.cfg.Store,
		logf: s.cfg.Logf, state: &connState{}}
	if err := api.WriteResponse(c, table.serve(rctx, cc, req)); err != nil {
		s.cfg.Logf("admin socket: write response: %v", err)
	}
}
