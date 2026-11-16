package service

import (
	"context"
	"strconv"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/tokens"
)

// Join token limits: never unlimited.
const (
	DefaultTokenUses = 1
	MaxTokenUses     = 1000
	DefaultTokenTTL  = 24 * time.Hour
	MaxTokenTTL      = 30 * 24 * time.Hour

	maxLabels     = 32
	maxLabelKey   = 64
	maxLabelValue = 256
)

// TokenParams are the inputs of CreateToken. Zero Uses means single use; zero TTL means 24h.
// Values over the caps (or negative) are rejected as bad_request.
type TokenParams struct {
	Fleet   string
	Labels  map[string]string
	Approve bool
	Uses    int
	TTL     time.Duration
}

// CreatedToken is returned once by CreateToken. Token is the only copy of the secret.
type CreatedToken struct {
	ID      string
	Token   string
	Expires time.Time
	MaxUses int
}

func validLabels(l map[string]string) bool {
	if len(l) > maxLabels {
		return false
	}
	for k, v := range l {
		if k == "" || len(k) > maxLabelKey || len(v) > maxLabelValue {
			return false
		}
	}
	return true
}

// CreateToken mints a join token for a fleet. Admin only. The full token is in the result and
// nowhere else: it is not stored, audited or logged (the audit subject is the token id).
func (s *Service) CreateToken(ctx context.Context, p auth.Principal, src Source, tp TokenParams) (CreatedToken, error) {
	if err := s.authorize(ctx, p, src, api.MethodTokenCreate, true); err != nil {
		return CreatedToken{}, err
	}
	if !store.ValidFleetName(tp.Fleet) {
		return CreatedToken{}, badRequest("invalid fleet name")
	}
	uses, ttl := tp.Uses, tp.TTL
	if uses == 0 {
		uses = DefaultTokenUses
	}
	if ttl == 0 {
		ttl = DefaultTokenTTL
	}
	if uses < 1 || uses > MaxTokenUses {
		return CreatedToken{}, badRequest("uses must be 1-%d", MaxTokenUses)
	}
	if ttl < 0 || ttl > MaxTokenTTL {
		return CreatedToken{}, badRequest("ttl must be at most %s", MaxTokenTTL)
	}
	if !validLabels(tp.Labels) {
		return CreatedToken{}, badRequest("invalid labels")
	}
	tok, id, hash, err := tokens.NewJoinToken(s.hostKey)
	if err != nil {
		return CreatedToken{}, s.fail("new join token", err)
	}
	now := s.now()
	exp := now.Add(ttl)
	ev := s.event(p, src, api.MethodTokenCreate)
	ev.Fleet = tp.Fleet
	ev.Subject = id
	ev.Params = map[string]string{
		"uses":    strconv.Itoa(uses),
		"ttl_s":   strconv.FormatInt(int64(ttl/time.Second), 10),
		"approve": strconv.FormatBool(tp.Approve),
	}
	_, err = s.st.CreateToken(ctx, store.JoinToken{
		TokenID: id, SecretHash: hash, Fleet: tp.Fleet, Labels: tp.Labels,
		AutoApprove: tp.Approve, MaxUses: uses, ExpiresAt: exp, CreatedBy: p.Name,
	}, now, ev)
	if err != nil {
		return CreatedToken{}, s.fail("create token", err)
	}
	return CreatedToken{ID: id, Token: tok, Expires: exp, MaxUses: uses}, nil
}

func validTokenID(id string) bool {
	if len(id) != 16 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// RevokeToken revokes a join token by id. Admin only. Revoking an already revoked token
// succeeds without a new audit row.
func (s *Service) RevokeToken(ctx context.Context, p auth.Principal, src Source, tokenID string) error {
	if err := s.authorize(ctx, p, src, api.MethodTokenRevoke, true); err != nil {
		return err
	}
	if !validTokenID(tokenID) {
		return badRequest("invalid token id")
	}
	now := s.now()
	ev := s.event(p, src, api.MethodTokenRevoke)
	ev.Subject = tokenID
	if err := s.st.RevokeToken(ctx, tokenID, now, ev); err != nil {
		return s.fail("revoke token", err)
	}
	return nil
}

// ListTokens lists join tokens (never their secrets); fleet "" means all fleets. Admin only.
func (s *Service) ListTokens(ctx context.Context, p auth.Principal, src Source, fleet string) ([]store.JoinToken, error) {
	if err := s.authorize(ctx, p, src, api.MethodTokenList, true); err != nil {
		return nil, err
	}
	if fleet != "" && !store.ValidFleetName(fleet) {
		return nil, badRequest("invalid fleet name")
	}
	ts, err := s.st.ListTokens(ctx, fleet)
	if err != nil {
		return nil, s.fail("list tokens", err)
	}
	return ts, nil
}
