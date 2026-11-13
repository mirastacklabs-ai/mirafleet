package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/store/storetest"
)

var t0 = time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)

func open(t *testing.T) *DB {
	t.Helper()
	d, err := Open(context.Background(), filepath.Join(t.TempDir(), "mirafleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store { return open(t) })
}

func userVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestMigrationsContiguous(t *testing.T) {
	ms, err := loadMigrations(migrationFS, "migrations")
	if err != nil || len(ms) == 0 {
		t.Fatalf("loadMigrations: %v (%d)", err, len(ms))
	}
	for i, m := range ms {
		if m.version != i+1 {
			t.Fatalf("migration %s at position %d", m.name, i+1)
		}
	}
}

func TestFreshMigrateAndReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "m.db")
	d, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	ms, _ := loadMigrations(migrationFS, "migrations")
	if v := userVersion(t, d.writer); v != len(ms) {
		t.Fatalf("user_version = %d, want %d", v, len(ms))
	}
	for _, tbl := range []string{"fleets", "runners", "join_tokens", "operators", "settings", "audit"} {
		var n int
		if err := d.reader.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", tbl).Scan(&n); err != nil || n != 1 {
			t.Fatalf("table %s missing (%v)", tbl, err)
		}
	}
	if _, err := d.CreateFleet(ctx, store.Fleet{Name: "a", Mode: store.ModeActions}, t0, store.AuditEvent{}); err != nil {
		t.Fatal(err)
	}
	d.Close()

	d, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if v := userVersion(t, d.writer); v != len(ms) {
		t.Fatalf("after reopen user_version = %d", v)
	}
	if _, err := d.GetFleet(ctx, "a"); err != nil {
		t.Fatalf("data lost on reopen: %v", err)
	}
}

func TestNewerDatabaseRefused(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "m.db")
	d, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.writer.Exec("PRAGMA user_version=99"); err != nil {
		t.Fatal(err)
	}
	d.Close()
	if _, err := Open(ctx, path); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("want newer-version error, got %v", err)
	}
}

func TestOpenRejectsBadPaths(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	for _, p := range []string{"", ":memory:", "file::memory:", dir} {
		if _, err := Open(ctx, p); err == nil {
			t.Errorf("Open(%q) succeeded", p)
		}
	}
	target := filepath.Join(dir, "real.db")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.db")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if _, err := Open(ctx, link); err == nil {
		t.Error("symlink accepted")
	}
}

func pragma(t *testing.T, c *sql.Conn, name string) string {
	t.Helper()
	var v string
	if err := c.QueryRowContext(context.Background(), "PRAGMA "+name).Scan(&v); err != nil {
		t.Fatalf("PRAGMA %s: %v", name, err)
	}
	return v
}

func TestPragmasOnEveryConnection(t *testing.T) {
	d := open(t)
	ctx := context.Background()
	check := func(pool *sql.DB, n int, queryOnly string) {
		var conns []*sql.Conn
		var mu sync.Mutex
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c, err := pool.Conn(ctx)
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				conns = append(conns, c)
				mu.Unlock()
			}()
		}
		wg.Wait()
		if len(conns) != n {
			t.Fatalf("got %d conns, want %d", len(conns), n)
		}
		for _, c := range conns {
			for name, want := range map[string]string{
				"foreign_keys": "1", "journal_mode": "wal", "synchronous": "2",
				"busy_timeout": "5000", "trusted_schema": "0", "query_only": queryOnly,
			} {
				if got := pragma(t, c, name); got != want {
					t.Errorf("PRAGMA %s = %q, want %q", name, got, want)
				}
			}
		}
		for _, c := range conns {
			c.Close()
		}
	}
	check(d.reader, readerConns, "1")
	check(d.writer, 1, "0")
}

func TestReaderCannotWrite(t *testing.T) {
	d := open(t)
	_, err := d.reader.Exec("INSERT INTO fleets (name, mode, created_at) VALUES ('x','actions',1)")
	if err == nil {
		t.Fatal("reader pool accepted a write")
	}
}

func TestFileModes(t *testing.T) {
	d := open(t)
	ctx := context.Background()
	if _, err := d.CreateFleet(ctx, store.Fleet{Name: "a", Mode: store.ModeActions}, t0, store.AuditEvent{}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ListFleets(ctx); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"", "-wal", "-shm"} {
		fi, err := os.Stat(d.path + s)
		if err != nil {
			t.Fatalf("stat %s: %v", s, err)
		}
		if m := fi.Mode().Perm(); m != 0o600 {
			t.Errorf("%q mode = %o, want 600", s, m)
		}
	}
}

