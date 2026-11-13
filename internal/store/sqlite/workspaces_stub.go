package sqlite

import (
	"context"
	"errors"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/store"
)

// Placeholders for the workspace table methods; S4b replaces this file with workspaces.go.

var errS4b = errors.New("not implemented (S4b)")

func (d *DB) ReserveWorkspace(context.Context, store.Workspace, int, int, time.Time, store.AuditEvent) (store.Workspace, bool, error) {
	return store.Workspace{}, false, errS4b
}

func (d *DB) SetWorkspaceState(context.Context, string, []store.WorkspaceState, store.WorkspaceState, string, time.Time, store.AuditEvent) (store.Workspace, error) {
	return store.Workspace{}, errS4b
}

func (d *DB) TouchWorkspace(context.Context, string, time.Time) error { return errS4b }

func (d *DB) GetWorkspace(context.Context, string) (store.Workspace, error) {
	return store.Workspace{}, errS4b
}

func (d *DB) ListWorkspaces(context.Context, store.WorkspaceFilter) ([]store.Workspace, error) {
	return nil, errS4b
}

func (d *DB) CountLiveWorkspaces(context.Context, []int64) (map[int64]int, error) {
	return nil, errS4b
}

func (d *DB) CloseWorkspacesOf(context.Context, store.WorkspaceOwner, string, time.Time, store.AuditEvent) ([]string, error) {
	return nil, errS4b
}
