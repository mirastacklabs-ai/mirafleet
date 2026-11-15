package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

const (
	backupDirName = "backups"
	backupKeep    = 3
)

// backupNameRE matches the backups this code writes; prune touches nothing else.
var backupNameRE = regexp.MustCompile(`^mirafleet-schema([0-9]+)-([0-9]{8}T[0-9]{6}Z)\.db$`)

// backupTmpRE matches the in-progress names (backup name + random suffix + .tmp); stale ones
// left by a crash are removed by prune.
var backupTmpRE = regexp.MustCompile(`^mirafleet-schema[0-9]+-[0-9]{8}T[0-9]{6}Z\.db\.[0-9a-f]{16}\.tmp$`)

// backupBeforeMigrate takes an online, consistent copy of the database (VACUUM INTO, which
// works on the live WAL database and never copies the open file) when a migration is pending
// on an existing database: 0 < user_version < len(ms). A fresh database (version 0) has nothing
// to protect, so no backup is made. A database newer than this binary is refused here, before
// anything is written. The backup lands in <dir of db>/backups (0700) as 0600:
// mirafleet-schema<N>-<UTC yyyymmddThhmmssZ>.db. On any backup failure the caller must not
// migrate. Old backups are pruned (newest backupKeep kept) right after a successful backup,
// whether or not the migration then succeeds.
func backupBeforeMigrate(ctx context.Context, db *sql.DB, dbPath string, ms []migration, now time.Time) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var cur int
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&cur); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if cur > len(ms) {
		return errNewerSchema(cur, len(ms))
	}
	if cur == 0 || cur == len(ms) {
		return nil
	}

	dir := filepath.Join(filepath.Dir(dbPath), backupDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("pre-migration backup: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("pre-migration backup: %w", err)
	}
	name := fmt.Sprintf("mirafleet-schema%d-%s.db", cur, now.UTC().Format("20060102T150405Z"))
	final := filepath.Join(dir, name)
	// VACUUM INTO needs a non-existent (or empty) target. Write to a temp name inside the 0700
	// directory, fix the mode, then rename, so a partial file never carries a backup name.
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return fmt.Errorf("pre-migration backup: %w", err)
	}
	tmp := final + "." + hex.EncodeToString(rnd[:]) + ".tmp"
	// Created exclusively with mode 0600 (an empty target is accepted by VACUUM INTO), so the
	// backup is never readable by others and a pre-planted file or symlink is refused.
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("pre-migration backup: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("pre-migration backup: %w", err)
	}
	// The path is derived from the database path, never user input; quote it for SQL anyway.
	q := "VACUUM INTO '" + strings.ReplaceAll(tmp, "'", "''") + "'"
	if _, err := conn.ExecContext(ctx, q); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("pre-migration backup (schema %d): %w; not migrating", cur, err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("pre-migration backup: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("pre-migration backup: %w", err)
	}
	// Prune now, not after the migration: a crash loop of failing migrations must not fill the
	// disk with one backup per start.
	pruneBackups(dir, backupKeep)
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("pre-migration backup: %w", err)
	}
	return nil
}

// syncDir fsyncs a directory so a rename in it is durable. Windows cannot open a directory for
// sync (central never runs there; only the orchestrator side builds for it).
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// pruneBackups deletes all but the newest keep backups (by embedded timestamp, then schema
// version) and removes stale .tmp leftovers. Errors are ignored: pruning is best effort.
func pruneBackups(dir string, keep int) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type bk struct{ name, ts string }
	var all []bk
	for _, e := range ents {
		n := e.Name()
		if backupTmpRE.MatchString(n) {
			_ = os.Remove(filepath.Join(dir, n))
			continue
		}
		if m := backupNameRE.FindStringSubmatch(n); m != nil && e.Type().IsRegular() {
			all = append(all, bk{n, m[2]})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].ts != all[j].ts {
			return all[i].ts > all[j].ts
		}
		return all[i].name > all[j].name
	})
	for i := keep; i < len(all); i++ {
		_ = os.Remove(filepath.Join(dir, all[i].name))
	}
}
