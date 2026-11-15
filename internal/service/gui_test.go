package service

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/policy"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/store/sqlite"
)

const (
	guiPW   = "correct horse battery"
	guiUser = "root-admin"
	guiAddr = "203.0.113.9:4444"
)

// guiEnv is a service on a temp SQLite file with a movable clock and a raw handle for tests
// that edit rows the service API cannot produce.
type guiEnv struct {
	t    *testing.T
	svc  *Service
	db   store.Store
	raw  *sql.DB
	logs *bytes.Buffer
	ctx  context.Context

	mu  sync.Mutex
	now time.Time
}

func newGUIEnv(t *testing.T) *guiEnv {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mirafleet.db")
	db, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	hostPub, _, _ := ed25519.GenerateKey(rand.Reader)
	host, err := ssh.NewPublicKey(hostPub)
	if err != nil {
		t.Fatal(err)
	}
	e := &guiEnv{t: t, db: db, raw: raw, logs: &bytes.Buffer{}, ctx: ctx, now: t0}
	log := &callLog{}
	svc, err := New(Config{Store: db, Policy: policy.TablePolicy{}, Live: &fakeLive{log: log}, HostKey: host,
		OperatorConns: &fakeConns{},
		Now: func() time.Time {
			e.mu.Lock()
			defer e.mu.Unlock()
			return e.now
		},
		Logf: func(f string, a ...any) {
			e.mu.Lock()
			fmt.Fprintf(e.logs, f+"\n", a...)
			e.mu.Unlock()
		}})
	if err != nil {
		t.Fatal(err)
	}
	e.svc = svc
	return e
}

func (e *guiEnv) advance(d time.Duration) {
	e.mu.Lock()
	e.now = e.now.Add(d)
	e.mu.Unlock()
}

// setup runs the first-start flow and returns the setup token, the first session token and the
// admin principal.
func (e *guiEnv) setup() (setupTok, sessTok string, p auth.Principal) {
	e.t.Helper()
	tok, created, err := e.svc.EnsureGUISetup(e.ctx)
	if err != nil || !created || !strings.HasPrefix(tok, SetupTokenPrefix) {
		e.t.Fatalf("EnsureGUISetup = %q %v %v", tok, created, err)
	}
	st, p, err := e.svc.CompleteGUISetup(e.ctx, guiAddr, "ua/1", tok, guiUser, guiPW)
	if err != nil {
		e.t.Fatal(err)
	}
	return tok, st, p
}

func (e *guiEnv) sessionCount() int {
	e.t.Helper()
	var n int
	if err := e.raw.QueryRow(`SELECT count(*) FROM gui_sessions`).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *guiEnv) hasSession(token string) bool {
	_, _, err := e.db.GetGUISession(e.ctx, tokenHash(token))
	return err == nil
}

func (e *guiEnv) auditJSON() string {
	e.t.Helper()
	evs, err := e.db.ListAudit(e.ctx, 0, 1000)
	if err != nil {
		e.t.Fatal(err)
	}
	b, _ := json.Marshal(evs)
	return string(b)
}

func (e *guiEnv) actions() []string {
	evs, _ := e.db.ListAudit(e.ctx, 0, 1000)
	var out []string
	for _, ev := range evs {
		out = append(out, ev.Action+"/"+ev.Outcome)
	}
	return out
}

func wantAPI(t *testing.T, err error, code string) {
	t.Helper()
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Code != code {
		t.Fatalf("err = %v, want code %s", err, code)
	}
}

