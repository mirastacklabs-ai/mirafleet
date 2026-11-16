package central

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/keys"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

func hsTotal(e *env) int {
	e.srv.hs.mu.Lock()
	defer e.srv.hs.mu.Unlock()
	return e.srv.hs.total
}

// A peer that never completes the handshake is dropped at the deadline, slot released.
func TestHandshakeDeadlineDropsSilentPeer(t *testing.T) {
	e := newEnvOpts(t, 15*time.Second, func(c *Config) { c.HandshakeTimeout = 300 * time.Millisecond })
	c, err := net.Dial("tcp", e.addr)
	must(t, err)
	defer c.Close()
	waitFor(t, func() bool { return hsTotal(e) == 1 })
	start := time.Now()
	waitFor(t, func() bool { return hsTotal(e) == 0 })
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("slot released after %v", d)
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 4096)
	var rerr error
	for rerr == nil { // the server's version banner may come first; then EOF
		_, rerr = c.Read(buf)
	}
	if ne, ok := rerr.(net.Error); ok && ne.Timeout() {
		t.Fatal("silent peer was not disconnected")
	}
	if DefaultHandshakeTimeout > 10*time.Second {
		t.Fatalf("default handshake timeout %v", DefaultHandshakeTimeout)
	}
}

// A source holding an authenticated connection can still connect with a full handshake
// bucket; an anonymous (enroll) connection does not earn the exemption.
func TestAuthedIPExemptFromHandshakeCap(t *testing.T) {
	e := newEnvOpts(t, 15*time.Second, func(c *Config) { c.MaxHandshakesPerIP = 2 })
	e.addOperator("op")
	_, err := keys.Generate(e.dir, "anon", "anon")
	must(t, err)
	enrol, err := e.dial(transport.UserEnroll, "anon")
	must(t, err)
	defer enrol.Close()
	op, err := e.dial(transport.UserOperator, "op")
	must(t, err)
	defer op.Close()
	waitFor(t, func() bool { return hsTotal(e) == 0 })
	for i := 0; i < 2; i++ {
		c, err := net.Dial("tcp", e.addr)
		must(t, err)
		defer c.Close()
	}
	waitFor(t, func() bool { return hsTotal(e) == 2 })
	op2, err := e.dial(transport.UserOperator, "op")
	if err != nil {
		t.Fatalf("authenticated source refused with a full bucket: %v", err)
	}
	op2.Close()
	// Unit: after the authenticated connections end, the cap applies again.
	l := newHSLimiter(1, 10)
	un := l.markAuthed("1.1.1.1:1")
	if _, _, ok := l.acquire("1.1.1.1:2"); !ok {
		t.Fatal("first acquire")
	}
	if _, _, ok := l.acquire("1.1.1.1:3"); !ok {
		t.Fatal("authed source must be exempt from the per-IP cap")
	}
	if _, _, ok := l.acquire("9.9.9.9:1"); !ok {
		t.Fatal("other source")
	}
	if _, _, ok := l.acquire("9.9.9.9:2"); ok {
		t.Fatal("unauthenticated source must hit the per-IP cap")
	}
	un()
	un()
	if _, _, ok := l.acquire("1.1.1.1:4"); ok {
		t.Fatal("exemption must end with the authenticated connection")
	}
}

// A peer that never reads a large response gets its whole connection closed at the write
// timeout (a local channel Close alone does not unblock the stalled write).
func TestStalledAPIWriteClosesConnection(t *testing.T) {
	oldT, oldW := api.WriteTimeout, writeResponse
	api.WriteTimeout = 300 * time.Millisecond
	// Real responses fit the SSH window; force one that does not.
	writeResponse = func(w io.Writer, _ *api.Response) error {
		junk := make([]byte, 64<<10)
		for i := 0; i < 128; i++ { // 8 MiB, far above the 2 MiB window
			if _, err := w.Write(junk); err != nil {
				return err
			}
		}
		return nil
	}
	t.Cleanup(func() { api.WriteTimeout, writeResponse = oldT, oldW })
	e := newEnv(t)
	e.addOperator("op")
	cl, err := e.dial(transport.UserOperator, "op")
	must(t, err)
	defer cl.Close()
	ch, reqs, err := cl.OpenChannel(transport.ChanAPI, nil)
	must(t, err)
	go ssh.DiscardRequests(reqs)
	rb, err := json.Marshal(api.Request{V: api.Version, ID: "t1", Method: api.MethodFleetsList})
	must(t, err)
	must(t, api.WriteFrame(ch, rb)) // and never read
	done := make(chan struct{})
	go func() { cl.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not close the connection of a peer that stopped reading")
	}
}

