package registry

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

const wsID = "0123456789abcdef0123456789abcdef"

// wsChan: what central writes is recorded; what central reads comes from pw.
type wsChan struct {
	pr *io.PipeReader
	pw *io.PipeWriter

	mu         sync.Mutex
	written    bytes.Buffer
	writeClose bool
	closed     chan struct{}
	wclosed    chan struct{}
	once       sync.Once
	wonce      sync.Once
	blockWrite bool
}

func newWSChan(blockWrite bool) *wsChan {
	pr, pw := io.Pipe()
	return &wsChan{pr: pr, pw: pw, closed: make(chan struct{}), wclosed: make(chan struct{}), blockWrite: blockWrite}
}

func (c *wsChan) Read(p []byte) (int, error) { return c.pr.Read(p) }
func (c *wsChan) Write(p []byte) (int, error) {
	if c.blockWrite {
		<-c.closed
		return 0, io.ErrClosedPipe
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.written.Write(p)
}
func (c *wsChan) Close() error {
	c.once.Do(func() { close(c.closed); c.pr.Close() })
	return nil
}
func (c *wsChan) CloseWrite() error                              { c.wonce.Do(func() { close(c.wclosed) }); return nil }
func (c *wsChan) SendRequest(string, bool, []byte) (bool, error) { return false, nil }
func (c *wsChan) Stderr() io.ReadWriter                          { return nil }
func (c *wsChan) got() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.written.Bytes()...)
}

type wsConn struct {
	openErr    error
	blockWrite bool
	opened     chan *wsChan
	datas      chan []byte
	script     func(c *wsChan)
	mu         sync.Mutex
	n          int
	closeOnce  sync.Once
}

func newWSConn(script func(c *wsChan)) *wsConn {
	return &wsConn{opened: make(chan *wsChan, 16), datas: make(chan []byte, 16), script: script}
}

func (c *wsConn) OpenChannel(name string, data []byte) (ssh.Channel, <-chan *ssh.Request, error) {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	if name != transport.ChanWorkspace {
		return nil, nil, errors.New("wrong channel type " + name)
	}
	if c.openErr != nil {
		return nil, nil, c.openErr
	}
	ch := newWSChan(c.blockWrite)
	c.opened <- ch
	c.datas <- data
	if c.script != nil {
		go c.script(ch)
	}
	reqs := make(chan *ssh.Request)
	close(reqs)
	return ch, reqs, nil
}
func (c *wsConn) Close() error { return nil }
func (c *wsConn) opens() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func wsReg(t *testing.T, st Status, c Conn) *Registry {
	t.Helper()
	r := newReg()
	s, err := r.Register(1, "r", "f", c, st, transport.Heartbeat{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Unregister(s) })
	return r
}

func okFrame(t *testing.T, w io.Writer, res transport.WorkspaceResult) {
	t.Helper()
	res.Code = transport.CodeOK
	if err := transport.WriteResult(w, &res, MaxWorkspaceResultBytes); err != nil {
		t.Error(err)
	}
}

func okScript(t *testing.T, res transport.WorkspaceResult, tail []byte) func(c *wsChan) {
	return func(c *wsChan) {
		<-c.wclosed
		okFrame(t, c.pw, res)
		if tail != nil {
			c.pw.Write(tail)
		}
		c.pw.Close()
	}
}

