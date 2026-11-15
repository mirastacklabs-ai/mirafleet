package service

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// slowSender records the delivery order and how many sends overlap.
type slowSender struct {
	*fakeSender
	inflight, maxIn atomic.Int32
}

func (s *slowSender) SyncCredentials(ctx context.Context, id int64, b *transport.CredsSyncBody) error {
	n := s.inflight.Add(1)
	for {
		m := s.maxIn.Load()
		if n <= m || s.maxIn.CompareAndSwap(m, n) {
			break
		}
	}
	time.Sleep(2 * time.Millisecond)
	err := s.fakeSender.SyncCredentials(ctx, id, b)
	s.inflight.Add(-1)
	return err
}

// H-01: deliveries to one runner are serialised, so generations reach the runner in order.
func TestDeliverRunnerSerialised(t *testing.T) {
	ce := newCredEnv(t)
	ce.fleet("f")
	r := ce.enrollRunner("f", "r1", true)
	ce.setTok("tok", decoyTok)
	ce.assign("tok", "f", "r1")
	ss := &slowSender{fakeSender: ce.snd}
	ce.cs.cred.sender = ss
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = ce.cs.DeliverRunner(ce.ctx, r.ID) }()
	}
	wg.Wait()
	if ss.maxIn.Load() != 1 {
		t.Fatalf("%d deliveries to one runner overlapped", ss.maxIn.Load())
	}
	var prev int64
	ce.snd.mu.Lock()
	defer ce.snd.mu.Unlock()
	for _, s := range ce.snd.sent {
		if s.runner == r.ID && s.gen <= prev {
			t.Fatalf("generation %d arrived after %d", s.gen, prev)
		} else if s.runner == r.ID {
			prev = s.gen
		}
	}
}

// H-02: disabling withdraws the secrets with an empty creds_sync; enabling delivers again.
func TestDisableWithdrawsEnableDelivers(t *testing.T) {
	ce := newCredEnv(t)
	ce.fleet("f")
	r := ce.enrollRunner("f", "r1", true)
	ce.setTok("tok", decoyTok)
	ce.assign("tok", "f", "r1")
	if got, ok := ce.snd.last(r.ID); !ok || got.creds["tok"] == "" {
		t.Fatalf("not delivered: %+v", got)
	}
	if _, err := ce.cs.Disable(ce.ctx, ce.admin, SourceCLI, "f/r1"); err != nil {
		t.Fatal(err)
	}
	got, _ := ce.snd.last(r.ID)
	if len(got.creds) != 0 {
		t.Fatalf("disable did not send an empty set: %+v", got)
	}
	disabledGen := got.gen
	if _, err := ce.cs.Enable(ce.ctx, ce.admin, SourceCLI, "f/r1"); err != nil {
		t.Fatal(err)
	}
	got, _ = ce.snd.last(r.ID)
	if got.creds["tok"] != decoyTok || got.gen <= disabledGen {
		t.Fatalf("enable did not redeliver: %+v", got)
	}
}

// H-04: reusing a session with a different credentials list is a conflict; the same list reuses.
func TestReuseSessionCredentialsNarrowing(t *testing.T) {
	ce := newCredEnv(t)
	e := ce.env
	e.hostFleet("vm", false)
	e.hostRunner("vm")
	ce.setTok("a", "va")
	ce.setTok("b", "vb")
	ce.assign("a", "vm", "")
	ce.assign("b", "vm", "")
	op := e.operator("ann")
	w, _, err := ce.cs.OpenWorkspaceWith(e.ctx, op, SourceAPI, "vm", "s1", "", nil, "", "", []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	w2, _, err := ce.cs.OpenWorkspaceWith(e.ctx, op, SourceAPI, "vm", "s1", "", nil, "", "", []string{"b", "a"})
	if err != nil || w2.WsID != w.WsID {
		t.Fatalf("same set, other order: %+v %v", w2, err)
	}
	_, _, err = ce.cs.OpenWorkspaceWith(e.ctx, op, SourceAPI, "vm", "s1", "", nil, "", "", []string{"a"})
	if c := code(t, err); c != api.CodeConflict {
		t.Fatalf("narrower list on a reused session: %q", c)
	}
}

// capRing keeps the plaintext slices it is given or returns, to check they are cleared.
type capRing struct {
	testRing
	mu    sync.Mutex
	plain [][]byte
}

func (c *capRing) Seal(pt, aad []byte) ([]byte, error) {
	c.mu.Lock()
	c.plain = append(c.plain, pt)
	c.mu.Unlock()
	return c.testRing.Seal(pt, aad)
}

func (c *capRing) Open(ct, aad []byte) ([]byte, error) {
	pt, err := c.testRing.Open(ct, aad)
	c.mu.Lock()
	c.plain = append(c.plain, pt)
	c.mu.Unlock()
	return pt, err
}

// H-05: the plaintext JSON copy is zeroed after Seal and after Unmarshal.
func TestPlaintextCopiesCleared(t *testing.T) {
	ce := newCredEnv(t)
	ring := &capRing{testRing: testRing{ce.key}}
	svc, err := New(Config{Store: ce.db, Policy: ce.policy, Live: ce.live, HostKey: ce.host, OperatorConns: ce.conns,
		Now: func() time.Time { return t0 }, Logf: func(string, ...any) {},
		Credentials: CredentialsConfig{Key: ring, Sender: ce.snd}})
	if err != nil {
		t.Fatal(err)
	}
	ce.fleet("f")
	ce.enrollRunner("f", "r1", true)
	if _, err := svc.SetCredential(ce.ctx, ce.admin, SourceCLI, setParams("tok", transport.CredHTTPSToken, decoyTok), []byte(decoyTok)); err != nil {
		t.Fatal(err)
	}
	sc, err := ce.db.GetSealedCredential(ce.ctx, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.open(sc); err != nil {
		t.Fatal(err)
	}
	ring.mu.Lock()
	defer ring.mu.Unlock()
	if len(ring.plain) < 2 {
		t.Fatalf("captured %d plaintext slices", len(ring.plain))
	}
	for i, p := range ring.plain {
		for _, b := range p {
			if b != 0 {
				t.Fatalf("plaintext copy %d not cleared: %q", i, p)
			}
		}
	}
}
