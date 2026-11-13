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
)

// Channel types and SSH users spoken to central. Defined here (not in internal/transport) so
// this package adds nothing to the shared file.
const (
	ChanAPI      = "api@mirafleet"
	ChanJoin     = "join@mirafleet"
	ChanRegister = "register@mirafleet"

	UserOperator = "operator"
	UserEnroll   = "enroll"
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
	return callOn(ctx, c.c, ChanAPI, method, params, result)
}

func newID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// callOn opens one channel of chanType on cl, sends one framed request, reads one framed
// response and closes the channel.
func callOn(ctx context.Context, cl *ssh.Client, chanType, method string, params, result any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	req := api.Request{V: api.Version, Method: method}
	var err error
	if req.ID, err = newID(); err != nil {
		return err
	}
	if params != nil {
		if req.Params, err = json.Marshal(params); err != nil {
			return fmt.Errorf("encode params: %w", err)
		}
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if len(payload) > api.MaxRequestBytes {
		return fmt.Errorf("apiclient: request exceeds %d bytes", api.MaxRequestBytes)
	}

	ch, reqs, err := cl.OpenChannel(chanType, nil)
	if err != nil {
		if ce := ctx.Err(); ce != nil {
			return ce
		}
		return err
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
		return fail(err)
	}
	raw, err := api.ReadFrame(ch, api.MaxResponseBytes)
	if err != nil {
		return fail(fmt.Errorf("read response: %w", err))
	}
	var resp api.Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("malformed response: %w", err)
	}
	if resp.V != api.Version {
		return fmt.Errorf("unsupported response version %d", resp.V)
	}
	if resp.ID != req.ID {
		return errors.New("response id does not match request")
	}
	if resp.Error != nil {
		return resp.Error
	}
	if len(resp.Result) == 0 {
		return errors.New("response has neither result nor error")
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(resp.Result, result); err != nil {
		return fmt.Errorf("decode result: %w", err)
	}
	return nil
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
	return typedCall[api.HelloResult](ctx, c, api.MethodHello, api.HelloParams{Client: client})
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
