package sqlite

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/mirastacklabs-ai/mirafleet/internal/store"
)

const (
	guiAdminCols   = `id, username, pw_hash, generation, status, created_at, updated_at`
	guiSessionCols = `id, admin_id, generation, created_at, last_active_at, expires_at, remote, user_agent`
)

func scanGUIAdmin(sc interface{ Scan(...any) error }) (store.GUIAdmin, error) {
	var (
		a        store.GUIAdmin
		created  int64
		updated  int64
		scanDest = []any{&a.ID, &a.Username, &a.PwHash, &a.Generation, &a.Status, &created, &updated}
	)
	if err := sc.Scan(scanDest...); err != nil {
		return store.GUIAdmin{}, err
	}
	a.CreatedAt = time.UnixMilli(created).UTC()
	a.UpdatedAt = time.UnixMilli(updated).UTC()
	return a, nil
}

// scanGUISession scans guiSessionCols plus any extra destinations.
func scanGUISession(sc interface{ Scan(...any) error }, extra ...any) (store.GUISession, error) {
	var (
		s                  store.GUISession
		created, act, expi int64
	)
	dst := append([]any{&s.ID, &s.AdminID, &s.Generation, &created, &act, &expi, &s.Remote, &s.UserAgent}, extra...)
	if err := sc.Scan(dst...); err != nil {
		return store.GUISession{}, err
	}
	s.CreatedAt = time.UnixMilli(created).UTC()
	s.LastActiveAt = time.UnixMilli(act).UTC()
	s.ExpiresAt = time.UnixMilli(expi).UTC()
	return s, nil
}

func checkHash32(hash []byte, what string) error {
	if len(hash) != 32 {
		return fmt.Errorf("%w: %s hash must be 32 bytes", store.ErrInvalid, what)
	}
	return nil
}

// clipUA cuts s to at most store.MaxGUIUserAgent bytes at a rune boundary.
func clipUA(s string) string {
	if len(s) <= store.MaxGUIUserAgent {
		return s
	}
	n := store.MaxGUIUserAgent
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func getActiveGUIAdmin(ctx context.Context, q querier) (store.GUIAdmin, error) {
	a, err := scanGUIAdmin(q.QueryRowContext(ctx,
		`SELECT `+guiAdminCols+` FROM gui_admins WHERE status = 'active'`))
	if errors.Is(err, sql.ErrNoRows) {
		return store.GUIAdmin{}, fmt.Errorf("%w: gui admin", store.ErrNotFound)
	}
	return a, err
}

func auditIfSet(ctx context.Context, tx *sql.Tx, ev store.AuditEvent, now time.Time) error {
	if ev.Action == "" {
		return nil
	}
	if ev.Time.IsZero() {
		ev.Time = now
	}
	_, err := insertAuditTx(ctx, tx, ev)
	return err
}

// GetGUIAdmin returns the active GUI admin or ErrNotFound.
func (d *DB) GetGUIAdmin(ctx context.Context) (store.GUIAdmin, error) {
	return getActiveGUIAdmin(ctx, d.reader)
}

func hasActiveGUIAdmin(ctx context.Context, tx *sql.Tx) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM gui_admins WHERE status = 'active'`).Scan(&n)
	return n > 0, err
}

// PutGUISetup upserts the single setup row.
func (d *DB) PutGUISetup(ctx context.Context, hash []byte, expires, now time.Time, ev store.AuditEvent) error {
	_, err := d.putGUISetup(ctx, hash, expires, now, ev, false)
	return err
}

// EnsureGUISetup is PutGUISetup that writes only when the row is missing or expired at now,
// all in one transaction (no check-then-act window). created=false means a live token was left
// untouched and nothing (no audit row) was written.
func (d *DB) EnsureGUISetup(ctx context.Context, hash []byte, expires, now time.Time, ev store.AuditEvent) (bool, error) {
	return d.putGUISetup(ctx, hash, expires, now, ev, true)
}

func (d *DB) putGUISetup(ctx context.Context, hash []byte, expires, now time.Time, ev store.AuditEvent, onlyIfNeeded bool) (bool, error) {
	if err := checkHash32(hash, "setup token"); err != nil {
		return false, err
	}
	if !expires.After(now) {
		return false, fmt.Errorf("%w: setup token expiry must be after now", store.ErrInvalid)
	}
	if ev.Time.IsZero() {
		ev.Time = now
	}
	created := false
	err := d.withTx(ctx, func(tx *sql.Tx) error {
		created = false
		if has, err := hasActiveGUIAdmin(ctx, tx); err != nil {
			return err
		} else if has {
			return fmt.Errorf("%w: an active GUI admin exists", store.ErrConflict)
		}
		if onlyIfNeeded {
			var exp int64
			err := tx.QueryRowContext(ctx, `SELECT expires_at FROM gui_setup WHERE id = 1`).Scan(&exp)
			if err == nil && now.UnixMilli() < exp {
				return nil
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO gui_setup (id, token_hash, expires_at, created_at) VALUES (1,?,?,?)
 ON CONFLICT(id) DO UPDATE SET token_hash = excluded.token_hash, expires_at = excluded.expires_at, created_at = excluded.created_at`,
			hash, expires.UnixMilli(), now.UnixMilli()); err != nil {
			return err
		}
		created = true
		_, err := insertAuditTx(ctx, tx, ev)
		return err
	})
	if err != nil {
		return false, err
	}
	return created, nil
}

