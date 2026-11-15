// Package apiclient is the SSH client for central: operator calls on api@mirafleet and the
// enroll channels. Pure Go and cross-platform (no unix-only calls): the orchestrator runs on
// linux, darwin and windows.
package apiclient

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/protover"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// HandshakeTimeout bounds TCP connect plus SSH handshake and authentication. Variable so tests
// can shorten it.
var HandshakeTimeout = 30 * time.Second

// Client is an authenticated SSH connection to central. Safe for concurrent Call.
type Client struct {
	c *ssh.Client
}

// Dial connects to addr (host:port) as user. hostKey decides whether the server is trusted; it
// is mandatory and there is no fallback (a nil callback is an error, never "accept anything").
// The handshake honours ctx.
func Dial(ctx context.Context, addr, user string, signer ssh.Signer, hostKey ssh.HostKeyCallback) (*Client, error) {
	if hostKey == nil {
		return nil, errors.New("apiclient: host key callback required")
	}
	if signer == nil {
		return nil, errors.New("apiclient: signer required")
	}
	var d net.Dialer
	hctx, cancel := context.WithTimeout(ctx, HandshakeTimeout)
	defer cancel()
	nc, err := d.DialContext(hctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	// Closing the conn unblocks a handshake stuck on a silent peer.
	stop := context.AfterFunc(hctx, func() { nc.Close() })
	cfg := &ssh.ClientConfig{
		User:              user,
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback:   hostKey,
		HostKeyAlgorithms: []string{ssh.KeyAlgoED25519},
		Timeout:           HandshakeTimeout,
	}
	sc, chans, reqs, err := ssh.NewClientConn(nc, addr, cfg)
	if !stop() && err == nil {
		// hctx ended around completion; the conn may be closed.
		err = hctx.Err()
		if err == nil {
			err = errors.New("apiclient: handshake interrupted")
		}
	}
	if err != nil {
		nc.Close()
		if ce := ctx.Err(); ce != nil {
			return nil, ce
		}
		return nil, err
	}
	return &Client{c: ssh.NewClient(sc, chans, reqs)}, nil
}

// Close closes the connection.
func (c *Client) Close() error { return c.c.Close() }

// Call performs one request on its own api@mirafleet channel. params may be nil; result may be
// nil to discard the result. A wire error is returned as *api.Error. Cancelling ctx closes the
// channel and returns ctx.Err().
func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	return callOn(ctx, c.c, transport.ChanAPI, method, params, result)
}

func newID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// CallBody is Call for a method whose request is followed by one body frame (workspace.write_file,
// workspace.exec with stdin). body may be empty but not larger than api.MaxFileChunk. Central
// reads the frame only after it has authorised the call; a refused call is answered without
// consuming it, which this client handles (the write fits the channel window).
func (c *Client) CallBody(ctx context.Context, method string, params any, body []byte, result any) error {
	if len(body) > api.MaxFileChunk {
		return fmt.Errorf("apiclient: body exceeds %d bytes", api.MaxFileChunk)
	}
	_, err := callOnBody(ctx, c.c, transport.ChanAPI, method, params, body, true, result, false, nil)
	return err
}

// CallReadBodyIf is CallReadBody for a method whose response carries a body frame only
// sometimes: hasBody is called after the result is decoded into result (workspace.fs: an image).
func (c *Client) CallReadBodyIf(ctx context.Context, method string, params, result any, hasBody func() bool) ([]byte, error) {
	return callOnBody(ctx, c.c, transport.ChanAPI, method, params, nil, false, result, true, hasBody)
}

// CallReadBody is Call for a method whose response is followed by one body frame
// (workspace.read_file). The body is returned only when the call succeeded; its size is bounded
// by api.MaxFileChunk before anything is allocated.
func (c *Client) CallReadBody(ctx context.Context, method string, params, result any) ([]byte, error) {
	return callOnBody(ctx, c.c, transport.ChanAPI, method, params, nil, false, result, true, nil)
}

// callOn opens one channel of chanType on cl, sends one framed request, reads one framed
// response and closes the channel.
func callOn(ctx context.Context, cl *ssh.Client, chanType, method string, params, result any) error {
	_, err := callOnBody(ctx, cl, chanType, method, params, nil, false, result, false, nil)
	return err
}

