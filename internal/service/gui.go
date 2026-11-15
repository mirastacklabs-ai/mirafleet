package service

// Admin GUI accounts and sessions (docs/M4.md §2.3, §2.4, §2.9). The service owns the session
// clock: GUIAuthenticate compares the credential generation and both expiries on every request,
// using Config.Now, so expiry is identical after a restart and testable with an injected clock.
// The store only holds rows. Secrets (setup token, session token) are returned to the caller
// once and never written anywhere: the store keeps SHA-256 of them, and no audit row or log
// line contains a token, a password or a hash.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strconv"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
)

// Session and setup limits (docs/M4.md §2.3, §2.4).
const (
	GUIIdleTimeout     = 30 * time.Minute
	GUIAbsoluteTimeout = 12 * time.Hour
	// GUITouchEvery throttles last-active writes: at most one per session per interval.
	GUITouchEvery = 60 * time.Second
	// GUIMaxSessionsPerAdmin: the oldest rows beyond this are evicted when a session is created.
	GUIMaxSessionsPerAdmin = 16
	GUISetupTTL            = 24 * time.Hour

	// SetupTokenPrefix starts every setup token.
	SetupTokenPrefix = "mfs1."
	// maxCredentialBytes bounds any token or username hashed or compared before a lookup.
	maxCredentialBytes = 512
)

// Audit actions of the GUI account flows (gui.reset_admin is api.MethodGUIResetAdmin).
const (
	ActionGUISetupToken     = "gui.setup_token"
	ActionGUISetup          = "gui.setup"
	ActionGUILogin          = "gui.login"
	ActionGUILogout         = "gui.logout"
	ActionGUIPasswordChange = "gui.password_change"
	ActionGUISessionRevoke  = "gui.session_revoke"
)

var (
	// errBadLogin is the single refusal for an unknown user and a wrong password.
	errBadLogin = api.Errorf(api.CodeUnauthenticated, "invalid username or password")
	// errSetupRefused is the single refusal for a wrong, expired or already-used setup token
	// and for a lost setup race (G-07).
	errSetupRefused = api.Errorf(api.CodeConflict, "setup is not available")
	errNoSession    = api.Errorf(api.CodeUnauthenticated, "unauthenticated")

	// ErrInvalidUsername and ErrWrongCurrentPassword are sentinels (bad_request) the GUI matches
	// with errors.Is to put the message on the right form field.
	ErrInvalidUsername      = api.Errorf(api.CodeBadRequest, "invalid username")
	ErrWrongCurrentPassword = api.Errorf(api.CodeBadRequest, "current password is incorrect")
)

func tokenHash(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// newToken returns 32 random bytes as unpadded base64url, with an optional prefix.
func newToken(prefix string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b), nil
}

func (s *Service) systemEvent(action string) store.AuditEvent {
	return store.AuditEvent{Time: s.now(), Source: string(SourceSystem), ActorKind: "system",
		Actor: "gui", Action: action, Outcome: OutcomeOK}
}

func (s *Service) guiEvent(username, remote, action string) store.AuditEvent {
	return store.AuditEvent{Time: s.now(), Source: string(SourceGUI), ActorKind: string(auth.KindAdmin),
		Actor: username, Remote: remote, Action: action, Outcome: OutcomeOK}
}

// EnsureGUISetup creates a setup token when no active admin exists and no live token does, and
// returns it (created true). The caller (the GUI at start) writes it to its token file; the
// service records only the hash. With an admin or a live token present it returns created false.
func (s *Service) EnsureGUISetup(ctx context.Context) (token string, created bool, err error) {
	if _, err := s.st.GetGUIAdmin(ctx); err == nil {
		return "", false, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", false, s.fail("load gui admin", err)
	}
	tok, err := newToken(SetupTokenPrefix)
	if err != nil {
		return "", false, s.fail("new setup token", err)
	}
	now := s.now()
	ev := s.systemEvent(ActionGUISetupToken)
	ev.Params = map[string]string{"expires": now.Add(GUISetupTTL).UTC().Format(time.RFC3339)}
	created, err = s.st.EnsureGUISetup(ctx, tokenHash(tok), now.Add(GUISetupTTL), now, ev)
	if errors.Is(err, store.ErrConflict) { // another start created the admin or token first
		return "", false, nil
	}
	if err != nil {
		return "", false, s.fail("ensure gui setup", err)
	}
	if !created {
		return "", false, nil
	}
	return tok, true, nil
}

