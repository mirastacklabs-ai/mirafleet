package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// A-15: resource limits above the bounds are bad_request.
func TestSetFleetWorkspaceUpperBounds(t *testing.T) {
	e := newEnv(t)
	e.wsFleet("dev", false, nil)
	cur, err := e.db.GetFleetWorkspace(e.ctx, "dev")
	if err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*store.WorkspaceConfig){
		"memory": func(c *store.WorkspaceConfig) { c.MemoryMB = store.MaxMemoryMB + 1 },
		"disk":   func(c *store.WorkspaceConfig) { c.DiskMB = 1 << 40 },
		"cpu":    func(c *store.WorkspaceConfig) { c.CPUMilli = store.MaxCPUMilli + 1 },
		"pids":   func(c *store.WorkspaceConfig) { c.Pids = 1 << 40 },
		"tmpfs":  func(c *store.WorkspaceConfig) { c.TmpfsMB = store.MaxTmpfsMB + 1 },
	} {
		c := cur
		mut(&c)
		if cd := code(t, errOf(e.svc.SetFleetWorkspace(e.ctx, e.admin, SourceCLI, "dev", c))); cd != api.CodeBadRequest {
			t.Errorf("%s: %q", name, cd)
		}
	}
	c := cur
	c.MemoryMB, c.DiskMB = store.MaxMemoryMB, store.MaxDiskMB
	if _, err := e.svc.SetFleetWorkspace(e.ctx, e.admin, SourceCLI, "dev", c); err != nil {
		t.Fatalf("at the bound: %v", err)
	}
}

// C-14: a sticky open of a row still opening is a conflict, not reused=true.
func TestOpenWorkspaceStickyStillOpening(t *testing.T) {
	e := newEnv(t)
	e.wsFleet("dev", false, nil)
	r := e.wsRunner("dev", rspec{slots: 2})
	ann := e.operator("ann")
	w := e.rowAt(1, "dev", r, ann, store.WorkspaceOpening, t0)
	_, reused, err := e.svc.OpenWorkspace(e.ctx, ann, SourceAPI, "dev", w.Session, "", nil, "")
	if code(t, err) != api.CodeConflict || reused {
		t.Fatalf("got %v reused=%v", err, reused)
	}
	if n := len(e.wsCalls("create")); n != 0 {
		t.Fatalf("%d creates", n)
	}
}

// C-17: an inconsistent read reply sends nothing to the caller.
func TestReadWorkspaceFileInconsistentSendsNothing(t *testing.T) {
	e := newEnv(t)
	e.wsFleet("dev", false, nil)
	r := e.wsRunner("dev", rspec{slots: 2})
	op := e.operator("ann")
	w := e.rowAt(1, "dev", r, op, store.WorkspaceOpen, t0)
	e.live.wsFn = func(_ context.Context, _ int64, _ transport.WorkspaceRequest, _ io.Reader, out io.Writer) (*transport.WorkspaceResult, error) {
		_, _ = out.Write([]byte("partial"))
		return &transport.WorkspaceResult{Code: transport.CodeOK, File: &transport.WorkspaceFile{Length: 99}}, nil
	}
	var got bytes.Buffer
	if cd := code(t, errOf(e.svc.ReadWorkspaceFile(e.ctx, op, SourceAPI, w.WsID, "f", 0, 0, &got))); cd != api.CodeInternal {
		t.Fatalf("code %q", cd)
	}
	if got.Len() != 0 {
		t.Fatalf("wrote %q", got.String())
	}
	// A consistent reply is delivered whole.
	e.live.wsFn = func(_ context.Context, _ int64, _ transport.WorkspaceRequest, _ io.Reader, out io.Writer) (*transport.WorkspaceResult, error) {
		_, _ = out.Write([]byte("hello"))
		return &transport.WorkspaceResult{Code: transport.CodeOK, File: &transport.WorkspaceFile{Length: 5, Size: 5, EOF: true}}, nil
	}
	if _, err := e.svc.ReadWorkspaceFile(e.ctx, op, SourceAPI, w.WsID, "f", 0, 0, &got); err != nil || got.String() != "hello" {
		t.Fatalf("%v %q", err, got.String())
	}
}

// C-13: one hung runner does not stall the sweep of another.
func TestReapOneHungRunnerDoesNotStallOthers(t *testing.T) {
	e := newEnv(t)
	e.wsFleet("dev", false, nil)
	r1 := e.wsRunner("dev", rspec{slots: 5})
	r2 := e.wsRunner("dev", rspec{slots: 5})
	ann := e.operator("ann")
	w1 := e.rowAt(1, "dev", r1, ann, store.WorkspaceClosing, t0)
	w2 := e.rowAt(2, "dev", r2, ann, store.WorkspaceClosing, t0)
	release := make(chan struct{})
	var once sync.Once
	e.live.wsFn = func(ctx context.Context, id int64, req transport.WorkspaceRequest, _ io.Reader, _ io.Writer) (*transport.WorkspaceResult, error) {
		if id == r1.ID {
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil, ctx.Err()
		}
		return &transport.WorkspaceResult{Code: transport.CodeOK}, nil
	}
	defer once.Do(func() { close(release) })
	done := make(chan struct{})
	go func() { e.svc.ReapOnce(e.ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for e.state(w2.WsID).State != store.WorkspaceClosed {
		if time.Now().After(deadline) {
			t.Fatal("healthy runner's row was not closed while the other hangs")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if e.state(w1.WsID).State != store.WorkspaceClosing {
		t.Fatal("hung runner's row moved")
	}
	once.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ReapOnce did not return")
	}
}

// D-05: kill reports the resulting state.
func TestKillWorkspaceState(t *testing.T) {
	e := newEnv(t)
	e.wsFleet("dev", false, nil)
	r := e.wsRunner("dev", rspec{slots: 2})
	ann := e.operator("ann")
	w := e.rowAt(1, "dev", r, ann, store.WorkspaceOpen, t0)
	e.live.wsFn = func(context.Context, int64, transport.WorkspaceRequest, io.Reader, io.Writer) (*transport.WorkspaceResult, error) {
		return nil, errors.New("runner is not online")
	}
	got, err := e.svc.KillWorkspace(e.ctx, e.admin, SourceCLI, w.WsID)
	if err != nil || got.State != store.WorkspaceClosing {
		t.Fatalf("offline: %+v %v", got, err)
	}
	e.live.wsFn = nil
	got, err = e.svc.KillWorkspace(e.ctx, e.admin, SourceCLI, w.WsID)
	if err != nil || got.State != store.WorkspaceClosed {
		t.Fatalf("online: %+v %v", got, err)
	}
}
