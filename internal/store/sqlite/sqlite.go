package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/store"
)

const readerConns = 4

// DB is the SQLite-backed store.Store. It holds two pools on one file: a single-connection
// writer (BEGIN IMMEDIATE transactions) and a small query_only reader pool. Under WAL, readers
// never block the writer and the single writer connection serialises writes in-process, so
// callers never see SQLITE_BUSY.
type DB struct {
	writer *sql.DB
	reader *sql.DB
	path   string
}

var _ store.Store = (*DB)(nil)

// Open opens (creating if needed) the database at path and migrates it to the latest schema.
//
// The data directory must be mode 0700 and flock'ed by central (S10); that closes the one
// window Open cannot: SQLite re-opens the file by name after the checks here, so a rename of
// the path between those checks and SQLite's own open is only possible for someone who can
// write the directory. Within that, Open opens the file with O_NOFOLLOW (unix; elsewhere the
// Lstat checks stand in), requires a regular file whose identity matches a fresh Lstat of the
// path (os.SameFile), refuses -wal/-shm sidecars that are symlinks or not regular files, and
// tightens an existing file to 0600. The caller should set umask 077 before Open so SQLite
// creates the sidecars 0600 too. ":memory:" is refused (each connection would get its own
// database).
func Open(ctx context.Context, path string) (*DB, error) {
	if path == "" || path == ":memory:" || strings.HasPrefix(path, "file:") {
		return nil, fmt.Errorf("sqlite: invalid database path %q", path)
	}
	if err := checkDBFile(path); err != nil {
		return nil, err
	}
	if err := checkSidecars(path); err != nil {
		return nil, err
	}

	ms, err := loadMigrations(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}

	w, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	w.SetMaxIdleConns(1)
	if err := pingRetry(ctx, w); err != nil {
		w.Close()
		return nil, fmt.Errorf("sqlite: open writer: %w", err)
	}
	if err := backupBeforeMigrate(ctx, w, path, ms, time.Now()); err != nil {
		w.Close()
		return nil, fmt.Errorf("sqlite: %w", err)
	}
	if err := migrate(ctx, w, ms); err != nil {
		w.Close()
		return nil, fmt.Errorf("sqlite: %w", err)
	}
	r, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(readerConns)
	r.SetMaxIdleConns(readerConns)
	if err := r.PingContext(ctx); err != nil {
		r.Close()
		w.Close()
		return nil, fmt.Errorf("sqlite: open reader: %w", err)
	}
	d := &DB{writer: w, reader: r, path: path}
	d.tightenModes()
	return d, nil
}

// checkDBFile creates the file 0600 if absent and verifies it is a regular, non-symlink file.
func checkDBFile(path string) error {
	if fi, err := os.Lstat(path); err == nil {
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("sqlite: %s is not a regular file (symlinks refused)", path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("sqlite: %w", err)
	}
	f, err := openNoFollow(path)
	if err != nil {
		return fmt.Errorf("sqlite: open database file: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("sqlite: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("sqlite: %s is not a regular file", path)
	}
	li, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("sqlite: %w", err)
	}
	if !os.SameFile(fi, li) {
		return fmt.Errorf("sqlite: %s changed while opening", path)
	}
	// Chmod is advisory on Windows; never fail Open over it there.
	if err := f.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		return fmt.Errorf("sqlite: chmod database file: %w", err)
	}
	return nil
}

// checkSidecars refuses pre-existing -wal/-shm entries that are not regular files (a symlink
// there would redirect SQLite's writes).
func checkSidecars(path string) error {
	for _, s := range []string{"-wal", "-shm"} {
		fi, err := os.Lstat(path + s)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("sqlite: %w", err)
		}
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("sqlite: %s is not a regular file (symlinks refused)", path+s)
		}
	}
	return nil
}

// tightenModes best-effort chmods the file and its -wal/-shm sidecars to 0600, skipping
// anything that is not a regular file.
func (d *DB) tightenModes() {
	for _, s := range []string{"", "-wal", "-shm"} {
		if fi, err := os.Lstat(d.path + s); err == nil && fi.Mode().IsRegular() {
			_ = os.Chmod(d.path+s, 0o600)
		}
	}
}

// dsn builds the connection string; every pooled connection gets the pragmas. busy_timeout
// comes first so the journal_mode switch itself waits when two processes open a fresh file
// together.
func dsn(path string, writer bool) string {
	esc := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(path)
	q := url.Values{}
	for _, p := range []string{
		"busy_timeout(5000)", "foreign_keys(1)", "journal_mode(WAL)",
		"trusted_schema(0)", "synchronous(FULL)",
	} {
		q.Add("_pragma", p)
	}
	if writer {
		q.Set("_txlock", "immediate")
	} else {
		q.Add("_pragma", "query_only(1)")
	}
	return "file:" + esc + "?" + q.Encode()
}

// Close closes both pools.
func (d *DB) Close() error {
	return errors.Join(d.reader.Close(), d.writer.Close())
}

// withTx runs fn in an immediate transaction on the writer connection, committing on nil. The
// deferred rollback runs on error and on panic, so the single writer connection is never left
// inside an open transaction.
func (d *DB) withTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := d.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

// pingRetry opens the first connection of db. Switching a fresh file to WAL (a connection
// pragma) needs an exclusive lock that SQLite's busy handler does not always wait for when two
// processes open the same new file together, so retry briefly on SQLITE_BUSY.
func pingRetry(ctx context.Context, db *sql.DB) error {
	var err error
	for i := 0; i < 100; i++ {
		if err = db.PingContext(ctx); err == nil || !isBusy(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(10+i) * time.Millisecond):
		}
	}
	return err
}
