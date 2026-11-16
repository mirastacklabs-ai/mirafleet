package orch

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/keys"
	"github.com/mirastacklabs-ai/mirafleet/internal/opconf"
)

// A known_hosts with no entry is refused before any dial.
func TestEmptyKnownHostsNeverDials(t *testing.T) {
	for name, content := range map[string]string{"empty": "", "comments": "# nothing\n\n  \n"} {
		t.Run(name, func(t *testing.T) {
			dir := setup(t, opconf.Context{Name: "a", Addr: "127.0.0.1:1"})
			if err := os.WriteFile(opconf.KnownHostsPath(dir), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			d := &dialer{mk: func(int) (Conn, error) { return &fakeConn{}, nil }}
			o := newOrch(t, dir, d)
			_, err := o.FleetsList(context.Background(), "a")
			if !errors.Is(err, ErrNotSetUp) {
				t.Fatalf("err = %v, want ErrNotSetUp", err)
			}
			if d.n.Load() != 0 {
				t.Fatal("dialled with no pinned host key")
			}
		})
	}
}

// PinnedHostKeys wraps ErrHostKeyMismatch and orch shows the mismatch message.
func TestHostKeyMismatchMessage(t *testing.T) {
	pub := func() ssh.PublicKey {
		p, _, _ := ed25519.GenerateKey(rand.Reader)
		k, err := ssh.NewPublicKey(p)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	pinned, other := pub(), pub()
	kh := t.TempDir() + "/known_hosts"
	if err := os.WriteFile(kh, []byte("h:1 "+string(ssh.MarshalAuthorizedKey(pinned))), 0o600); err != nil {
		t.Fatal(err)
	}
	cb := keys.PinnedHostKeys(kh)
	addr := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}
	if err := cb("h:1", addr, pinned); err != nil {
		t.Fatalf("pinned key refused: %v", err)
	}
	mismatch := cb("h:1", addr, other)
	if !errors.Is(mismatch, keys.ErrHostKeyMismatch) {
		t.Fatalf("PinnedHostKeys error %v does not wrap keys.ErrHostKeyMismatch", mismatch)
	}

	d := &dialer{mk: func(int) (Conn, error) { return nil, fmt.Errorf("ssh: handshake failed: %w", mismatch) }}
	o := newOrch(t, setup(t, opconf.Context{Name: "a", Addr: "127.0.0.1:1"}), d)
	_, err := o.FleetsList(context.Background(), "a")
	if err == nil || !strings.Contains(err.Error(), "does not match the pinned key") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "SHA256") || strings.Contains(err.Error(), kh) {
		t.Fatalf("mismatch text leaks a fingerprint or path: %v", err)
	}
}

// A dial that finishes after the pool was replaced (re-login) is closed, not kept.
func TestRetiredPoolDropsLateDial(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 4)
	var first *fakeConn
	d := &dialer{mk: func(n int) (Conn, error) {
		c := &fakeConn{handler: fleetsHandler(api.FleetInfo{Name: "dev"})}
		if n == 1 {
			first = c
			started <- struct{}{}
			<-release
		}
		return c, nil
	}}
	dir := setup(t, opconf.Context{Name: "a", Addr: "127.0.0.1:1"})
	o := newOrch(t, dir, d)
	errc := make(chan error, 1)
	go func() {
		_, err := o.FleetsList(context.Background(), "a")
		errc <- err
	}()
	<-started
	// Re-login to another central under the same name.
	if err := opconf.SaveContexts(dir, &opconf.Contexts{Contexts: []opconf.Context{{Name: "a", Addr: "127.0.0.1:2"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := o.FleetsList(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-errc; err == nil {
		t.Fatal("the call on the retired pool succeeded")
	}
	eventually(t, "late connection closed", func() bool { return first.closed.Load() })
	o.mu.Lock()
	cur := o.pools["a"]
	o.mu.Unlock()
	cur.mu.Lock()
	n := 0
	if cur.cur != nil {
		n = 1
	}
	cur.mu.Unlock()
	if n != 1 {
		t.Fatal("the new pool has no connection")
	}
}

// The guarded cache is keyed by name and address, and concurrent refreshes share one call.
func TestGuardedCacheKeyedByAddrAndSingleFlight(t *testing.T) {
	var lists atomic.Int32
	d := &dialer{mk: func(n int) (Conn, error) {
		guarded := n == 1 // the first central guards prod, the second does not
		return &fakeConn{handler: func(ctx context.Context, m string, res any) error {
			lists.Add(1)
			time.Sleep(50 * time.Millisecond)
			res.(*api.FleetsListResult).Fleets = []api.FleetInfo{{Name: "prod", Confirm: guarded}}
			return nil
		}}, nil
	}}
	dir := setup(t, opconf.Context{Name: "a", Addr: "127.0.0.1:1", DefaultFleet: "prod"})
	o := newOrch(t, dir, d)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := o.Resolve(context.Background(), "a", ""); !errors.Is(err, ErrGuardedDefault) {
				t.Errorf("err = %v, want ErrGuardedDefault", err)
			}
		}()
	}
	wg.Wait()
	if n := lists.Load(); n != 1 {
		t.Fatalf("fleets.list calls = %d, want 1 (single flight)", n)
	}

	// Re-login to another central: the cached "prod is guarded" must not apply within the TTL.
	if err := opconf.SaveContexts(dir, &opconf.Contexts{Contexts: []opconf.Context{
		{Name: "a", Addr: "127.0.0.1:2", DefaultFleet: "prod"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Resolve(context.Background(), "a", ""); err != nil {
		t.Fatalf("stale guarded entry applied to the new central: %v", err)
	}
}

// A guarded refresh keyed by the old address of a re-logged-in context (FleetsList writes under
// the new address) must not return the old, expired entry as if it were fresh: it fails closed.
func TestGuardedRefreshAcrossReloginFailsClosed(t *testing.T) {
	d := &dialer{mk: func(int) (Conn, error) {
		return &fakeConn{handler: fleetsHandler(api.FleetInfo{Name: "prod", Confirm: true})}, nil
	}}
	// The context is already re-logged in to :2, but the caller still holds the key for :1.
	dir := setup(t, opconf.Context{Name: "a", Addr: "127.0.0.1:2", DefaultFleet: "prod"})
	o := newOrch(t, dir, d)
	oldKey := guardKey("a", "127.0.0.1:1")
	o.mu.Lock()
	o.guarded[oldKey] = guardEntry{at: o.cfg.Now().Add(-time.Hour), fleets: map[string]bool{"prod": false}}
	o.mu.Unlock()
	e, err := o.refreshGuarded(context.Background(), "a", oldKey)
	if !errors.Is(err, errRetired) {
		t.Fatalf("err = %v, entry = %+v; want errRetired", err, e)
	}
}
