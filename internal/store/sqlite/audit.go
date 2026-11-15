package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mirastacklabs-ai/mirafleet/internal/store"
)

const (
	defaultAuditLimit = 100
	maxAuditLimit     = 1000
)

const insertAudit = `INSERT INTO audit
 (ts, source, actor_kind, actor, remote, fleet, runner, subject, action, params, outcome, exit_code, duration_ms, message, request_id, workspace)
 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`

// insertAuditTx appends ev inside tx. ev.Time must be non-zero.
func insertAuditTx(ctx context.Context, tx *sql.Tx, ev store.AuditEvent) (int64, error) {
	if ev.Time.IsZero() {
		return 0, fmt.Errorf("%w: audit event time is zero", store.ErrInvalid)
	}
	var params any
	if len(ev.Params) > 0 {
		b, err := json.Marshal(ev.Params)
		if err != nil {
			return 0, fmt.Errorf("%w: audit params: %v", store.ErrInvalid, err)
		}
		params = string(b)
	}
	var exit any
	if ev.ExitCode != nil {
		exit = int64(*ev.ExitCode)
	}
	res, err := tx.ExecContext(ctx, insertAudit,
		ev.Time.UnixMilli(), ev.Source, ev.ActorKind, ev.Actor, ev.Remote, ev.Fleet, ev.Runner,
		ev.Subject, ev.Action, params, ev.Outcome, exit, ev.DurationMS, ev.Message, ev.RequestID, ev.Workspace)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// RecordAudit appends ev and returns its id.
func (d *DB) RecordAudit(ctx context.Context, ev store.AuditEvent) (int64, error) {
	if ev.Time.IsZero() {
		return 0, fmt.Errorf("%w: audit event time is zero", store.ErrInvalid)
	}
	var id int64
	err := d.withTx(ctx, func(tx *sql.Tx) (err error) {
		id, err = insertAuditTx(ctx, tx, ev)
		return err
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// ListAudit returns events with id > sinceID ascending, capped at maxAuditLimit rows
// (limit <= 0 means the default of 100). It is QueryAudit{AfterID: sinceID, Limit: limit}.
func (d *DB) ListAudit(ctx context.Context, sinceID int64, limit int) ([]store.AuditEvent, error) {
	return d.QueryAudit(ctx, store.AuditFilter{AfterID: sinceID, Limit: limit})
}

// likeEscaper escapes the LIKE wildcards and the escape character itself.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// QueryAudit returns events matching f in id order (descending when f.Desc), at most
// f.Limit rows (default 100, cap 1000). All values are bound parameters.
func (d *DB) QueryAudit(ctx context.Context, f store.AuditFilter) ([]store.AuditEvent, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultAuditLimit
	}
	if limit > maxAuditLimit {
		limit = maxAuditLimit
	}
	var (
		where []string
		args  []any
	)
	add := func(cond string, a ...any) {
		where = append(where, cond)
		args = append(args, a...)
	}
	if f.AfterID > 0 {
		add("id > ?", f.AfterID)
	}
	if f.BeforeID > 0 {
		add("id < ?", f.BeforeID)
	}
	if !f.From.IsZero() {
		add("ts >= ?", f.From.UnixMilli())
	}
	if !f.To.IsZero() {
		add("ts <= ?", f.To.UnixMilli())
	}
	for _, c := range []struct{ col, v string }{
		{"fleet", f.Fleet}, {"runner", f.Runner}, {"actor", f.Actor}, {"source", f.Source},
		{"outcome", f.Outcome}, {"workspace", f.Workspace}, {"request_id", f.RequestID},
	} {
		if c.v != "" {
			add(c.col+" = ?", c.v)
		}
	}
	if f.Action != "" {
		if f.ActionPrefix {
			// LIKE is ASCII case-insensitive in SQLite, so a case-sensitive substr check backs it.
			add(`action LIKE ? ESCAPE '\' AND substr(action, 1, ?) = ?`,
				likeEscaper.Replace(f.Action)+"%", utf8.RuneCountInString(f.Action), f.Action)
		} else {
			add("action = ?", f.Action)
		}
	}
	q := `SELECT id, ts, source, actor_kind, actor, remote, fleet, runner,
 subject, action, params, outcome, exit_code, duration_ms, message, request_id, workspace FROM audit`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	if f.Desc {
		q += " ORDER BY id DESC"
	} else {
		q += " ORDER BY id"
	}
	q += " LIMIT ?"
	args = append(args, limit)
	rows, err := d.reader.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.AuditEvent
	for rows.Next() {
		var (
			ev     store.AuditEvent
			ts     int64
			params sql.NullString
			exit   sql.NullInt64
		)
		if err := rows.Scan(&ev.ID, &ts, &ev.Source, &ev.ActorKind, &ev.Actor, &ev.Remote, &ev.Fleet,
			&ev.Runner, &ev.Subject, &ev.Action, &params, &ev.Outcome, &exit, &ev.DurationMS,
			&ev.Message, &ev.RequestID, &ev.Workspace); err != nil {
			return nil, err
		}
		ev.Time = time.UnixMilli(ts).UTC()
		if params.Valid {
			if err := json.Unmarshal([]byte(params.String), &ev.Params); err != nil {
				return nil, fmt.Errorf("audit %d: bad params: %w", ev.ID, err)
			}
		}
		if exit.Valid {
			c := int(exit.Int64)
			ev.ExitCode = &c
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}