// ReissueGUISetup replaces the setup token with a fresh one while no active admin exists (the
// GUI calls it when the live token's file is gone: the raw token cannot be recovered from its
// hash). With an admin present it returns created false and writes nothing.
func (s *Service) ReissueGUISetup(ctx context.Context) (token string, created bool, err error) {
	if _, err := s.st.GetGUIAdmin(ctx); err == nil {
		return "", false, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", false, s.fail("load gui admin", err)
	}
	tok, err := newToken(SetupTokenPrefix)
	if err != nil {
		return "", false, s.fail("new setup token", err)
	}
	now := s.now()
	ev := s.systemEvent(ActionGUISetupToken)
	ev.Params = map[string]string{"expires": now.Add(GUISetupTTL).UTC().Format(time.RFC3339)}
	if err := s.st.PutGUISetup(ctx, tokenHash(tok), now.Add(GUISetupTTL), now, ev); err != nil {
		if errors.Is(err, store.ErrConflict) { // an admin appeared meanwhile
			return "", false, nil
		}
		return "", false, s.fail("put gui setup", err)
	}
	return tok, true, nil
}

// newSession builds a session row for admin a and returns it with the raw token.
func (s *Service) newSession(adminID, generation int64, remote, ua string) (store.GUISession, string, error) {
	tok, err := newToken("")
	if err != nil {
		return store.GUISession{}, "", err
	}
	now := s.now()
	return store.GUISession{TokenHash: tokenHash(tok), AdminID: adminID, Generation: generation,
		CreatedAt: now, LastActiveAt: now, ExpiresAt: now.Add(GUIAbsoluteTimeout),
		Remote: remote, UserAgent: ua}, tok, nil
}

// CompleteGUISetup consumes the setup token, creates the first admin and logs it in. The token
// is the credential. A wrong, expired or used token and a lost race all return one generic
// error (G-07). Returns the raw session token (for the cookie) and the admin principal.
func (s *Service) CompleteGUISetup(ctx context.Context, remote, ua, token, username, password string) (string, auth.Principal, error) {
	if !store.ValidOperatorName(username) {
		return "", auth.Principal{}, ErrInvalidUsername
	}
	if err := auth.ValidatePassword(password, username); err != nil {
		return "", auth.Principal{}, badRequest("%s", err.Error())
	}
	if len(token) == 0 || len(token) > maxCredentialBytes {
		return "", auth.Principal{}, errSetupRefused
	}
	// G-21: the token is checked (constant time) before the expensive password hash, so a guess
	// with a wrong token costs no Argon2 run. The transaction below repeats the check.
	now := s.now()
	if err := s.st.CheckGUISetup(ctx, tokenHash(token), now); err != nil {
		if errors.Is(err, store.ErrStale) || errors.Is(err, store.ErrInvalid) {
			return "", auth.Principal{}, errSetupRefused
		}
		return "", auth.Principal{}, s.fail("check gui setup", err)
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return "", auth.Principal{}, s.fail("hash password", err)
	}
	ev := s.guiEvent(username, remote, ActionGUISetup)
	ev.Subject = username
	adm, err := s.st.CompleteGUISetup(ctx, tokenHash(token), store.GUIAdmin{Username: username, PwHash: hash}, now, ev)
	if err != nil {
		if errors.Is(err, store.ErrStale) || errors.Is(err, store.ErrConflict) {
			return "", auth.Principal{}, errSetupRefused
		}
		return "", auth.Principal{}, s.fail("complete gui setup", err)
	}
	return s.openSession(ctx, adm, remote, ua, "after setup")
}

// openSession creates a session for adm with its gui.login audit row.
func (s *Service) openSession(ctx context.Context, adm store.GUIAdmin, remote, ua, msg string) (string, auth.Principal, error) {
	sess, tok, err := s.newSession(adm.ID, adm.Generation, remote, ua)
	if err != nil {
		return "", auth.Principal{}, s.fail("new session", err)
	}
	ev := s.guiEvent(adm.Username, remote, ActionGUILogin)
	ev.Message = msg
	if _, err := s.st.CreateGUISession(ctx, sess, GUIMaxSessionsPerAdmin, s.now(), ev); err != nil {
		return "", auth.Principal{}, s.fail("create gui session", err)
	}
	return tok, auth.GUIAdmin(adm.Username, remote), nil
}

