//go:build linux

package ctrd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"

	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
)

// Disk accounting (M3 decision 8: a soft quota).
//
// The usage of a workspace is the allocated size (st_blocks) of everything in its directory,
// found by a bounded walk through os.Root that never follows symlinks and counts hardlinked
// files once. Usage is measured on demand: Usage(id) re-measures when the last measurement is
// older than diskInterval (60 s), and WriteFile re-measures when it is older than writeStale
// (2 s), then adds the bytes it writes itself. Over the quota (used >= quota) WriteFile fails
// with ErrQuota. At twice the quota the workspace is "over the kill line": Usage reports Kill
// and the runner's reaper, which polls Usage on its tick, should destroy the workspace. The
// quota is the runner's limits.disk unless the container carries a "mirafleet.disk" label
// (bytes) set at Create.
const (
	labelDisk = "mirafleet.disk"

	writeStale     = 2 * time.Second
	walkTimeout    = 20 * time.Second
	maxWalkEntries = 2_000_000
)

// DiskUsage is one workspace's disk accounting (shared with the runner's sampler).
type DiskUsage = workspace.DiskUsage

type diskEntry struct {
	mu       sync.Mutex // held while measuring: concurrent callers share one walk
	quota    int64
	used     int64
	partial  bool
	measured time.Time
}

type diskTable struct {
	mu sync.Mutex
	m  map[workspace.ID]*diskEntry
}

// diskTables keeps one table per executor without adding fields to Executor.
var diskTables sync.Map // *Executor -> *diskTable

func (e *Executor) disks() *diskTable {
	if t, ok := diskTables.Load(e); ok {
		return t.(*diskTable)
	}
	t, _ := diskTables.LoadOrStore(e, &diskTable{m: map[workspace.ID]*diskEntry{}})
	return t.(*diskTable)
}

// add accounts bytes just written.
func (t *diskTable) add(id workspace.ID, n int64) {
	t.mu.Lock()
	en := t.m[id]
	t.mu.Unlock()
	if en != nil {
		en.mu.Lock()
		en.used += n
		en.mu.Unlock()
	}
}

// forget drops entries of workspaces that no longer exist (called when Usage finds none).
func (t *diskTable) forget(id workspace.ID) {
	t.mu.Lock()
	delete(t.m, id)
	t.mu.Unlock()
}

func (e *Executor) quotaFor(ctx context.Context, id workspace.ID) (int64, error) {
	q := int64(e.cfg.Limits.Disk)
	c, err := e.client.LoadContainer(namespaces.WithNamespace(ctx, e.ns), ctrName(id))
	if err != nil {
		if errdefs.IsNotFound(err) {
			return 0, fmt.Errorf("%w: workspace %s", workspace.ErrNotFound, id)
		}
		return 0, unavailable("look up container", err)
	}
	labels, err := c.Labels(namespaces.WithNamespace(ctx, e.ns))
	if err != nil {
		return 0, unavailable("container labels", err)
	}
	if v, ok := labels[labelDisk]; ok {
		if n, perr := strconv.ParseInt(v, 10, 64); perr == nil && n > 0 && n < q {
			q = n // a label can only lower the runner's cap
		}
	}
	return q, nil
}

// usage returns the (possibly cached) usage; maxAge is how stale a measurement may be.
func (e *Executor) usage(ctx context.Context, id workspace.ID, ws *wsDir, maxAge time.Duration) (DiskUsage, error) {
	t := e.disks()
	t.mu.Lock()
	en := t.m[id]
	if en == nil {
		en = &diskEntry{}
		t.m[id] = en
	}
	t.mu.Unlock()

	en.mu.Lock()
	defer en.mu.Unlock()
	if en.quota == 0 {
		q, err := e.quotaFor(ctx, id)
		if err != nil {
			return DiskUsage{}, err
		}
		en.quota = q
	}
	if en.measured.IsZero() || time.Since(en.measured) > maxAge {
		wctx, cancel := context.WithTimeout(ctx, walkTimeout)
		used, partial, err := measure(wctx, ws.root.FS())
		cancel()
		if err != nil {
			return DiskUsage{}, fmt.Errorf("%w: measure disk usage: %v", workspace.ErrUnavailable, err)
		}
		en.used, en.partial, en.measured = used, partial, time.Now()
	}
	return en.snapshot(), nil
}

func (en *diskEntry) snapshot() DiskUsage {
	return DiskUsage{
		Used: en.used, Quota: en.quota, Partial: en.partial, Measured: en.measured,
		Over: en.used >= en.quota || en.partial,
		Kill: en.used >= 2*en.quota,
	}
}

// Usage reports the disk usage of a workspace, re-measuring when the last measurement is older
// than 60 seconds. The runner's reaper should call it on its tick and destroy workspaces for
// which Kill is set.
func (e *Executor) Usage(ctx context.Context, id workspace.ID) (DiskUsage, error) {
	ws, err := e.openWorkspace(ctx, id)
	if err != nil {
		if !isNotFound(err) {
			return DiskUsage{}, err
		}
		e.disks().forget(id)
		return DiskUsage{}, err
	}
	defer ws.Close()
	return e.usage(ctx, id, ws, diskInterval)
}

// checkQuota refuses a write when the workspace is over its quota.
func (e *Executor) checkQuota(ctx context.Context, id workspace.ID, ws *wsDir) error {
	u, err := e.usage(ctx, id, ws, writeStale)
	if err != nil {
		return err
	}
	if u.Over || u.Kill {
		return fmt.Errorf("%w: workspace uses %d of %d bytes", workspace.ErrQuota, u.Used, u.Quota)
	}
	return nil
}

// measure returns the allocated bytes under fsys. It stops at maxWalkEntries or when ctx ends
// (partial). Directories it cannot read are skipped. Symlinks are counted, never followed.
func measure(ctx context.Context, fsys fs.FS) (used int64, partial bool, err error) {
	type key struct{ dev, ino uint64 }
	seen := map[key]struct{}{}
	n := 0
	werr := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		n++
		if n > maxWalkEntries {
			partial = true
			return fs.SkipAll
		}
		if n%512 == 0 && ctx.Err() != nil {
			partial = true
			return fs.SkipAll
		}
		fi, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			used += fi.Size()
			return nil
		}
		if st.Nlink > 1 && !fi.IsDir() {
			k := key{uint64(st.Dev), uint64(st.Ino)}
			if _, dup := seen[k]; dup {
				return nil
			}
			seen[k] = struct{}{}
		}
		used += st.Blocks * 512
		return nil
	})
	return used, partial, werr
}

func isNotFound(err error) bool { return errors.Is(err, workspace.ErrNotFound) }

// diskInterval is how stale a measurement may be for Usage (a var so tests can shorten it).
var diskInterval = 60 * time.Second
