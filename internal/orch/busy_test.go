package orch

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/apiclient"
)

// A refused channel (central at its limit) must not drop the connection or cancel the calls
// already running, and the refused call is retried after a short backoff.
func TestChannelRefusedKeepsConnection(t *testing.T) {
	release := make(chan struct{})
	var started atomic.Int32
	var shortage atomic.Int32 // refusals left to hand out
	d := &dialer{mk: func(int) (Conn, error) {
		return &fakeConn{handler: func(ctx context.Context, m string, res any) error {
			if m == api.MethodWorkspaceList {
				started.Add(1)
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			if shortage.Add(-1) >= 0 {
				return &ssh.OpenChannelError{Reason: ssh.ResourceShortage, Message: "too many concurrent channels"}
			}
			return nil
		}}, nil
	}}
	o := newOrch(t, oneCtx(t), d, func(c *Config) { c.KeepaliveInterval = 10 * time.Millisecond })

	var wg sync.WaitGroup
	errs := make(chan error, api.MaxChannelsPerConn)
	for i := 0; i < api.MaxChannelsPerConn; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := o.WorkspaceList(context.Background(), "a", "")
			errs <- err
		}()
	}
	eventually(t, "8 calls running", func() bool { return started.Load() == api.MaxChannelsPerConn })
	c := d.conns[0].(*fakeConn)
	time.Sleep(100 * time.Millisecond) // several keepalive ticks
	if n := c.pings.Load(); n != 0 {
		t.Fatalf("keepalive pinged a busy connection %d times", n)
	}
	// The 9th call waits for a slot; once the 8 finish, central still refuses it twice.
	shortage.Store(2)
	done := make(chan error, 1)
	go func() { _, err := o.WorkspaceClose(context.Background(), "a", "w"); done <- err }()
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("in-flight call failed: %v", err)
		}
	}
	if err := <-done; err != nil {
		t.Fatalf("refused call not retried to success: %v", err)
	}
	if c.closed.Load() || d.n.Load() != 1 {
		t.Fatalf("connection dropped (closed=%v dials=%d)", c.closed.Load(), d.n.Load())
	}
}

func TestChannelRefusedGivesUp(t *testing.T) {
	d := &dialer{mk: func(int) (Conn, error) {
		return &fakeConn{handler: func(context.Context, string, any) error {
			return &ssh.OpenChannelError{Reason: ssh.ResourceShortage}
		}}, nil
	}}
	o := newOrch(t, oneCtx(t), d)
	_, err := o.WorkspaceClose(context.Background(), "a", "w")
	var e *Error
	if !errors.As(err, &e) || e.Code != api.CodeUnavailable {
		t.Fatalf("err = %v", err)
	}
	c := d.conns[0].(*fakeConn)
	if c.closed.Load() || len(c.methods()) != len(channelBackoff)+1 {
		t.Fatalf("closed=%v calls=%d", c.closed.Load(), len(c.methods()))
	}
}

func TestChannelRefusedHonoursContext(t *testing.T) {
	d := &dialer{mk: func(int) (Conn, error) {
		return &fakeConn{handler: func(context.Context, string, any) error {
			return &ssh.OpenChannelError{Reason: ssh.ResourceShortage}
		}}, nil
	}}
	o := newOrch(t, oneCtx(t), d)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := o.WorkspaceClose(ctx, "a", "w"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestProtocolErrorKeepsConnection(t *testing.T) {
	for _, msg := range []string{"malformed response: x", "decode result: x", "response id does not match request"} {
		t.Run(msg, func(t *testing.T) {
			d := &dialer{mk: func(int) (Conn, error) {
				return &fakeConn{handler: func(context.Context, string, any) error {
					return fmt.Errorf("%s", msg)
				}}, nil
			}}
			o := newOrch(t, oneCtx(t), d)
			_, err := o.WorkspaceList(context.Background(), "a", "")
			var e *Error
			if !errors.As(err, &e) || e.Code != api.CodeUnavailable {
				t.Fatalf("err = %v", err)
			}
			c := d.conns[0].(*fakeConn)
			if c.closed.Load() || d.n.Load() != 1 || len(c.methods()) != 1 {
				t.Fatalf("closed=%v dials=%d calls=%v", c.closed.Load(), d.n.Load(), c.methods())
			}
		})
	}
}

func TestKeepaliveRefusalKeepsConnection(t *testing.T) {
	d := &dialer{mk: func(int) (Conn, error) { return &fakeConn{}, nil }}
	o := newOrch(t, oneCtx(t), d, func(c *Config) { c.KeepaliveInterval = 10 * time.Millisecond })
	if _, err := o.WorkspaceList(context.Background(), "a", ""); err != nil {
		t.Fatal(err)
	}
	c := d.conns[0].(*fakeConn)
	c.pingErr.Store(&ssh.OpenChannelError{Reason: ssh.ResourceShortage})
	eventually(t, "pings", func() bool { return c.pings.Load() >= 3 })
	if c.closed.Load() {
		t.Fatal("a refused ping dropped the connection")
	}
}

type hangHello struct{}

func (hangHello) Hello(ctx context.Context, _ string) (*api.HelloResult, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestHelloHasDeadline(t *testing.T) {
	old := apiclient.HandshakeTimeout
	apiclient.HandshakeTimeout = 50 * time.Millisecond
	defer func() { apiclient.HandshakeTimeout = old }()
	done := make(chan error, 1)
	go func() { done <- checkHello(context.Background(), hangHello{}) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hello without a deadline hung")
	}
}

func TestErrGuardedDefaultIsError(t *testing.T) {
	var e *Error
	if !errors.As(ErrGuardedDefault, &e) || e.Code != api.CodeBadRequest {
		t.Fatalf("ErrGuardedDefault = %v", ErrGuardedDefault)
	}
}