// GUILogin checks the credentials and creates a new session (always a fresh token). An unknown
// user and a wrong password return the same error after the same work (a dummy hash is burned
// for an unknown user). When the stored hash was made with older parameters it is replaced by
// a fresh one in the same transaction as the session; that is a credential change, so it bumps
// the generation and ends the admin's other sessions.
func (s *Service) GUILogin(ctx context.Context, remote, ua, username, password string) (string, auth.Principal, error) {
	if len(username) > maxCredentialBytes || len(password) > auth.MaxPasswordBytes {
		return "", auth.Principal{}, errBadLogin
	}
	adm, err := s.st.GetGUIAdmin(ctx)
	switch {
	case errors.Is(err, store.ErrNotFound):
		auth.DummyVerify(password)
		return "", auth.Principal{}, errBadLogin
	case err != nil:
		return "", auth.Principal{}, s.fail("load gui admin", err)
	}
	if subtle.ConstantTimeCompare([]byte(username), []byte(adm.Username)) != 1 {
		auth.DummyVerify(password)
		return "", auth.Principal{}, errBadLogin
	}
	if !auth.VerifyPassword(password, adm.PwHash) {
		return "", auth.Principal{}, errBadLogin
	}
	if !auth.NeedsRehash(adm.PwHash) {
		return s.openSession(ctx, adm, remote, ua, "")
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return "", auth.Principal{}, s.fail("hash password", err)
	}
	// The kept session is created for the generation the CAS will produce.
	sess, tok, err := s.newSession(adm.ID, adm.Generation+1, remote, ua)
	if err != nil {
		return "", auth.Principal{}, s.fail("new session", err)
	}
	ev := s.guiEvent(adm.Username, remote, ActionGUILogin)
	ev.Message = "password hash upgraded"
	if _, err := s.st.SetGUIPassword(ctx, adm.ID, adm.Generation, hash, sess, s.now(), ev); err != nil {
		if !errors.Is(err, store.ErrConflict) {
			return "", auth.Principal{}, s.fail("rehash gui password", err)
		}
		// Another login (or a password change) got there first. The password was verified
		// against the old hash only, so verify it again against what is stored now, then open
		// a plain session; the rehash is not retried.
		cur, gerr := s.st.GetGUIAdmin(ctx)
		if gerr != nil {
			if errors.Is(gerr, store.ErrNotFound) {
				return "", auth.Principal{}, errBadLogin
			}
			return "", auth.Principal{}, s.fail("reload gui admin", gerr)
		}
		if subtle.ConstantTimeCompare([]byte(username), []byte(cur.Username)) != 1 || !auth.VerifyPassword(password, cur.PwHash) {
			return "", auth.Principal{}, errBadLogin
		}
		return s.openSession(ctx, cur, remote, ua, "")
	}
	return tok, auth.GUIAdmin(adm.Username, remote), nil
}

// GUIAuthenticate resolves a session token on every request. It looks the hash up, then judges
// the row itself on the service clock: the admin's generation must equal the session's, the
// absolute expiry and the 30 minute idle limit must not have passed. A dead row is deleted
// (unaudited housekeeping) and the same error as an unknown token is returned. Unless passive,
// last-active is touched at most once per GUITouchEvery. Returns the admin principal and the
// session id; no hash or secret.
func (s *Service) GUIAuthenticate(ctx context.Context, remote, sessionToken string, passive bool) (auth.Principal, int64, error) {
	if sessionToken == "" || len(sessionToken) > maxCredentialBytes {
		return auth.Principal{}, 0, errNoSession
	}
	sess, adm, err := s.st.GetGUISession(ctx, tokenHash(sessionToken))
	if errors.Is(err, store.ErrNotFound) {
		return auth.Principal{}, 0, errNoSession
	}
	if err != nil {
		return auth.Principal{}, 0, s.fail("get gui session", err)
	}
	now := s.now()
	if sess.Generation != adm.Generation || !now.Before(sess.ExpiresAt) || !now.Before(sess.LastActiveAt.Add(GUIIdleTimeout)) {
		if err := s.st.DeleteGUISession(ctx, adm.ID, sess.ID, now, store.AuditEvent{}); err != nil && !errors.Is(err, store.ErrNotFound) {
			s.logf("service: delete dead gui session: %v", err)
		}
		return auth.Principal{}, 0, errNoSession
	}
	if !passive && now.Sub(sess.LastActiveAt) >= GUITouchEvery {
		if err := s.st.TouchGUISession(ctx, sess.ID, now); err != nil && !errors.Is(err, store.ErrNotFound) {
			s.logf("service: touch gui session: %v", err)
		}
	}
	return auth.GUIAdmin(adm.Username, remote), sess.ID, nil
}

