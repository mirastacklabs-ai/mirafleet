package central

import (
	"bytes"
	"context"
	"errors"
	"io"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
)

// Workspace handlers (docs/M3.md 1.1). They decode strictly, call the service and map to api
// DTOs; store rows, specs and runner ids never reach the wire.
//
// Body frames: the service gates a call (policy, ownership, state, input, confirm) BEFORE it
// pulls the request body through the lazy readers below, so an unauthorised caller can never make
// central buffer up to 1 MiB. The result body of read_file is buffered (at most MaxFileChunk) and
// written by the channel server after the JSON result frame.

// Per-method table timeouts. The service bounds the runner call itself more tightly (exec:
// the effective exec timeout plus its margin; files and close: 60 s); these are the outer
// safety nets, with room for the 30 s body-frame wait.
const (
	wsOpenOuterTimeout  = 5*time.Minute + 30*time.Second
	wsExecOuterTimeout  = api.HardWorkspaceExecCeiling + time.Minute
	wsFileOuterTimeout  = 100 * time.Second
	wsCloseOuterTimeout = 60*time.Second + 30*time.Second
)

// callBody is the body I/O of one api@ channel call: the channel to read a request body from and
// the response body a handler wants written after the result frame. nil on the admin socket.
type callBody struct {
	r      io.Reader
	closer io.Closer
	out    []byte
	hasOut bool
}

// takesBody reports whether the method may be followed by a request body frame.
func takesBody(method string) bool {
	return method == api.MethodWorkspaceWriteFile || method == api.MethodWorkspaceExec
}

// readBody reads the one request body frame (at most max bytes), arming the body timer.
func (b *callBody) readBody(max int) ([]byte, error) {
	if b == nil {
		return nil, api.Errorf(api.CodeBadRequest, "no body frame on this channel")
	}
	return api.ReadBodyTimeout(b.r, b.closer, max)
}

// lazyStdin is exec's stdin: a service.DeferredBody that reads the body frame when asked.
type lazyStdin struct{ b *callBody }

func (l *lazyStdin) Read([]byte) (int, error) { return 0, io.EOF } // never read directly

func (l *lazyStdin) Body() (io.Reader, int64, error) {
	buf, err := l.b.readBody(api.MaxFileChunk)
	if err != nil {
		return nil, 0, err
	}
	return bytes.NewReader(buf), int64(len(buf)), nil
}

// lazyFileBody is write_file's body: the frame is read on the first Read, which the service only
// reaches after gating. It must be exactly length bytes; a failure is kept so the handler can
// report the precise error (too_large, timeout) instead of the service's generic one.
type lazyFileBody struct {
	b      *callBody
	length int
	rd     *bytes.Reader
	err    error
}

func (l *lazyFileBody) Read(p []byte) (int, error) {
	if l.rd == nil {
		if l.err != nil {
			return 0, l.err
		}
		buf, err := l.b.readBody(l.length)
		if err == nil && len(buf) != l.length {
			err = api.Errorf(api.CodeBadRequest, "body is %d bytes, length says %d", len(buf), l.length)
		}
		if err != nil {
			l.err = err
			return 0, err
		}
		l.rd = bytes.NewReader(buf)
	}
	return l.rd.Read(p)
}

// capWriter buffers at most max bytes and fails past that, so a misbehaving runner reply can
// never grow central's memory beyond one body frame.
type capWriter struct {
	buf bytes.Buffer
	max int
}

var errCapExceeded = errors.New("read result exceeds one body frame")

func (c *capWriter) Write(p []byte) (int, error) {
	if c.buf.Len()+len(p) > c.max {
		return 0, errCapExceeded
	}
	return c.buf.Write(p)
}

// wsInfo maps a store row to the wire DTO. The operator name is set on the admin listing only.
func wsInfo(w store.Workspace, withOperator bool) api.WorkspaceInfo {
	i := api.WorkspaceInfo{ID: w.WsID, Fleet: w.Fleet, Runner: w.Runner, Session: w.Session, Image: w.Image,
		State: string(w.State), Created: w.CreatedAt, LastUsed: w.LastUsedAt, Expires: w.ExpiresAt}
	if withOperator {
		i.Operator = w.Operator
	}
	return i
}

func handleWorkspaceOpen(ctx context.Context, c *callCtx, req *api.Request) (any, error) {
	var p api.WorkspaceOpenParams
	if e := api.DecodeParams(req.Params, &p); e != nil {
		return nil, e
	}
	w, reused, err := c.svc.OpenWorkspace(ctx, c.p, c.src, p.Fleet, p.Session, p.Image, p.Labels, p.Confirm)
	if err != nil {
		return nil, err
	}
	return api.WorkspaceOpenResult{Workspace: wsInfo(w, false), Reused: reused}, nil
}

