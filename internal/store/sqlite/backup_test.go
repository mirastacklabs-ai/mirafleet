package sqlite

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// seedSchema builds a database at schema version n with one fleet row, using only the first n
// migrations, then closes it.
func seedSchema(t *testing.T, path string, n int) {
	t.Helper()
	ms, err := loadMigrations(migrationFS, "migrations")
	if err != nil || len(ms) <= n {
		t.Fatalf("migrations: %v (%d)", err, len(ms))
	}
	if err := checkDBFile(path); err != nil {
		t.Fatal(err)
	}
	w, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.SetMaxOpenConns(1)
	ctx := context.Background()
	if err := migrate(ctx, w, ms[:n]); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Exec("INSERT INTO fleets (name, mode, created_at) VALUES ('prod','actions',1)"); err != nil {
		t.Fatal(err)
	}
}

func listBackups(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(dir, backupDirName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func TestBackupFreshDBNone(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(context.Background(), filepath.Join(dir, "mirafleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	if b := listBackups(t, dir); len(b) != 0 {
		t.Fatalf("fresh DB made backups: %v", b)
	}
	// Reopening an up-to-date DB makes none either.
	d, err = Open(context.Background(), filepath.Join(dir, "mirafleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	if b := listBackups(t, dir); len(b) != 0 {
		t.Fatalf("up-to-date DB made backups: %v", b)
	}
}

func TestBackupOnPendingMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mirafleet.db")
	seedSchema(t, path, 1)
	d, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()

	b := listBackups(t, dir)
	if len(b) != 1 || !strings.HasPrefix(b[0], "mirafleet-schema1-") || !strings.HasSuffix(b[0], "Z.db") {
		t.Fatalf("backups = %v", b)
	}
	bp := filepath.Join(dir, backupDirName, b[0])
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(bp); fi.Mode().Perm() != 0o600 {
			t.Errorf("backup mode %v", fi.Mode().Perm())
		}
		if fi, _ := os.Stat(filepath.Join(dir, backupDirName)); fi.Mode().Perm() != 0o700 {
			t.Errorf("backups dir mode %v", fi.Mode().Perm())
		}
	}
	bdb, err := sql.Open("sqlite", bp)
	if err != nil {
		t.Fatal(err)
	}
	defer bdb.Close()
	var ver, n int
	if err := bdb.QueryRow("PRAGMA user_version").Scan(&ver); err != nil || ver != 1 {
		t.Fatalf("backup version = %d, %v", ver, err)
	}
	if err := bdb.QueryRow("SELECT count(*) FROM fleets WHERE name='prod'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("backup rows = %d, %v", n, err)
	}
}

func TestBackupRetentionKeepsThree(t *testing.T) {
	dir := t.TempDir()
	bd := filepath.Join(dir, backupDirName)
	if err := os.MkdirAll(bd, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{
		"mirafleet-schema1-20260101T000000Z.db",
		"mirafleet-schema2-20260102T000000Z.db",
		"mirafleet-schema2-20260103T000000Z.db",
		"mirafleet-schema10-20260104T000000Z.db",
		"notes.txt",
	} {
		if err := os.WriteFile(filepath.Join(bd, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "mirafleet.db")
	seedSchema(t, path, 1)
	d, err := Open(context.Background(), path) // newest backup is "now"
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	b := listBackups(t, dir)
	if len(b) != 4 { // 3 kept + unrelated notes.txt
		t.Fatalf("backups = %v", b)
	}
	for _, gone := range []string{"20260101T", "20260102T"} {
		for _, n := range b {
			if strings.Contains(n, gone) {
				t.Errorf("%s not pruned: %v", gone, b)
			}
		}
	}
}

func TestBackupNewerDBRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mirafleet.db")
	seedSchema(t, path, 1)
	w, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Exec("PRAGMA user_version=999"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	_, err = Open(context.Background(), path)
	if err == nil || !strings.Contains(err.Error(), "newer than this binary supports") || !strings.Contains(err.Error(), "restore a backup") {
		t.Fatalf("err = %v", err)
	}
	if b := listBackups(t, dir); len(b) != 0 {
		t.Fatalf("backup made for newer DB: %v", b)
	}
}

func TestBackupFailureBlocksMigration(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs permission enforcement")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "mirafleet.db")
	seedSchema(t, path, 1)
	bd := filepath.Join(dir, backupDirName)
	if err := os.Mkdir(bd, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(bd, 0o700) })
	// Open re-tightens the dir to 0700, so make the dir unwritable by other means: a regular
	// file where the directory should be.
	if err := os.Remove(bd); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bd, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), path); err == nil {
		t.Fatal("Open succeeded despite backup failure")
	}
	w, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	var ver int
	if err := w.QueryRow("PRAGMA user_version").Scan(&ver); err != nil || ver != 1 {
		t.Fatalf("version after failed backup = %d, %v", ver, err)
	}
}

func TestBackupRestoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mirafleet.db")
	seedSchema(t, path, 1)
	d, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	b := listBackups(t, dir)
	if len(b) != 1 {
		t.Fatal(b)
	}
	for _, s := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(path + s)
	}
	data, err := os.ReadFile(filepath.Join(dir, backupDirName, b[0]))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	d, err = Open(context.Background(), path) // restored schema-1 DB migrates again
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
}

func TestPruneBackupsOrdering(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		n := "mirafleet-schema1-" + now.Add(time.Duration(i)*time.Second).Format("20060102T150405Z") + ".db"
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pruneBackups(dir, 3)
	ents, _ := os.ReadDir(dir)
	if len(ents) != 3 {
		t.Fatalf("left %d", len(ents))
	}
}

// Pruning happens right after a successful backup, before (and regardless of) the migration, so
// a crash loop of failing migrations cannot fill the disk. A stale tmp file from a crash is
// swept, and the new backup is 0600 with no tmp file left behind.
func TestBackupPrunesBeforeMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mirafleet.db")
	seedSchema(t, path, 1)
	bd := filepath.Join(dir, backupDirName)
	if err := os.MkdirAll(bd, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 5; i++ {
		n := "mirafleet-schema1-2026010" + string(rune('0'+i)) + "T000000Z.db"
		if err := os.WriteFile(filepath.Join(bd, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stale := "mirafleet-schema1-20260101T000000Z.db.0123456789abcdef.tmp"
	if err := os.WriteFile(filepath.Join(bd, stale), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	ms, err := loadMigrations(migrationFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	w, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.SetMaxOpenConns(1)
	// No migrate call: this is the state after a backup whose migration then failed.
	if err := backupBeforeMigrate(context.Background(), w, path, ms, time.Now()); err != nil {
		t.Fatal(err)
	}
	b := listBackups(t, dir)
	if len(b) != backupKeep {
		t.Fatalf("backups after backup = %v, want %d", b, backupKeep)
	}
	for _, n := range b {
		if strings.HasSuffix(n, ".tmp") {
			t.Errorf("tmp file left: %s", n)
		}
		fi, err := os.Stat(filepath.Join(bd, n))
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v, want 0600", n, fi.Mode().Perm())
		}
	}
}

// Two backups in the same second reuse the final name through rename; the random tmp suffix
// means they never collide on the O_EXCL tmp file.
func TestBackupTmpNamesUnique(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mirafleet.db")
	seedSchema(t, path, 1)
	ms, err := loadMigrations(migrationFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	w, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.SetMaxOpenConns(1)
	now := time.Now()
	for i := 0; i < 2; i++ {
		if err := backupBeforeMigrate(context.Background(), w, path, ms, now); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if b := listBackups(t, dir); len(b) != 1 {
		t.Fatalf("backups = %v", b)
	}
}
