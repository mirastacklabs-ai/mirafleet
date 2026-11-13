// Package storetest is the conformance suite every store.Store implementation must pass.
package storetest

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/store"
)

var t0 = time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)

// Run executes the suite; newStore must return a fresh, empty store (closed via t.Cleanup).
func Run(t *testing.T, newStore func(t *testing.T) store.Store) {
	t.Run("Fleets", func(t *testing.T) { fleets(t, newStore(t)) })
	t.Run("Audit", func(t *testing.T) { audit(t, newStore(t)) })
	t.Run("FleetWorkspace", func(t *testing.T) { fleetWorkspace(t, newStore(t)) })
	t.Run("Workspaces", func(t *testing.T) { workspaces(t, newStore(t)) })
	t.Run("Workspaces", func(t *testing.T) { workspaces(t, newStore(t)) })
	t.Run("WorkspaceFilters", func(t *testing.T) { workspaceFilters(t, newStore(t)) })
	t.Run("WorkspaceClose", func(t *testing.T) { workspaceClose(t, newStore(t)) })
	t.Run("WorkspaceSticky", func(t *testing.T) { workspaceSticky(t, newStore(t)) })
	t.Run("WorkspaceRace", func(t *testing.T) { workspaceRace(t, newStore(t)) })
	t.Run("Enroll", func(t *testing.T) { enroll(t, newStore(t)) })
	t.Run("EnrollRaceSingleUse", func(t *testing.T) { enrollRace(t, newStore(t), 1, 1) })
	t.Run("EnrollRaceMultiUse", func(t *testing.T) { enrollRace(t, newStore(t), 3, 3) })
	t.Run("KeyDisjoint", func(t *testing.T) { keyDisjoint(t, newStore(t)) })
	t.Run("KeyDisjointRace", func(t *testing.T) { keyDisjointRace(t, newStore(t)) })
	t.Run("Runners", func(t *testing.T) { runners(t, newStore(t)) })
	t.Run("Tokens", func(t *testing.T) { tokens(t, newStore(t)) })
	t.Run("Operators", func(t *testing.T) { operators(t, newStore(t)) })
	t.Run("Settings", func(t *testing.T) { settings(t, newStore(t)) })
	t.Run("Validation", func(t *testing.T) { validation(t, newStore(t)) })
	t.Run("ContextCancelled", func(t *testing.T) { ctxCancelled(t, newStore(t)) })
}

func ev(action string) store.AuditEvent {
	return store.AuditEvent{Source: "cli", ActorKind: "admin", Actor: "root", Action: action, Outcome: "ok"}
}

func fleets(t *testing.T, s store.Store) {
	ctx := context.Background()
	if _, err := s.GetFleet(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetFleet missing: %v", err)
	}
	if l, err := s.ListFleets(ctx); err != nil || len(l) != 0 {
		t.Fatalf("empty ListFleets = %v, %v", l, err)
	}

	f, err := s.CreateFleet(ctx, store.Fleet{Name: "web", Mode: store.ModeActions, Confirm: true}, t0, ev("fleet.create"))
	if err != nil {
		t.Fatal(err)
	}
	if f.ID == 0 || f.Name != "web" || f.Mode != store.ModeActions || !f.Confirm || !f.CreatedAt.Equal(t0) {
		t.Fatalf("created = %+v", f)
	}
	got, err := s.GetFleet(ctx, "web")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != f.ID || !got.Confirm || got.Mode != store.ModeActions || !got.CreatedAt.Equal(t0) || got.RunnerCount != 0 {
		t.Fatalf("got = %+v", got)
	}
	if _, err := s.CreateFleet(ctx, store.Fleet{Name: "db", Mode: store.ModeWorkspace, Workspace: wsCfg()}, t0.Add(time.Second), ev("fleet.create")); err != nil {
		t.Fatal(err)
	}
	l, err := s.ListFleets(ctx)
	if err != nil || len(l) != 2 || l[0].Name != "db" || l[1].Name != "web" || l[0].Confirm {
		t.Fatalf("ListFleets = %+v, %v", l, err)
	}

	if _, err := s.CreateFleet(ctx, store.Fleet{Name: "web", Mode: store.ModeActions}, t0, ev("fleet.create")); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate: %v", err)
	}
	for _, bad := range []store.Fleet{
		{Name: "", Mode: store.ModeActions},
		{Name: "Upper", Mode: store.ModeActions},
		{Name: "a/b", Mode: store.ModeActions},
		{Name: "ok", Mode: "bogus"},
		{Name: "ok", Mode: ""},
	} {
		if _, err := s.CreateFleet(ctx, bad, t0, ev("fleet.create")); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("%+v: want ErrInvalid, got %v", bad, err)
		}
	}

	// Atomic with audit: one row per successful create, none for failures.
	evs, err := s.ListAudit(ctx, 0, 100)
	if err != nil || len(evs) != 2 {
		t.Fatalf("audit after fleets = %d, %v", len(evs), err)
	}
	if !evs[0].Time.Equal(t0) {
		t.Fatalf("zero ev.Time should default to now, got %v", evs[0].Time)
	}
}

func audit(t *testing.T, s store.Store) {
	ctx := context.Background()
	if _, err := s.RecordAudit(ctx, ev("x")); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("zero time: %v", err)
	}
	code := 3
	full := store.AuditEvent{
		ID: 999, Time: t0, Source: "api", ActorKind: "operator", Actor: "alice", Remote: "1.2.3.4",
		Fleet: "web", Runner: "r1", Subject: "s", Action: "action.run",
		Params: map[string]string{"a": "b"}, Outcome: "ok", ExitCode: &code, DurationMS: 42,
		Message: "m", RequestID: "req1", Workspace: "0123456789abcdef0123456789abcdef",
	}
	id, err := s.RecordAudit(ctx, full)
	if err != nil || id <= 0 {
		t.Fatalf("RecordAudit = %d, %v", id, err)
	}
	got, err := s.ListAudit(ctx, 0, 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("ListAudit = %v, %v", got, err)
	}
	g := got[0]
	if g.ID != id || !g.Time.Equal(t0) || g.Params["a"] != "b" || g.ExitCode == nil || *g.ExitCode != 3 ||
		g.DurationMS != 42 || g.Actor != "alice" || g.RequestID != "req1" || g.Fleet != "web" || g.Message != "m" || g.Workspace != full.Workspace {
		t.Fatalf("round trip = %+v", g)
	}
	// nil ExitCode stays nil; empty params stay empty.
	id2, err := s.RecordAudit(ctx, store.AuditEvent{Time: t0.Add(time.Second), Action: "y"})
	if err != nil {
		t.Fatal(err)
	}
	if id2 <= id {
		t.Fatalf("ids not increasing: %d then %d", id, id2)
	}
	got, _ = s.ListAudit(ctx, id, 10)
	if len(got) != 1 || got[0].ID != id2 || got[0].ExitCode != nil || len(got[0].Params) != 0 {
		t.Fatalf("second = %+v", got)
	}

	for i := 0; i < 5; i++ {
		if _, err := s.RecordAudit(ctx, store.AuditEvent{Time: t0, Action: fmt.Sprint("n", i)}); err != nil {
			t.Fatal(err)
		}
	}
	got, _ = s.ListAudit(ctx, 0, 3)
	if len(got) != 3 || got[0].ID != id {
		t.Fatalf("limit 3 = %d rows", len(got))
	}
	rest, _ := s.ListAudit(ctx, got[2].ID, 100)
	if len(rest) != 4 {
		t.Fatalf("since = %d rows, want 4", len(rest))
	}
	for i := 1; i < len(rest); i++ {
		if rest[i].ID <= rest[i-1].ID {
			t.Fatal("not ascending")
		}
	}
	if none, err := s.ListAudit(ctx, rest[len(rest)-1].ID, 10); err != nil || len(none) != 0 {
		t.Fatalf("past end = %v, %v", none, err)
	}
	// Huge limit is capped, not an error.
	if _, err := s.ListAudit(ctx, 0, 1<<30); err != nil {
		t.Fatal(err)
	}
}

