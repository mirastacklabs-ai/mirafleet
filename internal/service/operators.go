package service

import (
	"context"
	"crypto/rand"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/tokens"
)

// OperatorConns lets the service drop an operator's live SSH connections (implemented by the
// central api handler, S7). In-flight execs started by that operator die with the connection
// because their request ctx is derived from it.
type OperatorConns interface {
	KickOperator(id int64, reason string)
}

// OperatorRevokeReason is the kick reason for a revoked operator.
const OperatorRevokeReason = "operator revoked"

const actionInitSecret = "operator.init_secret"

func newSecret() ([]byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}

// InitRegistrationSecret creates the registration secret (used by `central init`). Admin only.
// store.ErrConflict (mapped to conflict) if one exists. The secret is never audited or logged.
func (s *Service) InitRegistrationSecret(ctx context.Context, p auth.Principal, src Source) error {
	if err := s.authorize(ctx, p, src, actionInitSecret, true); err != nil {
		return err
	}
	sec, err := newSecret()
	if err != nil {
		return s.fail("new secret", err)
	}
	now := s.now()
	if err := s.st.CreateSetting(ctx, store.SettingRegistrationSecret, sec, now, s.event(p, src, actionInitSecret)); err != nil {
		return s.fail("create registration secret", err)
	}
	return nil
}

// RotateRegistrationSecret replaces the secret, invalidating every outstanding code. Admin
// only; audited.
func (s *Service) RotateRegistrationSecret(ctx context.Context, p auth.Principal, src Source) error {
	if err := s.authorize(ctx, p, src, api.MethodOperatorRotateSecret, true); err != nil {
		return err
	}
	sec, err := newSecret()
	if err != nil {
		return s.fail("new secret", err)
	}
	now := s.now()
	if err := s.st.PutSetting(ctx, store.SettingRegistrationSecret, sec, now, s.event(p, src, api.MethodOperatorRotateSecret)); err != nil {
		return s.fail("rotate registration secret", err)
	}
	return nil
}

// OperatorCode returns the current registration code, the seconds left in its step and central's
// host-key fingerprint. Admin only. Showing it is audited (who/when); the code is not. If the
// audit write fails the code is not returned.
func (s *Service) OperatorCode(ctx context.Context, p auth.Principal, src Source) (api.OperatorCodeResult, error) {
	if err := s.authorize(ctx, p, src, api.MethodOperatorCode, true); err != nil {
		return api.OperatorCodeResult{}, err
	}
	secret, err := s.st.GetSetting(ctx, store.SettingRegistrationSecret)
	if err != nil {
		return api.OperatorCodeResult{}, s.fail("load registration secret", err)
	}
	now := s.now()
	c, err := tokens.Code(secret, tokens.Step(now), s.hostKey)
	if err != nil {
		return api.OperatorCodeResult{}, s.fail("make code", err)
	}
	if _, err := s.st.RecordAudit(ctx, s.event(p, src, api.MethodOperatorCode)); err != nil {
		return api.OperatorCodeResult{}, s.fail("audit code view", err)
	}
	return api.OperatorCodeResult{Code: c, SecondsLeft: tokens.SecondsLeft(now), HostFingerprint: ssh.FingerprintSHA256(s.hostKey)}, nil
}

// ListOperators lists operators by id. Admin only.
func (s *Service) ListOperators(ctx context.Context, p auth.Principal, src Source, includeRevoked bool) ([]store.Operator, error) {
	if err := s.authorize(ctx, p, src, api.MethodOperatorList, true); err != nil {
		return nil, err
	}
	ops, err := s.st.ListOperators(ctx, includeRevoked)
	if err != nil {
		return nil, s.fail("list operators", err)
	}
	return ops, nil
}

// RevokeOperator revokes the named operator (commit + audit), then kicks their live connections
// through OperatorConns. Admin only.
func (s *Service) RevokeOperator(ctx context.Context, p auth.Principal, src Source, name string) (store.Operator, error) {
	if err := s.authorize(ctx, p, src, api.MethodOperatorRevoke, true); err != nil {
		return store.Operator{}, err
	}
	if !store.ValidOperatorName(name) {
		return store.Operator{}, badRequest("invalid operator name")
	}
	ev := s.event(p, src, api.MethodOperatorRevoke)
	ev.Subject = name
	ev.Message = OperatorRevokeReason
	op, err := s.st.RevokeOperator(ctx, name, s.now(), ev)
	if err != nil {
		return store.Operator{}, s.fail("revoke operator", err)
	}
	s.opConns.KickOperator(op.ID, OperatorRevokeReason)
	return op, nil
}
