package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/store"
)

// CreateFleet inserts a fleet and its audit event in one transaction.
func (d *DB) CreateFleet(ctx context.Context, f store.Fleet, now time.Time, ev store.AuditEvent) (store.Fleet, error) {
	if !store.ValidFleetName(f.Name) {
		return store.Fleet{}, fmt.Errorf("%w: fleet name must match %s", store.ErrInvalid, store.FleetNameRE)
	}
	if !f.Mode.Valid() {
		return store.Fleet{}, fmt.Errorf("%w: fleet mode must be actions or workspace", store.ErrInvalid)
	}
	var cfg *store.WorkspaceConfig
	switch {
	case f.Mode == store.ModeWorkspace && f.Workspace == nil:
		return store.Fleet{}, fmt.Errorf("%w: a workspace-mode fleet needs a workspace config", store.ErrInvalid)
	case f.Mode != store.ModeWorkspace && f.Workspace != nil:
		return store.Fleet{}, fmt.Errorf("%w: only workspace-mode fleets take a workspace config", store.ErrInvalid)
	case f.Workspace != nil:
		c, err := normalizeWS(*f.Workspace)
		if err != nil {
			return store.Fleet{}, err
		}
		c.UpdatedAt = now.UTC().Truncate(time.Millisecond)
		cfg = &c
	}
	if ev.Time.IsZero() {
		ev.Time = now
	}
	out := store.Fleet{Name: f.Name, Mode: f.Mode, Confirm: f.Confirm, CreatedAt: now.UTC().Truncate(time.Millisecond), Workspace: cfg}
	err := d.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO fleets (name, mode, confirm, created_at) VALUES (?,?,?,?)`,
			f.Name, string(f.Mode), f.Confirm, now.UnixMilli())
		if err != nil {
			if isUnique(err) {
				return fmt.Errorf("%w: fleet %q already exists", store.ErrConflict, f.Name)
			}
			return err
		}
		if out.ID, err = res.LastInsertId(); err != nil {
			return err
		}
		if cfg != nil {
			if err := insertFleetWorkspaceTx(ctx, tx, out.ID, *cfg, now); err != nil {
				return err
			}
		}
		_, err = insertAuditTx(ctx, tx, ev)
		return err
	})
	if err != nil {
		return store.Fleet{}, err
	}
	return out, nil
}

func scanFleet(sc interface{ Scan(...any) error }, withCount bool) (store.Fleet, error) {
	var (
		f    store.Fleet
		mode string
		ts   int64
	)
	dst := []any{&f.ID, &f.Name, &mode, &f.Confirm, &ts}
	if withCount {
		dst = append(dst, &f.RunnerCount)
	}
	if err := sc.Scan(dst...); err != nil {
		return store.Fleet{}, err
	}
	f.Mode = store.Mode(mode)
	f.CreatedAt = time.UnixMilli(ts).UTC()
	return f, nil
}

// GetFleet returns the fleet by name.
func (d *DB) GetFleet(ctx context.Context, name string) (store.Fleet, error) {
	f, err := scanFleet(d.reader.QueryRowContext(ctx,
		`SELECT id, name, mode, confirm, created_at FROM fleets WHERE name = ?`, name), false)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Fleet{}, fmt.Errorf("%w: fleet", store.ErrNotFound)
	}
	if err != nil {
		return f, err
	}
	if f.Mode == store.ModeWorkspace {
		if f.Workspace, err = getFleetWorkspace(ctx, d.reader, f.ID); err != nil {
			return store.Fleet{}, err
		}
	}
	return f, nil
}

// ListFleets returns all fleets by name with the non-revoked runner count, in one query.
func (d *DB) ListFleets(ctx context.Context) ([]store.Fleet, error) {
	rows, err := d.reader.QueryContext(ctx, `SELECT f.id, f.name, f.mode, f.confirm, f.created_at,
 (SELECT COUNT(*) FROM runners r WHERE r.fleet_id = f.id AND r.status <> 'revoked')
 FROM fleets f ORDER BY f.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Fleet
	for rows.Next() {
		f, err := scanFleet(rows, true)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	cfgs, err := d.loadAllFleetWorkspaces(ctx)
	if err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Mode == store.ModeWorkspace {
			out[i].Workspace = cfgs[out[i].ID]
		}
	}
	return out, nil
}

// SetFleetConfirm sets the fleet's guard flag and returns the fleet, atomically with ev.
func (d *DB) SetFleetConfirm(ctx context.Context, name string, confirm bool, now time.Time, ev store.AuditEvent) (store.Fleet, error) {
	if !store.ValidFleetName(name) {
		return store.Fleet{}, fmt.Errorf("%w: fleet name must match %s", store.ErrInvalid, store.FleetNameRE)
	}
	if ev.Time.IsZero() {
		ev.Time = now
	}
	var out store.Fleet
	err := d.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE fleets SET confirm = ? WHERE name = ?`, confirm, name)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return fmt.Errorf("%w: fleet", store.ErrNotFound)
		}
		out, err = scanFleet(tx.QueryRowContext(ctx,
			`SELECT id, name, mode, confirm, created_at FROM fleets WHERE name = ?`, name), false)
		if err != nil {
			return err
		}
		if out.Mode == store.ModeWorkspace {
			if out.Workspace, err = getFleetWorkspace(ctx, tx, out.ID); err != nil {
				return err
			}
		}
		_, err = insertAuditTx(ctx, tx, ev)
		return err
	})
	if err != nil {
		return store.Fleet{}, err
	}
	return out, nil
}