// CheckGUISetup is the read-only token check that runs before the password is hashed (G-21).
func (d *DB) CheckGUISetup(ctx context.Context, hash []byte, now time.Time) error {
	if err := checkHash32(hash, "setup token"); err != nil {
		return err
	}
	var (
		stored  []byte
		expires int64
	)
	err := d.reader.QueryRowContext(ctx, `SELECT token_hash, expires_at FROM gui_setup WHERE id = 1`).Scan(&stored, &expires)
	missing := errors.Is(err, sql.ErrNoRows)
	if err != nil && !missing {
		return err
	}
	if missing {
		stored = make([]byte, 32)
	}
	if missing || subtle.ConstantTimeCompare(stored, hash) != 1 || now.UnixMilli() >= expires {
		return fmt.Errorf("%w: setup token", store.ErrStale)
	}
	return nil
}

// CompleteGUISetup verifies the setup token and creates the first admin in one transaction.
func (d *DB) CompleteGUISetup(ctx context.Context, hash []byte, a store.GUIAdmin, now time.Time, ev store.AuditEvent) (store.GUIAdmin, error) {
	if err := checkHash32(hash, "setup token"); err != nil {
		return store.GUIAdmin{}, err
	}
	if !store.ValidOperatorName(a.Username) {
		return store.GUIAdmin{}, fmt.Errorf("%w: username must match %s", store.ErrInvalid, store.OperatorNameRE)
	}
	if a.PwHash == "" {
		return store.GUIAdmin{}, fmt.Errorf("%w: empty password hash", store.ErrInvalid)
	}
	if ev.Time.IsZero() {
		ev.Time = now
	}
	out := store.GUIAdmin{Username: a.Username, PwHash: a.PwHash, Generation: 1, Status: store.GUIAdminActive,
		CreatedAt: now.UTC().Truncate(time.Millisecond), UpdatedAt: now.UTC().Truncate(time.Millisecond)}
	err := d.withTx(ctx, func(tx *sql.Tx) error {
		var (
			stored  []byte
			expires int64
		)
		err := tx.QueryRowContext(ctx, `SELECT token_hash, expires_at FROM gui_setup WHERE id = 1`).Scan(&stored, &expires)
		missing := errors.Is(err, sql.ErrNoRows)
		if err != nil && !missing {
			return err
		}
		// Constant-time compare against a dummy when the row is missing, so timing does not
		// tell "no setup pending" from "wrong token".
		if missing {
			stored = make([]byte, 32)
		}
		match := subtle.ConstantTimeCompare(stored, hash) == 1
		if missing || !match || now.UnixMilli() >= expires {
			return fmt.Errorf("%w: setup token", store.ErrStale)
		}
		if has, err := hasActiveGUIAdmin(ctx, tx); err != nil {
			return err
		} else if has {
			return fmt.Errorf("%w: an active GUI admin exists", store.ErrConflict)
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO gui_admins (username, pw_hash, generation, status, created_at, updated_at)
 VALUES (?,?,1,'active',?,?)`, out.Username, out.PwHash, now.UnixMilli(), now.UnixMilli())
		if err != nil {
			if isUnique(err) {
				return fmt.Errorf("%w: an active GUI admin exists", store.ErrConflict)
			}
			return err
		}
		if out.ID, err = res.LastInsertId(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM gui_setup WHERE id = 1`); err != nil {
			return err
		}
		_, err = insertAuditTx(ctx, tx, ev)
		return err
	})
	if err != nil {
		return store.GUIAdmin{}, err
	}
	return out, nil
}

func checkSessionIn(s store.GUISession) error {
	if err := checkHash32(s.TokenHash, "session token"); err != nil {
		return err
	}
	if s.ExpiresAt.IsZero() {
		return fmt.Errorf("%w: session expiry is zero", store.ErrInvalid)
	}
	return nil
}