func auditCount(t *testing.T, s store.Store) int {
	t.Helper()
	evs, err := s.ListAudit(context.Background(), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return len(evs)
}

func secretHash(secret string) []byte { h := sha256.Sum256([]byte(secret)); return h[:] }

func mkFleet(t *testing.T, s store.Store, name string) store.Fleet {
	t.Helper()
	f, err := s.CreateFleet(context.Background(), store.Fleet{Name: name, Mode: store.ModeActions}, t0, ev("fleet.create"))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func mkToken(t *testing.T, s store.Store, fleet, id, secret string, max int, auto bool, exp time.Time) store.JoinToken {
	t.Helper()
	tok, err := s.CreateToken(context.Background(), store.JoinToken{
		TokenID: id, SecretHash: secretHash(secret), Fleet: fleet, Labels: map[string]string{"env": "prod"},
		AutoApprove: auto, MaxUses: max, ExpiresAt: exp, CreatedBy: "root",
	}, t0, ev("token.create"))
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func newRunner(name string, key byte) store.Runner {
	return store.Runner{Name: name, PubKey: []byte{key, 1, 2, 3}, Fingerprint: fmt.Sprintf("SHA256:%d", key)}
}

func uses(t *testing.T, s store.Store, fleet string) int {
	t.Helper()
	l, err := s.ListTokens(context.Background(), fleet)
	if err != nil || len(l) == 0 {
		t.Fatalf("ListTokens: %v %v", l, err)
	}
	return l[0].Uses
}

func enroll(t *testing.T, s store.Store) {
	ctx := context.Background()
	now := t0.Add(time.Minute)
	mkFleet(t, s, "web")
	exp := t0.Add(time.Hour)
	mkToken(t, s, "web", "aaaaaaaaaaaaaaaa", "sec", 5, false, exp)
	base := auditCount(t, s)

	r, err := s.Enroll(ctx, "aaaaaaaaaaaaaaaa", secretHash("sec"), now, newRunner("r1", 1), ev("runner.enrol"))
	if err != nil {
		t.Fatal(err)
	}
	if r.ID == 0 || r.Fleet != "web" || r.Status != store.StatusPending || r.Labels["env"] != "prod" ||
		r.JoinTokenID == nil || !r.CreatedAt.Equal(now) || !r.LastSeenAt.IsZero() || r.Name != "r1" {
		t.Fatalf("enrolled = %+v", r)
	}
	if uses(t, s, "web") != 1 || auditCount(t, s) != base+1 {
		t.Fatal("uses/audit after enrol")
	}

	// Wrong secret and unknown id: ErrNotFound, nothing consumed, no audit.
	if _, err := s.Enroll(ctx, "aaaaaaaaaaaaaaaa", secretHash("bad"), now, newRunner("r2", 2), ev("x")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("wrong secret: %v", err)
	}
	if _, err := s.Enroll(ctx, "bbbbbbbbbbbbbbbb", secretHash("sec"), now, newRunner("r2", 2), ev("x")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
	// Bad name; duplicate live name; duplicate key. Nothing consumed.
	if _, err := s.Enroll(ctx, "aaaaaaaaaaaaaaaa", secretHash("sec"), now, newRunner("Bad Name", 3), ev("x")); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("bad name: %v", err)
	}
	if _, err := s.Enroll(ctx, "aaaaaaaaaaaaaaaa", secretHash("sec"), now, newRunner("r1", 4), ev("x")); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("dup name: %v", err)
	}
	if _, err := s.Enroll(ctx, "aaaaaaaaaaaaaaaa", secretHash("sec"), now, newRunner("r9", 1), ev("x")); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("dup key: %v", err)
	}
	if uses(t, s, "web") != 1 || auditCount(t, s) != base+1 {
		t.Fatalf("failed enrols consumed uses=%d audit=%d", uses(t, s, "web"), auditCount(t, s))
	}

	// Revoked name reusable by a NEW key; the revoked key never enrols again.
	if _, err := s.SetRunnerStatus(ctx, r.ID, []store.Status{store.StatusPending}, store.StatusRevoked, now, ev("runner.revoke")); err != nil {
		t.Fatal(err)
	}
	r2, err := s.Enroll(ctx, "aaaaaaaaaaaaaaaa", secretHash("sec"), now, newRunner("r1", 5), ev("runner.enrol"))
	if err != nil || r2.ID == r.ID {
		t.Fatalf("reuse name: %+v %v", r2, err)
	}
	if _, err := s.Enroll(ctx, "aaaaaaaaaaaaaaaa", secretHash("sec"), now, newRunner("r7", 1), ev("x")); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("revoked key re-enrol: %v", err)
	}
	if uses(t, s, "web") != 2 {
		t.Fatalf("uses = %d, want 2", uses(t, s, "web"))
	}

	// Auto-approve token yields an active runner.
	mkToken(t, s, "web", "cccccccccccccccc", "s2", 1, true, exp)
	a, err := s.Enroll(ctx, "cccccccccccccccc", secretHash("s2"), now, newRunner("auto", 6), ev("runner.enrol"))
	if err != nil || a.Status != store.StatusActive {
		t.Fatalf("auto: %+v %v", a, err)
	}

	// Expired (at exactly expires_at), revoked and used-up tokens: ErrStale, nothing consumed.
	mkToken(t, s, "web", "dddddddddddddddd", "s3", 2, true, exp)
	if _, err := s.Enroll(ctx, "dddddddddddddddd", secretHash("s3"), exp, newRunner("e1", 7), ev("x")); !errors.Is(err, store.ErrStale) {
		t.Fatalf("expired: %v", err)
	}
	if _, err := s.Enroll(ctx, "cccccccccccccccc", secretHash("s2"), now, newRunner("e2", 8), ev("x")); !errors.Is(err, store.ErrStale) {
		t.Fatalf("used up: %v", err)
	}
	if err := s.RevokeToken(ctx, "dddddddddddddddd", now, ev("token.revoke")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enroll(ctx, "dddddddddddddddd", secretHash("s3"), now, newRunner("e3", 9), ev("x")); !errors.Is(err, store.ErrStale) {
		t.Fatalf("revoked: %v", err)
	}
	all, _ := s.ListTokens(ctx, "web")
	if all[2].Uses != 0 {
		t.Fatalf("stale token consumed: %+v", all[2])
	}
	if _, err := s.GetRunnerByKey(ctx, newRunner("", 7).PubKey); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("stale enrol left a runner: %v", err)
	}
}

func enrollRace(t *testing.T, s store.Store, maxUses, wantOK int) {
	ctx := context.Background()
	mkFleet(t, s, "web")
	mkToken(t, s, "web", "eeeeeeeeeeeeeeee", "sec", maxUses, true, t0.Add(time.Hour))
	base := auditCount(t, s)
	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Enroll(ctx, "eeeeeeeeeeeeeeee", secretHash("sec"), t0, newRunner(fmt.Sprint("r", i), byte(i)), ev("runner.enrol"))
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	ok, stale := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, store.ErrStale):
			stale++
		default:
			t.Errorf("unexpected: %v", err)
		}
	}
	if ok != wantOK || stale != n-wantOK {
		t.Fatalf("ok=%d stale=%d, want %d/%d", ok, stale, wantOK, n-wantOK)
	}
	if u := uses(t, s, "web"); u != wantOK {
		t.Fatalf("uses = %d", u)
	}
	rs, _ := s.ListRunners(ctx, "web", true)
	if len(rs) != wantOK || auditCount(t, s) != base+wantOK {
		t.Fatalf("runners=%d audit=%d", len(rs), auditCount(t, s)-base)
	}
}

func runners(t *testing.T, s store.Store) {
	ctx := context.Background()
	now := t0.Add(time.Minute)
	mkFleet(t, s, "web")
	mkFleet(t, s, "db")
	mkToken(t, s, "web", "ffffffffffffffff", "sec", 10, false, t0.Add(time.Hour))
	if _, err := s.ListRunners(ctx, "nope", false); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ListRunners unknown fleet: %v", err)
	}
	if l, err := s.ListRunners(ctx, "db", false); err != nil || len(l) != 0 {
		t.Fatalf("empty list: %v %v", l, err)
	}
	var rs []store.Runner
	for i, n := range []string{"b", "a", "c"} {
		r, err := s.Enroll(ctx, "ffffffffffffffff", secretHash("sec"), now, newRunner(n, byte(i+1)), ev("runner.enrol"))
		if err != nil {
			t.Fatal(err)
		}
		rs = append(rs, r)
	}
	l, _ := s.ListRunners(ctx, "web", false)
	if len(l) != 3 || l[0].Name != "a" || l[1].Name != "b" || l[2].Name != "c" {
		t.Fatalf("order: %+v", l)
	}
	// Lifecycle: pending->active ok; active->pending not allowed; wrong from set -> conflict.
	base := auditCount(t, s)
	b := rs[0]
	got, err := s.SetRunnerStatus(ctx, b.ID, []store.Status{store.StatusPending}, store.StatusActive, now.Add(time.Second), ev("runner.approve"))
	if err != nil || got.Status != store.StatusActive || !got.UpdatedAt.Equal(now.Add(time.Second)) || !got.CreatedAt.Equal(now) {
		t.Fatalf("approve: %+v %v", got, err)
	}
	if _, err := s.SetRunnerStatus(ctx, b.ID, []store.Status{store.StatusPending}, store.StatusActive, now, ev("x")); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale from: %v", err)
	}
	if _, err := s.SetRunnerStatus(ctx, b.ID, []store.Status{store.StatusActive}, store.StatusPending, now, ev("x")); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("bad transition: %v", err)
	}
	if _, err := s.SetRunnerStatus(ctx, b.ID, nil, store.StatusDisabled, now, ev("x")); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("empty from: %v", err)
	}
	if _, err := s.SetRunnerStatus(ctx, b.ID, []store.Status{"bogus"}, store.StatusDisabled, now, ev("x")); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("bad from: %v", err)
	}
	if _, err := s.SetRunnerStatus(ctx, 9999, []store.Status{store.StatusActive}, store.StatusDisabled, now, ev("x")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
	if _, err := s.SetRunnerStatus(ctx, b.ID, []store.Status{store.StatusActive}, store.StatusDisabled, now, ev("runner.disable")); err != nil {
		t.Fatal(err)
	}
	if auditCount(t, s) != base+2 {
		t.Fatalf("audit rows = %d, want %d", auditCount(t, s), base+2)
	}
	if _, err := s.SetRunnerStatus(ctx, b.ID, []store.Status{store.StatusDisabled}, store.StatusRevoked, now, ev("runner.revoke")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetRunnerStatus(ctx, b.ID, []store.Status{store.StatusRevoked}, store.StatusActive, now, ev("x")); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("revoked is terminal: %v", err)
	}

	// Getters: revoked hidden from GetRunner/list, visible by key/id/includeRevoked.
	if _, err := s.GetRunner(ctx, store.Ref{Fleet: "web", Runner: "b"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetRunner revoked: %v", err)
	}
	if g, err := s.GetRunnerByKey(ctx, b.PubKey); err != nil || g.ID != b.ID || g.Status != store.StatusRevoked {
		t.Fatalf("ByKey: %+v %v", g, err)
	}
	if g, err := s.GetRunnerByID(ctx, b.ID); err != nil || g.Fingerprint != b.Fingerprint {
		t.Fatalf("ByID: %+v %v", g, err)
	}
	if _, err := s.GetRunnerByID(ctx, 9999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ByID missing: %v", err)
	}
	if _, err := s.GetRunnerByKey(ctx, []byte("nope")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ByKey missing: %v", err)
	}
	if l, _ := s.ListRunners(ctx, "web", false); len(l) != 2 {
		t.Fatalf("live list = %d", len(l))
	}
	if l, _ := s.ListRunners(ctx, "web", true); len(l) != 3 {
		t.Fatalf("full list = %d", len(l))
	}
	fl, _ := s.ListFleets(ctx)
	if fl[1].Name != "web" || fl[1].RunnerCount != 2 {
		t.Fatalf("RunnerCount: %+v", fl)
	}

	// Touch: sets last seen + info, not audited, never changes status.
	a := rs[1]
	base = auditCount(t, s)
	seen := now.Add(time.Hour)
	if err := s.TouchRunner(ctx, a.ID, seen, map[string]string{"os": "linux"}); err != nil {
		t.Fatal(err)
	}
	g, _ := s.GetRunner(ctx, store.Ref{Fleet: "web", Runner: "a"})
	if !g.LastSeenAt.Equal(seen) || g.Info["os"] != "linux" || g.Status != store.StatusPending || auditCount(t, s) != base {
		t.Fatalf("touched: %+v", g)
	}
	if err := s.TouchRunner(ctx, 9999, seen, nil); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("touch unknown: %v", err)
	}
}