func TestGUISetupTokensAndSecretsStayOutOfAuditAndLogs(t *testing.T) {
	e := newGUIEnv(t)
	setupTok, sessTok, p := e.setup()
	if p.Kind != auth.KindAdmin || p.Name != guiUser || p.Remote != guiAddr {
		t.Fatalf("principal = %+v", p)
	}
	// A live token exists/admin exists: nothing more is created.
	if tok, created, err := e.svc.EnsureGUISetup(e.ctx); tok != "" || created || err != nil {
		t.Fatalf("second EnsureGUISetup = %q %v %v", tok, created, err)
	}
	// A second login for a second session token.
	loginTok, _, err := e.svc.GUILogin(e.ctx, guiAddr, "ua/2", guiUser, guiPW)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ChangeGUIPassword(e.ctx, p, mustSessionID(t, e, sessTok), guiPW, "another long secret"); err != nil {
		t.Fatal(err)
	}
	all := e.auditJSON() + e.logs.String()
	adm, _ := e.db.GetGUIAdmin(e.ctx)
	secrets := []string{setupTok, strings.TrimPrefix(setupTok, SetupTokenPrefix), sessTok, loginTok, guiPW, "another long secret", adm.PwHash}
	for _, h := range [][]byte{tokenHash(setupTok), tokenHash(sessTok), tokenHash(loginTok)} {
		secrets = append(secrets, base64.StdEncoding.EncodeToString(h), base64.RawURLEncoding.EncodeToString(h), fmt.Sprintf("%x", h))
	}
	for _, s := range secrets {
		if strings.Contains(all, s) {
			t.Fatalf("secret %q found in audit or logs", s)
		}
	}
	// Only hashes are stored: the raw token is not a lookup key, its SHA-256 is.
	var n int
	if err := e.raw.QueryRow(`SELECT count(*) FROM gui_sessions WHERE token_hash = ?`, []byte(sessTok)).Scan(&n); err != nil || n != 0 {
		t.Fatalf("raw token stored: %d %v", n, err)
	}
	var stored []byte
	if err := e.raw.QueryRow(`SELECT token_hash FROM gui_sessions ORDER BY id DESC LIMIT 1`).Scan(&stored); err != nil || len(stored) != sha256.Size {
		t.Fatalf("stored hash: %d %v", len(stored), err)
	}
	var rows int
	if err := e.raw.QueryRow(`SELECT count(*) FROM gui_setup`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("setup row remains after use: %d %v", rows, err)
	}
}

