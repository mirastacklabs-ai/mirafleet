package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/store"
)

var tokenIDRE = regexp.MustCompile(`^[0-9a-f]{16}$`)

// secretHashLen is the length of a SHA-256 join-token secret hash.
const secretHashLen = 32

// CreateToken inserts a join token atomically with its audit event. The returned token has
// SecretHash zeroed (nil): the hash is never handed back.
func (d *DB) CreateToken(ctx context.Context, t store.JoinToken, now time.Time, ev store.AuditEvent) (store.JoinToken, error) {
	switch {
	case len(t.SecretHash) != secretHashLen:
		return store.JoinToken{}, fmt.Errorf("%w: secret hash must be %d bytes", store.ErrInvalid, secretHashLen)
	case !tokenIDRE.MatchString(t.TokenID):
		return store.JoinToken{}, fmt.Errorf("%w: token id must be 16 lowercase hex characters", store.ErrInvalid)
	case t.MaxUses < 1:
		return store.JoinToken{}, fmt.Errorf("%w: max uses must be >= 1", store.ErrInvalid)
	case t.ExpiresAt.IsZero() || ms(t.ExpiresAt) <= ms(now):
		return store.JoinToken{}, fmt.Errorf("%w: expiry must be after now", store.ErrInvalid)
	}
	if ev.Time.IsZero() {
		ev.Time = now
	}
	var out store.JoinToken
	err := d.withTx(ctx, func(tx *sql.Tx) error {
		var fleetID int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM fleets WHERE name = ?`, t.Fleet).Scan(&fleetID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: fleet", store.ErrNotFound)
		}
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO join_tokens
 (token_id, secret_hash, fleet_id, labels, auto_approve, max_uses, uses, expires_at, created_by, created_at)
 VALUES (?,?,?,?,?,?,0,?,?,?)`,
			t.TokenID, t.SecretHash, fleetID, marshalMap(t.Labels), t.AutoApprove, t.MaxUses,
			ms(t.ExpiresAt), t.CreatedBy, ms(now))
		if err != nil {
			if isUnique(err) {
				return fmt.Errorf("%w: token id exists", store.ErrConflict)
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
		out = t
		out.SecretHash = nil
		out.ID, out.FleetID, out.Uses, out.CreatedAt = id, fleetID, 0, now.UTC().Truncate(time.Millisecond)
		out.ExpiresAt = out.ExpiresAt.UTC().Truncate(time.Millisecond)
		out.RevokedAt = time.Time{}
		return nil
	})
	if err != nil {
		return store.JoinToken{}, err
	}
	return out, nil
}

// RevokeToken revokes a token; revoking twice is a no-op without an event.
func (d *DB) RevokeToken(ctx context.Context, tokenID string, now time.Time, ev store.AuditEvent) error {
	if ev.Time.IsZero() {
		ev.Time = now
	}
	return d.withTx(ctx, func(tx *sql.Tx) error {
		var revoked sql.NullInt64
		err := tx.QueryRowContext(ctx, `SELECT revoked_at FROM join_tokens WHERE token_id = ?`, tokenID).Scan(&revoked)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: token", store.ErrNotFound)
		}
		if err != nil {
			return err
		}
		if revoked.Valid {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE join_tokens SET revoked_at = ? WHERE token_id = ? AND revoked_at IS NULL`,
			ms(now), tokenID); err != nil {
			return err
		}
		_, err = insertAuditTx(ctx, tx, ev)
		return err
	})
}

// ListTokens returns tokens by id; SecretHash is never populated.
func (d *DB) ListTokens(ctx context.Context, fleet string) ([]store.JoinToken, error) {
	q := `SELECT t.id, t.token_id, t.fleet_id, f.name, t.labels, t.auto_approve, t.max_uses, t.uses,
 t.expires_at, t.revoked_at, t.created_by, t.created_at
 FROM join_tokens t JOIN fleets f ON f.id = t.fleet_id`
	var args []any
	if fleet != "" {
		q += ` WHERE f.name = ?`
		args = append(args, fleet)
	}
	rows, err := d.reader.QueryContext(ctx, q+` ORDER BY t.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.JoinToken
	for rows.Next() {
		var (
			t            store.JoinToken
			labels       string
			exp, created int64
			revoked      sql.NullInt64
		)
		if err := rows.Scan(&t.ID, &t.TokenID, &t.FleetID, &t.Fleet, &labels, &t.AutoApprove, &t.MaxUses,
			&t.Uses, &exp, &revoked, &t.CreatedBy, &created); err != nil {
			return nil, err
		}
		if t.Labels, err = unmarshalMap(labels); err != nil {
			return nil, fmt.Errorf("token %d: bad labels: %w", t.ID, err)
		}
		t.ExpiresAt, t.CreatedAt = fromMS(exp), fromMS(created)
		if revoked.Valid {
			t.RevokedAt = fromMS(revoked.Int64)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