func insertGUISessionTx(ctx context.Context, tx *sql.Tx, s store.GUISession, now time.Time) (store.GUISession, error) {
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}
	if s.LastActiveAt.IsZero() {
		s.LastActiveAt = s.CreatedAt
	}
	s.UserAgent = clipUA(s.UserAgent)
	res, err := tx.ExecContext(ctx, `INSERT INTO gui_sessions
 (token_hash, admin_id, generation, created_at, last_active_at, expires_at, remote, user_agent)
 VALUES (?,?,?,?,?,?,?,?)`, s.TokenHash, s.AdminID, s.Generation, s.CreatedAt.UnixMilli(),
		s.LastActiveAt.UnixMilli(), s.ExpiresAt.UnixMilli(), s.Remote, s.UserAgent)
	if err != nil {
		if isUnique(err) {
			return store.GUISession{}, fmt.Errorf("%w: duplicate session token", store.ErrConflict)
		}
		return store.GUISession{}, err
	}
	if s.ID, err = res.LastInsertId(); err != nil {
		return store.GUISession{}, err
	}
	s.TokenHash = nil
	s.CreatedAt = s.CreatedAt.UTC().Truncate(time.Millisecond)
	s.LastActiveAt = s.LastActiveAt.UTC().Truncate(time.Millisecond)
	s.ExpiresAt = s.ExpiresAt.UTC().Truncate(time.Millisecond)
	return s, nil
}

// SetGUIPassword swaps the password under a generation compare-and-swap, kills every session
// of the admin and inserts the re-issued current one, in one transaction.
func (d *DB) SetGUIPassword(ctx context.Context, id, generation int64, hash string, keep store.GUISession, now time.Time, ev store.AuditEvent) (store.GUIAdmin, error) {
	if hash == "" {
		return store.GUIAdmin{}, fmt.Errorf("%w: empty password hash", store.ErrInvalid)
	}
	if err := checkSessionIn(keep); err != nil {
		return store.GUIAdmin{}, err
	}
	if ev.Time.IsZero() {
		ev.Time = now
	}
	var out store.GUIAdmin
	err := d.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE gui_admins SET pw_hash = ?, generation = generation + 1, updated_at = ?
 WHERE id = ? AND status = 'active' AND generation = ?`, hash, now.UnixMilli(), id, generation)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			var g int64
			err := tx.QueryRowContext(ctx, `SELECT generation FROM gui_admins WHERE id = ? AND status = 'active'`, id).Scan(&g)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: gui admin", store.ErrNotFound)
			}
			if err != nil {
				return err
			}
			return fmt.Errorf("%w: credential generation changed", store.ErrConflict)
		}
		if out, err = scanGUIAdmin(tx.QueryRowContext(ctx, `SELECT `+guiAdminCols+` FROM gui_admins WHERE id = ?`, id)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM gui_sessions WHERE admin_id = ?`, id); err != nil {
			return err
		}
		keep.AdminID, keep.Generation = id, out.Generation
		if _, err := insertGUISessionTx(ctx, tx, keep, now); err != nil {
			return err
		}
		_, err = insertAuditTx(ctx, tx, ev)
		return err
	})
	if err != nil {
		return store.GUIAdmin{}, err
	}
	return out, nil
}

// ResetGUIAdmin marks the active admin reset and deletes every session, atomically with ev.
func (d *DB) ResetGUIAdmin(ctx context.Context, now time.Time, ev store.AuditEvent) error {
	if ev.Time.IsZero() {
		ev.Time = now
	}
	return d.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE gui_admins SET status = 'reset', updated_at = ? WHERE status = 'active'`, now.UnixMilli()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM gui_sessions`); err != nil {
			return err
		}
		_, err := insertAuditTx(ctx, tx, ev)
		return err
	})
}

