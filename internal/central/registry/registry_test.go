package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// fakeChan is an exec channel whose Read blocks until released (a JSON result follows) or closed.
type fakeChan struct {
	release chan struct{}
	closed  chan struct{}
	once    sync.Once
	rd      io.Reader
}

func newFakeChan() *fakeChan {
	return &fakeChan{release: make(chan struct{}), closed: make(chan struct{})}
}

func (c *fakeChan) Read(p []byte) (int, error) {
	if c.rd == nil {
		select {
		case <-c.release:
			b, _ := json.Marshal(transport.ExecResult{Stdout: "ok"})
			c.rd = bytesReader(b)
		case <-c.closed:
			return 0, io.EOF
		}
	}
	return c.rd.Read(p)
}
func (c *fakeChan) Write(p []byte) (int, error)                    { return len(p), nil }
func (c *fakeChan) Close() error                                   { c.once.Do(func() { close(c.closed) }); return nil }
func (c *fakeChan) CloseWrite() error                              { return nil }
func (c *fakeChan) SendRequest(string, bool, []byte) (bool, error) { return false, nil }
func (c *fakeChan) Stderr() io.ReadWriter                          { return nil }

type sliceReader struct{ b []byte }

func bytesReader(b []byte) io.Reader { return &sliceReader{b} }
func (r *sliceReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.b)
	r.b = r.b[n:]
	return n, nil
}

type fakeConn struct {
	mu     sync.Mutex
	chans  []*fakeChan
	opened chan *fakeChan
	closed chan struct{}
	once   sync.Once
}

func newFakeConn() *fakeConn {
	return &fakeConn{opened: make(chan *fakeChan, 1024), closed: make(chan struct{})}
}

func (c *fakeConn) OpenChannel(string, []byte) (ssh.Channel, <-chan *ssh.Request, error) {
	select {
	case <-c.closed:
		return nil, nil, io.EOF
	default:
	}
	ch := newFakeChan()
	c.mu.Lock()
	c.chans = append(c.chans, ch)
	c.mu.Unlock()
	c.opened <- ch
	reqs := make(chan *ssh.Request)
	close(reqs)
	return ch, reqs, nil
}

func (c *fakeConn) Close() error {
	c.once.Do(func() {
		close(c.closed)
		c.mu.Lock()
		for _, ch := range c.chans {
			ch.Close()
		}
		c.mu.Unlock()
	})
	return nil
}

func newReg() *Registry {
	r, err := New(Options{StaleAfter: time.Minute})
	if err != nil {
		panic(err)
	}
	return r
}

func reg(t *testing.T, r *Registry, id int64, st Status) (*Session, *fakeConn) {
	t.Helper()
	c := newFakeConn()
	s, err := r.Register(id, "r", "f", c, st, transport.Heartbeat{OS: "linux"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Unregister(s) })
	return s, c
}

type execOut struct {
	res *transport.ExecResult
	err error
}

func startExec(r *Registry, ctx context.Context, id int64) chan execOut {
	out := make(chan execOut, 1)
	go func() { res, err := r.Exec(ctx, id, transport.ExecRequest{Action: "x"}); out <- execOut{res, err} }()
	return out
}

func within(t *testing.T, ch chan execOut) execOut {
	t.Helper()
	select {
	case o := <-ch:
		return o
	case <-time.After(5 * time.Second):
		t.Fatal("exec did not return")
		return execOut{}
	}
}

func TestRegisterAfterTombstoneRefused(t *testing.T) {
	r := newReg()
	r.Tombstone(7)
	if _, err := r.Register(7, "r", "", newFakeConn(), StatusActive, transport.Heartbeat{}); !errors.Is(err, ErrTombstoned) {
		t.Fatalf("err = %v", err)
	}
	if len(r.Snapshot()) != 0 {
		t.Fatal("tombstoned id registered")
	}
	r.ClearTombstone(7)
	reg(t, r, 7, StatusActive)
}

func TestKickCancelsContextAndInflightExec(t *testing.T) {
	r := newReg()
	s, c := reg(t, r, 1, StatusActive)
	out := startExec(r, context.Background(), 1)
	<-c.opened
	r.Kick(1, "revoked")
	o := within(t, out)
	var ke *KickError
	if !errors.As(o.err, &ke) || ke.Reason != "revoked" {
		t.Fatalf("err = %v", o.err)
	}
	if s.Context().Err() == nil {
		t.Fatal("session context not cancelled")
	}
	select {
	case <-c.closed:
	default:
		t.Fatal("conn not closed")
	}
	if _, err := r.Exec(context.Background(), 1, transport.ExecRequest{}); err == nil {
		t.Fatal("exec on kicked session succeeded")
	}
}

func TestExecCallerContextCancels(t *testing.T) {
	r := newReg()
	_, c := reg(t, r, 1, StatusActive)
	ctx, cancel := context.WithCancel(context.Background())
	out := startExec(r, ctx, 1)
	<-c.opened
	cancel()
	if o := within(t, out); !errors.Is(o.err, context.Canceled) {
		t.Fatalf("err = %v", o.err)
	}
}

