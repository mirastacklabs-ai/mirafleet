package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

type migration struct {
	version int
	name    string
	sql     string
}

// loadMigrations reads NNNN_*.sql files from fsys/dir, sorted by version, and requires them to
// be numbered contiguously from 0001.
func loadMigrations(fsys fs.FS, dir string) ([]migration, error) {
	ents, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	var ms []migration
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".sql") {
			continue
		}
		numStr, _, ok := strings.Cut(n, "_")
		v, convErr := strconv.Atoi(numStr)
		if !ok || convErr != nil || len(numStr) != 4 || v < 1 {
			return nil, fmt.Errorf("migration %q: want NNNN_name.sql", n)
		}
		b, err := fs.ReadFile(fsys, dir+"/"+n)
		if err != nil {
			return nil, err
		}
		ms = append(ms, migration{version: v, name: n, sql: string(b)})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })
	for i, m := range ms {
		if m.version != i+1 {
			return nil, fmt.Errorf("migrations not contiguous: expected %04d, found %s", i+1, m.name)
		}
	}
	return ms, nil
}

// migrate brings the database to the latest version using the writer pool, all in one
// BEGIN IMMEDIATE transaction: the version is read inside it, so two processes starting
// together serialise on the write lock and the second sees the first's result. Any failure
// rolls everything back. A database newer than this binary fails closed.
func migrate(ctx context.Context, db *sql.DB, ms []migration) (err error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			// Use a fresh context: ctx may be the reason we failed.
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	var cur int
	if err = conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&cur); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if cur > len(ms) {
		return fmt.Errorf("database schema version %d is newer than this binary supports (%d): refusing to open; upgrade mirafleet", cur, len(ms))
	}
	for _, m := range ms[cur:] {
		if _, err = conn.ExecContext(ctx, m.sql); err != nil {
			return fmt.Errorf("apply migration %s: %w", m.name, err)
		}
		// PRAGMA does not accept bound parameters; version is an int we parsed, never user input.
		if _, err = conn.ExecContext(ctx, "PRAGMA user_version="+strconv.Itoa(m.version)); err != nil {
			return fmt.Errorf("apply migration %s: %w", m.name, err)
		}
	}
	_, err = conn.ExecContext(ctx, "COMMIT")
	return err
}
