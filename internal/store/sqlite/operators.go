package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/store"
)

const operatorCols = `id, name, pubkey, fingerprint, status, created_at, updated_at, last_seen_at`

func scanOperator(sc interface{ Scan(...any) error }) (store.Operator, error) {
	var (
		o                store.Operator
		status           string
		created, updated int64
		seen             sql.NullInt64
	)
	if err := sc.Scan(&o.ID, &o.Name, &o.PubKey, &o.Fingerprint, &status, &created, &updated, &seen); err != nil {
		return store.Operator{}, err
	}
	o.Status = store.OperatorStatus(status)
	o.CreatedAt, o.UpdatedAt = fromMS(created), fromMS(updated)
	if seen.Valid {
		o.LastSeenAt = fromMS(seen.Int64)
	}
	return o, nil
}

func getOperator(ctx context.Context, q queryer, where string, args ...any) (store.Operator, error) {
	o, err := scanOperator(q.QueryRowContext(ctx, `SELECT `+operatorCols+` FROM operators WHERE `+where, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return store.Operator{}, fmt.Errorf("%w: operator", store.ErrNotFound)
	}
	return o, err
}

// RegisterOperator inserts an active operator, idempotent per key.
func (d *DB) RegisterOperator(ctx context.Context, name string, pubKey []byte, fingerprint string, now time.Time, ev store.AuditEvent) (store.Operator, bool, error) {
	if !store.ValidOperatorName(name) {
		return store.Operator{}, false, fmt.Errorf("%w: operator name must match %s", store.ErrInvalid, store.OperatorNameRE)
	}
	if len(pubKey) == 0 || fingerprint == "" {
		return store.Operator{}, false, fmt.Errorf("%w: operator key and fingerprint are required", store.ErrInvalid)
	}
	if ev.Time.IsZero() {
		ev.Time = now
	}
	var (
		out     store.Operator
		created bool
	)
	err := d.withTx(ctx, func(tx *sql.Tx) error {
		cur, err := getOperator(ctx, tx, `pubkey = ?`, pubKey)
		switch {
		case err == nil:
			if cur.Status != store.OperatorActive {
				return fmt.Errorf("%w: operator key is revoked", store.ErrConflict)
			}
			out = cur
			return nil
		case !errors.Is(err, store.ErrNotFound):
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO operators (name, pubkey, fingerprint, status, created_at, updated_at)
 VALUES (?,?,?,'active',?,?)`, name, pubKey, fingerprint, ms(now), ms(now))
		if err != nil {
			if isUnique(err) {
				return fmt.Errorf("%w: operator name %q is in use", store.ErrConflict, name)
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
		created = true
		out, err = getOperator(ctx, tx, `id = ?`, id)
		return err
	})
	if err != nil {
		return store.Operator{}, false, err
	}
	return out, created, nil
}

func (d *DB) GetOperatorByKey(ctx context.Context, pubKey []byte) (store.Operator, error) {
	return getOperator(ctx, d.reader, `pubkey = ?`, pubKey)
}

func (d *DB) GetOperatorByID(ctx context.Context, id int64) (store.Operator, error) {
	return getOperator(ctx, d.reader, `id = ?`, id)
}

func (d *DB) GetOperatorByName(ctx context.Context, name string) (store.Operator, error) {
	return getOperator(ctx, d.reader, `name = ? AND status <> 'revoked'`, name)
}

// TouchOperator records last_seen_at; never changes status.
func (d *DB) TouchOperator(ctx context.Context, id int64, seen time.Time) error {
	return d.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE operators SET last_seen_at = ? WHERE id = ?`, ms(seen), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("%w: operator", store.ErrNotFound)
		}
		return nil
	})
}

// RevokeOperator revokes the non-revoked operator named name.
func (d *DB) RevokeOperator(ctx context.Context, name string, now time.Time, ev store.AuditEvent) (store.Operator, error) {
	if ev.Time.IsZero() {
		ev.Time = now
	}
	var out store.Operator
	err := d.withTx(ctx, func(tx *sql.Tx) error {
		cur, err := getOperator(ctx, tx, `name = ? AND status <> 'revoked'`, name)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE operators SET status = 'revoked', updated_at = ? WHERE id = ?`,
			ms(now), cur.ID); err != nil {
			return err
		}
		if _, err := insertAuditTx(ctx, tx, ev); err != nil {
			return err
		}
		out, err = getOperator(ctx, tx, `id = ?`, cur.ID)
		return err
	})
	if err != nil {
		return store.Operator{}, err
	}
	return out, nil
}

// ListOperators returns operators by id.
func (d *DB) ListOperators(ctx context.Context, includeRevoked bool) ([]store.Operator, error) {
	q := `SELECT ` + operatorCols + ` FROM operators`
	if !includeRevoked {
		q += ` WHERE status <> 'revoked'`
	}
	rows, err := d.reader.QueryContext(ctx, q+` ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Operator
	for rows.Next() {
		o, err := scanOperator(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