func tokens(t *testing.T, s store.Store) {
	ctx := context.Background()
	mkFleet(t, s, "web")
	mkFleet(t, s, "db")
	exp := t0.Add(time.Hour)
	if _, err := s.CreateToken(ctx, store.JoinToken{TokenID: "1111111111111111", SecretHash: secretHash("a"), Fleet: "nope", MaxUses: 1, ExpiresAt: exp}, t0, ev("x")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown fleet: %v", err)
	}
	if _, err := s.CreateToken(ctx, store.JoinToken{TokenID: "1111111111111111", SecretHash: secretHash("a"), Fleet: "web", MaxUses: 0, ExpiresAt: exp}, t0, ev("x")); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("max uses 0: %v", err)
	}
	if auditCount(t, s) != 2 {
		t.Fatalf("failed creates audited: %d", auditCount(t, s))
	}
	tk := mkToken(t, s, "web", "1111111111111111", "a", 2, true, exp)
	if tk.SecretHash != nil {
		t.Fatal("CreateToken returned the secret hash")
	}
	if tk.ID == 0 || tk.Uses != 0 || !tk.CreatedAt.Equal(t0) || tk.FleetID == 0 || tk.Fleet != "web" {
		t.Fatalf("created = %+v", tk)
	}
	if _, err := s.CreateToken(ctx, store.JoinToken{TokenID: "1111111111111111", SecretHash: secretHash("b"), Fleet: "db", MaxUses: 1, ExpiresAt: exp}, t0, ev("x")); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("dup token id: %v", err)
	}
	mkToken(t, s, "db", "2222222222222222", "b", 1, false, exp)

	all, err := s.ListTokens(ctx, "")
	if err != nil || len(all) != 2 || all[0].TokenID != "1111111111111111" || all[1].TokenID != "2222222222222222" {
		t.Fatalf("all = %+v %v", all, err)
	}
	for _, x := range all {
		if x.SecretHash != nil {
			t.Fatal("ListTokens exposed SecretHash")
		}
	}
	if !all[0].AutoApprove || all[0].MaxUses != 2 || all[0].Labels["env"] != "prod" || !all[0].ExpiresAt.Equal(exp) ||
		all[0].CreatedBy != "root" || !all[0].RevokedAt.IsZero() || all[0].Fleet != "web" {
		t.Fatalf("round trip = %+v", all[0])
	}
	if l, _ := s.ListTokens(ctx, "db"); len(l) != 1 || l[0].TokenID != "2222222222222222" {
		t.Fatalf("by fleet = %+v", l)
	}

	base := auditCount(t, s)
	rt := t0.Add(time.Minute)
	if err := s.RevokeToken(ctx, "1111111111111111", rt, ev("token.revoke")); err != nil {
		t.Fatal(err)
	}
	// Idempotent: second revoke writes no event and keeps the first time.
	if err := s.RevokeToken(ctx, "1111111111111111", rt.Add(time.Hour), ev("token.revoke")); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeToken(ctx, "9999999999999999", rt, ev("x")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	if auditCount(t, s) != base+1 {
		t.Fatalf("audit rows = %d, want %d", auditCount(t, s), base+1)
	}
	l, _ := s.ListTokens(ctx, "web")
	if !l[0].RevokedAt.Equal(rt) || l[0].Usable(t0) {
		t.Fatalf("revoked = %+v", l[0])
	}
}

func operators(t *testing.T, s store.Store) {
	ctx := context.Background()
	key := func(b byte) []byte { return []byte{b, 9, 9} }
	if _, _, err := s.RegisterOperator(ctx, "Bad Name", key(1), "fp1", t0, ev("x")); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("bad name: %v", err)
	}
	if auditCount(t, s) != 0 {
		t.Fatal("failed register audited")
	}
	a, created, err := s.RegisterOperator(ctx, "alice", key(1), "fp1", t0, ev("operator.register"))
	if err != nil || !created || a.ID == 0 || a.Status != store.OperatorActive || a.Name != "alice" ||
		!a.CreatedAt.Equal(t0) || !a.LastSeenAt.IsZero() {
		t.Fatalf("register: %+v %v %v", a, created, err)
	}
	// Idempotent per key, even with a different requested name; no audit row.
	again, created, err := s.RegisterOperator(ctx, "other", key(1), "fp1", t0.Add(time.Second), ev("operator.register"))
	if err != nil || created || again.ID != a.ID || again.Name != "alice" {
		t.Fatalf("idempotent: %+v %v %v", again, created, err)
	}
	if auditCount(t, s) != 1 {
		t.Fatalf("audit rows = %d, want 1", auditCount(t, s))
	}
	// Name held by a different active operator.
	if _, _, err := s.RegisterOperator(ctx, "alice", key(2), "fp2", t0, ev("x")); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("name taken: %v", err)
	}
	b, _, err := s.RegisterOperator(ctx, "bob", key(2), "fp2", t0.Add(time.Minute), ev("operator.register"))
	if err != nil {
		t.Fatal(err)
	}

	for name, get := range map[string]func() (store.Operator, error){
		"key":  func() (store.Operator, error) { return s.GetOperatorByKey(ctx, key(1)) },
		"id":   func() (store.Operator, error) { return s.GetOperatorByID(ctx, a.ID) },
		"name": func() (store.Operator, error) { return s.GetOperatorByName(ctx, "alice") },
	} {
		if op, err := get(); err != nil || op.ID != a.ID || op.Fingerprint != "fp1" || string(op.PubKey) != string(key(1)) {
			t.Fatalf("getter by %s: %+v %v", name, op, err)
		}
	}
	if _, err := s.GetOperatorByKey(ctx, key(7)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ByKey missing: %v", err)
	}
	if _, err := s.GetOperatorByID(ctx, 9999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ByID missing: %v", err)
	}
	if _, err := s.GetOperatorByName(ctx, "zed"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ByName missing: %v", err)
	}

	base := auditCount(t, s)
	seen := t0.Add(time.Hour)
	if err := s.TouchOperator(ctx, a.ID, seen); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchOperator(ctx, 9999, seen); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("touch unknown: %v", err)
	}
	g, _ := s.GetOperatorByID(ctx, a.ID)
	if !g.LastSeenAt.Equal(seen) || g.Status != store.OperatorActive || auditCount(t, s) != base {
		t.Fatalf("touched: %+v", g)
	}

	rt := t0.Add(2 * time.Hour)
	r, err := s.RevokeOperator(ctx, "alice", rt, ev("operator.revoke"))
	if err != nil || r.Status != store.OperatorRevoked || !r.UpdatedAt.Equal(rt) {
		t.Fatalf("revoke: %+v %v", r, err)
	}
	if _, err := s.RevokeOperator(ctx, "alice", rt, ev("x")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoke twice: %v", err)
	}
	if auditCount(t, s) != base+1 {
		t.Fatalf("audit rows = %d, want %d", auditCount(t, s), base+1)
	}
	if _, err := s.GetOperatorByName(ctx, "alice"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ByName revoked: %v", err)
	}
	if g, err := s.GetOperatorByKey(ctx, key(1)); err != nil || g.Status != store.OperatorRevoked {
		t.Fatalf("ByKey revoked: %+v %v", g, err)
	}
	// Revoked key can never register again, under any name; the name is free for a new key.
	if _, _, err := s.RegisterOperator(ctx, "alice", key(1), "fp1", t0, ev("x")); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("revoked key: %v", err)
	}
	if _, _, err := s.RegisterOperator(ctx, "fresh", key(1), "fp1", t0, ev("x")); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("revoked key new name: %v", err)
	}
	a2, created, err := s.RegisterOperator(ctx, "alice", key(3), "fp3", t0, ev("operator.register"))
	if err != nil || !created || a2.ID == a.ID {
		t.Fatalf("name reuse: %+v %v %v", a2, created, err)
	}

	live, _ := s.ListOperators(ctx, false)
	if len(live) != 2 || live[0].ID != b.ID || live[1].ID != a2.ID {
		t.Fatalf("live = %+v", live)
	}
	full, _ := s.ListOperators(ctx, true)
	if len(full) != 3 || full[0].ID != a.ID {
		t.Fatalf("full = %+v", full)
	}
}

