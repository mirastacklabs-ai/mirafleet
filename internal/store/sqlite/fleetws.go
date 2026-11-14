package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/store"
)

// querier is the read half shared by *sql.DB, *sql.Tx and *sql.Conn.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

const wsCols = `executor, default_image, images, pull, network, egress, cpu_milli, memory_mb, pids,
 tmpfs_mb, disk_mb, idle_ttl_s, max_ttl_s, exec_default_s, exec_max_s, updated_at`

// wsUpsertSet assigns every non-key column from the proposed row (A-17: an UPSERT, not a delete+insert).
var wsUpsertSet = func() string {
	var parts []string
	for _, c := range strings.Split(wsCols, ",") {
		c = strings.TrimSpace(c)
		parts = append(parts, c+" = excluded."+c)
	}
	return strings.Join(parts, ", ")
}()

func secs(d time.Duration) int64 { return int64(d / time.Second) }

func jsonList(l []string) (string, error) {
	if l == nil {
		l = []string{}
	}
	b, err := json.Marshal(l)
	return string(b), err
}

// normalizeWS validates c and truncates durations to whole seconds, so what is stored equals
// what is returned.
func normalizeWS(c store.WorkspaceConfig) (store.WorkspaceConfig, error) {
	c.IdleTTL = c.IdleTTL.Truncate(time.Second)
	c.MaxTTL = c.MaxTTL.Truncate(time.Second)
	c.ExecDefault = c.ExecDefault.Truncate(time.Second)
	c.ExecMax = c.ExecMax.Truncate(time.Second)
	if err := c.Validate(); err != nil {
		return c, err
	}
	return c, nil
}

func wsArgs(c store.WorkspaceConfig, now time.Time) ([]any, error) {
	images, err := jsonList(c.Images)
	if err != nil {
		return nil, err
	}
	egress, err := jsonList(c.Egress)
	if err != nil {
		return nil, err
	}
	return []any{string(c.Executor), c.DefaultImage, images, string(c.Pull), string(c.Network), egress,
		c.CPUMilli, c.MemoryMB, c.Pids, c.TmpfsMB, c.DiskMB, secs(c.IdleTTL), secs(c.MaxTTL),
		secs(c.ExecDefault), secs(c.ExecMax), now.UnixMilli()}, nil
}

func scanWS(sc interface{ Scan(...any) error }, pre ...any) (store.WorkspaceConfig, []any, error) {
	var (
		c                                    store.WorkspaceConfig
		exec, pull, net, images, egress      string
		idle, maxTTL, execDef, execMax, upMS int64
	)
	dst := append(pre, &exec, &c.DefaultImage, &images, &pull, &net, &egress, &c.CPUMilli, &c.MemoryMB,
		&c.Pids, &c.TmpfsMB, &c.DiskMB, &idle, &maxTTL, &execDef, &execMax, &upMS)
	if err := sc.Scan(dst...); err != nil {
		return c, nil, err
	}
	c.Executor, c.Pull, c.Network = store.Executor(exec), store.PullPolicy(pull), store.NetworkMode(net)
	if err := json.Unmarshal([]byte(images), &c.Images); err != nil {
		return c, nil, fmt.Errorf("fleet_workspace images: %w", err)
	}
	if err := json.Unmarshal([]byte(egress), &c.Egress); err != nil {
		return c, nil, fmt.Errorf("fleet_workspace egress: %w", err)
	}
	if len(c.Egress) == 0 {
		c.Egress = nil
	}
	c.IdleTTL = time.Duration(idle) * time.Second
	c.MaxTTL = time.Duration(maxTTL) * time.Second
	c.ExecDefault = time.Duration(execDef) * time.Second
	c.ExecMax = time.Duration(execMax) * time.Second
	c.UpdatedAt = time.UnixMilli(upMS).UTC()
	return c, dst, nil
}

func insertFleetWorkspaceTx(ctx context.Context, tx *sql.Tx, fleetID int64, c store.WorkspaceConfig, now time.Time) error {
	args, err := wsArgs(c, now)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO fleet_workspace (fleet_id, `+wsCols+`)
 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, append([]any{fleetID}, args...)...)
	return err
}

func getFleetWorkspace(ctx context.Context, q querier, fleetID int64) (*store.WorkspaceConfig, error) {
	c, _, err := scanWS(q.QueryRowContext(ctx, `SELECT `+wsCols+` FROM fleet_workspace WHERE fleet_id = ?`, fleetID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// GetFleetWorkspace returns the workspace config of a workspace-mode fleet.
func (d *DB) GetFleetWorkspace(ctx context.Context, fleet string) (store.WorkspaceConfig, error) {
	f, err := d.GetFleet(ctx, fleet)
	if err != nil {
		return store.WorkspaceConfig{}, err
	}
	if f.Workspace == nil {
		return store.WorkspaceConfig{}, fmt.Errorf("%w: fleet %q has no workspace config", store.ErrNotFound, fleet)
	}
	return *f.Workspace, nil
}

// PutFleetWorkspace replaces the config of a workspace-mode fleet, atomically with ev.
func (d *DB) PutFleetWorkspace(ctx context.Context, fleet string, c store.WorkspaceConfig, now time.Time, ev store.AuditEvent) (store.WorkspaceConfig, error) {
	c, err := normalizeWS(c)
	if err != nil {
		return store.WorkspaceConfig{}, err
	}
	if ev.Time.IsZero() {
		ev.Time = now
	}
	args, err := wsArgs(c, now)
	if err != nil {
		return store.WorkspaceConfig{}, err
	}
	c.UpdatedAt = now.UTC().Truncate(time.Millisecond)
	err = d.withTx(ctx, func(tx *sql.Tx) error {
		var (
			id   int64
			mode string
		)
		if err := tx.QueryRowContext(ctx, `SELECT id, mode FROM fleets WHERE name = ?`, fleet).Scan(&id, &mode); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: fleet", store.ErrNotFound)
			}
			return err
		}
		if store.Mode(mode) != store.ModeWorkspace {
			return fmt.Errorf("%w: fleet %q is not in workspace mode", store.ErrInvalid, fleet)
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO fleet_workspace (fleet_id, `+wsCols+`)
 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
 ON CONFLICT(fleet_id) DO UPDATE SET `+wsUpsertSet, append([]any{id}, args...)...)
		if err != nil {
			return err
		}
		_, err = insertAuditTx(ctx, tx, ev)
		return err
	})
	if err != nil {
		return store.WorkspaceConfig{}, err
	}
	return c, nil
}

// loadAllFleetWorkspaces returns every config keyed by fleet id.
func (d *DB) loadAllFleetWorkspaces(ctx context.Context) (map[int64]*store.WorkspaceConfig, error) {
	rows, err := d.reader.QueryContext(ctx, `SELECT fleet_id, `+wsCols+` FROM fleet_workspace`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]*store.WorkspaceConfig{}
	for rows.Next() {
		var id int64
		c, _, err := scanWS(rows, &id)
		if err != nil {
			return nil, err
		}
		out[id] = &c
	}
	return out, rows.Err()
}
