package service

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/tokens"
)

// Joiner-facing errors. Every non-conflict failure of Join (malformed, unknown id, wrong secret,
// expired, revoked, used up, wrong host-key pin) returns errJoinRefused, byte-identical, so a
// probe learns nothing about which token ids exist. Likewise errRegisterRefused for Register.
var (
	errJoinRefused     = api.Errorf(api.CodeUnauthenticated, "join refused")
	errRegisterRefused = api.Errorf(api.CodeUnauthenticated, "registration refused")
	errEnrollConflict  = api.Errorf(api.CodeConflict, "conflict")
)

func hostHashEqual(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// requireEnroll is the direct kind check for enrolment methods (independent of the policy).
func (s *Service) requireEnroll(ctx context.Context, p auth.Principal, src Source, method string) error {
	if err := s.authorize(ctx, p, src, method, false); err != nil {
		return err
	}
	if p.Kind != auth.KindEnroll {
		ev := s.event(p, src, method)
		ev.Outcome = OutcomeDenied
		if _, err := s.st.RecordAudit(ctx, ev); err != nil {
			s.logf("service: audit denial of %s failed: %v", method, err)
		}
		return errForbidden
	}
	return nil
}

// JoinResult is what a joining runner learns.
type JoinResult = api.EnrollJoinResult

// Join enrols a runner (p.Kind must be enroll). The token's host-key pin must equal central's;
// the key must not belong to an operator (checked atomically in store.Enroll); labels come from the token only. Failures other than
// a name/key conflict return one identical error and the real reason is logged server-side only.
// A successful join is audited (source enroll); failed joins are logged, not audited.
func (s *Service) Join(ctx context.Context, p auth.Principal, src Source, token, name string, key ssh.PublicKey) (JoinResult, error) {
	if err := s.requireEnroll(ctx, p, src, api.MethodEnrollJoin); err != nil {
		return JoinResult{}, err
	}
	if key == nil || key.Type() != ssh.KeyAlgoED25519 {
		return JoinResult{}, badRequest("ssh-ed25519 key required")
	}
	if !store.ValidRunnerName(name) {
		return JoinResult{}, badRequest("invalid runner name")
	}
	id, secret, pin, err := tokens.ParseJoinToken(token)
	if err != nil {
		s.logf("service: join from %s refused: malformed token", p.Remote)
		return JoinResult{}, errJoinRefused
	}
	if !hostHashEqual(pin, tokens.HostKeyHash(s.hostKey)) {
		s.logf("service: join from %s refused: token %s pins another host key", p.Remote, id)
		return JoinResult{}, errJoinRefused
	}
	// Runner/operator key disjointness is enforced by store.Enroll, after the token is verified.
	wire := key.Marshal()
	now := s.now()
	h := sha256.Sum256(secret)
	fp := ssh.FingerprintSHA256(key)
	ev := s.event(p, src, api.MethodEnrollJoin)
	ev.Runner, ev.Subject = name, id
	ev.Params = map[string]string{"fingerprint": fp}
	// store.Enroll fills ev.Fleet from the token's fleet inside its transaction.
	r, err := s.st.Enroll(ctx, id, h[:], now, store.Runner{Name: name, PubKey: wire, Fingerprint: fp}, ev)
	switch {
	case err == nil:
		return JoinResult{Fleet: r.Fleet, Runner: r.Name, Status: string(r.Status)}, nil
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrStale):
		s.logf("service: join from %s refused: token %s: %v", p.Remote, id, err)
		return JoinResult{}, errJoinRefused
	case errors.Is(err, store.ErrConflict):
		s.logf("service: join from %s conflict: token %s name %q", p.Remote, id, name)
		return JoinResult{}, errEnrollConflict
	}
	return JoinResult{}, s.fail("enroll", err)
}

// RegisterResult is what a registering operator learns.
type RegisterResult = api.EnrollRegisterResult

// Register registers an operator key with the current registration code (p.Kind must be enroll).
// The code must be for the current 60 s step only and pin central's host key; any number of
// operators may use the same code in its window. Bad/expired/mismatched codes (and a missing
// registration secret) all return one identical error, real reason logged only. A key that
// belongs to a runner, to a revoked operator, or an existing operator with another name is a
// conflict. Idempotent re-registration succeeds and is still audited.
func (s *Service) Register(ctx context.Context, p auth.Principal, src Source, codeStr, name string, key ssh.PublicKey) (RegisterResult, error) {
	if err := s.requireEnroll(ctx, p, src, api.MethodEnrollRegister); err != nil {
		return RegisterResult{}, err
	}
	if key == nil || key.Type() != ssh.KeyAlgoED25519 {
		return RegisterResult{}, badRequest("ssh-ed25519 key required")
	}
	if !store.ValidOperatorName(name) {
		return RegisterResult{}, badRequest("invalid operator name")
	}
	body, pin, err := tokens.ParseCode(codeStr)
	if err != nil {
		s.logf("service: register from %s refused: malformed code", p.Remote)
		return RegisterResult{}, errRegisterRefused
	}
	if !hostHashEqual(pin, tokens.HostKeyHash(s.hostKey)) {
		s.logf("service: register from %s refused: code pins another host key", p.Remote)
		return RegisterResult{}, errRegisterRefused
	}
	secret, err := s.st.GetSetting(ctx, store.SettingRegistrationSecret)
	if err != nil {
		s.logf("service: register from %s refused: registration secret unavailable: %v", p.Remote, err)
		return RegisterResult{}, errRegisterRefused
	}
	now := s.now()
	if !tokens.VerifyCode(secret, now, body) {
		s.logf("service: register from %s refused: code not valid for the current step", p.Remote)
		return RegisterResult{}, errRegisterRefused
	}

	wire := key.Marshal()
	fp := ssh.FingerprintSHA256(key)
	ev := s.event(p, src, api.MethodEnrollRegister)
	ev.Subject = name
	ev.Params = map[string]string{"fingerprint": fp}
	op, created, err := s.st.RegisterOperator(ctx, name, wire, fp, now, ev)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			s.logf("service: register from %s conflict for %q", p.Remote, name)
			return RegisterResult{}, errEnrollConflict
		}
		return RegisterResult{}, s.fail("register operator", err)
	}
	if !created {
		if op.Name != name {
			s.logf("service: register from %s: key already registered under another name", p.Remote)
			return RegisterResult{}, errEnrollConflict
		}
		ev.Message = "already registered"
		if _, err := s.st.RecordAudit(ctx, ev); err != nil {
			return RegisterResult{}, s.fail("audit re-register", err)
		}
	}
	return RegisterResult{Operator: op.Name, Status: string(op.Status)}, nil
}
