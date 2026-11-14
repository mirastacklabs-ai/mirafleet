package orch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/apiclient"
	"github.com/mirastacklabs-ai/mirafleet/internal/keys"
)

// panicOnGuard makes the guard panic instead of returning an error. Tests set it so a code path
// that reaches call with a non-operator method cannot go unnoticed.
var panicOnGuard = false

// idempotent lists the only methods call retries after a dead connection. exec, write_file,
// action.run and close are never retried: they may already have taken effect.
func idempotent(method string) bool {
	switch method {
	case api.MethodFleetsList, api.MethodRunnersList, api.MethodWorkspaceList,
		api.MethodWorkspaceReadFile, api.MethodWorkspaceOpen:
		return true
	}
	return false
}

// channelBackoff is the wait before each retry of a call whose channel central refused.
var channelBackoff = []time.Duration{50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond}

// isTransport reports whether err means the connection itself is gone (as opposed to a refused
// channel or a bad reply on a working connection).
func isTransport(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, net.ErrClosed) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	s := err.Error()
	for _, m := range []string{"connection reset", "broken pipe", "use of closed", "connection lost", "ssh: disconnect"} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// callIO carries the optional bodies and the result of one call.
type callIO struct {
	result   any
	body     []byte // request body frame, when hasBody
	hasBody  bool
	wantBody bool // a response body frame follows the result
	got      []byte
	addr     string // set by call: the address of the context the call went to
}

// clean makes a string safe to put in an error: sanitised and clipped.
func clean(s string) string {
	s = Sanitize([]byte(s))
	if utf8.RuneCountInString(s) > 300 {
		r := []rune(s)
		s = string(r[:300]) + "…"
	}
	return s
}

// call is the only path to apiclient. It refuses any method that is not an operator method, so
// admin and enrol methods are unreachable from here whatever the caller passes (docs/M5.md §1.11).
func (o *Orch) call(ctx context.Context, cname, method string, params any, x *callIO) error {
	if !api.IsOperatorMethod(method) {
		if panicOnGuard {
			panic("orch: guard: not an operator method: " + method)
		}
		return errf(api.CodeForbidden, nil, "method not available to the orchestrator")
	}
	if x == nil {
		x = &callIO{}
	}
	if o.isClosed() {
		return errClosed
	}
	cs, err := o.loadContexts()
	if err != nil {
		return err
	}
	c, ok := cs.Find(cname)
	if !ok {
		return errf(api.CodeNotFound, nil, fmt.Sprintf("unknown context %q", clean(cname)))
	}
	x.addr = c.Addr
	p, err := o.poolFor(c.Name, c.Addr)
	if err != nil {
		return err
	}

	raw, _ := json.Marshal(params)
	ctx, cancel := context.WithTimeout(ctx, CallLimit(method, raw))
	defer cancel()
	stop := context.AfterFunc(o.ctx, cancel) // Close cancels in-flight calls
	defer stop()

	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
	case <-ctx.Done():
		return o.mapErr(ctx, ctx.Err())
	}

	busy := 0
	for attempt := 0; ; attempt++ {
		lv, err := p.get(ctx)
		if err != nil {
			return o.mapErr(ctx, err)
		}
		err = o.invoke(ctx, lv.conn, method, params, x)
		if err == nil {
			return nil
		}
		var ae *api.Error
		if errors.As(err, &ae) || ctx.Err() != nil {
			return o.mapErr(ctx, err)
		}
		var oce *ssh.OpenChannelError
		if errors.As(err, &oce) {
			// The channel never opened (central is at its channel limit): nothing was sent, so
			// every method may be retried, and the connection is healthy.
			if busy < len(channelBackoff) {
				t := time.NewTimer(channelBackoff[busy])
				busy++
				select {
				case <-t.C:
					attempt-- // a refused channel does not use the redial retry
					continue
				case <-ctx.Done():
					t.Stop()
					return o.mapErr(ctx, ctx.Err())
				}
			}
			return errf(api.CodeUnavailable, nil, "central is busy: too many concurrent calls; try again")
		}
		if !isTransport(err) {
			// A bad reply on a working connection: report it, keep the connection (dropping it
			// would cancel every other call in flight).
			o.cfg.Logf("call %s on context %s: protocol error: %v", method, p.name, err)
			return errf(api.CodeUnavailable, nil, "central sent an unexpected reply")
		}
		// The connection failed. Drop it so the next call redials; retry once only when the call
		// is safe to repeat.
		o.cfg.Logf("call %s on context %s: connection error: %v", method, p.name, err)
		p.drop(lv)
		if attempt == 0 && idempotent(method) {
			continue
		}
		return errf(api.CodeUnavailable, nil, "the connection to central was lost; the call may or may not have completed")
	}
}

func (o *Orch) invoke(ctx context.Context, c Conn, method string, params any, x *callIO) error {
	switch {
	case x.wantBody:
		b, err := c.CallReadBody(ctx, method, params, x.result)
		x.got = b
		return err
	case x.hasBody:
		return c.CallBody(ctx, method, params, x.body, x.result)
	}
	return c.Call(ctx, method, params, x.result)
}

