package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// TestSchemaChecks drives the 0004 CHECK constraints directly, below the store API.
func TestSchemaChecks(t *testing.T) {
	ctx := context.Background()
	d := open(t)
	exec := func(q string, args ...any) error {
		_, err := d.writer.ExecContext(ctx, q, args...)
		return err
	}
	for _, q := range []string{
		`INSERT INTO fleets (id, name, mode, confirm, created_at) VALUES (1, 'f', 'actions', 0, 0)`,
		`INSERT INTO runners (id, fleet_id, name, pubkey, fingerprint, status, labels, info, created_at, updated_at)
		 VALUES (1, 1, 'r', x'01', 'fp', 'active', '{}', '{}', 0, 0)`,
		`INSERT INTO credentials (id, name, kind, hosts, nonce, ciphertext, version, created_at)
		 VALUES (1, 'c', 'ssh-key', '["h"]', zeroblob(12), x'01', 1, 0)`,
	} {
		if err := exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	ins := `INSERT INTO credential_assignments (credential_id, runner_id, fleet_id, created_at) VALUES (1, ?, ?, 0)`
	if err := exec(ins, nil, nil); err == nil {
		t.Error("an assignment with neither target was accepted")
	}
	if err := exec(ins, 1, 1); err == nil {
		t.Error("an assignment with both targets was accepted")
	}
	if err := exec(ins, 1, nil); err != nil {
		t.Errorf("runner assignment: %v", err)
	}
	if err := exec(ins, 1, nil); err == nil {
		t.Error("a duplicate runner assignment was accepted")
	}
	if err := exec(ins, nil, 1); err != nil {
		t.Errorf("fleet assignment: %v", err)
	}
	for name, q := range map[string]string{
		"bad kind":    `INSERT INTO credentials (name, kind, hosts, nonce, ciphertext, version, created_at) VALUES ('k', 'password', '["h"]', zeroblob(12), x'01', 1, 0)`,
		"no hosts":    `INSERT INTO credentials (name, kind, hosts, nonce, ciphertext, version, created_at) VALUES ('k', 'ssh-key', '[]', zeroblob(12), x'01', 1, 0)`,
		"short nonce": `INSERT INTO credentials (name, kind, hosts, nonce, ciphertext, version, created_at) VALUES ('k', 'ssh-key', '["h"]', zeroblob(4), x'01', 1, 0)`,
		"empty value": `INSERT INTO credentials (name, kind, hosts, nonce, ciphertext, version, created_at) VALUES ('k', 'ssh-key', '["h"]', zeroblob(12), x'', 1, 0)`,
		"dup name":    `INSERT INTO credentials (name, kind, hosts, nonce, ciphertext, version, created_at) VALUES ('c', 'ssh-key', '["h"]', zeroblob(12), x'01', 1, 0)`,
	} {
		if err := exec(q); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}

	// Deleting the runner or the fleet cascades to its assignments.
	if err := exec(`DELETE FROM credential_assignments`); err != nil {
		t.Fatal(err)
	}
	if err := exec(ins, 1, nil); err != nil {
		t.Fatal(err)
	}
	if err := exec(`DELETE FROM runners WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := d.writer.QueryRowContext(ctx, `SELECT count(*) FROM credential_assignments`).Scan(&n); err != nil || n != 0 {
		t.Errorf("assignments after runner delete = %d, %v", n, err)
	}
}

// TestMigrate0003To0004 rebuilds fleet_workspace under existing data and opens it to 'host'.
func TestMigrate0003To0004(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "m.db")
	ms, err := loadMigrations(migrationFS, "migrations")
	if err != nil || len(ms) < 4 {
		t.Fatalf("loadMigrations: %v (%d)", err, len(ms))
	}
	w, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.SetMaxOpenConns(1)
	if err := migrate(ctx, w, ms[:3]); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO fleets (id, name, mode, confirm, created_at) VALUES (1, 'old', 'workspace', 0, 0)`,
		`INSERT INTO fleet_workspace VALUES (1, 'containerd', 'img:1', '["img:1"]', 'never', 'none', '[]', 1, 2, 3, 4, 5, 60, 120, 10, 20, 7)`,
	} {
		if _, err := w.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := migrate(ctx, w, ms); err != nil {
		t.Fatal(err)
	}
	var exec, img string
	var cpu, upd int64
	if err := w.QueryRow(`SELECT executor, default_image, cpu_milli, updated_at FROM fleet_workspace WHERE fleet_id = 1`).Scan(&exec, &img, &cpu, &upd); err != nil ||
		exec != "containerd" || img != "img:1" || cpu != 1 || upd != 7 {
		t.Fatalf("row after rebuild = %q %q %d %d, %v", exec, img, cpu, upd, err)
	}
	ins := func(executor string, cpu int) error {
		_, err := w.Exec(`DELETE FROM fleet_workspace WHERE fleet_id = 1`)
		if err != nil {
			t.Fatal(err)
		}
		_, err = w.Exec(`INSERT INTO fleet_workspace VALUES (1, ?, '', '[]', 'never', 'none', '[]', ?, ?, ?, ?, ?, 60, 120, 10, 20, 7)`,
			executor, cpu, cpu, cpu, cpu, cpu)
		return err
	}
	if err := ins("host", 0); err != nil {
		t.Errorf("host with zero limits: %v", err)
	}
	if err := ins("containerd", 0); err == nil || !strings.Contains(err.Error(), "CHECK") {
		t.Errorf("containerd with zero limits: %v", err)
	}
	if err := ins("docker", 1); err == nil || !strings.Contains(err.Error(), "CHECK") {
		t.Errorf("unknown executor: %v", err)
	}
}