// CreateGUISession inserts a session for the active admin and evicts that admin's oldest rows
// beyond maxPerAdmin, atomically with ev.
func (d *DB) CreateGUISession(ctx context.Context, s store.GUISession, maxPerAdmin int, now time.Time, ev store.AuditEvent) (store.GUISession, error) {
	if err := checkSessionIn(s); err != nil {
		return store.GUISession{}, err
	}
	if maxPerAdmin <= 0 {
		return store.GUISession{}, fmt.Errorf("%w: maxPerAdmin must be positive", store.ErrInvalid)
	}
	if ev.Time.IsZero() {
		ev.Time = now
	}
	var out store.GUISession
	err := d.withTx(ctx, func(tx *sql.Tx) error {
		var gen int64
		err := tx.QueryRowContext(ctx, `SELECT generation FROM gui_admins WHERE id = ? AND status = 'active'`, s.AdminID).Scan(&gen)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: gui admin", store.ErrNotFound)
		}
		if err != nil {
			return err
		}
		if gen != s.Generation {
			return fmt.Errorf("%w: credential generation changed", store.ErrConflict)
		}
		if out, err = insertGUISessionTx(ctx, tx, s, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM gui_sessions WHERE admin_id = ? AND id NOT IN
 (SELECT id FROM gui_sessions WHERE admin_id = ? ORDER BY id DESC LIMIT ?)`, s.AdminID, s.AdminID, maxPerAdmin); err != nil {
			return err
		}
		_, err = insertAuditTx(ctx, tx, ev)
		return err
	})
	if err != nil {
		return store.GUISession{}, err
	}
	return out, nil
}

// GetGUISession returns the session and its admin; the admin must be active.
func (d *DB) GetGUISession(ctx context.Context, tokenHash []byte) (store.GUISession, store.GUIAdmin, error) {
	if len(tokenHash) != 32 {
		return store.GUISession{}, store.GUIAdmin{}, fmt.Errorf("%w: gui session", store.ErrNotFound)
	}
	var (
		a                  store.GUIAdmin
		aCreated, aUpdated int64
	)
	s, err := scanGUISession(d.reader.QueryRowContext(ctx, `SELECT s.id, s.admin_id, s.generation, s.created_at,
 s.last_active_at, s.expires_at, s.remote, s.user_agent, a.id, a.username, a.pw_hash, a.generation, a.status, a.created_at, a.updated_at
 FROM gui_sessions s JOIN gui_admins a ON a.id = s.admin_id
 WHERE s.token_hash = ? AND a.status = 'active'`, tokenHash),
		&a.ID, &a.Username, &a.PwHash, &a.Generation, &a.Status, &aCreated, &aUpdated)
	if errors.Is(err, sql.ErrNoRows) {
		return store.GUISession{}, store.GUIAdmin{}, fmt.Errorf("%w: gui session", store.ErrNotFound)
	}
	if err != nil {
		return store.GUISession{}, store.GUIAdmin{}, err
	}
	a.CreatedAt = time.UnixMilli(aCreated).UTC()
	a.UpdatedAt = time.UnixMilli(aUpdated).UTC()
	return s, a, nil
}

// TouchGUISession records activity. Not audited.
func (d *DB) TouchGUISession(ctx context.Context, id int64, now time.Time) error {
	return d.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE gui_sessions SET last_active_at = ? WHERE id = ?`, now.UnixMilli(), id)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return fmt.Errorf("%w: gui session", store.ErrNotFound)
		}
		return nil
	})
}

// DeleteGUISession deletes one of the admin's own sessions. A zero ev.Action writes no audit row.
func (d *DB) DeleteGUISession(ctx context.Context, adminID, id int64, now time.Time, ev store.AuditEvent) error {
	return d.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM gui_sessions WHERE id = ? AND admin_id = ?`, id, adminID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return fmt.Errorf("%w: gui session", store.ErrNotFound)
		}
		return auditIfSet(ctx, tx, ev, now)
	})
}

// DeleteOtherGUISessions deletes every session of the admin except keepID.
func (d *DB) DeleteOtherGUISessions(ctx context.Context, adminID, keepID int64, now time.Time, ev store.AuditEvent) (int, error) {
	if ev.Action == "" {
		return 0, fmt.Errorf("%w: audit event action is required", store.ErrInvalid)
	}
	var n int64
	err := d.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM gui_sessions WHERE admin_id = ? AND id <> ?`, adminID, keepID)
		if err != nil {
			return err
		}
		if n, err = res.RowsAffected(); err != nil {
			return err
		}
		return auditIfSet(ctx, tx, ev, now)
	})
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// ListGUISessions returns the admin's sessions oldest first, without token hashes.
func (d *DB) ListGUISessions(ctx context.Context, adminID int64) ([]store.GUISession, error) {
	rows, err := d.reader.QueryContext(ctx, `SELECT `+guiSessionCols+` FROM gui_sessions WHERE admin_id = ? ORDER BY id`, adminID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.GUISession
	for rows.Next() {
		s, err := scanGUISession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// PurgeGUISessions deletes expired, idle and dead-credential sessions. Not audited.
func (d *DB) PurgeGUISessions(ctx context.Context, idle time.Duration, now time.Time) (int, error) {
	if idle <= 0 {
		return 0, fmt.Errorf("%w: idle timeout must be positive", store.ErrInvalid)
	}
	var n int64
	err := d.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM gui_sessions WHERE expires_at <= ? OR last_active_at <= ?
 OR NOT EXISTS (SELECT 1 FROM gui_admins a WHERE a.id = gui_sessions.admin_id AND a.status = 'active'
                AND a.generation = gui_sessions.generation)`,
			now.UnixMilli(), now.Add(-idle).UnixMilli())
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	if err != nil {
		return 0, err
	}
	return int(n), nil
}