func settings(t *testing.T, s store.Store) {
	ctx := context.Background()
	if _, err := s.GetSetting(ctx, "k"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if err := s.CreateSetting(ctx, "k", []byte("v1"), t0, ev("setting.create")); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSetting(ctx, "k", []byte("v2"), t0, ev("setting.create")); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("create twice: %v", err)
	}
	if v, err := s.GetSetting(ctx, "k"); err != nil || string(v) != "v1" {
		t.Fatalf("get = %q %v", v, err)
	}
	if auditCount(t, s) != 1 {
		t.Fatalf("audit rows = %d, want 1", auditCount(t, s))
	}
	if err := s.PutSetting(ctx, "k", []byte("v3"), t0, ev("setting.put")); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSetting(ctx, "new", []byte{0, 1, 2}, t0, ev("setting.put")); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.GetSetting(ctx, "k"); string(v) != "v3" {
		t.Fatalf("overwrite = %q", v)
	}
	if v, _ := s.GetSetting(ctx, "new"); string(v) != "\x00\x01\x02" {
		t.Fatalf("binary = %q", v)
	}
	if auditCount(t, s) != 3 {
		t.Fatalf("audit rows = %d, want 3", auditCount(t, s))
	}
}

func validation(t *testing.T, s store.Store) {
	ctx := context.Background()
	mkFleet(t, s, "web")
	exp := t0.Add(time.Hour)
	good := store.JoinToken{TokenID: "1234567890abcdef", SecretHash: secretHash("a"), Fleet: "web", MaxUses: 1, ExpiresAt: exp}
	base := auditCount(t, s)

	for name, mut := range map[string]func(*store.JoinToken){
		"short hash":     func(j *store.JoinToken) { j.SecretHash = j.SecretHash[:31] },
		"nil hash":       func(j *store.JoinToken) { j.SecretHash = nil },
		"long hash":      func(j *store.JoinToken) { j.SecretHash = append(j.SecretHash, 0) },
		"short id":       func(j *store.JoinToken) { j.TokenID = "abc" },
		"upper-case id":  func(j *store.JoinToken) { j.TokenID = "1234567890ABCDEF" },
		"non-hex id":     func(j *store.JoinToken) { j.TokenID = "zzzzzzzzzzzzzzzz" },
		"zero expiry":    func(j *store.JoinToken) { j.ExpiresAt = time.Time{} },
		"expiry == now":  func(j *store.JoinToken) { j.ExpiresAt = t0 },
		"expiry in past": func(j *store.JoinToken) { j.ExpiresAt = t0.Add(-time.Second) },
		"max uses 0":     func(j *store.JoinToken) { j.MaxUses = 0 },
	} {
		j := good
		j.SecretHash = append([]byte(nil), good.SecretHash...)
		mut(&j)
		if _, err := s.CreateToken(ctx, j, t0, ev("x")); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("CreateToken %s: want ErrInvalid, got %v", name, err)
		}
	}
	if _, err := s.CreateToken(ctx, good, t0, ev("token.create")); err != nil {
		t.Fatal(err)
	}

	// Enroll: input-shape errors, before any lookup (even for an unknown id).
	for name, call := range map[string]func() error{
		"short secret hash": func() error {
			_, err := s.Enroll(ctx, good.TokenID, []byte("short"), t0, newRunner("r1", 1), ev("x"))
			return err
		},
		"short secret hash, unknown id": func() error {
			_, err := s.Enroll(ctx, "ffffffffffffffff", nil, t0, newRunner("r1", 1), ev("x"))
			return err
		},
		"empty key": func() error {
			r := newRunner("r1", 1)
			r.PubKey = nil
			_, err := s.Enroll(ctx, good.TokenID, secretHash("a"), t0, r, ev("x"))
			return err
		},
		"empty fingerprint": func() error {
			r := newRunner("r1", 1)
			r.Fingerprint = ""
			_, err := s.Enroll(ctx, good.TokenID, secretHash("a"), t0, r, ev("x"))
			return err
		},
		"operator empty key": func() error {
			_, _, err := s.RegisterOperator(ctx, "alice", nil, "fp", t0, ev("x"))
			return err
		},
		"operator empty fingerprint": func() error {
			_, _, err := s.RegisterOperator(ctx, "alice", []byte{1}, "", t0, ev("x"))
			return err
		},
		"create setting empty key": func() error { return s.CreateSetting(ctx, "", []byte("v"), t0, ev("x")) },
		"create setting nil value": func() error { return s.CreateSetting(ctx, "k", nil, t0, ev("x")) },
		"put setting empty key":    func() error { return s.PutSetting(ctx, "", []byte("v"), t0, ev("x")) },
		"put setting empty value":  func() error { return s.PutSetting(ctx, "k", []byte{}, t0, ev("x")) },
	} {
		if err := call(); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
	if uses(t, s, "web") != 0 || auditCount(t, s) != base+1 {
		t.Fatalf("invalid calls had effects: uses=%d audit=%d (base %d)", uses(t, s, "web"), auditCount(t, s), base)
	}

	// TouchRunner caps info size.
	r, err := s.Enroll(ctx, good.TokenID, secretHash("a"), t0, newRunner("r1", 1), ev("runner.enrol"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.TouchRunner(ctx, r.ID, t0, map[string]string{"blob": strings.Repeat("x", 17<<10)}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("oversize info: %v", err)
	}
	if err := s.TouchRunner(ctx, r.ID, t0, map[string]string{"blob": strings.Repeat("x", 8<<10)}); err != nil {
		t.Fatalf("8 KiB info: %v", err)
	}
}

func ctxCancelled(t *testing.T, s store.Store) {
	mkFleet(t, s, "web")
	base := auditCount(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.GetFleet(ctx, "web"); !errors.Is(err, context.Canceled) {
		t.Fatalf("read with cancelled ctx: %v", err)
	}
	if _, err := s.CreateFleet(ctx, store.Fleet{Name: "db", Mode: store.ModeActions}, t0, ev("fleet.create")); !errors.Is(err, context.Canceled) {
		t.Fatalf("write with cancelled ctx: %v", err)
	}
	if _, err := s.GetFleet(context.Background(), "db"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cancelled write left a fleet: %v", err)
	}
	if auditCount(t, s) != base {
		t.Fatal("cancelled write left an audit row")
	}
	// The store still works afterwards.
	if _, err := s.CreateFleet(context.Background(), store.Fleet{Name: "db", Mode: store.ModeActions}, t0, ev("fleet.create")); err != nil {
		t.Fatal(err)
	}
}

// keyDisjoint covers the cross-table rule: a key belongs to a runner or an operator, never both,
// enforced inside the Enroll and RegisterOperator transactions.
func keyDisjoint(t *testing.T, s store.Store) {
	ctx := context.Background()
	mkFleet(t, s, "web")
	exp := t0.Add(time.Hour)
	mkToken(t, s, "web", "aaaaaaaaaaaaaaaa", "sec", 5, true, exp)
	opKey := []byte{50, 9, 9}
	if _, _, err := s.RegisterOperator(ctx, "alice", opKey, "fpo", t0, ev("operator.register")); err != nil {
		t.Fatal(err)
	}
	base := auditCount(t, s)

	// Operator key with a valid token: conflict, nothing consumed.
	nr := store.Runner{Name: "r1", PubKey: opKey, Fingerprint: "fpo"}
	if _, err := s.Enroll(ctx, "aaaaaaaaaaaaaaaa", secretHash("sec"), t0, nr, ev("x")); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("operator key enrol: %v", err)
	}
	if uses(t, s, "web") != 0 || auditCount(t, s) != base {
		t.Fatal("refused enrol consumed a use or wrote audit")
	}
	// Operator key with a BAD token: the token error, never a conflict (no existence leak).
	if _, err := s.Enroll(ctx, "aaaaaaaaaaaaaaaa", secretHash("bad"), t0, nr, ev("x")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("bad secret + operator key: %v", err)
	}
	if _, err := s.Enroll(ctx, "bbbbbbbbbbbbbbbb", secretHash("sec"), t0, nr, ev("x")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown id + operator key: %v", err)
	}
	mkToken(t, s, "web", "cccccccccccccccc", "s2", 1, true, t0.Add(time.Second))
	if _, err := s.Enroll(ctx, "cccccccccccccccc", secretHash("s2"), t0.Add(time.Minute), nr, ev("x")); !errors.Is(err, store.ErrStale) {
		t.Fatalf("stale token + operator key: %v", err)
	}

	// Runner key (any status) cannot register as an operator.
	rr, err := s.Enroll(ctx, "aaaaaaaaaaaaaaaa", secretHash("sec"), t0, newRunner("r2", 51), ev("runner.enrol"))
	if err != nil {
		t.Fatal(err)
	}
	if rr.Fleet != "web" {
		t.Fatalf("runner fleet = %q", rr.Fleet)
	}
	if _, _, err := s.RegisterOperator(ctx, "bob", newRunner("", 51).PubKey, "fpr", t0, ev("x")); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("runner key register: %v", err)
	}
	if _, err := s.SetRunnerStatus(ctx, rr.ID, []store.Status{store.StatusActive}, store.StatusRevoked, t0, ev("runner.revoke")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RegisterOperator(ctx, "bob", newRunner("", 51).PubKey, "fpr", t0, ev("x")); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("revoked runner key register: %v", err)
	}
	// Revoked operator's key still cannot enrol as a runner.
	if _, err := s.RevokeOperator(ctx, "alice", t0, ev("operator.revoke")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enroll(ctx, "aaaaaaaaaaaaaaaa", secretHash("sec"), t0, nr, ev("x")); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("revoked operator key enrol: %v", err)
	}
}

// keyDisjointRace: Join and Register racing with the same key; exactly one wins.
func keyDisjointRace(t *testing.T, s store.Store) {
	ctx := context.Background()
	mkFleet(t, s, "web")
	mkToken(t, s, "web", "aaaaaaaaaaaaaaaa", "sec", 50, true, t0.Add(time.Hour))
	for i := 0; i < 10; i++ {
		key := []byte{byte(100 + i), 7, 7}
		var wg sync.WaitGroup
		var joinErr, regErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, joinErr = s.Enroll(ctx, "aaaaaaaaaaaaaaaa", secretHash("sec"), t0,
				store.Runner{Name: fmt.Sprint("r", i), PubKey: key, Fingerprint: fmt.Sprint("fr", i)}, ev("runner.enrol"))
		}()
		go func() {
			defer wg.Done()
			_, _, regErr = s.RegisterOperator(ctx, fmt.Sprint("op", i), key, fmt.Sprint("fo", i), t0, ev("operator.register"))
		}()
		wg.Wait()
		if (joinErr == nil) == (regErr == nil) {
			t.Fatalf("round %d: join=%v register=%v, want exactly one success", i, joinErr, regErr)
		}
		if joinErr != nil && !errors.Is(joinErr, store.ErrConflict) || regErr != nil && !errors.Is(regErr, store.ErrConflict) {
			t.Fatalf("round %d: loser must get ErrConflict: join=%v register=%v", i, joinErr, regErr)
		}
	}
}

