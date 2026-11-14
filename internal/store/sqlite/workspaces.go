package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/store"
)

const (
	maxAuditWsIDs         = 4096 // byte cap of the ws_ids audit param
	defaultWorkspaceLimit = 100
	maxWorkspaceLimit     = 1000
)

const wsSelect = `SELECT w.id, w.ws_id, w.fleet_id, f.name, w.runner_id, r.name, w.operator_id, o.name,
 w.session, w.image, w.spec, w.state, w.reason, w.created_at, w.last_used_at, w.expires_at, w.closed_at
 FROM workspaces w
 JOIN fleets f ON f.id = w.fleet_id
 JOIN runners r ON r.id = w.runner_id
 JOIN operators o ON o.id = w.operator_id`

func scanWorkspace(sc interface{ Scan(...any) error }) (store.Workspace, error) {
	var (
		w                    store.Workspace
		state                string
		created, used, expir int64
		closed               sql.NullInt64
	)
	if err := sc.Scan(&w.ID, &w.WsID, &w.FleetID, &w.Fleet, &w.RunnerID, &w.Runner, &w.OperatorID, &w.Operator,
		&w.Session, &w.Image, &w.Spec, &state, &w.Reason, &created, &used, &expir, &closed); err != nil {
		return w, err
	}
	w.State = store.WorkspaceState(state)
	w.CreatedAt = time.UnixMilli(created).UTC()
	w.LastUsedAt = time.UnixMilli(used).UTC()
	w.ExpiresAt = time.UnixMilli(expir).UTC()
	if closed.Valid {
		w.ClosedAt = time.UnixMilli(closed.Int64).UTC()
	}
	return w, nil
}

func getWorkspaceBy(ctx context.Context, q querier, where string, arg any) (store.Workspace, error) {
	w, err := scanWorkspace(q.QueryRowContext(ctx, wsSelect+` WHERE `+where, arg))
	if errors.Is(err, sql.ErrNoRows) {
		return w, fmt.Errorf("%w: workspace", store.ErrNotFound)
	}
	return w, err
}

func validWsID(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func invalidWS(format string, a ...any) error {
	return fmt.Errorf("%w: workspace: %s", store.ErrInvalid, fmt.Sprintf(format, a...))
}

func exists(ctx context.Context, tx *sql.Tx, table string, id int64) (bool, error) {
	var one int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM `+table+` WHERE id = ?`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func countLive(ctx context.Context, tx *sql.Tx, col string, id int64) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM workspaces WHERE `+col+` = ? AND state IN ('opening','open','closing')`, id).Scan(&n)
	return n, err
}