// guiAdmin authorises an admin-only GUI action and loads the active admin; p must be that
// admin (a local-socket admin has no GUI account).
func (s *Service) guiAdmin(ctx context.Context, p auth.Principal, src Source, action string) (store.GUIAdmin, error) {
	if err := s.authorize(ctx, p, src, action, true); err != nil {
		return store.GUIAdmin{}, err
	}
	adm, err := s.st.GetGUIAdmin(ctx)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.GUIAdmin{}, s.deny(ctx, p, src, action)
		}
		return store.GUIAdmin{}, s.fail("load gui admin", err)
	}
	if subtle.ConstantTimeCompare([]byte(p.Name), []byte(adm.Username)) != 1 {
		return store.GUIAdmin{}, s.deny(ctx, p, src, action)
	}
	return adm, nil
}

// ChangeGUIPassword checks the current password, stores the new one, bumps the credential
// generation, deletes every other session and re-issues the current one under a new token (it
// keeps its remote, user agent and absolute expiry). Returns the new raw session token.
func (s *Service) ChangeGUIPassword(ctx context.Context, p auth.Principal, sessionID int64, current, next string) (string, error) {
	adm, err := s.guiAdmin(ctx, p, SourceGUI, ActionGUIPasswordChange)
	if err != nil {
		return "", err
	}
	if len(current) > auth.MaxPasswordBytes || !auth.VerifyPassword(current, adm.PwHash) {
		return "", ErrWrongCurrentPassword
	}
	if err := auth.ValidatePassword(next, adm.Username); err != nil {
		return "", badRequest("%s", err.Error())
	}
	var old *store.GUISession
	list, err := s.st.ListGUISessions(ctx, adm.ID)
	if err != nil {
		return "", s.fail("list gui sessions", err)
	}
	for i := range list {
		if list[i].ID == sessionID {
			old = &list[i]
		}
	}
	if old == nil {
		return "", api.Errorf(api.CodeNotFound, "not found")
	}
	hash, err := auth.HashPassword(next)
	if err != nil {
		return "", s.fail("hash password", err)
	}
	tok, err := newToken("")
	if err != nil {
		return "", s.fail("new session token", err)
	}
	now := s.now()
	keep := store.GUISession{TokenHash: tokenHash(tok), AdminID: adm.ID, Generation: adm.Generation + 1,
		CreatedAt: old.CreatedAt, LastActiveAt: now, ExpiresAt: old.ExpiresAt, Remote: old.Remote, UserAgent: old.UserAgent}
	ev := s.event(p, SourceGUI, ActionGUIPasswordChange)
	ev.Subject = adm.Username
	if _, err := s.st.SetGUIPassword(ctx, adm.ID, adm.Generation, hash, keep, now, ev); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return "", api.Errorf(api.CodeConflict, "the account changed while you were editing it; try again")
		}
		return "", s.fail("set gui password", err)
	}
	return tok, nil
}

// ResetGUIAdmin is the recovery path (`central gui reset-admin`): the active admin becomes
// "reset", every session is deleted, and a fresh setup token is created and returned once. Admin
// only. It is two transactions (reset, then the new setup row); if the second fails there is no
// admin and no setup token, which fails closed: run it again, or restart the GUI, which creates
// the token (G-08).
func (s *Service) ResetGUIAdmin(ctx context.Context, p auth.Principal, src Source) (string, error) {
	if err := s.authorize(ctx, p, src, api.MethodGUIResetAdmin, true); err != nil {
		return "", err
	}
	// Recovery is a local-socket (CLI) operation: never from the GUI or the API, whatever the principal.
	if src != SourceCLI {
		return "", s.deny(ctx, p, src, api.MethodGUIResetAdmin)
	}
	if err := s.st.ResetGUIAdmin(ctx, s.now(), s.event(p, src, api.MethodGUIResetAdmin)); err != nil {
		return "", s.fail("reset gui admin", err)
	}
	tok, err := newToken(SetupTokenPrefix)
	if err != nil {
		return "", s.fail("new setup token", err)
	}
	now := s.now()
	ev := s.event(p, src, ActionGUISetupToken)
	ev.Params = map[string]string{"expires": now.Add(GUISetupTTL).UTC().Format(time.RFC3339)}
	if err := s.st.PutGUISetup(ctx, tokenHash(tok), now.Add(GUISetupTTL), now, ev); err != nil {
		return "", s.fail("put gui setup", err)
	}
	return tok, nil
}

