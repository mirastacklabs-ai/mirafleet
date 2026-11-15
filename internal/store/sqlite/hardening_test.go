package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	msqlite "modernc.org/sqlite"

	"github.com/mirastacklabs-ai/mirafleet/internal/store"
)

func TestConcurrentOpenFreshFile(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "m.db")
	const n = 2
	var wg sync.WaitGroup
	dbs := make([]*DB, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			dbs[i], errs[i] = Open(ctx, path)
		}()
	}
	close(start)
	wg.Wait()
	for i := range dbs {
		if errs[i] != nil {
			t.Fatalf("Open #%d: %v", i, errs[i])
		}
		defer dbs[i].Close()
	}
	ms, _ := loadMigrations(migrationFS, "migrations")
	if v := userVersion(t, dbs[0].writer); v != len(ms) {
		t.Fatalf("user_version = %d, want %d", v, len(ms))
	}
}

func TestLoadMigrationsNegative(t *testing.T) {
	ok := &fstest.MapFile{Data: []byte("SELECT 1;")}
	for name, fsys := range map[string]fstest.MapFS{
		"gap":        {"m/0001_a.sql": ok, "m/0003_c.sql": ok},
		"not from 1": {"m/0002_b.sql": ok},
		"duplicate":  {"m/0001_a.sql": ok, "m/0001_b.sql": ok},
		"bad name":   {"m/init.sql": ok},
		"short num":  {"m/1_a.sql": ok},
		"zero":       {"m/0000_a.sql": ok},
		"non-number": {"m/00x1_a.sql": ok},
	} {
		if _, err := loadMigrations(fsys, "m"); err == nil {
			t.Errorf("%s: loadMigrations accepted it", name)
		}
	}
	if ms, err := loadMigrations(fstest.MapFS{"m/0001_a.sql": ok, "m/0002_b.sql": ok, "m/README": ok}, "m"); err != nil || len(ms) != 2 {
		t.Fatalf("valid set: %v %d", err, len(ms))
	}
}

func TestMigrationFailureRollsBackFully(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "m.db")
	w, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.SetMaxOpenConns(1)
	ms := []migration{
		{version: 1, name: "0001_a.sql", sql: "CREATE TABLE a (x INTEGER); INSERT INTO a VALUES (1);"},
		{version: 2, name: "0002_b.sql", sql: "CREATE TABLE b (x INTEGER); INSERT INTO nosuchtable VALUES (1);"},
	}
	err = migrate(ctx, w, ms)
	if err == nil || !strings.Contains(err.Error(), "0002_b.sql") {
		t.Fatalf("want failure naming 0002_b.sql, got %v", err)
	}
	if v := userVersion(t, w); v != 0 {
		t.Fatalf("user_version = %d after failed migrate, want 0", v)
	}
	var n int
	if err := w.QueryRow("SELECT count(*) FROM sqlite_master WHERE name IN ('a','b')").Scan(&n); err != nil || n != 0 {
		t.Fatalf("partial schema left behind: %d %v", n, err)
	}
	// The connection is usable afterwards (not stuck in a transaction).
	if err := migrate(ctx, w, ms[:1]); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if v := userVersion(t, w); v != 1 {
		t.Fatalf("user_version = %d, want 1", v)
	}
}

func TestOpenTightensExistingMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("modes are advisory on windows")
	}
	path := filepath.Join(t.TempDir(), "m.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", fi.Mode().Perm())
	}
}

func TestOpenRefusesBadSidecars(t *testing.T) {
	ctx := context.Background()
	for _, suffix := range []string{"-wal", "-shm"} {
		dir := t.TempDir()
		path := filepath.Join(dir, "m.db")
		target := filepath.Join(dir, "elsewhere")
		if err := os.WriteFile(target, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path+suffix); err != nil {
			t.Skip("symlinks unavailable:", err)
		}
		if _, err := Open(ctx, path); err == nil {
			t.Errorf("symlink %s accepted", suffix)
		}
		_ = os.Remove(path + suffix)
		if err := os.Mkdir(path+suffix, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(ctx, path); err == nil {
			t.Errorf("directory %s accepted", suffix)
		}
	}
}

func TestWithTxPanicRollsBack(t *testing.T) {
	d := open(t)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("panic was swallowed")
			}
		}()
		_ = d.withTx(context.Background(), func(tx *sql.Tx) error {
			if _, err := tx.Exec(`INSERT INTO fleets (name, mode, created_at) VALUES ('ghost','actions',1)`); err != nil {
				t.Error(err)
			}
			panic("boom")
		})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := d.CreateFleet(ctx, store.Fleet{Name: "after", Mode: store.ModeActions}, t0, store.AuditEvent{}); err != nil {
		t.Fatalf("write after panic: %v", err)
	}
	if _, err := d.GetFleet(ctx, "ghost"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("panicked tx leaked a row: %v", err)
	}
}

// failAuditInserts makes every INSERT INTO audit abort; the returned func drops the trigger.
func failAuditInserts(t *testing.T, d *DB) func() {
	t.Helper()
	if _, err := d.writer.Exec(`CREATE TRIGGER test_audit_fail BEFORE INSERT ON audit
BEGIN SELECT RAISE(ABORT, 'audit insert refused'); END`); err != nil {
		t.Fatal(err)
	}
	return func() {
		if _, err := d.writer.Exec(`DROP TRIGGER test_audit_fail`); err != nil {
			t.Fatal(err)
		}
	}
}