// wsCfg returns a valid workspace configuration.
func wsCfg() *store.WorkspaceConfig {
	return &store.WorkspaceConfig{
		Executor: store.ExecutorContainerd, DefaultImage: "docker.io/library/golang:1.27",
		Images: []string{"docker.io/library/golang:1.27", "ghcr.io/acme/dev@sha256:*"},
		Pull:   store.PullIfNotPresent, Network: store.NetworkEgress, Egress: []string{"proxy.golang.org:443", "*.golang.org:443"},
		CPUMilli: 2000, MemoryMB: 4096, Pids: 1024, TmpfsMB: 512, DiskMB: 10240,
		IdleTTL: 24 * time.Hour, MaxTTL: 168 * time.Hour,
		ExecDefault: 120 * time.Second, ExecMax: 30 * time.Minute,
	}
}

func fleetWorkspace(t *testing.T, s store.Store) {
	ctx := context.Background()

	// Mode/config pairing.
	if _, err := s.CreateFleet(ctx, store.Fleet{Name: "w", Mode: store.ModeWorkspace}, t0, ev("fleet.create")); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("workspace mode without config: %v", err)
	}
	if _, err := s.CreateFleet(ctx, store.Fleet{Name: "a", Mode: store.ModeActions, Workspace: wsCfg()}, t0, ev("fleet.create")); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("actions mode with config: %v", err)
	}
	if n := auditCount(t, s); n != 0 {
		t.Fatalf("rejected creates wrote %d audit rows", n)
	}

	// Each mutation must be rejected with ErrInvalid and leave nothing behind.
	for name, mut := range map[string]func(*store.WorkspaceConfig){
		"bad executor":           func(c *store.WorkspaceConfig) { c.Executor = "docker" },
		"bad pull":               func(c *store.WorkspaceConfig) { c.Pull = "sometimes" },
		"bad network":            func(c *store.WorkspaceConfig) { c.Network = "host" },
		"empty default image":    func(c *store.WorkspaceConfig) { c.DefaultImage = "" },
		"default not in list":    func(c *store.WorkspaceConfig) { c.DefaultImage = "evil/img:1" },
		"default is a pattern":   func(c *store.WorkspaceConfig) { c.DefaultImage = "ghcr.io/acme/dev@sha256:*" },
		"empty allowlist":        func(c *store.WorkspaceConfig) { c.Images = nil },
		"bare trailing wildcard": func(c *store.WorkspaceConfig) { c.Images = append(c.Images, "ghcr.io/acme/*") },
		"prefix wildcard":        func(c *store.WorkspaceConfig) { c.Images = append(c.Images, "ghcr.io/acme/dev:v*") },
		"mid-string wildcard":    func(c *store.WorkspaceConfig) { c.Images = append(c.Images, "a*b") },
		"whitespace in image":    func(c *store.WorkspaceConfig) { c.Images = append(c.Images, "a b") },
		"too many images":        func(c *store.WorkspaceConfig) { c.Images = make([]string, 65) },
		"empty egress rule":      func(c *store.WorkspaceConfig) { c.Egress = []string{""} },
		"egress loopback cidr":   func(c *store.WorkspaceConfig) { c.Egress = []string{"127.0.0.0/8:443"} },
		"egress everything":      func(c *store.WorkspaceConfig) { c.Egress = []string{"0.0.0.0/0:443"} },
		"egress metadata":        func(c *store.WorkspaceConfig) { c.Egress = []string{"169.254.169.254:80"} },
		"egress bad host":        func(c *store.WorkspaceConfig) { c.Egress = []string{"*.com"} },
		"egress with none":       func(c *store.WorkspaceConfig) { c.Network = store.NetworkNone },
		"zero cpu":               func(c *store.WorkspaceConfig) { c.CPUMilli = 0 },
		"zero memory":            func(c *store.WorkspaceConfig) { c.MemoryMB = 0 },
		"negative pids":          func(c *store.WorkspaceConfig) { c.Pids = -1 },
		"zero tmpfs":             func(c *store.WorkspaceConfig) { c.TmpfsMB = 0 },
		"zero disk":              func(c *store.WorkspaceConfig) { c.DiskMB = 0 },
		"zero idle ttl":          func(c *store.WorkspaceConfig) { c.IdleTTL = 0 },
		"sub-second idle ttl":    func(c *store.WorkspaceConfig) { c.IdleTTL = time.Millisecond },
		"max ttl < idle ttl":     func(c *store.WorkspaceConfig) { c.MaxTTL = c.IdleTTL - time.Second },
		"zero exec default":      func(c *store.WorkspaceConfig) { c.ExecDefault = 0 },
		"exec default > max":     func(c *store.WorkspaceConfig) { c.ExecDefault = c.ExecMax + time.Second },
		"exec max > 24h":         func(c *store.WorkspaceConfig) { c.ExecMax = 24*time.Hour + time.Second },
	} {
		c := *wsCfg()
		mut(&c)
		if _, err := s.CreateFleet(ctx, store.Fleet{Name: "bad", Mode: store.ModeWorkspace, Workspace: &c}, t0, ev("fleet.create")); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("CreateFleet %s: want ErrInvalid, got %v", name, err)
		}
	}
	if l, _ := s.ListFleets(ctx); len(l) != 0 || auditCount(t, s) != 0 {
		t.Fatalf("invalid creates had effects: %d fleets, %d audit rows", len(l), auditCount(t, s))
	}

	// Boundary values are accepted: exec_max exactly 24h, exec_default == exec_max, max == idle ttl.
	edge := *wsCfg()
	edge.ExecMax, edge.ExecDefault, edge.MaxTTL, edge.IdleTTL = 24*time.Hour, 24*time.Hour, time.Hour, time.Hour
	if _, err := s.CreateFleet(ctx, store.Fleet{Name: "edge", Mode: store.ModeWorkspace, Workspace: &edge}, t0, ev("fleet.create")); err != nil {
		t.Fatalf("edge config: %v", err)
	}

	// Create, then read back through every accessor.
	in := wsCfg()
	f, err := s.CreateFleet(ctx, store.Fleet{Name: "dev", Mode: store.ModeWorkspace, Confirm: true, Workspace: in}, t0, ev("fleet.create"))
	if err != nil {
		t.Fatal(err)
	}
	want := *in
	want.UpdatedAt = t0
	same := func(what string, got, want store.WorkspaceConfig) {
		t.Helper()
		if fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", want) {
			t.Fatalf("%s:\n got %+v\nwant %+v", what, got, want)
		}
	}
	if f.Workspace == nil {
		t.Fatal("CreateFleet dropped the config")
	}
	same("CreateFleet", *f.Workspace, want)
	g, err := s.GetFleet(ctx, "dev")
	if err != nil || g.Workspace == nil {
		t.Fatalf("GetFleet = %+v, %v", g, err)
	}
	same("GetFleet", *g.Workspace, want)
	got, err := s.GetFleetWorkspace(ctx, "dev")
	if err != nil {
		t.Fatal(err)
	}
	same("GetFleetWorkspace", got, want)
	l, err := s.ListFleets(ctx)
	if err != nil || len(l) != 2 || l[0].Name != "dev" || l[0].Workspace == nil || l[1].Name != "edge" || l[1].Workspace == nil {
		t.Fatalf("ListFleets = %+v, %v", l, err)
	}
	same("ListFleets", *l[0].Workspace, want)

	// Actions fleets and unknown fleets have no config.
	mkFleet(t, s, "web")
	if g, _ := s.GetFleet(ctx, "web"); g.Workspace != nil {
		t.Fatalf("actions fleet has a config: %+v", g.Workspace)
	}
	if _, err := s.GetFleetWorkspace(ctx, "web"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetFleetWorkspace on actions fleet: %v", err)
	}
	if _, err := s.GetFleetWorkspace(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetFleetWorkspace unknown: %v", err)
	}

	// Put: replaces, stamps UpdatedAt, writes audit atomically; invalid and wrong-mode are
	// rejected without effect.
	base := auditCount(t, s)
	upd := *wsCfg()
	upd.Network, upd.Egress, upd.Pull = store.NetworkNone, nil, store.PullNever
	upd.ExecMax = 2 * time.Hour
	t1 := t0.Add(time.Hour)
	put, err := s.PutFleetWorkspace(ctx, "dev", upd, t1, ev("fleet.workspace.set"))
	if err != nil {
		t.Fatal(err)
	}
	upd.UpdatedAt = t1
	same("Put result", put, upd)
	got, _ = s.GetFleetWorkspace(ctx, "dev")
	same("after Put", got, upd)
	if auditCount(t, s) != base+1 {
		t.Fatalf("Put wrote %d audit rows", auditCount(t, s)-base)
	}
	bad := upd
	bad.Pids = 0
	if _, err := s.PutFleetWorkspace(ctx, "dev", bad, t1, ev("x")); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("Put invalid: %v", err)
	}
	if _, err := s.PutFleetWorkspace(ctx, "web", upd, t1, ev("x")); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("Put on actions fleet: %v", err)
	}
	if _, err := s.PutFleetWorkspace(ctx, "nope", upd, t1, ev("x")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Put unknown fleet: %v", err)
	}
	got, _ = s.GetFleetWorkspace(ctx, "dev")
	same("after failed Puts", got, upd)
	if auditCount(t, s) != base+1 {
		t.Fatalf("failed Puts wrote audit rows")
	}

	// Sub-second durations are truncated to whole seconds consistently.
	frac := *wsCfg()
	frac.ExecDefault = 90*time.Second + 900*time.Millisecond
	r, err := s.PutFleetWorkspace(ctx, "dev", frac, t1, ev("x"))
	if err != nil || r.ExecDefault != 90*time.Second {
		t.Fatalf("truncation: %+v, %v", r, err)
	}
}

