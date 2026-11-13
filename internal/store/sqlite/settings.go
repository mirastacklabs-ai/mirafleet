package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/store"
)

// GetSetting returns a setting value. Values may be secret; never log them.
func (d *DB) GetSetting(ctx context.Context, key string) ([]byte, error) {
	var v []byte
	err := d.reader.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: setting", store.ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	if v == nil {
		v = []byte{}
	}
	return v, nil
}

func checkSetting(key string, value []byte) error {
	if key == "" || len(value) == 0 {
		return fmt.Errorf("%w: setting key and value must be non-empty", store.ErrInvalid)
	}
	return nil
}

// CreateSetting inserts a setting; ErrConflict if present.
func (d *DB) CreateSetting(ctx context.Context, key string, value []byte, now time.Time, ev store.AuditEvent) error {
	if err := checkSetting(key, value); err != nil {
		return err
	}
	if ev.Time.IsZero() {
		ev.Time = now
	}
	return d.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?,?)`, key, value); err != nil {
			if isUnique(err) {
				return fmt.Errorf("%w: setting exists", store.ErrConflict)
			}
			return err
		}
		_, err := insertAuditTx(ctx, tx, ev)
		return err
	})
}

// PutSetting upserts a setting.
func (d *DB) PutSetting(ctx context.Context, key string, value []byte, now time.Time, ev store.AuditEvent) error {
	if err := checkSetting(key, value); err != nil {
		return err
	}
	if ev.Time.IsZero() {
		ev.Time = now
	}
	return d.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?,?)
 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value); err != nil {
			return err
		}
		_, err := insertAuditTx(ctx, tx, ev)
		return err
	})
}
