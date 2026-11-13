package sqlite

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/store"
)

// dummyHash stands in for the stored hash when a token id is unknown, so unknown id and wrong
// secret run the same comparison.
var dummyHash = make([]byte, 32)

const runnerCols = `r.id, r.fleet_id, f.name, r.name, r.pubkey, r.fingerprint, r.status, r.labels, r.info,
 r.join_token_id, r.created_at, r.updated_at, r.last_seen_at`

// maxInfoBytes caps the JSON a runner may store in info (TouchRunner is high-frequency
// heartbeat traffic; keep rows small).
const maxInfoBytes = 16 << 10

const runnerFrom = ` FROM runners r JOIN fleets f ON f.id = r.fleet_id`

func marshalMap(m map[string]string) string {
	if len(m) == 0 {
		return "{}"
	}
	b, _ := json.Marshal(m) // map[string]string cannot fail
	return string(b)
}

func unmarshalMap(s string) (map[string]string, error) {
	m := map[string]string{}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, err
	}
	return m, nil
}

func ms(t time.Time) int64 { return t.UnixMilli() }

func fromMS(v int64) time.Time { return time.UnixMilli(v).UTC() }

func scanRunner(sc interface{ Scan(...any) error }) (store.Runner, error) {
	var (
		r                    store.Runner
		status, labels, info string
		jt, lastSeen         sql.NullInt64
		created, updated     int64
	)
	if err := sc.Scan(&r.ID, &r.FleetID, &r.Fleet, &r.Name, &r.PubKey, &r.Fingerprint, &status, &labels, &info,
		&jt, &created, &updated, &lastSeen); err != nil {
		return store.Runner{}, err
	}
	var err error
	if r.Labels, err = unmarshalMap(labels); err != nil {
		return store.Runner{}, fmt.Errorf("runner %d: bad labels: %w", r.ID, err)
	}
	if r.Info, err = unmarshalMap(info); err != nil {
		return store.Runner{}, fmt.Errorf("runner %d: bad info: %w", r.ID, err)
	}
	r.Status = store.Status(status)
	if jt.Valid {
		v := jt.Int64
		r.JoinTokenID = &v
	}
	r.CreatedAt, r.UpdatedAt = fromMS(created), fromMS(updated)
	if lastSeen.Valid {
		r.LastSeenAt = fromMS(lastSeen.Int64)
	}
	return r, nil
}

type queryer interface {
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

func getRunner(ctx context.Context, q queryer, where string, args ...any) (store.Runner, error) {
	r, err := scanRunner(q.QueryRowContext(ctx, `SELECT `+runnerCols+runnerFrom+` WHERE `+where, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return store.Runner{}, fmt.Errorf("%w: runner", store.ErrNotFound)
	}
	return r, err
}

// ListRunners returns the fleet's runners ordered by name (then id).
func (d *DB) ListRunners(ctx context.Context, fleet string, includeRevoked bool) ([]store.Runner, error) {
	var n int
	if err := d.reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM fleets WHERE name = ?`, fleet).Scan(&n); err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, fmt.Errorf("%w: fleet", store.ErrNotFound)
	}
	q := `SELECT ` + runnerCols + runnerFrom + ` WHERE f.name = ?`
	if !includeRevoked {
		q += ` AND r.status <> 'revoked'`
	}
	rows, err := d.reader.QueryContext(ctx, q+` ORDER BY r.name, r.id`, fleet)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Runner
	for rows.Next() {
		r, err := scanRunner(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetRunner returns the non-revoked runner.
func (d *DB) GetRunner(ctx context.Context, ref store.Ref) (store.Runner, error) {
	return getRunner(ctx, d.reader, `f.name = ? AND r.name = ? AND r.status <> 'revoked'`, ref.Fleet, ref.Runner)
}

// GetRunnerByKey returns the runner with that key, any status.
func (d *DB) GetRunnerByKey(ctx context.Context, pubKey []byte) (store.Runner, error) {
	return getRunner(ctx, d.reader, `r.pubkey = ?`, pubKey)
}

// GetRunnerByID returns the runner by id, any status.
func (d *DB) GetRunnerByID(ctx context.Context, id int64) (store.Runner, error) {
	return getRunner(ctx, d.reader, `r.id = ?`, id)
}

// TouchRunner records last_seen_at and info; never changes status.
func (d *DB) TouchRunner(ctx context.Context, id int64, seen time.Time, info map[string]string) error {
	infoJSON := marshalMap(info)
	if len(infoJSON) > maxInfoBytes {
		return fmt.Errorf("%w: runner info exceeds %d bytes", store.ErrInvalid, maxInfoBytes)
	}
	return d.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE runners SET last_seen_at = ?, info = ? WHERE id = ?`,
			ms(seen), infoJSON, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("%w: runner", store.ErrNotFound)
		}
		return nil
	})
}