// wsFixture creates a workspace fleet, n runners and m operators.
func wsFixture(t *testing.T, s store.Store, runnersN, opsN int) (store.Fleet, []store.Runner, []store.Operator) {
	t.Helper()
	ctx := context.Background()
	f, err := s.CreateFleet(ctx, store.Fleet{Name: "wsf", Mode: store.ModeWorkspace, Workspace: wsCfg()}, t0, ev("fleet.create"))
	if err != nil {
		t.Fatal(err)
	}
	mkToken(t, s, "wsf", "cccccccccccccccc", "sec", 100, true, t0.Add(time.Hour))
	var rs []store.Runner
	for i := 0; i < runnersN; i++ {
		r, err := s.Enroll(ctx, "cccccccccccccccc", secretHash("sec"), t0, newRunner(fmt.Sprintf("wr%d", i), byte(10+i)), ev("runner.enrol"))
		if err != nil {
			t.Fatal(err)
		}
		rs = append(rs, r)
	}
	var ops []store.Operator
	for i := 0; i < opsN; i++ {
		o, _, err := s.RegisterOperator(ctx, fmt.Sprintf("op%d", i), []byte{byte(50 + i), 7, 7}, fmt.Sprintf("fp%d", i), t0, ev("operator.register"))
		if err != nil {
			t.Fatal(err)
		}
		ops = append(ops, o)
	}
	return f, rs, ops
}

func wsID(n int) string { return fmt.Sprintf("%032x", n+1) }

func wsRow(n int, f store.Fleet, r store.Runner, o store.Operator, session string) store.Workspace {
	return store.Workspace{WsID: wsID(n), FleetID: f.ID, RunnerID: r.ID, OperatorID: o.ID, Session: session,
		Image: "docker.io/library/golang:1.27", Spec: `{"cpu":1}`, ExpiresAt: t0.Add(time.Hour)}
}