// ReserveWorkspace places w on its runner; see store.Store.
func (d *DB) ReserveWorkspace(ctx context.Context, w store.Workspace, slots, maxPerOp int, now time.Time, ev store.AuditEvent) (store.Workspace, bool, error) {
	switch {
	case !validWsID(w.WsID):
		return store.Workspace{}, false, invalidWS("ws_id must be 32 lowercase hex")
	case w.FleetID <= 0 || w.RunnerID <= 0 || w.OperatorID <= 0:
		return store.Workspace{}, false, invalidWS("fleet, runner and operator ids are required")
	case w.Session == "" || w.Image == "":
		return store.Workspace{}, false, invalidWS("session and image are required")
	case w.Spec == "":
		return store.Workspace{}, false, invalidWS("spec is required")
	case !json.Valid([]byte(w.Spec)):
		return store.Workspace{}, false, invalidWS("spec is not valid JSON")
	case w.ExpiresAt.IsZero():
		return store.Workspace{}, false, invalidWS("expires_at is required")
	case slots < 1:
		return store.Workspace{}, false, invalidWS("slots must be >= 1")
	}
	if ev.Time.IsZero() {
		ev.Time = now
	}
	ev.Workspace = w.WsID
	var (
		out      store.Workspace
		existing bool
	)
	err := d.withTx(ctx, func(tx *sql.Tx) error {
		for _, c := range []struct {
			table, what string
			id          int64
		}{{"fleets", "fleet", w.FleetID}, {"runners", "runner", w.RunnerID}, {"operators", "operator", w.OperatorID}} {
			ok, err := exists(ctx, tx, c.table, c.id)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("%w: %s", store.ErrNotFound, c.what)
			}
		}
		var runnerFleet int64
		var runnerStatus string
		if err := tx.QueryRowContext(ctx, `SELECT fleet_id, status FROM runners WHERE id = ?`, w.RunnerID).
			Scan(&runnerFleet, &runnerStatus); err != nil {
			return err
		}
		if runnerFleet != w.FleetID {
			return invalidWS("runner does not belong to the fleet")
		}
		if runnerStatus == string(store.StatusRevoked) {
			return invalidWS("runner is revoked")
		}
		// 1. Sticky: a live (opening or open) workspace for the same (operator, fleet, session).
		cur, err := scanWorkspace(tx.QueryRowContext(ctx, wsSelect+
			` WHERE w.operator_id = ? AND w.fleet_id = ? AND w.session = ? AND w.state IN ('opening','open')`,
			w.OperatorID, w.FleetID, w.Session))
		if err == nil {
			out, existing = cur, true
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		// 2. Per-operator cap.
		if maxPerOp > 0 {
			n, err := countLive(ctx, tx, "operator_id", w.OperatorID)
			if err != nil {
				return err
			}
			if n >= maxPerOp {
				return fmt.Errorf("%w: operator holds %d workspaces", store.ErrFull, n)
			}
		}
		// 3. Runner slots.
		n, err := countLive(ctx, tx, "runner_id", w.RunnerID)
		if err != nil {
			return err
		}
		if n >= slots {
			return fmt.Errorf("%w: runner holds %d of %d slots", store.ErrFull, n, slots)
		}
		// 4. Insert with the audit row.
		_, err = tx.ExecContext(ctx, `INSERT INTO workspaces
 (ws_id, fleet_id, runner_id, operator_id, session, image, spec, state, reason, created_at, last_used_at, expires_at, closed_at)
 VALUES (?,?,?,?,?,?,?,'opening','',?,?,?,NULL)`,
			w.WsID, w.FleetID, w.RunnerID, w.OperatorID, w.Session, w.Image, w.Spec,
			now.UnixMilli(), now.UnixMilli(), w.ExpiresAt.UnixMilli())
		if err != nil {
			if isUnique(err) {
				return fmt.Errorf("%w: workspace exists", store.ErrConflict)
			}
			return err
		}
		if _, err := insertAuditTx(ctx, tx, ev); err != nil {
			return err
		}
		out, err = getWorkspaceBy(ctx, tx, `w.ws_id = ?`, w.WsID)
		return err
	})
	if err != nil {
		return store.Workspace{}, false, err
	}
	return out, existing, nil
}