// §1.7: a state change and its audit event commit together or not at all.
func TestAuditFailureRollsBackStateChange(t *testing.T) {
	d := open(t)
	ctx := context.Background()
	ev := store.AuditEvent{Action: "x"}
	hash := sha256.Sum256([]byte("sec"))
	if _, err := d.CreateFleet(ctx, store.Fleet{Name: "web", Mode: store.ModeActions}, t0, ev); err != nil {
		t.Fatal(err)
	}
	tok, err := d.CreateToken(ctx, store.JoinToken{TokenID: "aaaaaaaaaaaaaaaa", SecretHash: hash[:], Fleet: "web",
		MaxUses: 5, ExpiresAt: t0.Add(time.Hour)}, t0, ev)
	if err != nil {
		t.Fatal(err)
	}
	r, err := d.Enroll(ctx, tok.TokenID, hash[:], t0, store.Runner{Name: "r1", PubKey: []byte{1}, Fingerprint: "fp"}, ev)
	if err != nil {
		t.Fatal(err)
	}
	var before int
	if err := d.reader.QueryRow(`SELECT count(*) FROM audit`).Scan(&before); err != nil {
		t.Fatal(err)
	}

	drop := failAuditInserts(t, d)
	if _, err := d.CreateFleet(ctx, store.Fleet{Name: "fresh", Mode: store.ModeActions}, t0, ev); err == nil {
		t.Error("CreateFleet succeeded with failing audit")
	}
	if _, err := d.GetFleet(ctx, "fresh"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("fleet persisted without audit: %v", err)
	}
	if _, err := d.Enroll(ctx, tok.TokenID, hash[:], t0, store.Runner{Name: "r2", PubKey: []byte{2}, Fingerprint: "fp"}, ev); err == nil {
		t.Error("Enroll succeeded with failing audit")
	}
	if _, err := d.GetRunnerByKey(ctx, []byte{2}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("runner persisted without audit: %v", err)
	}
	if ts, _ := d.ListTokens(ctx, "web"); len(ts) != 1 || ts[0].Uses != 1 {
		t.Errorf("token use consumed without audit: %+v", ts)
	}
	if _, err := d.SetRunnerStatus(ctx, r.ID, []store.Status{store.StatusPending}, store.StatusActive, t0, ev); err == nil {
		t.Error("SetRunnerStatus succeeded with failing audit")
	}
	if got, _ := d.GetRunnerByID(ctx, r.ID); got.Status != store.StatusPending {
		t.Errorf("status changed without audit: %s", got.Status)
	}
	drop()

	var after int
	if err := d.reader.QueryRow(`SELECT count(*) FROM audit`).Scan(&after); err != nil || after != before {
		t.Fatalf("audit rows %d -> %d (%v)", before, after, err)
	}
	// Writes work again once the trigger is gone.
	if _, err := d.CreateFleet(ctx, store.Fleet{Name: "fresh", Mode: store.ModeActions}, t0, ev); err != nil {
		t.Fatalf("after drop: %v", err)
	}
}

// _txlock=immediate must take the write lock at BEGIN: a transaction that has only read must
// already block a second connection's write.
func TestWriterTxIsImmediate(t *testing.T) {
	d := open(t)
	ctx := context.Background()
	tx, err := d.writer.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var one int
	if err := tx.QueryRow(`SELECT 1`).Scan(&one); err != nil { // read only: a deferred tx would hold no write lock
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", "file:"+d.path+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	_, err = raw.Exec(`INSERT INTO settings (key, value) VALUES ('k', x'01')`)
	var se *msqlite.Error
	if !errors.As(err, &se) || se.Code()&0xff != 5 { // SQLITE_BUSY
		t.Fatalf("write alongside an open writer tx: want SQLITE_BUSY, got %v", err)
	}
}

func TestConstraintErrorsOtherThanUniqueAreNotConflicts(t *testing.T) {
	d := open(t)
	_, err := d.writer.Exec(`INSERT INTO fleets (name, mode, created_at) VALUES ('x','bogus',1)`)
	if err == nil || isUnique(err) {
		t.Fatalf("CHECK violation mapped as unique: %v", err)
	}
	_, err = d.writer.Exec(`INSERT INTO fleets (name, mode, created_at) VALUES ('x','actions',1)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.writer.Exec(`INSERT INTO fleets (name, mode, created_at) VALUES ('x','actions',1)`)
	if !isUnique(err) {
		t.Fatalf("UNIQUE violation not detected: %v", err)
	}
}

func TestSchemaTokenChecks(t *testing.T) {
	d := open(t)
	if _, err := d.writer.Exec(`INSERT INTO fleets (name, mode, created_at) VALUES ('f','actions',1)`); err != nil {
		t.Fatal(err)
	}
	ins := `INSERT INTO join_tokens (token_id,secret_hash,fleet_id,max_uses,uses,expires_at,created_at) VALUES (?,?,1,?,?,1,1)`
	if _, err := d.writer.Exec(ins, "t1", make([]byte, 31), 1, 0); err == nil {
		t.Error("31-byte secret_hash accepted")
	}
	if _, err := d.writer.Exec(ins, "t2", make([]byte, 32), 1, 2); err == nil {
		t.Error("uses > max_uses accepted")
	}
	if _, err := d.writer.Exec(ins, "t3", make([]byte, 32), 1, 1); err != nil {
		t.Errorf("valid row rejected: %v", err)
	}
}
