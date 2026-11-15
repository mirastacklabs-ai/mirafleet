package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/store"
)

const (
	credCols    = `c.id, c.name, c.kind, c.hosts, c.meta, c.fingerprint, c.version, c.created_at, c.rotated_at, c.expires_at`
	credSealed  = credCols + `, c.nonce, c.ciphertext`
	gcmNonceLen = 12
)

// rowsQuerier is the multi-row read half shared by *sql.DB and *sql.Tx.
type rowsQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func invalidCred(format string, a ...any) error {
	return fmt.Errorf("%w: credential: %s", store.ErrInvalid, fmt.Sprintf(format, a...))
}

func scanCred(sc interface{ Scan(...any) error }, sealed bool) (store.SealedCredential, error) {
	var (
		s                         store.SealedCredential
		kind, hosts, meta         string
		created                   int64
		rotated, expires          sql.NullInt64
	)
	dst := []any{&s.ID, &s.Name, &kind, &hosts, &meta, &s.Fingerprint, &s.Version, &created, &rotated, &expires}
	if sealed {
		dst = append(dst, &s.Nonce, &s.Ciphertext)
	}
	if err := sc.Scan(dst...); err != nil {
		return s, err
	}
	s.Kind = store.CredentialKind(kind)
	if err := unmarshalList(hosts, &s.Hosts); err != nil {
		return s, fmt.Errorf("credential %d: bad hosts: %w", s.ID, err)
	}
	var err error
	if s.Meta, err = unmarshalMap(meta); err != nil {
		return s, fmt.Errorf("credential %d: bad meta: %w", s.ID, err)
	}
	if len(s.Meta) == 0 {
		s.Meta = nil
	}
	s.CreatedAt = fromMS(created)
	if rotated.Valid {
		s.RotatedAt = fromMS(rotated.Int64)
	}
	if expires.Valid {
		s.ExpiresAt = fromMS(expires.Int64)
	}
	return s, nil
}

func unmarshalList(s string, dst *[]string) error {
	return json.Unmarshal([]byte(s), dst)
}

// attachAssignments fills Runners and Fleets of the credentials in cs (keyed by id).
func attachAssignments(ctx context.Context, q rowsQuerier, cs map[int64]*store.Credential) error {
	rows, err := q.QueryContext(ctx, `SELECT a.credential_id, f.name, r.name
 FROM credential_assignments a
 LEFT JOIN runners r ON r.id = a.runner_id
 JOIN fleets f ON f.id = COALESCE(r.fleet_id, a.fleet_id)
 ORDER BY a.credential_id, f.name, r.name`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id     int64
			fleet  string
			runner sql.NullString
		)
		if err := rows.Scan(&id, &fleet, &runner); err != nil {
			return err
		}
		c := cs[id]
		if c == nil {
			continue
		}
		if runner.Valid {
			c.Runners = append(c.Runners, store.Ref{Fleet: fleet, Runner: runner.String})
		} else {
			c.Fleets = append(c.Fleets, fleet)
		}
	}
	return rows.Err()
}

