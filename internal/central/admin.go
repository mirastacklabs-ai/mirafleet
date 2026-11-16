package central

// Local admin socket: `mirafleet central` listens on a unix socket in its data dir (admin.sock)
// and speaks the internal/api framing: one framed request, one framed response per connection.
// Access control (unix only; central is linux-only): socket mode 0600,
// created under umask 077, plus a peer-UID check on every connection BEFORE anything is read.
// The 0700 data dir is enforced by keys.EnsureDir (called from prepareDataDir in cmd/mirafleet/central_linux.go), not here. On non-unix
// OSes admin_other.go fails closed (listen errors, peers are rejected); on unix OSes other than
// linux/darwin admin_peer_other.go rejects every peer.
//
// Disconnect-cancel (a client going away cancels its in-flight request, e.g. Ctrl-C of
// `central exec`) is linux-only: it needs POLLHUP from a zero-event poll, which is reliable on
// Linux but not guaranteed on macOS. There (and on other unix OSes) peerClosed always returns
// false, so a vanished client does not cancel its request; the request is still bounded by its
// own timeout (an action's own deadline, or api.ListTimeout). Only an outright read error
// (connection reset) cancels everywhere.
//
// The principal is auth.LocalAdmin(uid) with source cli; every call goes through the service
// (policy, validation, audit). The request size limit is enforced by api.ReadRequestTimeout
// before any decoding.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/adminclient"
	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
)

// AdminActionTimeoutS is the action.run timeout the CLI asks for (the service caps it).
const AdminActionTimeoutS = adminclient.ActionTimeoutS

// AdminCall is the client side of the admin socket; see adminclient.Call.
func AdminCall(socketPath, method string, params, result any) error {
	return adminclient.Call(socketPath, method, params, result)
}

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
			api.MethodFleetWorkspaceSet:    handleFleetWorkspaceSet,
			api.MethodWorkspaceAdminList:   handleWorkspaceAdminList,
			api.MethodWorkspaceKill:        handleWorkspaceKill,
			api.MethodGUIResetAdmin:        handleGUIResetAdmin,
			api.MethodFleetSetConfirm:      handleFleetSetConfirm,

			api.MethodCredentialSet:      handleCredentialSet,
			api.MethodCredentialList:     handleCredentialList,
			api.MethodCredentialDelete:   handleCredentialDelete,
			api.MethodCredentialAssign:   handleCredentialAssign,
			api.MethodCredentialUnassign: handleCredentialUnassign,
			api.MethodCredentialRotate:   handleCredentialRotate,
		},
		timeout: func(m string) time.Duration {
			switch m {
			case api.MethodActionRun:
				return 0
			case api.MethodWorkspaceKill:
				return wsCloseOuterTimeout
			case api.MethodCredentialRotate:
				return 2 * time.Minute
			}
			return api.ListTimeout
		},
	}
}

// ServeAdmin serves the admin socket until ctx is done.
//
// The caller MUST hold the exclusive data-dir lock (`<data>/central.lock`, see cmd/mirafleet)
// for as long as the socket is served: stale-socket removal below is only race-free under that
// lock, because no other central can then be starting on the same directory. As defence in
// depth the existing socket is dialled first and removed only if the dial is refused (nothing
// is listening) and it really is a socket.
func (s *Server) ServeAdmin(ctx context.Context, path string) error {
	ln, err := listenUnix(path) // created under umask 077: never briefly world-accessible
	if errors.Is(err, syscall.EADDRINUSE) {
		// Removed only after a real bind attempt, so the fail-closed (non-unix) path never
		// deletes anything.
		if err := removeStaleSocket(path); err != nil {
			return err
		}
		ln, err = listenUnix(path)
	}
	if err != nil {
		return err
	}
	// The listener does not unlink its path on Close (see listenUnix); this server removes the
	// file itself, and only while it is still this server's file (D-20).
	id, err := statSocket(path)
	if err != nil {
		ln.Close()
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		removeSocketIfSame(path, id)
		return err
	}
	// track takes the accept loop's WaitGroup slot under the lock Close uses, so Add can never
	// race Close's Wait (D-17); the loop's own tracking of each connection happens while it
	// still holds its slot.
	if !s.track(ln) {
		ln.Close()
		removeSocketIfSame(path, id)
		return errors.New("central: server closed")
	}
	loopDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			ln.Close()
		case <-loopDone:
		}
	}()
	table := adminTable()
	go func() {
		defer close(loopDone)
		defer s.untrack(ln)
		defer removeSocketIfSame(path, id) // runs first: ln is closed by ctx or Server.Close
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
			if !s.track(c) { // also takes the slot Server.Close waits on for in-flight calls
				c.Close()
				return
			}
			go func() {
				defer s.untrack(c)
				s.adminConn(ctx, c, table)
			}()
		}
	}()
	return nil
}

