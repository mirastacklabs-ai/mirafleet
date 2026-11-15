package sqlite

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/store"
)

// Sessions of a non-active admin or of a stale generation cannot be made through the Store
// API (reset and password change delete them), so these rows are planted directly.
func TestGUISessionDeadCredentials(t *testing.T) {
	ctx := context.Background()
	d := open(t)
	now := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	ev := store.AuditEvent{Source: "gui", ActorKind: "admin", Actor: "boss", Action: "gui.x", Outcome: "ok"}
	h := func(s string) []byte { x := sha256.Sum256([]byte(s)); return x[:] }

	if err := d.PutGUISetup(ctx, h("t"), now.Add(time.Hour), now, ev); err != nil {
		t.Fatal(err)
	}
	a, err := d.CompleteGUISetup(ctx, h("t"), store.GUIAdmin{Username: "boss", PwHash: "x"}, now, ev)
	if err != nil {
		t.Fatal(err)
	}
	plant := func(tok string, admin, gen int64) {
		t.Helper()
		_, err := d.writer.Exec(`INSERT INTO gui_sessions (token_hash, admin_id, generation, created_at, last_active_at, expires_at)
 VALUES (?,?,?,?,?,?)`, h(tok), admin, gen, now.UnixMilli(), now.UnixMilli(), now.Add(time.Hour).UnixMilli())
		if err != nil {
			t.Fatal(err)
		}
	}
	plant("live", a.ID, 1)
	plant("stale", a.ID, 2)
	if n, err := d.PurgeGUISessions(ctx, time.Hour, now); err != nil || n != 1 {
		t.Fatalf("stale generation purge = %d, %v", n, err)
	}
	if _, _, err := d.GetGUISession(ctx, h("live")); err != nil {
		t.Fatal(err)
	}

	// Mark the admin reset without going through ResetGUIAdmin (which would delete the row).
	if _, err := d.writer.Exec(`UPDATE gui_admins SET status = 'reset'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.GetGUISession(ctx, h("live")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("session of a non-active admin: %v", err)
	}
	if n, err := d.PurgeGUISessions(ctx, time.Hour, now); err != nil || n != 1 {
		t.Fatalf("non-active admin purge = %d, %v", n, err)
	}
}

func TestMigration0003Constraints(t *testing.T) {
	d := open(t)
	now := time.Now().UnixMilli()
	for _, q := range []string{
		`INSERT INTO gui_admins (username, pw_hash, status, created_at, updated_at) VALUES ('a','h','active',1,1)`,
	} {
		if _, err := d.writer.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.writer.Exec(`INSERT INTO gui_admins (username, pw_hash, status, created_at, updated_at) VALUES ('b','h','active',?,?)`, now, now); err == nil {
		t.Fatal("two active admins accepted")
	}
	if _, err := d.writer.Exec(`INSERT INTO gui_setup (id, token_hash, expires_at, created_at) VALUES (2, zeroblob(32), 1, 1)`); err == nil {
		t.Fatal("second setup row accepted")
	}
}