// Enroll consumes one token use and inserts the runner, in one transaction.
func (d *DB) Enroll(ctx context.Context, tokenID string, secretHash []byte, now time.Time, nr store.Runner, ev store.AuditEvent) (store.Runner, error) {
	if !store.ValidRunnerName(nr.Name) {
		return store.Runner{}, fmt.Errorf("%w: invalid runner name", store.ErrInvalid)
	}
	if len(secretHash) != secretHashLen {
		return store.Runner{}, fmt.Errorf("%w: secret hash must be %d bytes", store.ErrInvalid, secretHashLen)
	}
	if len(nr.PubKey) == 0 || nr.Fingerprint == "" {
		return store.Runner{}, fmt.Errorf("%w: runner key and fingerprint are required", store.ErrInvalid)
	}
	if ev.Time.IsZero() {
		ev.Time = now
	}
	var out store.Runner
	err := d.withTx(ctx, func(tx *sql.Tx) error {
		// 1. look up the token.
		var (
			rowID, fleetID int64
			stored         []byte
			labels         string
			auto           bool
		)
		err := tx.QueryRowContext(ctx, `SELECT id, secret_hash, fleet_id, labels, auto_approve FROM join_tokens WHERE token_id = ?`,
			tokenID).Scan(&rowID, &stored, &fleetID, &labels, &auto)
		known := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if !known {
			stored = dummyHash
		}
		// 2. constant-time compare.
		match := subtle.ConstantTimeCompare(secretHash, stored) == 1
		if !known || !match {
			return fmt.Errorf("%w: join token", store.ErrNotFound)
		}
		// 3. staleness: conditional consume.
		res, err := tx.ExecContext(ctx, `UPDATE join_tokens SET uses = uses + 1
 WHERE id = ? AND revoked_at IS NULL AND expires_at > ? AND uses < max_uses`, rowID, ms(now))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return store.ErrStale
		}
		// 4. insert the runner.
		status := store.StatusPending
		if auto {
			status = store.StatusActive
		}
		res, err = tx.ExecContext(ctx, `INSERT INTO runners
 (fleet_id, name, pubkey, fingerprint, status, labels, join_token_id, info, created_at, updated_at)
 VALUES (?,?,?,?,?,?,?,'{}',?,?)`,
			fleetID, nr.Name, nr.PubKey, nr.Fingerprint, string(status), labels, rowID, ms(now), ms(now))
		if err != nil {
			if isUnique(err) {
				return fmt.Errorf("%w: runner name or key already enrolled", store.ErrConflict)
			}
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		if _, err := insertAuditTx(ctx, tx, ev); err != nil {
			return err
		}
		out, err = getRunner(ctx, tx, `r.id = ?`, id)
		return err
	})
	if err != nil {
		return store.Runner{}, err
	}
	return out, nil
}

// SetRunnerStatus moves a runner between states atomically with its audit event.
func (d *DB) SetRunnerStatus(ctx context.Context, id int64, from []store.Status, to store.Status, now time.Time, ev store.AuditEvent) (store.Runner, error) {
	if len(from) == 0 || !to.Valid() {
		return store.Runner{}, fmt.Errorf("%w: from set must be non-empty and to valid", store.ErrInvalid)
	}
	for _, s := range from {
		if !s.Valid() {
			return store.Runner{}, fmt.Errorf("%w: invalid status %q", store.ErrInvalid, string(s))
		}
	}
	if ev.Time.IsZero() {
		ev.Time = now
	}
	var out store.Runner
	err := d.withTx(ctx, func(tx *sql.Tx) error {
		cur, err := getRunner(ctx, tx, `r.id = ?`, id)
		if err != nil {
			return err
		}
		in := false
		for _, s := range from {
			if s == cur.Status {
				in = true
			}
		}
		if !in || !store.CanTransition(cur.Status, to) {
			return fmt.Errorf("%w: runner is %s, cannot move to %s", store.ErrConflict, cur.Status, to)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE runners SET status = ?, updated_at = ? WHERE id = ?`,
			string(to), ms(now), id); err != nil {
			if isUnique(err) { // e.g. re-activating would never clash, but stay safe
				return fmt.Errorf("%w: %v", store.ErrConflict, err)
			}
			return err
		}
		if _, err := insertAuditTx(ctx, tx, ev); err != nil {
			return err
		}
		out, err = getRunner(ctx, tx, `r.id = ?`, id)
		return err
	})
	if err != nil {
		return store.Runner{}, err
	}
	return out, nil
}