func TestDisabledRefusesNewLetsInflightFinish(t *testing.T) {
	r := newReg()
	_, c := reg(t, r, 1, StatusActive)
	out := startExec(r, context.Background(), 1)
	ch := <-c.opened
	mustSet(t, r, 1, StatusDisabled)
	if _, err := r.Exec(context.Background(), 1, transport.ExecRequest{}); !errors.Is(err, ErrDisabled) {
		t.Fatalf("err = %v", err)
	}
	close(ch.release)
	if o := within(t, out); o.err != nil || o.res.Stdout != "ok" {
		t.Fatalf("%+v", o)
	}
	mustSet(t, r, 1, StatusActive)
	out = startExec(r, context.Background(), 1)
	close((<-c.opened).release)
	if o := within(t, out); o.err != nil {
		t.Fatal(o.err)
	}
}

func TestPendingNeverExec(t *testing.T) {
	r := newReg()
	_, c := reg(t, r, 1, StatusPending)
	if _, err := r.Exec(context.Background(), 1, transport.ExecRequest{}); !errors.Is(err, ErrPending) {
		t.Fatalf("err = %v", err)
	}
	select {
	case <-c.opened:
		t.Fatal("channel opened for pending runner")
	default:
	}
}

func TestSnapshot(t *testing.T) {
	r := newReg()
	reg(t, r, 2, StatusActive)
	s1, _ := reg(t, r, 1, StatusPending)
	s1.Heartbeat(transport.Heartbeat{Hostname: "h"})
	snap := r.Snapshot()
	if len(snap) != 2 || snap[0].ID != 1 || snap[1].ID != 2 {
		t.Fatalf("%+v", snap)
	}
	a := snap[0]
	if a.Name != "r" || a.Fleet != "f" || a.Status != StatusPending || !a.Online || a.Info.Hostname != "h" || a.LastHeartbeat.IsZero() || a.Since.IsZero() {
		t.Fatalf("%+v", a)
	}
	r.Kick(1, "x")
	if r.Snapshot()[0].Online {
		t.Fatal("kicked session still online")
	}
}

func TestStaleSessionClosedAndOffline(t *testing.T) {
	r, err := New(Options{StaleAfter: 60 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	_, c := reg(t, r, 1, StatusActive)
	select {
	case <-c.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("stale conn not closed")
	}
	if r.Get(1) != nil {
		t.Fatal("stale session still online")
	}
}

func TestReregisterReplacesOld(t *testing.T) {
	r := newReg()
	old, oc := reg(t, r, 1, StatusActive)
	s2, _ := reg(t, r, 1, StatusActive)
	<-oc.closed
	if old.Context().Err() == nil || r.Get(1) != s2 {
		t.Fatal("new session did not replace old")
	}
	r.Unregister(old) // stale unregister must not remove the new one
	if r.Get(1) != s2 {
		t.Fatal("old unregister removed new session")
	}
}

func mustSet(t *testing.T, r *Registry, id int64, st Status) {
	t.Helper()
	if err := r.SetStatus(id, st); err != nil {
		t.Fatal(err)
	}
}

func TestNewRequiresStaleAfter(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		if r, err := New(Options{StaleAfter: d}); err == nil || r != nil {
			t.Fatalf("StaleAfter %v accepted", d)
		}
	}
}

func TestExecDenyByDefaultUnknownStatus(t *testing.T) {
	for _, bad := range []Status{"", "revoked", "bogus"} {
		r := newReg()
		if _, err := r.Register(1, "r", "", newFakeConn(), bad, transport.Heartbeat{}); !errors.Is(err, ErrInvalidStatus) {
			t.Fatalf("Register(%q) err = %v", bad, err)
		}
		if err := r.SetStatus(1, bad); !errors.Is(err, ErrInvalidStatus) {
			t.Fatalf("SetStatus(%q) err = %v", bad, err)
		}
		// Force an unknown status onto a live session: Exec must still refuse and open nothing.
		s, c := reg(t, r, 2, StatusActive)
		s.mu.Lock()
		s.status = bad
		s.mu.Unlock()
		if _, err := r.Exec(context.Background(), 2, transport.ExecRequest{}); !errors.Is(err, ErrInvalidStatus) {
			t.Fatalf("Exec with status %q err = %v", bad, err)
		}
		select {
		case <-c.opened:
			t.Fatalf("channel opened for status %q", bad)
		default:
		}
		if len(r.Snapshot()) != 1 {
			t.Fatal("invalid register left a session")
		}
	}
}

func TestRecordedStatusAppliedOnRegister(t *testing.T) {
	// disable while offline, then the runner connects claiming active
	r := newReg()
	mustSet(t, r, 1, StatusDisabled)
	_, c := reg(t, r, 1, StatusActive)
	if snap := r.Snapshot(); len(snap) != 1 || snap[0].Status != StatusDisabled {
		t.Fatalf("snapshot = %+v", snap)
	}
	if _, err := r.Exec(context.Background(), 1, transport.ExecRequest{}); !errors.Is(err, ErrDisabled) {
		t.Fatalf("err = %v", err)
	}
	select {
	case <-c.opened:
		t.Fatal("channel opened for disabled runner")
	default:
	}

	// approve while offline: pending -> active before the runner connects
	r = newReg()
	mustSet(t, r, 2, StatusActive)
	reg(t, r, 2, StatusPending)
	if snap := r.Snapshot(); len(snap) != 1 || snap[0].Status != StatusActive {
		t.Fatalf("snapshot = %+v", snap)
	}
	// without a record the supplied status stands
	reg(t, r, 3, StatusPending)
	if _, err := r.Exec(context.Background(), 3, transport.ExecRequest{}); !errors.Is(err, ErrPending) {
		t.Fatalf("err = %v", err)
	}
}

func TestGetByNameFollowsRegisterUnregister(t *testing.T) {
	r := newReg()
	s, _ := reg(t, r, 1, StatusActive)
	if r.GetByName("r") != s {
		t.Fatal("lookup by name failed")
	}
	r.Unregister(s)
	if r.GetByName("r") != nil {
		t.Fatal("name survived unregister")
	}
}

// Tombstone+Kick must leave no live session, however Register interleaves with them.
func TestTombstoneKickVsRegisterRace(t *testing.T) {
	r := newReg()
	const id = 5
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var sessions []*Session
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				s, err := r.Register(id, "r", "", newFakeConn(), StatusActive, transport.Heartbeat{})
				if err != nil {
					if !errors.Is(err, ErrTombstoned) {
						t.Errorf("err = %v", err)
					}
					continue
				}
				mu.Lock()
				sessions = append(sessions, s)
				mu.Unlock()
			}
		}()
	}
	for i := 0; i < 200; i++ {
		r.Tombstone(id)
		r.Kick(id, "revoked")
		if r.Get(id) != nil {
			t.Fatal("live session after Tombstone+Kick")
		}
		r.ClearTombstone(id) // let registrations race in again
	}
	r.Tombstone(id)
	r.Kick(id, "revoked")
	close(stop)
	wg.Wait()
	if r.Get(id) != nil {
		t.Fatal("Get(id) != nil after Tombstone+Kick")
	}
	// Tombstone is still set, so nothing registered after it; every registered session was
	// registered before the final Tombstone and the final Kick must have cancelled the current one.
	if _, err := r.Register(id, "r", "", newFakeConn(), StatusActive, transport.Heartbeat{}); !errors.Is(err, ErrTombstoned) {
		t.Fatalf("err = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, s := range sessions {
		r.Unregister(s)
	}
}

