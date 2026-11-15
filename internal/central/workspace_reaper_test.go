package central

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/policy"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// wsListStore counts ListWorkspaces calls, flagging those after the store was "closed".
type wsListStore struct {
	store.Store
	closed    atomic.Bool
	late      atomic.Int64
	calls     atomic.Int64
	perRunner atomic.Int64 // calls filtered by runner (reconcile, and the reaper's closing pass)
	slow      time.Duration
}

func (w *wsListStore) ListWorkspaces(ctx context.Context, f store.WorkspaceFilter) ([]store.Workspace, error) {
	w.calls.Add(1)
	if f.RunnerID != 0 {
		w.perRunner.Add(1)
	}
	if w.closed.Load() {
		w.late.Add(1)
	}
	time.Sleep(w.slow)
	return w.Store.ListWorkspaces(ctx, f)
}

// svcOn builds a service over st with the given reaper interval.
func svcOn(t *testing.T, c *Config, st store.Store, every time.Duration) *service.Service {
	t.Helper()
	svc, err := service.New(service.Config{Store: st, Policy: policy.TablePolicy{}, Live: c.Registry,
		HostKey: c.HostKey.PublicKey(), Now: time.Now, OperatorConns: c.Operators, Logf: c.Logf, ReaperInterval: every})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// S17: the reaper runs inside the server wait group, and Close drains it before the caller may
// close the store: no store call arrives afterwards.
func TestReaperStopsBeforeStoreClose(t *testing.T) {
	ws := &wsListStore{slow: 15 * time.Millisecond}
	e := newEnvOpts(t, 15*time.Second, func(c *Config) {
		ws.Store = c.Store
		c.Store = ws
		c.Service = svcOn(t, c, ws, 5*time.Millisecond)
	}, "r1")
	waitFor(t, func() bool { return ws.calls.Load() >= 3 })
	e.srv.Close()
	ws.closed.Store(true)
	time.Sleep(200 * time.Millisecond)
	if n := ws.late.Load(); n != 0 {
		t.Fatalf("%d store calls after Close returned", n)
	}
}

// S17: central startup marks opening rows failed (a create we crashed in) before serving.
func TestStartupRecoversOpeningRows(t *testing.T) {
	var wsID string
	e := newEnvOpts(t, 15*time.Second, func(c *Config) {
		ctx := context.Background()
		now := time.Now()
		ev := auditEv("test.row")
		ev.Time = now
		rs, err := c.Store.ListRunners(ctx, testFleet, false)
		must(t, err)
		f, err := c.Store.GetFleet(ctx, testFleet)
		must(t, err)
		op, _, err := c.Store.RegisterOperator(ctx, "ann", []byte("k"), "SHA256:ann", now, ev)
		must(t, err)
		wsID = "0123456789abcdef0123456789abcdef"
		_, _, err = c.Store.ReserveWorkspace(ctx, store.Workspace{WsID: wsID, FleetID: f.ID, RunnerID: rs[0].ID, OperatorID: op.ID,
			Session: "s", Image: "i", Spec: `{}`, ExpiresAt: now.Add(time.Hour)}, 5, 0, now, ev)
		must(t, err)
	}, "r1")
	w, err := e.db.GetWorkspace(context.Background(), wsID)
	must(t, err)
	if w.State != store.WorkspaceFailed {
		t.Fatalf("state = %s, want failed", w.State)
	}
}

// S17: a runner's first heartbeat starts a reconcile (a store read of its rows) in the
// background, only when it has an executor; later heartbeats do not repeat it.
func TestReconcileOnFirstHeartbeat(t *testing.T) {
	for _, withExec := range []bool{true, false} {
		ws := &wsListStore{}
		e := newEnvOpts(t, 15*time.Second, func(c *Config) { ws.Store = c.Store; c.Store = ws; c.Service = svcOn(t, c, ws, 0) }, "r1")
		cl, err := e.dial(transport.UserRunner, "r1")
		must(t, err)
		defer cl.Close()
		hb := transport.Heartbeat{Hostname: "h", Proto: testProto}
		if withExec {
			hb.Slots, hb.Executor = 2, "containerd"
		}
		b, _ := json.Marshal(hb)
		for i := 0; i < 2; i++ {
			if ok, _, err := cl.SendRequest(transport.ReqHeartbeat, true, b); err != nil || !ok {
				t.Fatalf("heartbeat: %v %v", ok, err)
			}
		}
		time.Sleep(300 * time.Millisecond)
		got := ws.perRunner.Load()
		if withExec && got != 1 || !withExec && got != 0 {
			t.Fatalf("withExec=%v: reconcile reads = %d", withExec, got)
		}
	}
}