func TestWorkspaceOps(t *testing.T) {
	spec := &transport.WorkspaceSpec{Image: "img:1"}
	cases := []struct {
		name string
		req  transport.WorkspaceRequest
		body string
		res  transport.WorkspaceResult
		tail string
	}{
		{"create", transport.WorkspaceRequest{Op: transport.OpCreate, ID: wsID, Spec: spec}, "", transport.WorkspaceResult{}, ""},
		{"exec", transport.WorkspaceRequest{Op: transport.OpExec, ID: wsID, Argv: []string{"cat"}, StdinLen: 5}, "hello",
			transport.WorkspaceResult{Exec: &transport.WorkspaceExec{Stdout: "hello"}}, ""},
		{"write", transport.WorkspaceRequest{Op: transport.OpWrite, ID: wsID, Path: "a", Length: 3}, "abc", transport.WorkspaceResult{}, ""},
		{"read", transport.WorkspaceRequest{Op: transport.OpRead, ID: wsID, Path: "a", Limit: 10}, "",
			transport.WorkspaceResult{File: &transport.WorkspaceFile{Size: 4, Length: 4}}, "data"},
		{"destroy", transport.WorkspaceRequest{Op: transport.OpDestroy, ID: wsID}, "", transport.WorkspaceResult{}, ""},
		{"list", transport.WorkspaceRequest{Op: transport.OpList}, "",
			transport.WorkspaceResult{List: []transport.WorkspaceInfo{{ID: wsID}}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var tail []byte
			if tc.tail != "" {
				tail = []byte(tc.tail)
			}
			conn := newWSConn(okScript(t, tc.res, tail))
			r := wsReg(t, StatusActive, conn)
			var out bytes.Buffer
			res, err := r.Workspace(context.Background(), 1, tc.req, strings.NewReader(tc.body), &out)
			if err != nil {
				t.Fatal(err)
			}
			if res.Code != transport.CodeOK {
				t.Fatalf("code %q", res.Code)
			}
			data := <-conn.datas
			dec, err := transport.DecodeWorkspaceRequest(data)
			if err != nil || dec.Op != tc.req.Op {
				t.Fatalf("channel data %q: %v", data, err)
			}
			ch := <-conn.opened
			if string(ch.got()) != tc.body {
				t.Fatalf("body sent %q, want %q", ch.got(), tc.body)
			}
			if out.String() != tc.tail {
				t.Fatalf("out %q, want %q", out.String(), tc.tail)
			}
			select {
			case <-ch.closed:
			default:
				t.Fatal("channel left open")
			}
		})
	}
}

func TestWorkspaceBodyExactLength(t *testing.T) {
	req := transport.WorkspaceRequest{Op: transport.OpWrite, ID: wsID, Path: "a", Length: 4}
	for _, body := range []string{"abc", "abcde"} {
		conn := newWSConn(okScript(t, transport.WorkspaceResult{}, nil))
		r := wsReg(t, StatusActive, conn)
		_, err := r.Workspace(context.Background(), 1, req, strings.NewReader(body), nil)
		if !errors.Is(err, transport.ErrBodyLength) {
			t.Fatalf("body %q: err = %v", body, err)
		}
		ch := <-conn.opened
		if len(ch.got()) > 4 {
			t.Fatalf("sent %d bytes, more than announced", len(ch.got()))
		}
		select {
		case <-ch.closed:
		default:
			t.Fatal("channel left open")
		}
	}
	// A body is required when one is announced.
	r := wsReg(t, StatusActive, newWSConn(nil))
	if _, err := r.Workspace(context.Background(), 1, req, nil, nil); err == nil {
		t.Fatal("nil body with Length accepted")
	}
}