func TestStressConcurrent(t *testing.T) {
	r := newReg()
	deadline := time.Now().Add(1500 * time.Millisecond)
	var wg sync.WaitGroup
	run := func(f func(i int)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; time.Now().Before(deadline); i++ {
				f(i)
			}
		}()
	}
	for g := 0; g < 3; g++ {
		run(func(i int) {
			id := int64(i % 4)
			c := newFakeConn()
			s, err := r.Register(id, fmt.Sprintf("r%d", id), "", c, StatusActive, transport.Heartbeat{})
			if err != nil {
				if !errors.Is(err, ErrTombstoned) {
					t.Errorf("Register: %v", err)
				}
				return
			}
			s.Heartbeat(transport.Heartbeat{})
			time.Sleep(time.Millisecond)
			r.Unregister(s)
			c.Close()
		})
	}
	for g := 0; g < 3; g++ {
		run(func(i int) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			r.Exec(ctx, int64(i%4), transport.ExecRequest{})
		})
	}
	run(func(i int) { r.Kick(int64(i%4), "k") })
	run(func(i int) {
		r.Tombstone(int64(i % 4))
		r.ClearTombstone(int64((i + 1) % 4))
	})
	run(func(i int) {
		if err := r.SetStatus(int64(i%4), Status([]string{"active", "disabled", "pending"}[i%3])); err != nil {
			t.Errorf("SetStatus: %v", err)
		}
	})
	run(func(int) {
		for _, si := range r.Snapshot() {
			if !si.Status.Valid() {
				t.Errorf("invalid status %q in snapshot", si.Status)
			}
		}
	})
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("deadlock")
	}
	// Invariants at rest: the name index matches the sessions, and a tombstone+kick leaves
	// nothing live.
	r.mu.Lock()
	for name, id := range r.byName {
		if s := r.sessions[id]; s == nil || s.Name != name {
			t.Errorf("byName[%q]=%d inconsistent", name, id)
		}
	}
	for id, s := range r.sessions {
		if r.byName[s.Name] != id {
			t.Errorf("session %d missing from byName", id)
		}
	}
	r.mu.Unlock()
	for id := int64(0); id < 4; id++ {
		r.Tombstone(id)
		r.Kick(id, "end")
		if r.Get(id) != nil {
			t.Errorf("session %d live after Tombstone+Kick", id)
		}
	}
}