// listCreds runs a credential SELECT (credCols or credSealed) with an optional WHERE and returns
// the rows by name with their assignments. Ciphertext is only kept when sealed.
func listCreds(ctx context.Context, q rowsQuerier, sealed bool, where string, args ...any) ([]store.SealedCredential, error) {
	cols := credCols
	if sealed {
		cols = credSealed
	}
	query := `SELECT ` + cols + ` FROM credentials c`
	if where != "" {
		query += ` WHERE ` + where
	}
	rows, err := q.QueryContext(ctx, query+` ORDER BY c.name`, args...)
	if err != nil {
		return nil, err
	}
	var out []store.SealedCredential
	for rows.Next() {
		s, err := scanCred(rows, sealed)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	byID := make(map[int64]*store.Credential, len(out))
	for i := range out {
		byID[out[i].ID] = &out[i].Credential
	}
	if err := attachAssignments(ctx, q, byID); err != nil {
		return nil, err
	}
	return out, nil
}

func metaOnly(l []store.SealedCredential) []store.Credential {
	out := make([]store.Credential, len(l))
	for i := range l {
		out[i] = l[i].Credential
	}
	return out
}

func getCredTx(ctx context.Context, q rowsQuerier, sealed bool, name string) (store.SealedCredential, error) {
	l, err := listCreds(ctx, q, sealed, `c.name = ?`, name)
	if err != nil {
		return store.SealedCredential{}, err
	}
	if len(l) == 0 {
		return store.SealedCredential{}, fmt.Errorf("%w: credential", store.ErrNotFound)
	}
	return l[0], nil
}

func checkSealed(nonce, ct []byte) error {
	if len(nonce) != gcmNonceLen {
		return invalidCred("sealed nonce must be %d bytes", gcmNonceLen)
	}
	if len(ct) == 0 || len(ct) > store.MaxCredentialSealed {
		return invalidCred("sealed value must be 1 to %d bytes", store.MaxCredentialSealed)
	}
	return nil
}

// SetCredential creates or replaces a credential; see store.Store.
func (d *DB) SetCredential(ctx context.Context, c store.Credential, seal store.CredentialSealer, now time.Time, ev store.AuditEvent) (store.Credential, bool, error) {
	if err := c.Validate(); err != nil {
		return store.Credential{}, false, err
	}
	if seal == nil {
		return store.Credential{}, false, invalidCred("no sealer")
	}
	hosts, err := jsonList(c.Hosts)
	if err != nil {
		return store.Credential{}, false, err
	}
	meta := marshalMap(c.Meta)
	var expires any
	if !c.ExpiresAt.IsZero() {
		expires = c.ExpiresAt.UnixMilli()
	}
	if ev.Time.IsZero() {
		ev.Time = now
	}
	var (
		out     store.Credential
		created bool
	)
	err = d.withTx(ctx, func(tx *sql.Tx) error {
		var (
			id      int64
			version int64
		)
		err := tx.QueryRowContext(ctx, `SELECT id, version FROM credentials WHERE name = ?`, c.Name).Scan(&id, &version)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			created = true
			// Placeholder sealed bytes: the id is needed for the AEAD data, and are replaced below
			// before the transaction commits.
			res, err := tx.ExecContext(ctx, `INSERT INTO credentials
 (name, kind, hosts, meta, fingerprint, nonce, ciphertext, version, created_at, expires_at)
 VALUES (?,?,?,?,?,?,?,1,?,?)`,
				c.Name, string(c.Kind), hosts, meta, c.Fingerprint, make([]byte, gcmNonceLen), []byte{0}, now.UnixMilli(), expires)
			if err != nil {
				return err
			}
			if id, err = res.LastInsertId(); err != nil {
				return err
			}
			version = 1
		case err != nil:
			return err
		default:
			version++
		}
		nonce, ct, err := seal(id, version)
		if err != nil {
			return err
		}
		if err := checkSealed(nonce, ct); err != nil {
			return err
		}
		if created {
			_, err = tx.ExecContext(ctx, `UPDATE credentials SET nonce = ?, ciphertext = ? WHERE id = ?`, nonce, ct, id)
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE credentials SET kind = ?, hosts = ?, meta = ?, fingerprint = ?,
 nonce = ?, ciphertext = ?, version = ?, rotated_at = ?, expires_at = ? WHERE id = ?`,
				string(c.Kind), hosts, meta, c.Fingerprint, nonce, ct, version, now.UnixMilli(), expires, id)
		}
		if err != nil {
			return err
		}
		s, err := getCredTx(ctx, tx, false, c.Name)
		if err != nil {
			return err
		}
		out = s.Credential
		_, err = insertAuditTx(ctx, tx, ev)
		return err
	})
	if err != nil {
		return store.Credential{}, false, err
	}
	return out, created, nil
}

// GetCredential returns one credential's metadata.
func (d *DB) GetCredential(ctx context.Context, name string) (store.Credential, error) {
	s, err := getCredTx(ctx, d.reader, false, name)
	return s.Credential, err
}

// ListCredentials returns every credential's metadata.
func (d *DB) ListCredentials(ctx context.Context) ([]store.Credential, error) {
	l, err := listCreds(ctx, d.reader, false, "")
	if err != nil {
		return nil, err
	}
	return metaOnly(l), nil
}

// DeleteCredential removes a credential; its assignments go with it (ON DELETE CASCADE).
func (d *DB) DeleteCredential(ctx context.Context, name string, now time.Time, ev store.AuditEvent) error {
	if ev.Time.IsZero() {
		ev.Time = now
	}
	return d.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM credentials WHERE name = ?`, name)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return fmt.Errorf("%w: credential", store.ErrNotFound)
		}
		_, err = insertAuditTx(ctx, tx, ev)
		return err
	})
}