// removeStaleSocket deletes path when it is a unix socket nobody listens on. A live listener
// (successful dial) or any non-refusal dial error leaves it alone and fails.
func removeStaleSocket(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // vanished meanwhile: the retry binds
		}
		return err
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("admin socket path %s exists and is not a socket", path)
	}
	c, err := net.DialTimeout("unix", path, time.Second)
	if err == nil {
		c.Close()
		return fmt.Errorf("admin socket %s is in use by a running central", path)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("admin socket %s: %w", path, err)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
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
	// The peer closing the connection cancels the request (e.g. Ctrl-C of `central exec`). A
	// half-close (shutdown of the peer's write side after the request) does NOT: it only means
	// "no more input", and the response is still written (D-06).
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// credential.set reads its body frame from the connection, so nothing else may read it.
	if req.Method != api.MethodCredentialSet {
		go watchPeerClose(rctx, c, cancel)
	}

	// checkPeer required the peer to run as our own UID.
	cc := &callCtx{p: auth.LocalAdmin(os.Getuid()), src: table.src, svc: s.cfg.Service, st: s.cfg.Store,
		logf: s.cfg.Logf, state: &connState{}}
	if req.Method == api.MethodCredentialSet { // the secret follows as one body frame
		cc.body = &callBody{r: c, closer: c}
	}
	if err := writeResponseTimeout(c, c, table.serve(rctx, cc, req)); err != nil {
		s.cfg.Logf("admin socket: write response: %v", err)
	}
}

// peerClosedPollEvery is how often watchPeerClose re-checks a half-closed connection.
const peerClosedPollEvery = 100 * time.Millisecond

// watchPeerClose calls cancel when the peer fully closes c. Reading until EOF drains stray input;
// a read error (reset) means the peer is gone. EOF alone is ambiguous (half-close or full
// close), so after it the socket is polled for a hang-up until ctx ends.
func watchPeerClose(ctx context.Context, c net.Conn, cancel context.CancelFunc) {
	if _, err := io.Copy(io.Discard, c); err != nil {
		cancel()
		return
	}
	uc, ok := c.(*net.UnixConn)
	if !ok {
		cancel()
		return
	}
	t := time.NewTicker(peerClosedPollEvery)
	defer t.Stop()
	for {
		if peerClosed(uc) {
			cancel()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// writeResponse is api.WriteResponse; a variable so a test can force a response larger than
// the SSH channel window (real responses are capped below it).
var writeResponse = api.WriteResponse

// writeResponseTimeout writes resp to w and closes c if the write has not finished within
// api.WriteTimeout (D-09). ssh.Channel has no write deadline, so a peer that stops reading would
// otherwise pin the handler (and its channel slot) forever. A local Channel.Close does not
// unblock a write stalled on the peer's window in x/crypto/ssh (D-16), so for ssh channels the
// owning connection is passed in also and closed too, which does. On net.Conn, c alone suffices
// and WriteResponse additionally applies a real deadline.
func writeResponseTimeout(w io.Writer, c io.Closer, resp *api.Response, also ...io.Closer) error {
	return writeResponseBodyTimeout(w, c, resp, nil, false, also...)
}

// writeResponseBodyTimeout is writeResponseTimeout followed, when withBody is set, by one body
// frame (possibly empty) under the same timer. The body is written only if the result frame was.
func writeResponseBodyTimeout(w io.Writer, c io.Closer, resp *api.Response, body []byte, withBody bool, also ...io.Closer) error {
	t := time.AfterFunc(api.WriteTimeout, func() {
		c.Close()
		for _, a := range also {
			a.Close()
		}
	})
	defer t.Stop()
	if err := writeResponse(w, resp); err != nil {
		return err
	}
	if withBody {
		return api.WriteBody(w, body)
	}
	return nil
}