func wsAudit(t *testing.T, s store.Store, id string) int {
	t.Helper()
	evs, err := s.ListAudit(context.Background(), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range evs {
		if e.Workspace == id {
			n++
		}
	}
	return n
}

func workspaces(t *testing.T, s store.Store) {
	ctx := context.Background()
	f, rs, ops := wsFixture(t, s, 3, 3)
	now := t0.Add(time.Minute)

	// Reserve: fields, state forced to opening, audit carries the workspace id.
	bad := wsRow(0, f, rs[0], ops[0], "s")
	bad.State = store.WorkspaceOpen
	w, existing, err := s.ReserveWorkspace(ctx, bad, 2, 0, now, ev("workspace.open"))
	if err != nil || existing || w.State != store.WorkspaceOpening || w.ID == 0 || w.Fleet != "wsf" || w.Runner != "wr0" ||
		w.Operator != "op0" || !w.CreatedAt.Equal(now) || !w.LastUsedAt.Equal(now) || !w.ExpiresAt.Equal(t0.Add(time.Hour)) ||
		!w.ClosedAt.IsZero() || w.Session != "s" || w.Spec != `{"cpu":1}` {
		t.Fatalf("reserve: %+v %v %v", w, existing, err)
	}
	if wsAudit(t, s, w.WsID) != 1 {
		t.Fatal("reserve audit row missing the workspace id")
	}

	// Sticky reuse: same row, nothing written, even when the runner is otherwise full.
	base := auditCount(t, s)
	dup := wsRow(1, f, rs[0], ops[0], "s")
	g, existing, err := s.ReserveWorkspace(ctx, dup, 1, 1, now.Add(time.Second), ev("workspace.open"))
	if err != nil || !existing || g.WsID != w.WsID || g.ID != w.ID || auditCount(t, s) != base {
		t.Fatalf("sticky: %+v %v %v audit %d/%d", g, existing, err, auditCount(t, s), base)
	}
	if _, err := s.GetWorkspace(ctx, dup.WsID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("sticky inserted a row: %v", err)
	}
	// Still sticky once open; a different session or operator gets its own row.
	if _, err := s.SetWorkspaceState(ctx, w.WsID, []store.WorkspaceState{store.WorkspaceOpening}, store.WorkspaceOpen, "", now, ev("workspace.state")); err != nil {
		t.Fatal(err)
	}
	if g, existing, err = s.ReserveWorkspace(ctx, dup, 5, 0, now, ev("x")); err != nil || !existing || g.WsID != w.WsID || g.State != store.WorkspaceOpen {
		t.Fatalf("sticky open: %+v %v %v", g, existing, err)
	}

	// Validation and not-found.
	for name, mut := range map[string]func(*store.Workspace){
		"short id":    func(w *store.Workspace) { w.WsID = "abc" },
		"upper id":    func(w *store.Workspace) { w.WsID = strings.ToUpper(wsID(90)) },
		"no session":  func(w *store.Workspace) { w.Session = "" },
		"no image":    func(w *store.Workspace) { w.Image = "" },
		"no spec":     func(w *store.Workspace) { w.Spec = "" },
		"no expiry":   func(w *store.Workspace) { w.ExpiresAt = time.Time{} },
		"zero runner": func(w *store.Workspace) { w.RunnerID = 0 },
	} {
		x := wsRow(90, f, rs[1], ops[1], "v")
		mut(&x)
		if _, _, err := s.ReserveWorkspace(ctx, x, 2, 0, now, ev("x")); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, _, err := s.ReserveWorkspace(ctx, wsRow(91, f, rs[1], ops[1], "v"), 0, 0, now, ev("x")); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("slots 0: %v", err)
	}
	for name, mut := range map[string]func(*store.Workspace){
		"fleet":    func(w *store.Workspace) { w.FleetID = 9999 },
		"runner":   func(w *store.Workspace) { w.RunnerID = 9999 },
		"operator": func(w *store.Workspace) { w.OperatorID = 9999 },
	} {
		x := wsRow(92, f, rs[1], ops[1], "v")
		mut(&x)
		if _, _, err := s.ReserveWorkspace(ctx, x, 2, 0, now, ev("x")); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("unknown %s: %v", name, err)
		}
	}
	// A runner of another fleet, or a revoked runner, is refused (ErrInvalid), nothing written.
	f2, err := s.CreateFleet(ctx, store.Fleet{Name: "wsf2", Mode: store.ModeWorkspace, Workspace: wsCfg()}, t0, ev("fleet.create"))
	if err != nil {
		t.Fatal(err)
	}
	mkToken(t, s, "wsf2", "dddddddddddddddd", "sec2", 100, true, t0.Add(time.Hour))
	foreign, err := s.Enroll(ctx, "dddddddddddddddd", secretHash("sec2"), t0, newRunner("fr0", 40), ev("runner.enrol"))
	if err != nil {
		t.Fatal(err)
	}
	gone, err := s.Enroll(ctx, "cccccccccccccccc", secretHash("sec"), t0, newRunner("wrgone", 41), ev("runner.enrol"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetRunnerStatus(ctx, gone.ID, []store.Status{gone.Status}, store.StatusRevoked, now, ev("runner.revoke")); err != nil {
		t.Fatal(err)
	}
	baseAudit := auditCount(t, s)
	if _, _, err := s.ReserveWorkspace(ctx, wsRow(93, f, foreign, ops[1], "x"), 2, 0, now, ev("x")); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("runner of another fleet: %v", err)
	}
	if _, _, err := s.ReserveWorkspace(ctx, wsRow(94, f2, rs[1], ops[1], "x"), 2, 0, now, ev("x")); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("fleet that does not own the runner: %v", err)
	}
	if _, _, err := s.ReserveWorkspace(ctx, wsRow(95, f, gone, ops[1], "x"), 2, 0, now, ev("x")); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("revoked runner: %v", err)
	}
	for _, n := range []int{93, 94, 95} {
		if _, err := s.GetWorkspace(ctx, wsID(n)); !errors.Is(err, store.ErrNotFound) || auditCount(t, s) != baseAudit {
			t.Errorf("refused reserve %d left state: %v", n, err)
		}
	}
	if _, _, err := s.ReserveWorkspace(ctx, wsRow(0, f, rs[1], ops[1], "other"), 2, 0, now, ev("x")); !errors.Is(err, store.ErrConflict) {
		t.Errorf("duplicate ws_id: %v", err)
	}

	// Per-operator cap counts opening, open and closing; other operators are unaffected.
	if _, _, err := s.ReserveWorkspace(ctx, wsRow(10, f, rs[1], ops[1], "a"), 9, 2, now, ev("x")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ReserveWorkspace(ctx, wsRow(11, f, rs[1], ops[1], "b"), 9, 2, now, ev("x")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ReserveWorkspace(ctx, wsRow(12, f, rs[1], ops[1], "c"), 9, 2, now, ev("x")); !errors.Is(err, store.ErrFull) {
		t.Fatalf("per-operator cap: %v", err)
	}
	if _, _, err := s.ReserveWorkspace(ctx, wsRow(13, f, rs[1], ops[2], "c"), 9, 2, now, ev("x")); err != nil {
		t.Fatalf("other operator: %v", err)
	}
	// maxPerOp <= 0: no cap.
	if _, _, err := s.ReserveWorkspace(ctx, wsRow(14, f, rs[1], ops[1], "d"), 9, 0, now, ev("x")); err != nil {
		t.Fatalf("no cap: %v", err)
	}
	// Failed reserves write nothing.
	if wsAudit(t, s, wsID(12)) != 0 {
		t.Fatal("ErrFull audited")
	}

	// Transitions: all pairs, including illegal ones.
	all := []store.WorkspaceState{store.WorkspaceOpening, store.WorkspaceOpen, store.WorkspaceClosing,
		store.WorkspaceClosed, store.WorkspaceFailed, store.WorkspaceLost}
	n := 100
	for _, from := range all {
		for _, to := range all {
			n++
			sess := fmt.Sprintf("t%d", n)
			x, _, err := s.ReserveWorkspace(ctx, wsRow(n, f, rs[2], ops[2], sess), 1000, 0, now, ev("x"))
			if err != nil {
				t.Fatal(err)
			}
			// Drive to `from` along a legal path.
			path := map[store.WorkspaceState][]store.WorkspaceState{
				store.WorkspaceOpening: nil,
				store.WorkspaceOpen:    {store.WorkspaceOpen},
				store.WorkspaceClosing: {store.WorkspaceOpen, store.WorkspaceClosing},
				store.WorkspaceClosed:  {store.WorkspaceOpen, store.WorkspaceClosing, store.WorkspaceClosed},
				store.WorkspaceFailed:  {store.WorkspaceFailed},
				store.WorkspaceLost:    {store.WorkspaceOpen, store.WorkspaceLost},
			}[from]
			cur := store.WorkspaceOpening
			for _, step := range path {
				if _, err := s.SetWorkspaceState(ctx, x.WsID, []store.WorkspaceState{cur}, step, "setup", now, ev("x")); err != nil {
					t.Fatalf("setup %s: %v", from, err)
				}
				cur = step
			}
			before := wsAudit(t, s, x.WsID)
			got, err := s.SetWorkspaceState(ctx, x.WsID, []store.WorkspaceState{from}, to, "why", now.Add(time.Minute), ev("workspace.state"))
			if store.CanTransitionWorkspace(from, to) {
				if err != nil || got.State != to || got.Reason != "why" || wsAudit(t, s, x.WsID) != before+1 {
					t.Fatalf("%s->%s: %+v %v", from, to, got, err)
				}
				if to.Terminal() != got.ClosedAt.Equal(now.Add(time.Minute)) || (!to.Terminal() && !got.ClosedAt.IsZero()) {
					t.Fatalf("%s->%s closed_at = %v", from, to, got.ClosedAt)
				}
			} else if !errors.Is(err, store.ErrConflict) || wsAudit(t, s, x.WsID) != before {
				t.Fatalf("%s->%s should conflict: %v", from, to, err)
			}
		}
	}
	// B-12: a closing workspace may end closed, lost or failed (a dead runner must not pin a slot).
	for _, to := range all {
		want := to == store.WorkspaceClosed || to == store.WorkspaceLost || to == store.WorkspaceFailed
		if store.CanTransitionWorkspace(store.WorkspaceClosing, to) != want {
			t.Fatalf("closing->%s allowed = %v, want %v", to, !want, want)
		}
	}
	// `from` mismatch, unknown id, invalid arguments.
	m, _, _ := s.ReserveWorkspace(ctx, wsRow(300, f, rs[2], ops[0], "m"), 1000, 0, now, ev("x"))
	if _, err := s.SetWorkspaceState(ctx, m.WsID, []store.WorkspaceState{store.WorkspaceOpen}, store.WorkspaceClosing, "", now, ev("x")); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("from mismatch: %v", err)
	}
	if _, err := s.SetWorkspaceState(ctx, wsID(999), []store.WorkspaceState{store.WorkspaceOpen}, store.WorkspaceClosing, "", now, ev("x")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := s.SetWorkspaceState(ctx, m.WsID, nil, store.WorkspaceOpen, "", now, ev("x")); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("empty from: %v", err)
	}
	if _, err := s.SetWorkspaceState(ctx, m.WsID, []store.WorkspaceState{"bogus"}, store.WorkspaceOpen, "", now, ev("x")); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("bad from: %v", err)
	}
	if _, err := s.SetWorkspaceState(ctx, m.WsID, []store.WorkspaceState{store.WorkspaceOpening}, "bogus", "", now, ev("x")); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("bad to: %v", err)
	}

	// Touch: only open rows change; unknown is ErrNotFound; not audited.
	tc := now.Add(time.Hour)
	ab := auditCount(t, s)
	if err := s.TouchWorkspace(ctx, m.WsID, tc); err != nil { // opening: unchanged
		t.Fatal(err)
	}
	if g, _ := s.GetWorkspace(ctx, m.WsID); !g.LastUsedAt.Equal(now) {
		t.Fatalf("touched an opening workspace: %v", g.LastUsedAt)
	}
	if _, err := s.SetWorkspaceState(ctx, m.WsID, []store.WorkspaceState{store.WorkspaceOpening}, store.WorkspaceOpen, "", now, ev("x")); err != nil {
		t.Fatal(err)
	}
	ab = auditCount(t, s)
	if err := s.TouchWorkspace(ctx, m.WsID, tc); err != nil {
		t.Fatal(err)
	}
	if g, _ := s.GetWorkspace(ctx, m.WsID); !g.LastUsedAt.Equal(tc) || auditCount(t, s) != ab {
		t.Fatalf("touch open: %v audit %d/%d", g.LastUsedAt, auditCount(t, s), ab)
	}
	if err := s.TouchWorkspace(ctx, wsID(999), tc); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("touch unknown: %v", err)
	}
	if _, err := s.GetWorkspace(ctx, wsID(999)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get unknown: %v", err)
	}
}