// credTarget resolves a credential name and a target to (credential id, runner id or fleet id).
func credTarget(ctx context.Context, tx *sql.Tx, name string, t store.CredentialTarget) (credID int64, runnerID, fleetID sql.NullInt64, err error) {
	if err = tx.QueryRowContext(ctx, `SELECT id FROM credentials WHERE name = ?`, name).Scan(&credID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = fmt.Errorf("%w: credential", store.ErrNotFound)
		}
		return
	}
	if t.Runner == "" {
		var id int64
		if err = tx.QueryRowContext(ctx, `SELECT id FROM fleets WHERE name = ?`, t.Fleet).Scan(&id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				err = fmt.Errorf("%w: fleet", store.ErrNotFound)
			}
			return
		}
		fleetID = sql.NullInt64{Int64: id, Valid: true}
		return
	}
	var (
		id     int64
		status string
	)
	if err = tx.QueryRowContext(ctx, `SELECT r.id, r.status FROM runners r JOIN fleets f ON f.id = r.fleet_id
 WHERE f.name = ? AND r.name = ?`, t.Fleet, t.Runner).Scan(&id, &status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = fmt.Errorf("%w: runner", store.ErrNotFound)
		}
		return
	}
	runnerID = sql.NullInt64{Int64: id, Valid: true}
	return
}

// AssignCredential assigns a credential to a runner or a fleet.
func (d *DB) AssignCredential(ctx context.Context, name string, t store.CredentialTarget, now time.Time, ev store.AuditEvent) (bool, error) {
	if ev.Time.IsZero() {
		ev.Time = now
	}
	changed := false
	err := d.withTx(ctx, func(tx *sql.Tx) error {
		credID, runnerID, fleetID, err := credTarget(ctx, tx, name, t)
		if err != nil {
			return err
		}
		if runnerID.Valid {
			var status string
			if err := tx.QueryRowContext(ctx, `SELECT status FROM runners WHERE id = ?`, runnerID.Int64).Scan(&status); err != nil {
				return err
			}
			if store.Status(status) == store.StatusRevoked {
				return invalidCred("runner is revoked")
			}
		}
		var one int
		err = tx.QueryRowContext(ctx, `SELECT 1 FROM credential_assignments WHERE credential_id = ?
 AND runner_id IS ? AND fleet_id IS ?`, credID, runnerID, fleetID).Scan(&one)
		switch {
		case err == nil:
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.ExecContext(ctx, `INSERT INTO credential_assignments (credential_id, runner_id, fleet_id, created_at)
 VALUES (?,?,?,?)`, credID, runnerID, fleetID, now.UnixMilli()); err != nil {
				return err
			}
			changed = true
		default:
			return err
		}
		_, err = insertAuditTx(ctx, tx, ev)
		return err
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

// UnassignCredential removes one assignment.
func (d *DB) UnassignCredential(ctx context.Context, name string, t store.CredentialTarget, now time.Time, ev store.AuditEvent) error {
	if ev.Time.IsZero() {
		ev.Time = now
	}
	return d.withTx(ctx, func(tx *sql.Tx) error {
		credID, runnerID, fleetID, err := credTarget(ctx, tx, name, t)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM credential_assignments WHERE credential_id = ?
 AND runner_id IS ? AND fleet_id IS ?`, credID, runnerID, fleetID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return fmt.Errorf("%w: assignment", store.ErrNotFound)
		}
		_, err = insertAuditTx(ctx, tx, ev)
		return err
	})
}

// effectiveWhere selects the credentials assigned to runner ?1 or to its fleet.
const effectiveWhere = `c.id IN (
 SELECT credential_id FROM credential_assignments WHERE runner_id = ?1
 UNION
 SELECT credential_id FROM credential_assignments
  WHERE fleet_id = (SELECT fleet_id FROM runners WHERE id = ?1))`

func runnerExists(ctx context.Context, q rowsQuerier, id int64) error {
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM runners WHERE id = ?`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: runner", store.ErrNotFound)
	}
	return err
}

// EffectiveCredentials returns the metadata of a runner's runner-level union fleet-level set.
func (d *DB) EffectiveCredentials(ctx context.Context, runnerID int64) ([]store.Credential, error) {
	if err := runnerExists(ctx, d.reader, runnerID); err != nil {
		return nil, err
	}
	l, err := listCreds(ctx, d.reader, false, effectiveWhere, runnerID)
	if err != nil {
		return nil, err
	}
	return metaOnly(l), nil
}

// SealedCredentialsFor is the delivery list: the effective set with ciphertext.
func (d *DB) SealedCredentialsFor(ctx context.Context, runnerID int64) ([]store.SealedCredential, error) {
	if err := runnerExists(ctx, d.reader, runnerID); err != nil {
		return nil, err
	}
	return listCreds(ctx, d.reader, true, effectiveWhere, runnerID)
}

// GetSealedCredential returns one credential with its ciphertext.
func (d *DB) GetSealedCredential(ctx context.Context, name string) (store.SealedCredential, error) {
	return getCredTx(ctx, d.reader, true, name)
}

// CredentialRunners returns the non-revoked runners holding name.
func (d *DB) CredentialRunners(ctx context.Context, name string) ([]store.Runner, error) {
	var id int64
	if err := d.reader.QueryRowContext(ctx, `SELECT id FROM credentials WHERE name = ?`, name).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: credential", store.ErrNotFound)
		}
		return nil, err
	}
	rows, err := d.reader.QueryContext(ctx, `SELECT `+runnerCols+runnerFrom+` WHERE r.status <> 'revoked' AND (
 r.id IN (SELECT runner_id FROM credential_assignments WHERE credential_id = ?1 AND runner_id IS NOT NULL)
 OR r.fleet_id IN (SELECT fleet_id FROM credential_assignments WHERE credential_id = ?1 AND fleet_id IS NOT NULL))
 ORDER BY f.name, r.name`, id)
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

// GetSecretsKeyCheck returns the stored key-check value.
func (d *DB) GetSecretsKeyCheck(ctx context.Context) ([]byte, error) {
	var kc []byte
	err := d.reader.QueryRowContext(ctx, `SELECT key_check FROM secrets_meta WHERE id = 1`).Scan(&kc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: secrets key check", store.ErrNotFound)
	}
	return kc, err
}

const upsertKeyCheck = `INSERT INTO secrets_meta (id, key_check, updated_at) VALUES (1, ?, ?)
 ON CONFLICT(id) DO UPDATE SET key_check = excluded.key_check, updated_at = excluded.updated_at`

// SetSecretsKeyCheck upserts the key-check value.
func (d *DB) SetSecretsKeyCheck(ctx context.Context, keyCheck []byte, now time.Time, ev store.AuditEvent) error {
	if len(keyCheck) == 0 {
		return invalidCred("empty key check")
	}
	if ev.Time.IsZero() {
		ev.Time = now
	}
	return d.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, upsertKeyCheck, keyCheck, now.UnixMilli()); err != nil {
			return err
		}
		_, err := insertAuditTx(ctx, tx, ev)
		return err
	})
}

