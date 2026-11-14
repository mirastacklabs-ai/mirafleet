//go:build linux

package ctrd

import (
	"context"
	"fmt"
	"strings"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"

	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
)

// List returns the workspace containers of this executor's owner (the mirafleet.runner label =
// Options.Runner) in the configured namespace; other owners' workspaces are invisible, so list and
// prune never touch them. With no owner configured it returns every workspace container.
func (e *Executor) List(ctx context.Context) ([]workspace.Info, error) {
	ctx = namespaces.WithNamespace(ctx, e.ns)
	cs, err := e.client.Containers(ctx, `labels."`+labelWorkspace+`"`)
	if err != nil {
		return nil, fmt.Errorf("%w: list containers: %v", workspace.ErrUnavailable, err)
	}
	out := make([]workspace.Info, 0, len(cs))
	for _, c := range cs {
		info, err := c.Info(ctx, containerd.WithoutRefreshedMetadata)
		if err != nil {
			if errdefs.IsNotFound(err) {
				continue // deleted while listing
			}
			return nil, fmt.Errorf("%w: container info: %v", workspace.ErrUnavailable, err)
		}
		if e.runner != "" && info.Labels[labelRunner] != e.runner {
			continue // another runner's workspace in a shared namespace (C-16)
		}
		id := workspace.ID(info.Labels[labelWorkspace])
		if !workspace.ValidID(id) {
			continue
		}
		wi := workspace.Info{ID: id, Image: info.Image, Created: info.CreatedAt, State: workspace.StateStopped}
		if len(info.Labels) > 0 {
			wi.Labels = map[string]string{}
			for k, v := range info.Labels {
				if rest, ok := strings.CutPrefix(k, labelUserPrefix); ok {
					wi.Labels[rest] = v
				}
			}
		}
		if t, err := c.Task(ctx, nil); err == nil {
			st, err := t.Status(ctx)
			switch {
			case err != nil:
				wi.State = workspace.StateUnknown
			case st.Status == containerd.Running:
				wi.State = workspace.StateRunning
			case st.Status == containerd.Created:
				wi.State = workspace.StateCreated
			default:
				wi.State = workspace.StateStopped
			}
		} else if !errdefs.IsNotFound(err) {
			wi.State = workspace.StateUnknown
		}
		out = append(out, wi)
	}
	e.fillDisk(ctx, out)
	return out, nil
}

// Bounds on the measuring List may do: at most listWalks stale workspaces within
// listWalkBudget. The rest report their last measurement (0 when never measured).
const (
	listWalks      = 4
	listWalkBudget = 3 * time.Second
)

// fillDisk sets Info.DiskBytes from the last disk measurement. A measurement older than
// diskInterval is refreshed (bounded as above); a fresh one costs no walk.
func (e *Executor) fillDisk(ctx context.Context, infos []workspace.Info) {
	t := e.disks()
	mctx, cancel := context.WithTimeout(ctx, listWalkBudget)
	defer cancel()
	walks := 0
	for i := range infos {
		id := infos[i].ID
		t.mu.Lock()
		en := t.m[id]
		t.mu.Unlock()
		if en != nil {
			if !en.mu.TryLock() { // a walk is in progress: do not wait for it
				continue
			}
			used, at, partial := en.used, en.measured, en.partial
			en.mu.Unlock()
			infos[i].DiskBytes = used
			if !at.IsZero() && time.Since(at) <= staleAfter(diskInterval, partial) {
				continue
			}
		}
		if walks >= listWalks || mctx.Err() != nil {
			continue
		}
		walks++
		if u, err := e.Usage(mctx, id); err == nil {
			infos[i].DiskBytes = u.Used
		}
	}
}