// callOnBody is callOn with an optional request body frame (sendBody) and an optional response
// body frame (wantBody) after the JSON result. One channel per call; ctx cancellation closes it.
func callOnBody(ctx context.Context, cl *ssh.Client, chanType, method string, params any, body []byte, sendBody bool,
	result any, wantBody bool, bodyIf func() bool) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	req := api.Request{V: api.Version, Method: method}
	var err error
	if req.ID, err = newID(); err != nil {
		return nil, err
	}
	if params != nil {
		if req.Params, err = json.Marshal(params); err != nil {
			return nil, fmt.Errorf("encode params: %w", err)
		}
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if len(payload) > api.MaxRequestBytes {
		return nil, fmt.Errorf("apiclient: request exceeds %d bytes", api.MaxRequestBytes)
	}

	ch, reqs, err := cl.OpenChannel(chanType, nil)
	if err != nil {
		if ce := ctx.Err(); ce != nil {
			return nil, ce
		}
		return nil, err
	}
	go ssh.DiscardRequests(reqs)
	defer ch.Close()
	// ssh.Channel has no deadlines: cancellation closes the channel to unblock read and write.
	stop := context.AfterFunc(ctx, func() { ch.Close() })
	defer stop()

	fail := func(err error) error {
		if ce := ctx.Err(); ce != nil {
			return ce
		}
		return err
	}
	if err := api.WriteFrame(ch, payload); err != nil {
		return nil, fail(err)
	}
	var werr error
	if sendBody {
		// Written right behind the request: at most MaxFileChunk+4 bytes, inside the channel
		// window, so this never blocks even when central refuses the call without reading it.
		// A refusal may close the channel while the body is still going out: keep the write
		// error and let the response (the real reason) win if one arrived.
		werr = api.WriteBody(ch, body)
	}
	raw, err := api.ReadFrame(ch, api.MaxResponseBytes)
	if err != nil {
		if werr != nil {
			err = werr
		}
		return nil, fail(fmt.Errorf("read response: %w", err))
	}
	var resp api.Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("malformed response: %w", err)
	}
	if resp.V != api.Version {
		return nil, fmt.Errorf("unsupported response version %d", resp.V)
	}
	if resp.ID != req.ID {
		return nil, errors.New("response id does not match request")
	}
	if resp.Error != nil {
		return nil, resp.Error
	}
	if len(resp.Result) == 0 {
		return nil, errors.New("response has neither result nor error")
	}
	if result != nil {
		if err := json.Unmarshal(resp.Result, result); err != nil {
			return nil, fmt.Errorf("decode result: %w", err)
		}
	}
	if !wantBody || (bodyIf != nil && !bodyIf()) {
		return nil, nil
	}
	out, err := api.ReadBodyTimeout(ch, ch, api.MaxFileChunk)
	if err != nil {
		return nil, fail(fmt.Errorf("read response body: %w", err))
	}
	return out, nil
}

// typedCall performs Call and decodes the result into a new R.
func typedCall[R any](ctx context.Context, c *Client, method string, params any) (*R, error) {
	var r R
	if err := c.Call(ctx, method, params, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// Hello identifies the operator and the supported protocol versions.
func (c *Client) Hello(ctx context.Context, client string) (*api.HelloResult, error) {
	return c.HelloProto(ctx, client, protover.Current)
}

// HelloProto is Hello with an explicit protocol level (tests fake version skew with it).
func (c *Client) HelloProto(ctx context.Context, client string, proto int) (*api.HelloResult, error) {
	return typedCall[api.HelloResult](ctx, c, api.MethodHello, api.HelloParams{Client: client, Proto: proto})
}

// FleetsList lists fleets.
func (c *Client) FleetsList(ctx context.Context) (*api.FleetsListResult, error) {
	return typedCall[api.FleetsListResult](ctx, c, api.MethodFleetsList, nil)
}

// RunnersList lists the runners of a fleet.
func (c *Client) RunnersList(ctx context.Context, fleet string) (*api.RunnersListResult, error) {
	return typedCall[api.RunnersListResult](ctx, c, api.MethodRunnersList, api.RunnersListParams{Fleet: fleet})
}

// ActionRun runs an action on a runner.
func (c *Client) ActionRun(ctx context.Context, p api.ActionRunParams) (*api.ActionRunResult, error) {
	return typedCall[api.ActionRunResult](ctx, c, api.MethodActionRun, p)
}

// WorkspaceOpen opens (or reuses) the caller's workspace for a session.
func (c *Client) WorkspaceOpen(ctx context.Context, p api.WorkspaceOpenParams) (*api.WorkspaceOpenResult, error) {
	return typedCall[api.WorkspaceOpenResult](ctx, c, api.MethodWorkspaceOpen, p)
}

// WorkspaceExec runs argv in a workspace. When p.Stdin is set, stdin is sent as the body frame
// (at most api.MaxFileChunk bytes); otherwise it must be empty.
func (c *Client) WorkspaceExec(ctx context.Context, p api.WorkspaceExecParams, stdin []byte) (*api.WorkspaceExecResult, error) {
	var r api.WorkspaceExecResult
	var err error
	if p.Stdin {
		err = c.CallBody(ctx, api.MethodWorkspaceExec, p, stdin, &r)
	} else {
		err = c.Call(ctx, api.MethodWorkspaceExec, p, &r)
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// WorkspaceReadFile reads a chunk of a file; the bytes are returned beside the metadata.
func (c *Client) WorkspaceReadFile(ctx context.Context, p api.WorkspaceReadFileParams) (*api.WorkspaceReadFileResult, []byte, error) {
	var r api.WorkspaceReadFileResult
	b, err := c.CallReadBody(ctx, api.MethodWorkspaceReadFile, p, &r)
	if err != nil {
		return nil, nil, err
	}
	return &r, b, nil
}

// WorkspaceWriteFile writes data (p.Length and p.SHA256 must describe it) to a file.
func (c *Client) WorkspaceWriteFile(ctx context.Context, p api.WorkspaceWriteFileParams, data []byte) (*api.WorkspaceWriteFileResult, error) {
	var r api.WorkspaceWriteFileResult
	if err := c.CallBody(ctx, api.MethodWorkspaceWriteFile, p, data, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// WorkspaceClose closes a workspace.
func (c *Client) WorkspaceClose(ctx context.Context, id string) error {
	return c.Call(ctx, api.MethodWorkspaceClose, api.WorkspaceCloseParams{Workspace: id}, nil)
}

// WorkspaceList lists the caller's own workspaces, optionally of one fleet.
func (c *Client) WorkspaceList(ctx context.Context, fleet string) (*api.WorkspaceListResult, error) {
	return typedCall[api.WorkspaceListResult](ctx, c, api.MethodWorkspaceList, api.WorkspaceListParams{Fleet: fleet})
}