func TestWorkspaceReadBodyChecks(t *testing.T) {
	req := transport.WorkspaceRequest{Op: transport.OpRead, ID: wsID, Path: "a", Limit: 4}
	for name, c := range map[string]struct {
		file *transport.WorkspaceFile
		tail string
	}{
		"over limit": {&transport.WorkspaceFile{Length: 5}, "12345"},
		"short":      {&transport.WorkspaceFile{Length: 4}, "12"},
		"extra":      {&transport.WorkspaceFile{Length: 2}, "123"},
		"no file":    {nil, ""},
	} {
		conn := newWSConn(okScript(t, transport.WorkspaceResult{File: c.file}, []byte(c.tail)))
		r := wsReg(t, StatusActive, conn)
		if _, err := r.Workspace(context.Background(), 1, req, nil, io.Discard); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

func TestWorkspaceNonOKResultNoBody(t *testing.T) {
	req := transport.WorkspaceRequest{Op: transport.OpRead, ID: wsID, Path: "a"}
	conn := newWSConn(func(c *wsChan) {
		<-c.wclosed
		transport.WriteResult(c.pw, &transport.WorkspaceResult{Code: transport.CodeNotFound}, MaxWorkspaceResultBytes)
		c.pw.Close()
	})
	r := wsReg(t, StatusActive, conn)
	res, err := r.Workspace(context.Background(), 1, req, nil, io.Discard)
	if err != nil || res.Code != transport.CodeNotFound {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestWorkspaceResultTooLarge(t *testing.T) {
	conn := newWSConn(func(c *wsChan) {
		<-c.wclosed
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], MaxWorkspaceResultBytes+1)
		c.pw.Write(hdr[:])
		// The runner never needs to send the rest: central refuses on the header.
		<-c.closed
	})
	r := wsReg(t, StatusActive, conn)
	_, err := r.Workspace(context.Background(), 1, transport.WorkspaceRequest{Op: transport.OpList}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "exceeds limit") {
		t.Fatalf("err = %v", err)
	}
	if MaxWorkspaceResultBytes >= transport.MaxResultBytes {
		t.Fatal("workspace cap must be below the exec cap")
	}
}

func within5(t *testing.T, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { f(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("did not return promptly")
	}
}

func TestWorkspaceCancelAndKick(t *testing.T) {
	writeReq := transport.WorkspaceRequest{Op: transport.OpWrite, ID: wsID, Path: "a", Length: 3}
	readReq := transport.WorkspaceRequest{Op: transport.OpRead, ID: wsID, Path: "a", Limit: 8}
	hang := func(c *wsChan) { <-c.closed }
	// Result frame arrives, then the body never ends (no CloseWrite from the peer).
	stuckBody := func(c *wsChan) {
		<-c.wclosed
		okFrame(t, c.pw, transport.WorkspaceResult{File: &transport.WorkspaceFile{Length: 2}})
		c.pw.Write([]byte("ab"))
		<-c.closed
	}
	cases := []struct {
		name   string
		req    transport.WorkspaceRequest
		script func(*wsChan)
		block  bool
	}{
		{"during body", writeReq, hang, true},
		{"during result", writeReq, hang, false},
		{"during read body", readReq, stuckBody, false},
	}
	for _, kick := range []bool{false, true} {
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				conn := newWSConn(tc.script)
				conn.blockWrite = tc.block
				r := wsReg(t, StatusActive, conn)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				errc := make(chan error, 1)
				go func() {
					_, err := r.Workspace(ctx, 1, tc.req, strings.NewReader("abc"), io.Discard)
					errc <- err
				}()
				ch := <-conn.opened
				time.Sleep(20 * time.Millisecond)
				if kick {
					r.Kick(1, "revoked")
				} else {
					cancel()
				}
				within5(t, func() {
					err := <-errc
					var ke *KickError
					if kick && !errors.As(err, &ke) || !kick && !errors.Is(err, context.Canceled) {
						t.Errorf("kick=%v err = %v", kick, err)
					}
				})
				select {
				case <-ch.closed:
				case <-time.After(5 * time.Second):
					t.Fatal("channel not closed")
				}
			})
		}
	}
}

func TestWorkspaceCancelWhileOpening(t *testing.T) {
	gate := make(chan struct{})
	conn := &blockOpenConn{gate: gate, late: make(chan *wsChan, 1)}
	r := wsReg(t, StatusActive, conn)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	within5(t, func() {
		_, err := r.Workspace(ctx, 1, transport.WorkspaceRequest{Op: transport.OpList}, nil, nil)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v", err)
		}
	})
	close(gate)
	ch := <-conn.late
	select {
	case <-ch.closed: // orphan channel closed
	case <-time.After(5 * time.Second):
		t.Fatal("orphan channel not closed")
	}
}

type blockOpenConn struct {
	gate chan struct{}
	late chan *wsChan
}

func (c *blockOpenConn) OpenChannel(string, []byte) (ssh.Channel, <-chan *ssh.Request, error) {
	<-c.gate
	ch := newWSChan(false)
	c.late <- ch
	reqs := make(chan *ssh.Request)
	close(reqs)
	return ch, reqs, nil
}
func (c *blockOpenConn) Close() error { return nil }