// GUILogout ends the caller's own session (audited). A session that is already gone is fine.
func (s *Service) GUILogout(ctx context.Context, p auth.Principal, sessionID int64) error {
	adm, err := s.guiAdmin(ctx, p, SourceGUI, ActionGUILogout)
	if err != nil {
		return err
	}
	err = s.st.DeleteGUISession(ctx, adm.ID, sessionID, s.now(), s.event(p, SourceGUI, ActionGUILogout))
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return s.fail("delete gui session", err)
	}
	return nil
}

// ListGUISessions lists the admin's sessions (no token hashes), oldest first.
func (s *Service) ListGUISessions(ctx context.Context, p auth.Principal) ([]store.GUISession, error) {
	adm, err := s.guiAdmin(ctx, p, SourceGUI, ActionGUISessionRevoke)
	if err != nil {
		return nil, err
	}
	out, err := s.st.ListGUISessions(ctx, adm.ID)
	if err != nil {
		return nil, s.fail("list gui sessions", err)
	}
	return out, nil
}

// RevokeGUISession signs out one session of the admin ("sign out this session").
func (s *Service) RevokeGUISession(ctx context.Context, p auth.Principal, id int64) error {
	adm, err := s.guiAdmin(ctx, p, SourceGUI, ActionGUISessionRevoke)
	if err != nil {
		return err
	}
	ev := s.event(p, SourceGUI, ActionGUISessionRevoke)
	ev.Params = map[string]string{"session": strconv.FormatInt(id, 10)}
	if err := s.st.DeleteGUISession(ctx, adm.ID, id, s.now(), ev); err != nil {
		return s.fail("revoke gui session", err)
	}
	return nil
}

// RevokeOtherGUISessions signs out every session except keepID and returns how many.
func (s *Service) RevokeOtherGUISessions(ctx context.Context, p auth.Principal, keepID int64) (int, error) {
	adm, err := s.guiAdmin(ctx, p, SourceGUI, ActionGUISessionRevoke)
	if err != nil {
		return 0, err
	}
	ev := s.event(p, SourceGUI, ActionGUISessionRevoke)
	ev.Params = map[string]string{"keep": strconv.FormatInt(keepID, 10), "scope": "others"}
	n, err := s.st.DeleteOtherGUISessions(ctx, adm.ID, keepID, s.now(), ev)
	if err != nil {
		return 0, s.fail("revoke other gui sessions", err)
	}
	return n, nil
}

// PurgeGUISessions deletes expired, idle and dead session rows (housekeeping, not audited).
// Called by the GUI at start and every 10 minutes.
func (s *Service) PurgeGUISessions(ctx context.Context) (int, error) {
	n, err := s.st.PurgeGUISessions(ctx, GUIIdleTimeout, s.now())
	if err != nil {
		return 0, s.fail("purge gui sessions", err)
	}
	return n, nil
}

// ActionGUILockout is the audit action written when the login limiter locks an address out.
const ActionGUILockout = "gui.lockout"

// GUISetupOpen reports whether no active GUI admin exists, i.e. whether /setup may be offered.
// It reveals nothing else and writes nothing.
func (s *Service) GUISetupOpen(ctx context.Context) (bool, error) {
	_, err := s.st.GetGUIAdmin(ctx)
	switch {
	case err == nil:
		return false, nil
	case errors.Is(err, store.ErrNotFound):
		return true, nil
	}
	return false, s.fail("load gui admin", err)
}

// RecordGUILockout writes one gui.lockout audit row for a client address the GUI login limiter
// has just locked out (docs/M4.md 2.5). The caller calls it once per lockout window.
func (s *Service) RecordGUILockout(ctx context.Context, remote string) error {
	ev := s.systemEvent(ActionGUILockout)
	ev.Source = string(SourceGUI)
	ev.Remote = remote
	ev.Outcome = OutcomeDenied
	if _, err := s.st.RecordAudit(ctx, ev); err != nil {
		return s.fail("record gui lockout", err)
	}
	return nil
}