func workspaceFilters(t *testing.T, s store.Store) {
	ctx := context.Background()
	f, rs, ops := wsFixture(t, s, 2, 2)
	if _, err := s.CreateFleet(ctx, store.Fleet{Name: "wsg", Mode: store.ModeWorkspace, Workspace: wsCfg()}, t0, ev("x")); err != nil {
		t.Fatal(err)
	}
	g, err := s.GetFleet(ctx, "wsg")
	if err != nil {
		t.Fatal(err)
	}
	mkToken(t, s, "wsg", "eeeeeeeeeeeeeeee", "sec3", 100, true, t0.Add(time.Hour))
	gr0, err := s.Enroll(ctx, "eeeeeeeeeeeeeeee", secretHash("sec3"), t0, newRunner("gr0", 60), ev("runner.enrol"))
	if err != nil {
		t.Fatal(err)
	}
	now := t0.Add(time.Minute)
	reserve := func(n int, fl store.Fleet, r store.Runner, o store.Operator) {
		if _, _, err := s.ReserveWorkspace(ctx, wsRow(n, fl, r, o, fmt.Sprintf("s%d", n)), 100, 0, now, ev("x")); err != nil {
			t.Fatal(err)
		}
	}
	reserve(1, f, rs[0], ops[0])
	reserve(2, f, rs[0], ops[1])
	reserve(3, f, rs[1], ops[0])
	reserve(4, g, gr0, ops[1])
	if _, err := s.SetWorkspaceState(ctx, wsID(2), []store.WorkspaceState{store.WorkspaceOpening}, store.WorkspaceOpen, "", now, ev("x")); err != nil {
		t.Fatal(err)
	}
	list := func(fl store.WorkspaceFilter) []string {
		l, err := s.ListWorkspaces(ctx, fl)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, w := range l {
			ids = append(ids, w.WsID)
		}
		return ids
	}
	eq := func(name string, got []string, want ...int) {
		t.Helper()
		var w []string
		for _, n := range want {
			w = append(w, wsID(n))
		}
		if strings.Join(got, ",") != strings.Join(w, ",") {
			t.Errorf("%s = %v, want %v", name, got, w)
		}
	}
	eq("all", list(store.WorkspaceFilter{}), 1, 2, 3, 4)
	eq("fleet", list(store.WorkspaceFilter{Fleet: "wsg"}), 4)
	eq("unknown fleet", list(store.WorkspaceFilter{Fleet: "nope"}))
	eq("operator", list(store.WorkspaceFilter{OperatorID: ops[0].ID}), 1, 3)
	eq("runner", list(store.WorkspaceFilter{RunnerID: rs[0].ID}), 1, 2)
	eq("state", list(store.WorkspaceFilter{States: []store.WorkspaceState{store.WorkspaceOpen}}), 2)
	eq("states", list(store.WorkspaceFilter{States: []store.WorkspaceState{store.WorkspaceOpen, store.WorkspaceOpening}}), 1, 2, 3, 4)
	eq("combined", list(store.WorkspaceFilter{Fleet: "wsf", OperatorID: ops[1].ID, RunnerID: rs[0].ID}), 2)
	eq("limit", list(store.WorkspaceFilter{Limit: 2}), 1, 2)
	if _, err := s.ListWorkspaces(ctx, store.WorkspaceFilter{States: []store.WorkspaceState{"bogus"}}); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("bad state filter: %v", err)
	}

	// CountLiveWorkspaces across runners; closing counts, terminal does not.
	c, err := s.CountLiveWorkspaces(ctx, []int64{rs[0].ID, rs[1].ID, 9999})
	if err != nil || c[rs[0].ID] != 2 || c[rs[1].ID] != 1 || c[9999] != 0 || len(c) != 3 {
		t.Fatalf("count: %v %v", c, err)
	}
	if _, err := s.SetWorkspaceState(ctx, wsID(2), []store.WorkspaceState{store.WorkspaceOpen}, store.WorkspaceClosing, "", now, ev("x")); err != nil {
		t.Fatal(err)
	}
	if c, _ = s.CountLiveWorkspaces(ctx, []int64{rs[0].ID}); c[rs[0].ID] != 2 {
		t.Fatalf("closing must hold a slot: %v", c)
	}
	if _, err := s.SetWorkspaceState(ctx, wsID(2), []store.WorkspaceState{store.WorkspaceClosing}, store.WorkspaceClosed, "", now, ev("x")); err != nil {
		t.Fatal(err)
	}
	if c, _ = s.CountLiveWorkspaces(ctx, []int64{rs[0].ID}); c[rs[0].ID] != 1 {
		t.Fatalf("closed must free the slot: %v", c)
	}
	if c, err = s.CountLiveWorkspaces(ctx, nil); err != nil || len(c) != 0 {
		t.Fatalf("empty count: %v %v", c, err)
	}
}

func workspaceClose(t *testing.T, s store.Store) {
	ctx := context.Background()
	f, rs, ops := wsFixture(t, s, 2, 2)
	now := t0.Add(time.Minute)
	mk := func(n int, r store.Runner, o store.Operator, open bool) {
		if _, _, err := s.ReserveWorkspace(ctx, wsRow(n, f, r, o, fmt.Sprintf("s%d", n)), 100, 0, now, ev("x")); err != nil {
			t.Fatal(err)
		}
		if open {
			if _, err := s.SetWorkspaceState(ctx, wsID(n), []store.WorkspaceState{store.WorkspaceOpening}, store.WorkspaceOpen, "", now, ev("x")); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk(1, rs[0], ops[0], true)  // open
	mk(2, rs[0], ops[1], false) // opening
	mk(3, rs[1], ops[0], true)  // open, other runner
	mk(4, rs[1], ops[1], true)
	mk(5, rs[0], ops[0], true)
	if _, err := s.SetWorkspaceState(ctx, wsID(5), []store.WorkspaceState{store.WorkspaceOpen}, store.WorkspaceClosing, "", now, ev("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseWorkspacesOf(ctx, store.WorkspaceOwner{}, "r", now, ev("x")); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("no owner: %v", err)
	}
	if _, err := s.CloseWorkspacesOf(ctx, store.WorkspaceOwner{RunnerID: rs[0].ID, OperatorID: ops[0].ID}, "r", now, ev("x")); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("two owners: %v", err)
	}

	// By runner: open and opening -> closing, closing untouched.
	base := auditCount(t, s)
	later := now.Add(time.Minute)
	ids, err := s.CloseWorkspacesOf(ctx, store.WorkspaceOwner{RunnerID: rs[0].ID}, "runner revoked", later, ev("runner.revoke"))
	if err != nil || strings.Join(ids, ",") != wsID(1)+","+wsID(2) {
		t.Fatalf("by runner: %v %v", ids, err)
	}
	if auditCount(t, s) != base+1 {
		t.Fatalf("CloseWorkspacesOf must write ev once: %d/%d", auditCount(t, s), base)
	}
	state := func(n int) store.Workspace {
		w, err := s.GetWorkspace(ctx, wsID(n))
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	if w := state(1); w.State != store.WorkspaceClosing || w.Reason != "runner revoked" || !w.ClosedAt.IsZero() {
		t.Fatalf("ws1: %+v", w)
	}
	if w := state(2); w.State != store.WorkspaceClosing || w.Reason != "runner revoked" || !w.ClosedAt.IsZero() {
		t.Fatalf("ws2: %+v", w)
	}
	if state(3).State != store.WorkspaceOpen || state(5).State != store.WorkspaceClosing || state(5).Reason != "" {
		t.Fatal("other rows touched")
	}

	// By operator.
	ids, err = s.CloseWorkspacesOf(ctx, store.WorkspaceOwner{OperatorID: ops[0].ID}, "operator revoked", later, ev("operator.revoke"))
	if err != nil || strings.Join(ids, ",") != wsID(3) {
		t.Fatalf("by operator: %v %v", ids, err)
	}
	if state(3).State != store.WorkspaceClosing || state(4).State != store.WorkspaceOpen {
		t.Fatal("operator close touched the wrong rows")
	}
	// Nothing left to close: empty result.
	if ids, err = s.CloseWorkspacesOf(ctx, store.WorkspaceOwner{OperatorID: ops[0].ID}, "again", later, ev("x")); err != nil || len(ids) != 0 {
		t.Fatalf("repeat: %v %v", ids, err)
	}
}

func workspaceSticky(t *testing.T, s store.Store) {
	ctx := context.Background()
	f, rs, ops := wsFixture(t, s, 1, 1)
	now := t0.Add(time.Minute)
	open := []store.WorkspaceState{store.WorkspaceOpening}
	if _, _, err := s.ReserveWorkspace(ctx, wsRow(1, f, rs[0], ops[0], "s"), 1, 0, now, ev("x")); err != nil {
		t.Fatal(err)
	}
	// Failed opening frees the sticky index and the slot.
	if _, err := s.SetWorkspaceState(ctx, wsID(1), open, store.WorkspaceFailed, "boom", now, ev("x")); err != nil {
		t.Fatal(err)
	}
	w2, existing, err := s.ReserveWorkspace(ctx, wsRow(2, f, rs[0], ops[0], "s"), 1, 0, now, ev("x"))
	if err != nil || existing || w2.WsID != wsID(2) {
		t.Fatalf("after failed: %+v %v %v", w2, existing, err)
	}
	// A closing workspace holds its slot (ErrFull) but is no longer sticky for the session.
	for _, step := range []struct {
		from, to store.WorkspaceState
	}{{store.WorkspaceOpening, store.WorkspaceOpen}, {store.WorkspaceOpen, store.WorkspaceClosing}} {
		if _, err := s.SetWorkspaceState(ctx, wsID(2), []store.WorkspaceState{step.from}, step.to, "", now, ev("x")); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.ReserveWorkspace(ctx, wsRow(3, f, rs[0], ops[0], "s"), 1, 0, now, ev("x")); !errors.Is(err, store.ErrFull) {
		t.Fatalf("closing holds the slot: %v", err)
	}
	if _, err := s.SetWorkspaceState(ctx, wsID(2), []store.WorkspaceState{store.WorkspaceClosing}, store.WorkspaceClosed, "", now, ev("x")); err != nil {
		t.Fatal(err)
	}
	w3, existing, err := s.ReserveWorkspace(ctx, wsRow(3, f, rs[0], ops[0], "s"), 1, 0, now, ev("x"))
	if err != nil || existing || w3.WsID != wsID(3) {
		t.Fatalf("after close: %+v %v %v", w3, existing, err)
	}
}

func workspaceRace(t *testing.T, s store.Store) {
	ctx := context.Background()
	f, rs, ops := wsFixture(t, s, 1, 1)
	now := t0.Add(time.Minute)
	base := auditCount(t, s)
	const workers = 20
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		ok, full  int
		unexpects []error
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, err := s.ReserveWorkspace(ctx, wsRow(i, f, rs[0], ops[0], fmt.Sprintf("s%d", i)), 2, 0, now, ev("workspace.open"))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, store.ErrFull):
				full++
			default:
				unexpects = append(unexpects, err)
			}
		}(i)
	}
	wg.Wait()
	if ok != 2 || full != workers-2 || len(unexpects) > 0 {
		t.Fatalf("ok=%d full=%d other=%v", ok, full, unexpects)
	}
	if got := auditCount(t, s) - base; got != 2 {
		t.Fatalf("audit rows = %d, want 2", got)
	}
	if c, _ := s.CountLiveWorkspaces(ctx, []int64{rs[0].ID}); c[rs[0].ID] != 2 {
		t.Fatalf("live = %v", c)
	}
}
