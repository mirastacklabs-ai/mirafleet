package apiclient

import (
	"context"
	"errors"
	"net"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/tokens"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// Join enrols a runner with a join token (user "enroll", channel join@mirafleet). central's host
// key is pinned from the token's hash; there is never a trust-on-first-use fallback. It returns
// the verified host key so the caller can write known_hosts.
func Join(ctx context.Context, addr, token, name string, labels map[string]string, signer ssh.Signer) (*api.EnrollJoinResult, ssh.PublicKey, error) {
	_, _, hash, err := tokens.ParseJoinToken(token)
	if err != nil {
		return nil, nil, errors.New("invalid join token")
	}
	var res api.EnrollJoinResult
	hk, err := enroll(ctx, addr, hash, signer, transport.ChanJoin, api.MethodEnrollJoin,
		api.EnrollJoinParams{Token: token, Name: name, Labels: labels}, &res)
	if err != nil {
		return nil, nil, err
	}
	return &res, hk, nil
}

// Register registers an operator key with a registration code (user "enroll", channel
// register@mirafleet). central's host key is pinned from the code's hash. It returns the
// verified host key so the caller can write known_hosts.
func Register(ctx context.Context, addr, code, name string, signer ssh.Signer) (*api.EnrollRegisterResult, ssh.PublicKey, error) {
	_, hash, err := tokens.ParseCode(code)
	if err != nil {
		return nil, nil, errors.New("invalid registration code")
	}
	var res api.EnrollRegisterResult
	hk, err := enroll(ctx, addr, hash, signer, transport.ChanRegister, api.MethodEnrollRegister,
		api.EnrollRegisterParams{Code: code, Name: name}, &res)
	if err != nil {
		return nil, nil, err
	}
	return &res, hk, nil
}

func enroll(ctx context.Context, addr, hash string, signer ssh.Signer, chanType, method string, params, result any) (ssh.PublicKey, error) {
	var seen ssh.PublicKey
	pin := PinnedHash(hash)
	cb := func(h string, r net.Addr, k ssh.PublicKey) error {
		if err := pin(h, r, k); err != nil {
			return err
		}
		seen = k
		return nil
	}
	cl, err := Dial(ctx, addr, transport.UserEnroll, signer, cb)
	if err != nil {
		return nil, err
	}
	defer cl.Close()
	if err := callOn(ctx, cl.c, chanType, method, params, result); err != nil {
		return nil, err
	}
	return seen, nil
}