func handleWorkspaceExec(ctx context.Context, c *callCtx, req *api.Request) (any, error) {
	var p api.WorkspaceExecParams
	if e := api.DecodeParams(req.Params, &p); e != nil {
		return nil, e
	}
	var stdin io.Reader
	var n int64
	if p.Stdin {
		if c.body == nil {
			return nil, api.Errorf(api.CodeBadRequest, "stdin is not supported on this channel")
		}
		stdin, n = &lazyStdin{b: c.body}, service.StdinDeferred
	}
	return c.svc.ExecWorkspace(ctx, c.p, c.src, p.Workspace, p.Argv, p.Cwd, p.Env, p.TimeoutS, stdin, n, p.Confirm)
}

func handleWorkspaceReadFile(ctx context.Context, c *callCtx, req *api.Request) (any, error) {
	var p api.WorkspaceReadFileParams
	if e := api.DecodeParams(req.Params, &p); e != nil {
		return nil, e
	}
	if c.body == nil {
		return nil, api.Errorf(api.CodeBadRequest, "files are not supported on this channel")
	}
	out := &capWriter{max: api.MaxFileChunk}
	res, err := c.svc.ReadWorkspaceFile(ctx, c.p, c.src, p.Workspace, p.Path, p.Offset, p.Limit, out)
	if err != nil {
		return nil, err
	}
	c.body.out, c.body.hasOut = out.buf.Bytes(), true
	return res, nil
}

func handleWorkspaceWriteFile(ctx context.Context, c *callCtx, req *api.Request) (any, error) {
	var p api.WorkspaceWriteFileParams
	if e := api.DecodeParams(req.Params, &p); e != nil {
		return nil, e
	}
	if c.body == nil {
		return nil, api.Errorf(api.CodeBadRequest, "files are not supported on this channel")
	}
	body := &lazyFileBody{b: c.body, length: p.Length}
	res, err := c.svc.WriteWorkspaceFile(ctx, c.p, c.src, p.Workspace, p.Path, p.Offset, p.Truncate, p.Mode,
		p.Length, p.SHA256, body, p.Confirm)
	if err != nil {
		if body.err != nil {
			return nil, body.err
		}
		return nil, err
	}
	return res, nil
}

func handleWorkspaceClose(ctx context.Context, c *callCtx, req *api.Request) (any, error) {
	var p api.WorkspaceCloseParams
	if e := api.DecodeParams(req.Params, &p); e != nil {
		return nil, e
	}
	if _, err := c.svc.CloseWorkspace(ctx, c.p, c.src, p.Workspace); err != nil {
		return nil, err
	}
	return api.WorkspaceCloseResult{}, nil
}

func handleWorkspaceList(ctx context.Context, c *callCtx, req *api.Request) (any, error) {
	var p api.WorkspaceListParams
	if e := api.DecodeParams(req.Params, &p); e != nil {
		return nil, e
	}
	ws, err := c.svc.ListWorkspaces(ctx, c.p, c.src, p.Fleet)
	if err != nil {
		return nil, err
	}
	out := api.WorkspaceListResult{Workspaces: make([]api.WorkspaceInfo, len(ws))}
	for i, w := range ws {
		out.Workspaces[i] = wsInfo(w, false)
	}
	return out, nil
}

// Admin socket handlers.

func handleFleetWorkspaceSet(ctx context.Context, c *callCtx, req *api.Request) (any, error) {
	var p api.FleetWorkspaceSetParams
	if e := api.DecodeParams(req.Params, &p); e != nil {
		return nil, e
	}
	// Full replace: the complete config is mapped field by field, nothing is merged.
	if _, err := c.svc.SetFleetWorkspace(ctx, c.p, c.src, p.Fleet, *workspaceFromAPI(&p.Workspace)); err != nil {
		return nil, err
	}
	f, err := c.svc.GetFleet(ctx, c.p, c.src, p.Fleet)
	if err != nil {
		return nil, err
	}
	return api.FleetWorkspaceSetResult{Fleet: fleetInfo(f, c.p.Kind == auth.KindAdmin)}, nil
}

func handleWorkspaceAdminList(ctx context.Context, c *callCtx, req *api.Request) (any, error) {
	var p api.WorkspaceAdminListParams
	if e := api.DecodeParams(req.Params, &p); e != nil {
		return nil, e
	}
	ws, err := c.svc.AdminListWorkspaces(ctx, c.p, c.src, p.Fleet)
	if err != nil {
		return nil, err
	}
	out := api.WorkspaceAdminListResult{Workspaces: make([]api.WorkspaceInfo, len(ws))}
	for i, w := range ws {
		out.Workspaces[i] = wsInfo(w, true)
	}
	return out, nil
}

func handleWorkspaceKill(ctx context.Context, c *callCtx, req *api.Request) (any, error) {
	var p api.WorkspaceKillParams
	if e := api.DecodeParams(req.Params, &p); e != nil {
		return nil, e
	}
	if _, err := c.svc.KillWorkspace(ctx, c.p, c.src, p.ID); err != nil {
		return nil, err
	}
	return api.WorkspaceKillResult{}, nil
}