// SetWorkspaceState moves a workspace between states, atomically with ev.
func (d *DB) SetWorkspaceState(ctx context.Context, wsID string, from []store.WorkspaceState, to store.WorkspaceState, reason string, now time.Time, ev store.AuditEvent) (store.Workspace, error) {
	if len(from) == 0 {
		return store.Workspace{}, invalidWS("from is empty")
	}
	for _, s := range from {
		if !s.Valid() {
			return store.Workspace{}, invalidWS("invalid from state %q", s)
		}
	}
	if !to.Valid() {
		return store.Workspace{}, invalidWS("invalid state %q", to)
	}
	if ev.Time.IsZero() {
		ev.Time = now
	}
	ev.Workspace = wsID
	var out store.Workspace
	err := d.withTx(ctx, func(tx *sql.Tx) error {
		var cur string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM workspaces WHERE ws_id = ?`, wsID).Scan(&cur); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: workspace", store.ErrNotFound)
			}
			return err
		}
		cs := store.WorkspaceState(cur)
		in := false
		for _, s := range from {
			in = in || s == cs
		}
		if !in {
			return fmt.Errorf("%w: workspace is %s", store.ErrConflict, cs)
		}
		if !store.CanTransitionWorkspace(cs, to) {
			return fmt.Errorf("%w: workspace cannot move from %s to %s", store.ErrConflict, cs, to)
		}
		var closed any
		if to.Terminal() {
			closed = now.UnixMilli()
		}
		if _, err := tx.ExecContext(ctx, `UPDATE workspaces SET state = ?, reason = ?, closed_at = ? WHERE ws_id = ?`,
			string(to), reason, closed, wsID); err != nil {
			return err
		}
		if _, err := insertAuditTx(ctx, tx, ev); err != nil {
			return err
		}
		var err error
		out, err = getWorkspaceBy(ctx, tx, `w.ws_id = ?`, wsID)
		return err
	})
	if err != nil {
		return store.Workspace{}, err
	}
	return out, nil
}

// TouchWorkspace sets last_used_at on an open workspace.
func (d *DB) TouchWorkspace(ctx context.Context, wsID string, now time.Time) error {
	return d.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE workspaces SET last_used_at = ? WHERE ws_id = ? AND state = 'open'`, now.UnixMilli(), wsID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n > 0 {
			return err
		}
		ok := 0
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM workspaces WHERE ws_id = ?`, wsID).Scan(&ok); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: workspace", store.ErrNotFound)
			}
			return err
		}
		return nil
	})
}

// GetWorkspace returns a workspace by ws_id.
func (d *DB) GetWorkspace(ctx context.Context, wsID string) (store.Workspace, error) {
	return getWorkspaceBy(ctx, d.reader, `w.ws_id = ?`, wsID)
}

// ListWorkspaces returns workspaces matching f ordered by id.
func (d *DB) ListWorkspaces(ctx context.Context, f store.WorkspaceFilter) ([]store.Workspace, error) {
	var (
		conds []string
		args  []any
	)
	if f.Fleet != "" {
		conds = append(conds, `f.name = ?`)
		args = append(args, f.Fleet)
	}
	if f.OperatorID != 0 {
		conds = append(conds, `w.operator_id = ?`)
		args = append(args, f.OperatorID)
	}
	if f.RunnerID != 0 {
		conds = append(conds, `w.runner_id = ?`)
		args = append(args, f.RunnerID)
	}
	if len(f.States) > 0 {
		ph := make([]string, len(f.States))
		for i, s := range f.States {
			if !s.Valid() {
				return nil, invalidWS("invalid state filter %q", s)
			}
			ph[i] = "?"
			args = append(args, string(s))
		}
		conds = append(conds, `w.state IN (`+strings.Join(ph, ",")+`)`)
	}
	q := wsSelect
	if len(conds) > 0 {
		q += ` WHERE ` + strings.Join(conds, ` AND `)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = defaultWorkspaceLimit
	}
	if limit > maxWorkspaceLimit {
		limit = maxWorkspaceLimit
	}
	rows, err := d.reader.QueryContext(ctx, q+` ORDER BY w.id LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Workspace
	for rows.Next() {
		w, err := scanWorkspace(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// CountLiveWorkspaces returns the slots held (opening, open, closing) per runner id.
func (d *DB) CountLiveWorkspaces(ctx context.Context, runnerIDs []int64) (map[int64]int, error) {
	out := make(map[int64]int, len(runnerIDs))
	if len(runnerIDs) == 0 {
		return out, nil
	}
	ph := make([]string, len(runnerIDs))
	args := make([]any, len(runnerIDs))
	for i, id := range runnerIDs {
		ph[i], args[i] = "?", id
		out[id] = 0
	}
	rows, err := d.reader.QueryContext(ctx, `SELECT runner_id, count(*) FROM workspaces
 WHERE state IN ('opening','open','closing') AND runner_id IN (`+strings.Join(ph, ",")+`) GROUP BY runner_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// CloseWorkspacesOf moves one owner's opening and open workspaces to closing.
func (d *DB) CloseWorkspacesOf(ctx context.Context, who store.WorkspaceOwner, reason string, now time.Time, ev store.AuditEvent) ([]string, error) {
	var col string
	var id int64
	switch {
	case who.RunnerID != 0 && who.OperatorID == 0:
		col, id = "runner_id", who.RunnerID
	case who.OperatorID != 0 && who.RunnerID == 0:
		col, id = "operator_id", who.OperatorID
	default:
		return nil, invalidWS("exactly one of runner and operator must be set")
	}
	if ev.Time.IsZero() {
		ev.Time = now
	}
	var ids []string
	err := d.withTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT ws_id FROM workspaces WHERE `+col+` = ? AND state IN ('opening','open') ORDER BY id`, id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, s)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE workspaces SET state = 'closing', reason = ? WHERE `+col+` = ? AND state IN ('opening','open')`, reason, id); err != nil {
			return err
		}
		if ev.Message == "" {
			ev.Message = fmt.Sprintf("%d workspaces", len(ids))
		}
		ev.Params = closeParams(ev.Params, ids)
		_, err = insertAuditTx(ctx, tx, ev)
		return err
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// closeParams returns a copy of p carrying the affected count and id list (C-12, B-12), so the audit
// row has them whatever Message the caller set. The list is capped to keep the row small.
func closeParams(p map[string]string, ids []string) map[string]string {
	out := make(map[string]string, len(p)+2)
	for k, v := range p {
		out[k] = v
	}
	out["count"] = strconv.Itoa(len(ids))
	list := strings.Join(ids, ",")
	if len(list) > maxAuditWsIDs {
		// Cut at a comma so no id is split (E-05); count still holds the full total.
		list = list[:max(strings.LastIndexByte(list[:maxAuditWsIDs+1], ','), 0)]
		out["ws_ids_truncated"] = "true"
	}
	out["ws_ids"] = list
	return out
}