// RekeyCredentials re-seals every credential and stores the new key check in one transaction.
func (d *DB) RekeyCredentials(ctx context.Context, rekey store.CredentialRekeyer, keyCheck []byte, now time.Time, ev store.AuditEvent) (int, error) {
	if len(keyCheck) == 0 || rekey == nil {
		return 0, invalidCred("rekey needs a rekeyer and a key check")
	}
	if ev.Time.IsZero() {
		ev.Time = now
	}
	n := 0
	err := d.withTx(ctx, func(tx *sql.Tx) error {
		type row struct {
			id, version int64
			nonce, ct   []byte
		}
		rows, err := tx.QueryContext(ctx, `SELECT id, version, nonce, ciphertext FROM credentials ORDER BY id`)
		if err != nil {
			return err
		}
		var all []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.version, &r.nonce, &r.ct); err != nil {
				rows.Close()
				return err
			}
			all = append(all, r)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for _, r := range all {
			nonce, ct, err := rekey(r.id, r.version, r.nonce, r.ct)
			if err != nil {
				return err
			}
			if err := checkSealed(nonce, ct); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE credentials SET nonce = ?, ciphertext = ? WHERE id = ?`, nonce, ct, r.id); err != nil {
				return err
			}
		}
		n = len(all)
		if _, err := tx.ExecContext(ctx, upsertKeyCheck, keyCheck, now.UnixMilli()); err != nil {
			return err
		}
		_, err = insertAuditTx(ctx, tx, ev)
		return err
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}