// mapErr turns any error into a safe *Error (or the context's own cancellation).
func (o *Orch) mapErr(ctx context.Context, err error) error {
	var oe *Error
	var ae *api.Error
	switch {
	case errors.As(err, &oe):
		return oe
	case errors.As(err, &ae):
		return &Error{Code: ae.Code, Message: clean(ae.Message)}
	case o.isClosed():
		return errClosed
	case errors.Is(err, context.DeadlineExceeded):
		return errf(api.CodeTimeout, nil, "the call timed out")
	case errors.Is(err, context.Canceled):
		return err
	case errors.Is(err, apiclient.ErrHostKeyMismatch), errors.Is(err, keys.ErrHostKeyMismatch):
		return errf(api.CodeUnavailable, nil, "central's host key does not match the pinned key; the server may have been rebuilt or intercepted")
	case strings.Contains(err.Error(), "unable to authenticate"):
		return errf(api.CodeUnauthenticated, nil, "central rejected the operator key: run `mirafleet mcp login …`")
	}
	o.cfg.Logf("call failed: %v", err)
	return errf(api.CodeUnavailable, nil, "cannot reach central")
}

// FleetsList lists the fleets of a context and refreshes the guarded-fleet cache.
func (o *Orch) FleetsList(ctx context.Context, cname string) (*api.FleetsListResult, error) {
	var r api.FleetsListResult
	x := &callIO{result: &r}
	if err := o.call(ctx, cname, api.MethodFleetsList, nil, x); err != nil {
		return nil, err
	}
	o.noteFleets(cname, x.addr, &r)
	return &r, nil
}

// RunnersList lists the runners of a fleet.
func (o *Orch) RunnersList(ctx context.Context, cname, fleet string) (*api.RunnersListResult, error) {
	var r api.RunnersListResult
	if err := o.call(ctx, cname, api.MethodRunnersList, api.RunnersListParams{Fleet: fleet}, &callIO{result: &r}); err != nil {
		return nil, err
	}
	return &r, nil
}

// ActionRun runs an action on a runner. Never retried.
func (o *Orch) ActionRun(ctx context.Context, cname string, p api.ActionRunParams) (*api.ActionRunResult, error) {
	var r api.ActionRunResult
	if err := o.call(ctx, cname, api.MethodActionRun, p, &callIO{result: &r}); err != nil {
		return nil, err
	}
	return &r, nil
}

// WorkspaceOpen opens (or reuses) the caller's workspace for p.Session.
func (o *Orch) WorkspaceOpen(ctx context.Context, cname string, p api.WorkspaceOpenParams) (*api.WorkspaceOpenResult, error) {
	if e := api.ValidateSession(p.Session); e != nil {
		return nil, &Error{Code: e.Code, Message: e.Message}
	}
	var r api.WorkspaceOpenResult
	if err := o.call(ctx, cname, api.MethodWorkspaceOpen, p, &callIO{result: &r}); err != nil {
		return nil, err
	}
	return &r, nil
}

// WorkspaceExec runs argv in a workspace; stdin (at most api.MaxFileChunk bytes) is sent as the
// body frame when non-empty. p.Env is checked with api.ValidateEnv; nothing is taken from this
// process's environment. Never retried.
func (o *Orch) WorkspaceExec(ctx context.Context, cname string, p api.WorkspaceExecParams, stdin []byte) (*api.WorkspaceExecResult, error) {
	if e := api.ValidateEnv(p.Env); e != nil {
		return nil, &Error{Code: e.Code, Message: clean(e.Message)}
	}
	if len(stdin) > api.MaxFileChunk {
		return nil, errf(api.CodeTooLarge, nil, fmt.Sprintf("stdin exceeds %d bytes", api.MaxFileChunk))
	}
	p.Stdin = len(stdin) > 0
	var r api.WorkspaceExecResult
	x := &callIO{result: &r, body: stdin, hasBody: p.Stdin}
	if err := o.call(ctx, cname, api.MethodWorkspaceExec, p, x); err != nil {
		return nil, err
	}
	return &r, nil
}

// WorkspaceReadFile reads a chunk of a file.
func (o *Orch) WorkspaceReadFile(ctx context.Context, cname string, p api.WorkspaceReadFileParams) (*api.WorkspaceReadFileResult, []byte, error) {
	var r api.WorkspaceReadFileResult
	x := &callIO{result: &r, wantBody: true}
	if err := o.call(ctx, cname, api.MethodWorkspaceReadFile, p, x); err != nil {
		return nil, nil, err
	}
	return &r, x.got, nil
}

// WorkspaceWriteFile writes data (p.Length and p.SHA256 must describe it). Never retried.
func (o *Orch) WorkspaceWriteFile(ctx context.Context, cname string, p api.WorkspaceWriteFileParams, data []byte) (*api.WorkspaceWriteFileResult, error) {
	if len(data) > api.MaxFileChunk {
		return nil, errf(api.CodeTooLarge, nil, fmt.Sprintf("a write chunk is at most %d bytes", api.MaxFileChunk))
	}
	var r api.WorkspaceWriteFileResult
	if err := o.call(ctx, cname, api.MethodWorkspaceWriteFile, p, &callIO{result: &r, body: data, hasBody: true}); err != nil {
		return nil, err
	}
	return &r, nil
}

// WorkspaceClose closes a workspace. Never retried.
func (o *Orch) WorkspaceClose(ctx context.Context, cname, id string) (*api.WorkspaceCloseResult, error) {
	var r api.WorkspaceCloseResult
	if err := o.call(ctx, cname, api.MethodWorkspaceClose, api.WorkspaceCloseParams{Workspace: id}, &callIO{result: &r}); err != nil {
		return nil, err
	}
	return &r, nil
}

// WorkspaceList lists the caller's own workspaces, optionally of one fleet.
func (o *Orch) WorkspaceList(ctx context.Context, cname, fleet string) (*api.WorkspaceListResult, error) {
	var r api.WorkspaceListResult
	if err := o.call(ctx, cname, api.MethodWorkspaceList, api.WorkspaceListParams{Fleet: fleet}, &callIO{result: &r}); err != nil {
		return nil, err
	}
	return &r, nil
}