func TestWorkspaceStatusGating(t *testing.T) {
	ops := map[string]transport.WorkspaceRequest{
		transport.OpCreate:  {Op: transport.OpCreate, ID: wsID, Spec: &transport.WorkspaceSpec{Image: "i"}},
		transport.OpExec:    {Op: transport.OpExec, ID: wsID, Argv: []string{"ls"}},
		transport.OpRead:    {Op: transport.OpRead, ID: wsID, Path: "a", Limit: 1},
		transport.OpWrite:   {Op: transport.OpWrite, ID: wsID, Path: "a"},
		transport.OpDestroy: {Op: transport.OpDestroy, ID: wsID},
		transport.OpList:    {Op: transport.OpList},
		"bogus":             {Op: "bogus", ID: wsID},
	}
	allowed := map[Status]map[string]bool{
		StatusActive:   {transport.OpCreate: true, transport.OpExec: true, transport.OpRead: true, transport.OpWrite: true, transport.OpDestroy: true, transport.OpList: true},
		StatusDisabled: {transport.OpRead: true, transport.OpDestroy: true, transport.OpList: true},
		StatusPending:  {},
	}
	for st, ok := range allowed {
		for op, req := range ops {
			conn := newWSConn(func(c *wsChan) {
				<-c.wclosed
				okFrame(t, c.pw, transport.WorkspaceResult{File: &transport.WorkspaceFile{}})
				c.pw.Close()
			})
			r := wsReg(t, st, conn)
			_, err := r.Workspace(context.Background(), 1, req, strings.NewReader(""), io.Discard)
			switch {
			case ok[op] && err != nil:
				t.Errorf("%s/%s: refused: %v", st, op, err)
			case !ok[op] && err == nil:
				t.Errorf("%s/%s: allowed", st, op)
			case !ok[op] && conn.opens() != 0:
				t.Errorf("%s/%s: channel opened despite refusal", st, op)
			}
			if !ok[op] && st == StatusPending && !errors.Is(err, ErrPending) {
				t.Errorf("%s/%s: err = %v", st, op, err)
			}
			if !ok[op] && st == StatusDisabled && op != "bogus" && !errors.Is(err, ErrDisabled) {
				t.Errorf("%s/%s: err = %v", st, op, err)
			}
		}
	}
	// Unknown status denies.
	conn := newWSConn(nil)
	r := wsReg(t, StatusActive, conn)
	sess := r.Get(1)
	sess.mu.Lock()
	sess.status = Status("weird")
	sess.mu.Unlock()
	if _, err := r.Workspace(context.Background(), 1, ops[transport.OpList], nil, nil); !errors.Is(err, ErrInvalidStatus) {
		t.Fatalf("err = %v", err)
	}
	// Offline runner.
	if _, err := newReg().Workspace(context.Background(), 9, ops[transport.OpList], nil, nil); err == nil {
		t.Fatal("offline runner accepted")
	}
}

func TestWorkspaceOpenRejections(t *testing.T) {
	for _, c := range []struct {
		reason ssh.RejectionReason
		want   error
	}{{ssh.Prohibited, ErrNoExecutor}, {ssh.ResourceShortage, ErrBusy}} {
		conn := newWSConn(nil)
		conn.openErr = &ssh.OpenChannelError{Reason: c.reason, Message: "x"}
		r := wsReg(t, StatusActive, conn)
		_, err := r.Workspace(context.Background(), 1, transport.WorkspaceRequest{Op: transport.OpList}, nil, nil)
		if !errors.Is(err, c.want) {
			t.Fatalf("reason %v: err = %v", c.reason, err)
		}
	}
	conn := newWSConn(nil)
	conn.openErr = errors.New("boom")
	r := wsReg(t, StatusActive, conn)
	_, err := r.Workspace(context.Background(), 1, transport.WorkspaceRequest{Op: transport.OpList}, nil, nil)
	if err == nil || errors.Is(err, ErrBusy) || errors.Is(err, ErrNoExecutor) {
		t.Fatalf("err = %v", err)
	}
}

func TestSnapshotCarriesHeartbeatFields(t *testing.T) {
	r := newReg()
	s, _ := reg(t, r, 1, StatusActive)
	s.Heartbeat(transport.Heartbeat{Slots: 4, Executor: "containerd", SecureRuntime: "gvisor", Images: []string{"a:*", "b:1"}})
	got := r.Snapshot()[0]
	if got.Slots != 4 || got.Executor != "containerd" || got.SecureRuntime != "gvisor" || len(got.Images) != 2 {
		t.Fatalf("%+v", got)
	}
	got.Images[0] = "mutated"
	if r.Snapshot()[0].Images[0] != "a:*" {
		t.Fatal("snapshot aliases the session's image slice")
	}
	var _ Live = r
}