func mustSessionID(t *testing.T, e *guiEnv, tok string) int64 {
	t.Helper()
	_, id, err := e.svc.GUIAuthenticate(e.ctx, guiAddr, tok, true)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestGUISetupGenericRefusal(t *testing.T) {
	e := newGUIEnv(t)
	tok, _, err := e.svc.EnsureGUISetup(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, _, werr := e.svc.CompleteGUISetup(e.ctx, guiAddr, "", "mfs1.wrong", guiUser, guiPW)
	wantAPI(t, werr, api.CodeConflict)
	if _, _, err := e.svc.CompleteGUISetup(e.ctx, guiAddr, "", tok, "Bad Name", guiPW); err == nil {
		t.Fatal("bad username accepted")
	}
	if _, _, err := e.svc.CompleteGUISetup(e.ctx, guiAddr, "", tok, guiUser, "short"); err == nil {
		t.Fatal("short password accepted")
	}
	if e.sessionCount() != 0 {
		t.Fatal("session from failed setup")
	}
	if _, _, err := e.svc.CompleteGUISetup(e.ctx, guiAddr, "", tok, guiUser, guiPW); err != nil {
		t.Fatal(err)
	}
	// Replay of a used token (ErrStale) and an expired token look the same as a wrong one.
	_, _, rerr := e.svc.CompleteGUISetup(e.ctx, guiAddr, "", tok, "other", guiPW)
	if rerr == nil || rerr.Error() != werr.Error() {
		t.Fatalf("replay err %v != wrong-token err %v", rerr, werr)
	}
	// Admin exists + valid-looking token: conflict path gives the same error.
	_, _, cerr := e.svc.CompleteGUISetup(e.ctx, guiAddr, "", "mfs1.zzzz", "other", guiPW)
	if cerr == nil || cerr.Error() != werr.Error() {
		t.Fatalf("conflict err %v != %v", cerr, werr)
	}
}

func TestGUISetupTokenExpires(t *testing.T) {
	e := newGUIEnv(t)
	tok, _, _ := e.svc.EnsureGUISetup(e.ctx)
	e.advance(GUISetupTTL + time.Second)
	_, _, err := e.svc.CompleteGUISetup(e.ctx, guiAddr, "", tok, guiUser, guiPW)
	wantAPI(t, err, api.CodeConflict)
	// At the next start an expired token is replaced.
	tok2, created, err := e.svc.EnsureGUISetup(e.ctx)
	if err != nil || !created || tok2 == tok {
		t.Fatalf("EnsureGUISetup after expiry = %q %v %v", tok2, created, err)
	}
}

func TestGUILoginOneError(t *testing.T) {
	e := newGUIEnv(t)
	// Before any admin exists: same error.
	_, _, none := e.svc.GUILogin(e.ctx, guiAddr, "", guiUser, guiPW)
	e.setup()
	_, _, unknown := e.svc.GUILogin(e.ctx, guiAddr, "", "nobody", guiPW)
	_, _, wrong := e.svc.GUILogin(e.ctx, guiAddr, "", guiUser, "wrong password!!")
	_, _, huge := e.svc.GUILogin(e.ctx, guiAddr, "", guiUser, strings.Repeat("x", auth.MaxPasswordBytes+1))
	for _, err := range []error{none, unknown, wrong, huge} {
		wantAPI(t, err, api.CodeUnauthenticated)
		if err.Error() != unknown.Error() {
			t.Fatalf("distinguishable login errors: %v vs %v", err, unknown)
		}
	}
	if strings.Contains(e.auditJSON(), `"gui.login"`) && e.sessionCount() != 1 {
		t.Fatal("failed login created a session")
	}
}

// cheapHash is a valid Argon2id PHC string with parameters other than the current ones.
func cheapHash(pw string) string {
	salt := []byte("0123456789abcdef")
	key := argon2.IDKey([]byte(pw), salt, 1, 8, 1, 32)
	return fmt.Sprintf("$argon2id$v=19$m=8,t=1,p=1$%s$%s",
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
}

func TestGUILoginRehashesOldParameters(t *testing.T) {
	e := newGUIEnv(t)
	tok := bytes.Repeat([]byte{7}, 32)
	h := sha256.Sum256(tok)
	if err := e.db.PutGUISetup(e.ctx, h[:], t0.Add(time.Hour), t0, store.AuditEvent{Time: t0, Source: "system", ActorKind: "system", Action: ActionGUISetupToken, Outcome: "ok"}); err != nil {
		t.Fatal(err)
	}
	old := cheapHash(guiPW)
	adm, err := e.db.CompleteGUISetup(e.ctx, h[:], store.GUIAdmin{Username: guiUser, PwHash: old}, t0,
		store.AuditEvent{Time: t0, Source: "gui", ActorKind: "admin", Actor: guiUser, Action: ActionGUISetup, Outcome: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	if !auth.NeedsRehash(old) {
		t.Fatal("test hash should need a rehash")
	}
	// An older session of that admin dies with the rehash (generation bump).
	first, _ := newToken("")
	if _, err := e.db.CreateGUISession(e.ctx, store.GUISession{TokenHash: tokenHash(first), AdminID: adm.ID, Generation: adm.Generation,
		ExpiresAt: t0.Add(time.Hour)}, 16, t0, store.AuditEvent{Time: t0, Source: "gui", ActorKind: "admin", Action: ActionGUILogin, Outcome: "ok"}); err != nil {
		t.Fatal(err)
	}
	tokn, p, err := e.svc.GUILogin(e.ctx, guiAddr, "ua", guiUser, guiPW)
	if err != nil {
		t.Fatal(err)
	}
	after, err := e.db.GetGUIAdmin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if auth.NeedsRehash(after.PwHash) || after.PwHash == old || after.Generation != adm.Generation+1 {
		t.Fatalf("hash not upgraded: gen %d rehash=%v", after.Generation, auth.NeedsRehash(after.PwHash))
	}
	if _, _, err := e.svc.GUIAuthenticate(e.ctx, guiAddr, tokn, true); err != nil {
		t.Fatalf("new session unusable: %v", err)
	}
	if e.hasSession(first) {
		t.Fatal("old session survived the credential change")
	}
	if p.Name != guiUser {
		t.Fatalf("principal %+v", p)
	}
	// The upgraded hash verifies and needs no further rehash: the next login is a plain one.
	if _, _, err := e.svc.GUILogin(e.ctx, guiAddr, "ua", guiUser, guiPW); err != nil {
		t.Fatal(err)
	}
	again, _ := e.db.GetGUIAdmin(e.ctx)
	if again.Generation != after.Generation {
		t.Fatal("plain login bumped the generation")
	}
}

func lastActive(t *testing.T, e *guiEnv, id int64) time.Time {
	t.Helper()
	var ms int64
	if err := e.raw.QueryRow(`SELECT last_active_at FROM gui_sessions WHERE id = ?`, id).Scan(&ms); err != nil {
		t.Fatal(err)
	}
	return time.UnixMilli(ms).UTC()
}

func TestGUIAuthenticateClock(t *testing.T) {
	e := newGUIEnv(t)
	_, tok, _ := e.setup()
	p, id, err := e.svc.GUIAuthenticate(e.ctx, "198.51.100.1:1", tok, false)
	if err != nil || p.Name != guiUser || p.Remote != "198.51.100.1:1" {
		t.Fatalf("auth = %+v %v", p, err)
	}
	// Touch throttle: within 60 s no write, at 60 s a write.
	e.advance(59 * time.Second)
	if _, _, err := e.svc.GUIAuthenticate(e.ctx, "", tok, false); err != nil {
		t.Fatal(err)
	}
	if got := lastActive(t, e, id); !got.Equal(t0) {
		t.Fatalf("touched inside the throttle window: %v", got)
	}
	e.advance(time.Second)
	if _, _, err := e.svc.GUIAuthenticate(e.ctx, "", tok, false); err != nil {
		t.Fatal(err)
	}
	if got := lastActive(t, e, id); !got.Equal(t0.Add(60 * time.Second)) {
		t.Fatalf("not touched at 60s: %v", got)
	}
	// Passive requests never touch, however old the row.
	e.advance(10 * time.Minute)
	if _, _, err := e.svc.GUIAuthenticate(e.ctx, "", tok, true); err != nil {
		t.Fatal(err)
	}
	if got := lastActive(t, e, id); !got.Equal(t0.Add(60 * time.Second)) {
		t.Fatalf("passive request touched: %v", got)
	}
	// Idle: 30 min after the last active write, the passive polls did not keep it alive.
	e.advance(20*time.Minute - time.Second)
	if _, _, err := e.svc.GUIAuthenticate(e.ctx, "", tok, true); err != nil {
		t.Fatalf("29m59s idle refused: %v", err)
	}
	e.advance(time.Second)
	_, _, err = e.svc.GUIAuthenticate(e.ctx, "", tok, true)
	wantAPI(t, err, api.CodeUnauthenticated)
	if e.sessionCount() != 0 || e.hasSession(tok) {
		t.Fatal("idle session row not deleted")
	}
}

func TestGUIAuthenticateAbsoluteExpiry(t *testing.T) {
	e := newGUIEnv(t)
	_, tok, _ := e.setup()
	// Stay active every 20 minutes: the 12 h absolute limit still ends the session.
	for i := 0; i < 35; i++ {
		e.advance(20 * time.Minute)
		if _, _, err := e.svc.GUIAuthenticate(e.ctx, "", tok, false); err != nil {
			t.Fatalf("refused at %v: %v", e.now.Sub(t0), err)
		}
	}
	e.advance(20*time.Minute - time.Second) // 11h59m59s since login... (35*20m=11h40m + 19m59s)
	if _, _, err := e.svc.GUIAuthenticate(e.ctx, "", tok, false); err != nil {
		t.Fatalf("refused just before 12h: %v", err)
	}
	e.advance(time.Second)
	_, _, err := e.svc.GUIAuthenticate(e.ctx, "", tok, false)
	wantAPI(t, err, api.CodeUnauthenticated)
	if e.sessionCount() != 0 {
		t.Fatal("expired session row not deleted")
	}
}

func TestGUIAuthenticateRefusesStaleGenerationAndEditedRows(t *testing.T) {
	e := newGUIEnv(t)
	_, tok, _ := e.setup()
	loginTok, _, err := e.svc.GUILogin(e.ctx, guiAddr, "", guiUser, guiPW)
	if err != nil {
		t.Fatal(err)
	}
	// Generation edited behind the service's back: refused and the row deleted.
	if _, err := e.raw.Exec(`UPDATE gui_admins SET generation = generation + 1`); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{tok, loginTok} {
		_, _, err := e.svc.GUIAuthenticate(e.ctx, "", s, false)
		wantAPI(t, err, api.CodeUnauthenticated)
	}
	if e.sessionCount() != 0 {
		t.Fatalf("stale-generation rows remain: %d", e.sessionCount())
	}

	// Absolute expiry edited into the past (the store does not judge expiry): refused, deleted.
	e2 := newGUIEnv(t)
	_, tok2, _ := e2.setup()
	if _, err := e2.raw.Exec(`UPDATE gui_sessions SET expires_at = ?`, t0.Add(-time.Second).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	_, _, err = e2.svc.GUIAuthenticate(e2.ctx, "", tok2, false)
	wantAPI(t, err, api.CodeUnauthenticated)
	if e2.sessionCount() != 0 {
		t.Fatal("expired row remains")
	}

	// Unknown token, empty token, oversized token: the same error.
	for _, bad := range []string{"", "nope", strings.Repeat("a", 4096)} {
		_, _, err := e2.svc.GUIAuthenticate(e2.ctx, "", bad, false)
		wantAPI(t, err, api.CodeUnauthenticated)
	}
}

func TestGUIAuthenticateRefusesResetAdmin(t *testing.T) {
	e := newGUIEnv(t)
	_, tok, _ := e.setup()
	// Admin flipped to "reset" behind the service's back: the join no longer yields a session;
	// the row is removed by the purge.
	if _, err := e.raw.Exec(`UPDATE gui_admins SET status = 'reset'`); err != nil {
		t.Fatal(err)
	}
	_, _, err := e.svc.GUIAuthenticate(e.ctx, "", tok, false)
	wantAPI(t, err, api.CodeUnauthenticated)
	if n, err := e.svc.PurgeGUISessions(e.ctx); err != nil || n != 1 {
		t.Fatalf("purge = %d %v", n, err)
	}
	if e.sessionCount() != 0 {
		t.Fatal("dead row remains")
	}

	// The real reset path: all sessions gone at once, a new setup token, audited, token not logged.
	e2 := newGUIEnv(t)
	_, tok2, p := e2.setup()
	other, _, _ := e2.svc.GUILogin(e2.ctx, guiAddr, "", guiUser, guiPW)
	setup, err := e2.svc.ResetGUIAdmin(e2.ctx, auth.LocalAdmin(501), SourceCLI)
	if err != nil || !strings.HasPrefix(setup, SetupTokenPrefix) {
		t.Fatalf("reset = %q %v", setup, err)
	}
	for _, s := range []string{tok2, other} {
		_, _, err := e2.svc.GUIAuthenticate(e2.ctx, "", s, false)
		wantAPI(t, err, api.CodeUnauthenticated)
	}
	if e2.sessionCount() != 0 {
		t.Fatal("sessions remain after reset")
	}
	if strings.Contains(e2.auditJSON()+e2.logs.String(), setup) {
		t.Fatal("setup token in audit or logs")
	}
	if !strings.Contains(strings.Join(e2.actions(), " "), "gui.reset_admin/ok") {
		t.Fatalf("reset not audited: %v", e2.actions())
	}
	// The old principal no longer maps to an account.
	if _, err := e2.svc.ListGUISessions(e2.ctx, p); err == nil {
		t.Fatal("ListGUISessions without an active admin")
	}
	// The new token sets up a fresh admin; the old password no longer logs in.
	if _, _, err := e2.svc.CompleteGUISetup(e2.ctx, guiAddr, "", setup, "second-admin", "a brand new passphrase"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e2.svc.GUILogin(e2.ctx, guiAddr, "", guiUser, guiPW); err == nil {
		t.Fatal("old admin can log in")
	}
}

func TestGUIPasswordChange(t *testing.T) {
	e := newGUIEnv(t)
	_, cur, p := e.setup()
	other, _, err := e.svc.GUILogin(e.ctx, guiAddr, "ua/other", guiUser, guiPW)
	if err != nil {
		t.Fatal(err)
	}
	_, id, _ := e.svc.GUIAuthenticate(e.ctx, guiAddr, cur, true)
	before, _ := e.db.ListGUISessions(e.ctx, 1)

	// Wrong current password and a bad new one change nothing.
	if _, err := e.svc.ChangeGUIPassword(e.ctx, p, id, "not the password", "new passphrase 1"); err == nil {
		t.Fatal("wrong current password accepted")
	}
	if _, err := e.svc.ChangeGUIPassword(e.ctx, p, id, guiPW, "short"); err == nil {
		t.Fatal("weak new password accepted")
	}
	if !e.hasSession(cur) || !e.hasSession(other) {
		t.Fatal("failed change touched sessions")
	}

	newTok, err := e.svc.ChangeGUIPassword(e.ctx, p, id, guiPW, "new passphrase 1")
	if err != nil {
		t.Fatal(err)
	}
	if newTok == cur || newTok == "" {
		t.Fatal("no new session token")
	}
	for _, s := range []string{cur, other} {
		_, _, err := e.svc.GUIAuthenticate(e.ctx, "", s, false)
		wantAPI(t, err, api.CodeUnauthenticated)
	}
	if _, _, err := e.svc.GUIAuthenticate(e.ctx, "", newTok, false); err != nil {
		t.Fatalf("re-issued session refused: %v", err)
	}
	if e.sessionCount() != 1 {
		t.Fatalf("sessions = %d, want 1", e.sessionCount())
	}
	after, _ := e.db.ListGUISessions(e.ctx, 1)
	if len(after) != 1 || after[0].Remote != guiAddr || after[0].UserAgent != "ua/1" || !after[0].ExpiresAt.Equal(before[0].ExpiresAt) {
		t.Fatalf("kept session lost its attributes or got a longer life: %+v", after)
	}
	if _, _, err := e.svc.GUILogin(e.ctx, guiAddr, "", guiUser, guiPW); err == nil {
		t.Fatal("old password still works")
	}
	if _, _, err := e.svc.GUILogin(e.ctx, guiAddr, "", guiUser, "new passphrase 1"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(e.actions(), " "), "gui.password_change/ok") {
		t.Fatalf("not audited: %v", e.actions())
	}
}

func TestGUISessionManagement(t *testing.T) {
	e := newGUIEnv(t)
	_, cur, p := e.setup()
	t2, _, _ := e.svc.GUILogin(e.ctx, guiAddr, "", guiUser, guiPW)
	t3, _, _ := e.svc.GUILogin(e.ctx, guiAddr, "", guiUser, guiPW)
	list, err := e.svc.ListGUISessions(e.ctx, p)
	if err != nil || len(list) != 3 {
		t.Fatalf("list = %d %v", len(list), err)
	}
	for _, s := range list {
		if s.TokenHash != nil {
			t.Fatal("token hash in session view")
		}
	}
	_, id2, _ := e.svc.GUIAuthenticate(e.ctx, "", t2, true)
	if err := e.svc.RevokeGUISession(e.ctx, p, id2); err != nil || e.hasSession(t2) {
		t.Fatalf("revoke: %v", err)
	}
	wantAPI(t, e.svc.RevokeGUISession(e.ctx, p, id2), api.CodeNotFound)
	_, curID, _ := e.svc.GUIAuthenticate(e.ctx, "", cur, true)
	n, err := e.svc.RevokeOtherGUISessions(e.ctx, p, curID)
	if err != nil || n != 1 || e.hasSession(t3) || !e.hasSession(cur) {
		t.Fatalf("revoke others: %d %v", n, err)
	}
	if err := e.svc.GUILogout(e.ctx, p, curID); err != nil || e.hasSession(cur) {
		t.Fatalf("logout: %v", err)
	}
	if err := e.svc.GUILogout(e.ctx, p, curID); err != nil {
		t.Fatalf("second logout: %v", err)
	}
	acts := strings.Join(e.actions(), " ")
	for _, want := range []string{"gui.session_revoke/ok", "gui.logout/ok", "gui.login/ok", "gui.setup/ok", "gui.setup_token/ok"} {
		if !strings.Contains(acts, want) {
			t.Fatalf("missing audit %s in %v", want, e.actions())
		}
	}
	// Dead-session housekeeping is unaudited.
	if strings.Count(acts, "gui.logout") != 1 {
		t.Fatalf("logout audited %d times", strings.Count(acts, "gui.logout"))
	}
}

func TestGUIAdminMethodsDoubleGate(t *testing.T) {
	e := newGUIEnv(t)
	_, _, admin := e.setup()
	e.svc.CreateFleet(e.ctx, admin, SourceGUI, "dev", "", false, nil)
	if _, err := e.svc.CreateFleet(e.ctx, auth.LocalAdmin(501), SourceCLI, "ops", "", true, nil); err != nil {
		t.Fatal(err)
	}
	op := auth.Principal{Kind: auth.KindOperator, ID: 99, Name: "mallory"}
	enroll := auth.Principal{Kind: auth.KindEnroll}
	// A policy that allows everything: the admin-kind gate must still refuse.
	permissive := newGUIEnv(t)
	permissive.svc.pol = allowAll{}
	_, _, _ = permissive.setup()

	for _, env := range []*guiEnv{e, permissive} {
		for _, p := range []auth.Principal{op, enroll, {Kind: auth.KindRunner, ID: 1, Name: "r"}} {
			before := len(env.actions())
			calls := map[string]func() error{
				"reset":   func() error { _, err := env.svc.ResetGUIAdmin(env.ctx, p, SourceGUI); return err },
				"confirm": func() error { _, err := env.svc.SetFleetConfirm(env.ctx, p, SourceGUI, "dev", false); return err },
				"audit":   func() error { _, err := env.svc.ListAudit(env.ctx, p, SourceGUI, store.AuditFilter{}); return err },
				"export":  func() error { return env.svc.RecordAuditExport(env.ctx, p, SourceGUI, store.AuditFilter{}, "csv") },
				"runners": func() error { _, err := env.svc.ListRunnersAll(env.ctx, p, SourceGUI, "dev", true); return err },
				"sess":    func() error { _, err := env.svc.ListGUISessions(env.ctx, p); return err },
				"revoke":  func() error { return env.svc.RevokeGUISession(env.ctx, p, 1) },
				"others":  func() error { _, err := env.svc.RevokeOtherGUISessions(env.ctx, p, 1); return err },
				"logout":  func() error { return env.svc.GUILogout(env.ctx, p, 1) },
				"chpw": func() error {
					_, err := env.svc.ChangeGUIPassword(env.ctx, p, 1, guiPW, "yet another phrase")
					return err
				},
			}
			for name, call := range calls {
				wantAPI(t, call(), api.CodeForbidden)
				_ = name
			}
			if got := len(env.actions()) - before; got != len(calls) {
				t.Fatalf("%s: %d audit rows for %d denied calls", p.Kind, got, len(calls))
			}
		}
	}
	// Every denial is audited as denied; nothing changed.
	acts := strings.Join(e.actions(), " ")
	if !strings.Contains(acts, "gui.reset_admin/denied") || !strings.Contains(acts, "fleet.set_confirm/denied") || !strings.Contains(acts, "audit.list/denied") {
		t.Fatalf("denials not audited: %v", e.actions())
	}
	if f, _ := e.svc.GetFleet(e.ctx, e.adminP(), SourceCLI, "dev"); f.Confirm {
		t.Fatal("guard changed by a refused call")
	}
	// An admin principal whose name is not the GUI account (the local socket admin) has no
	// session methods.
	wantAPI(t, e.svc.GUILogout(e.ctx, auth.LocalAdmin(501), 1), api.CodeForbidden)
	// A non-admin cannot list revoked runners but can list the normal view only through ListRunners.
	if _, err := e.svc.ListRunnersAll(e.ctx, admin, SourceGUI, "dev", true); err != nil {
		t.Fatal(err)
	}
}

func (e *guiEnv) adminP() auth.Principal { return auth.LocalAdmin(501) }

func TestSetFleetConfirmAudited(t *testing.T) {
	e := newGUIEnv(t)
	_, _, admin := e.setup()
	if _, err := e.svc.CreateFleet(e.ctx, admin, SourceGUI, "dev", "", false, nil); err != nil {
		t.Fatal(err)
	}
	f, err := e.svc.SetFleetConfirm(e.ctx, admin, SourceGUI, "dev", true)
	if err != nil || !f.Confirm {
		t.Fatalf("set = %+v %v", f, err)
	}
	evs, _ := e.svc.ListAudit(e.ctx, admin, SourceGUI, store.AuditFilter{Action: api.MethodFleetSetConfirm})
	if len(evs) != 1 || evs[0].Source != "gui" || evs[0].Actor != guiUser || evs[0].Params["from"] != "false" || evs[0].Params["to"] != "true" {
		t.Fatalf("audit = %+v", evs)
	}
	wantAPI(t, func() error { _, err := e.svc.SetFleetConfirm(e.ctx, admin, SourceGUI, "nope", true); return err }(), api.CodeNotFound)
	if _, err := e.svc.SetFleetConfirm(e.ctx, admin, SourceGUI, "Bad Name", true); err == nil {
		t.Fatal("bad fleet name accepted")
	}
}

func TestListAuditAndExportRecord(t *testing.T) {
	e := newGUIEnv(t)
	_, _, admin := e.setup()
	f := store.AuditFilter{Action: "gui.", ActionPrefix: true, Desc: true, Limit: 2}
	evs, err := e.svc.ListAudit(e.ctx, admin, SourceGUI, f)
	if err != nil || len(evs) != 2 || evs[0].ID < evs[1].ID {
		t.Fatalf("ListAudit = %v %v", evs, err)
	}
	if _, err := e.svc.ListAudit(e.ctx, admin, SourceGUI, store.AuditFilter{Limit: -1}); err == nil {
		t.Fatal("negative limit accepted")
	}
	if err := e.svc.RecordAuditExport(e.ctx, admin, SourceGUI, store.AuditFilter{Actor: guiUser, From: t0}, "jsonl"); err != nil {
		t.Fatal(err)
	}
	ex, _ := e.svc.ListAudit(e.ctx, admin, SourceGUI, store.AuditFilter{Action: ActionAuditExport})
	if len(ex) != 1 || ex[0].Params["actor"] != guiUser || ex[0].Params["format"] != "jsonl" || ex[0].Params["from"] == "" {
		t.Fatalf("export row = %+v", ex)
	}
}

func TestListRunnersAllIncludesRevokedForAdminOnly(t *testing.T) {
	e := newEnv(t)
	e.fleet("dev")
	r := e.enrollRunner("dev", "r1", true)
	if _, err := e.svc.Revoke(e.ctx, e.admin, SourceCLI, "dev/"+r.Name); err != nil {
		t.Fatal(err)
	}
	e.enrollRunner("dev", "r2", true)
	if vs, err := e.svc.ListRunners(e.ctx, e.admin, SourceCLI, "dev"); err != nil || len(vs) != 1 {
		t.Fatalf("ListRunners = %d %v", len(vs), err)
	}
	vs, err := e.svc.ListRunnersAll(e.ctx, e.admin, SourceGUI, "dev", true)
	if err != nil || len(vs) != 2 {
		t.Fatalf("ListRunnersAll = %d %v", len(vs), err)
	}
	if vs, err := e.svc.ListRunnersAll(e.ctx, e.admin, SourceGUI, "dev", false); err != nil || len(vs) != 1 {
		t.Fatalf("ListRunnersAll(false) = %d %v", len(vs), err)
	}
	if _, err := e.svc.ListRunnersAll(e.ctx, e.admin, SourceGUI, "bad name", true); err == nil {
		t.Fatal("bad fleet accepted")
	}
}

// conflictStore forces store.ErrConflict from the calls a concurrent writer would win.
type conflictStore struct {
	store.Store
	setPassword, ensure bool
}

func (c conflictStore) SetGUIPassword(ctx context.Context, id, gen int64, hash string, keep store.GUISession, replaceID int64, now time.Time, ev store.AuditEvent) (store.GUIAdmin, error) {
	if c.setPassword {
		return store.GUIAdmin{}, store.ErrConflict
	}
	return c.Store.SetGUIPassword(ctx, id, gen, hash, keep, replaceID, now, ev)
}

func (c conflictStore) EnsureGUISetup(ctx context.Context, h []byte, exp, now time.Time, ev store.AuditEvent) (bool, error) {
	if c.ensure {
		return false, store.ErrConflict
	}
	return c.Store.EnsureGUISetup(ctx, h, exp, now, ev)
}

func TestGUILoginRehashConflictOpensSession(t *testing.T) {
	e := newGUIEnv(t)
	e.setup()
	if _, err := e.raw.Exec(`UPDATE gui_admins SET pw_hash = ?`, cheapHash(guiPW)); err != nil {
		t.Fatal(err)
	}
	e.svc.st = conflictStore{Store: e.db, setPassword: true}
	tok, p, err := e.svc.GUILogin(e.ctx, guiAddr, "ua", guiUser, guiPW)
	if err != nil || tok == "" || p.Name != guiUser {
		t.Fatalf("login after lost rehash race: %q %+v %v", tok, p, err)
	}
	if _, _, err := e.svc.GUIAuthenticate(e.ctx, guiAddr, tok, true); err != nil {
		t.Fatalf("session unusable: %v", err)
	}
	if _, _, err := e.svc.GUILogin(e.ctx, guiAddr, "ua", guiUser, "wrong password here"); err == nil {
		t.Fatal("wrong password accepted on the conflict path")
	}
}

func TestChangeGUIPasswordConflictIsConflict(t *testing.T) {
	e := newGUIEnv(t)
	tokSess, _, p := e.setup()
	_ = tokSess
	sessions, _ := e.svc.ListGUISessions(e.ctx, p)
	e.svc.st = conflictStore{Store: e.db, setPassword: true}
	_, err := e.svc.ChangeGUIPassword(e.ctx, p, sessions[0].ID, guiPW, "yet another phrase")
	wantAPI(t, err, api.CodeConflict)
}

func TestEnsureGUISetupConflictIsNotCreated(t *testing.T) {
	e := newGUIEnv(t)
	e.svc.st = conflictStore{Store: e.db, ensure: true}
	tok, created, err := e.svc.EnsureGUISetup(e.ctx)
	if err != nil || created || tok != "" {
		t.Fatalf("EnsureGUISetup = %q %v %v", tok, created, err)
	}
}

func TestResetGUIAdminOnlyFromCLI(t *testing.T) {
	e := newGUIEnv(t)
	_, sess, admin := e.setup()
	for _, c := range []struct {
		p   auth.Principal
		src Source
	}{{admin, SourceGUI}, {auth.LocalAdmin(501), SourceAPI}, {auth.LocalAdmin(501), SourceGUI}} {
		before := len(e.actions())
		_, err := e.svc.ResetGUIAdmin(e.ctx, c.p, c.src)
		wantAPI(t, err, api.CodeForbidden)
		if len(e.actions()) != before+1 || !strings.Contains(strings.Join(e.actions(), " "), "gui.reset_admin/denied") {
			t.Fatalf("%s denial not audited once: %v", c.src, e.actions())
		}
	}
	if !e.hasSession(sess) {
		t.Fatal("a refused reset ended the session")
	}
}

func TestRecordAuditExportValidates(t *testing.T) {
	e := newGUIEnv(t)
	_, _, admin := e.setup()
	before := len(e.actions())
	long := strings.Repeat("x", maxAuditFilterString+1)
	for name, c := range map[string]struct {
		f   store.AuditFilter
		fmt string
	}{
		"negative limit":  {store.AuditFilter{Limit: -1}, "csv"},
		"negative since":  {store.AuditFilter{AfterID: -1}, "csv"},
		"negative before": {store.AuditFilter{BeforeID: -1}, "csv"},
		"bad format":      {store.AuditFilter{}, "xml"},
		"empty format":    {store.AuditFilter{}, ""},
		"long actor":      {store.AuditFilter{Actor: long}, "csv"},
		"long action":     {store.AuditFilter{Action: long}, "jsonl"},
	} {
		wantAPI(t, e.svc.RecordAuditExport(e.ctx, admin, SourceGUI, c.f, c.fmt), api.CodeBadRequest)
		if len(e.actions()) != before {
			t.Fatalf("%s: a refused export wrote a row", name)
		}
	}
	for _, f := range []string{"csv", "jsonl"} {
		if err := e.svc.RecordAuditExport(e.ctx, admin, SourceGUI, store.AuditFilter{}, f); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
}