// The global enroll rate holds however the source addresses rotate or evict buckets.
func TestEnrollGlobalRate(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newIPLimiter(5, func() time.Time { return now })
	l.max = 2 // tiny table: every new source evicts another bucket
	l.setGlobal(3)
	for i := 0; i < 3; i++ {
		if !l.allow(net.JoinHostPort("10.0.0."+string(rune('1'+i)), "1")) {
			t.Fatalf("connection %d refused inside the global burst", i)
		}
	}
	for i := 0; i < 20; i++ {
		if l.allow(net.JoinHostPort("10.1.0."+string(rune('a'+i)), "1")) {
			t.Fatal("global rate bypassed by rotating sources")
		}
	}
	now = now.Add(21 * time.Second) // 1 token back at 3/min
	if !l.allow("10.2.0.1:1") || l.allow("10.2.0.2:1") {
		t.Fatal("global refill of one token expected")
	}
}

func TestEnrollGlobalRateConfigDefault(t *testing.T) {
	e := newEnv(t)
	if e.srv.cfg.EnrollGlobalRatePerMin != DefaultEnrollGlobalRatePerMin || e.srv.limiter.global == nil {
		t.Fatal("global enroll rate not enabled by default")
	}
}

// Teardown with a hung store completes promptly.
func TestRunnerTeardownDoesNotWaitOnStore(t *testing.T) {
	bs := &blockTouchStore{release: make(chan struct{})}
	e := newEnvOpts(t, 15*time.Second, func(c *Config) { bs.Store = c.Store; c.Store = bs }, "r1")
	t.Cleanup(func() { close(bs.release) })
	cl := e.rawRunner("r1", nil) // registers; the connect touch is now stuck in the store
	waitFor(t, func() bool { return e.session("r1") != nil })
	start := time.Now()
	cl.Close()
	waitFor(t, func() bool { return e.session("r1") == nil })
	// Server.Close drains touchers (bounded by storeTimeout), so it is not timed here.
	if d := time.Since(start); d > time.Second {
		t.Fatalf("connection teardown took %v with a hung store", d)
	}
}

type countTouchStore struct {
	store.Store
	n atomic.Int64
}

func (c *countTouchStore) TouchRunner(ctx context.Context, id int64, seen time.Time, info map[string]string) error {
	c.n.Add(1)
	return c.Store.TouchRunner(ctx, id, seen, info)
}

// The disconnect write is not skipped by edge throttling right after the connect write.
func TestDisconnectTouchNotThrottled(t *testing.T) {
	cs := &countTouchStore{}
	e := newEnvOpts(t, 15*time.Second, func(c *Config) {
		cs.Store = c.Store
		c.Store = cs
		c.TouchInterval = 10 * time.Minute // edge gap 1 min: a claimEdge on disconnect would fail
	}, "r1")
	cl := e.rawRunner("r1", nil)
	waitFor(t, func() bool { return cs.n.Load() >= 1 })
	cl.Close()
	waitFor(t, func() bool { return cs.n.Load() >= 2 })
	if strings.Contains(e.logText(), "touch runner") {
		t.Fatalf("touch failed: %s", e.logText())
	}
}

// closedFlagStore records TouchRunner calls that arrive after the store was "closed".
type closedFlagStore struct {
	store.Store
	closed atomic.Bool
	late   atomic.Int64
	calls  atomic.Int64
}

func (c *closedFlagStore) TouchRunner(ctx context.Context, id int64, seen time.Time, info map[string]string) error {
	c.calls.Add(1)
	if c.closed.Load() {
		c.late.Add(1)
	}
	time.Sleep(50 * time.Millisecond) // keep the queued disconnect write in flight at Close
	return c.Store.TouchRunner(ctx, id, seen, info)
}

// Server.Close drains touchers: no TouchRunner runs once the caller has closed the store.
func TestCloseDrainsTouchers(t *testing.T) {
	cs := &closedFlagStore{}
	e := newEnvOpts(t, 15*time.Second, func(c *Config) {
		cs.Store = c.Store
		c.Store = cs
	}, "r1")
	e.rawRunner("r1", nil)
	waitFor(t, func() bool { return e.session("r1") != nil })
	e.srv.Close() // drops the connection; the final touch must finish before Close returns
	cs.closed.Store(true)
	time.Sleep(300 * time.Millisecond)
	if n := cs.late.Load(); n != 0 {
		t.Fatalf("%d TouchRunner calls after the store was closed", n)
	}
	if cs.calls.Load() < 2 {
		t.Fatalf("expected connect and disconnect touches before Close returned, got %d", cs.calls.Load())
	}
}