func TestAuditAppendOnly(t *testing.T) {
	d := open(t)
	ctx := context.Background()
	if _, err := d.RecordAudit(ctx, store.AuditEvent{Time: t0, Action: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.writer.Exec("UPDATE audit SET action='b'"); err == nil {
		t.Error("UPDATE on audit succeeded")
	}
	if _, err := d.writer.Exec("DELETE FROM audit"); err == nil {
		t.Error("DELETE on audit succeeded")
	}
	evs, _ := d.ListAudit(ctx, 0, 10)
	if len(evs) != 1 || evs[0].Action != "a" {
		t.Fatalf("audit changed: %+v", evs)
	}
}

func TestFleetCountsNonRevokedRunners(t *testing.T) {
	d := open(t)
	ctx := context.Background()
	f, err := d.CreateFleet(ctx, store.Fleet{Name: "a", Mode: store.ModeActions}, t0, store.AuditEvent{})
	if err != nil {
		t.Fatal(err)
	}
	for i, st := range []string{"active", "pending", "revoked"} {
		_, err := d.writer.Exec(`INSERT INTO runners (fleet_id,name,pubkey,fingerprint,status,created_at,updated_at)
 VALUES (?,?,?,?,?,1,1)`, f.ID, fmt.Sprint("r", i), []byte{byte(i)}, "fp", st)
		if err != nil {
			t.Fatal(err)
		}
	}
	l, err := d.ListFleets(ctx)
	if err != nil || len(l) != 1 || l[0].RunnerCount != 2 {
		t.Fatalf("ListFleets = %+v, %v", l, err)
	}
	// FK RESTRICT and the live-name partial index are exercised in S2b; here, FKs are on.
	if _, err := d.writer.Exec(`INSERT INTO runners (fleet_id,name,pubkey,fingerprint,status,created_at,updated_at)
 VALUES (9999,'x',x'ff','fp','active',1,1)`); err == nil {
		t.Error("foreign key not enforced")
	}
}

func TestConcurrentWritersSerialise(t *testing.T) {
	d := open(t)
	ctx := context.Background()
	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := d.CreateFleet(ctx, store.Fleet{Name: fmt.Sprint("f", i), Mode: store.ModeActions}, t0, store.AuditEvent{})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent CreateFleet: %v", err)
		}
	}
	l, err := d.ListFleets(ctx)
	if err != nil || len(l) != n {
		t.Fatalf("fleets = %d, %v", len(l), err)
	}
	evs, _ := d.ListAudit(ctx, 0, 100)
	if len(evs) != n {
		t.Fatalf("audit rows = %d, want %d", len(evs), n)
	}
}

func TestFKRestrictAndLiveNameIndex(t *testing.T) {
	d := open(t)
	ctx := context.Background()
	f, err := d.CreateFleet(ctx, store.Fleet{Name: "a", Mode: store.ModeActions}, t0, store.AuditEvent{})
	if err != nil {
		t.Fatal(err)
	}
	ins := `INSERT INTO runners (fleet_id,name,pubkey,fingerprint,status,created_at,updated_at) VALUES (?,?,?,?,?,1,1)`
	if _, err := d.writer.Exec(ins, f.ID, "r", []byte{1}, "fp", "active"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.writer.Exec(`DELETE FROM fleets WHERE id = ?`, f.ID); err == nil {
		t.Error("deleting a fleet with runners succeeded (ON DELETE RESTRICT)")
	}
	if _, err := d.writer.Exec(ins, f.ID, "r", []byte{2}, "fp", "pending"); err == nil {
		t.Error("duplicate live name accepted")
	}
	if _, err := d.writer.Exec(`UPDATE runners SET status='revoked'`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.writer.Exec(ins, f.ID, "r", []byte{2}, "fp", "pending"); err != nil {
		t.Errorf("name not reusable after revoke: %v", err)
	}
	if _, err := d.writer.Exec(ins, f.ID, "other", []byte{1}, "fp", "pending"); err == nil {
		t.Error("revoked key accepted again (pubkey must stay unique)")
	}
	// Deleting a token nulls the runner's join_token_id (SET NULL).
	if _, err := d.writer.Exec(`INSERT INTO join_tokens (token_id,secret_hash,fleet_id,max_uses,expires_at,created_at) VALUES ('t',zeroblob(32),?,1,1,1)`, f.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.writer.Exec(`UPDATE runners SET join_token_id=(SELECT id FROM join_tokens)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.writer.Exec(`DELETE FROM join_tokens`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := d.writer.QueryRow(`SELECT COUNT(*) FROM runners WHERE join_token_id IS NOT NULL`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("join_token_id not nulled: %d %v", n, err)
	}
}
