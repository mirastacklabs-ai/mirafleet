package sshagent

import (
	"bytes"
	"encoding/binary"
	"errors"
)

// Wire constants of RFC 4252 section 7 (public key authentication).
const (
	msgUserAuthRequest = 50
	serviceConnection  = "ssh-connection"
	methodPublicKey    = "publickey"
)

// UserAuthRequest is the data an SSH client asks an agent to sign during public key
// authentication: the session id, then the SSH_MSG_USERAUTH_REQUEST it is about to send.
type UserAuthRequest struct {
	SessionID []byte
	User      string
	Algorithm string // public key algorithm name, e.g. ssh-ed25519 or rsa-sha2-512
	PublicKey []byte // wire-format public key blob
}

var errNotUserAuth = errors.New("sshagent: data is not an SSH user-authentication request")

func readString(b []byte) (s, rest []byte, ok bool) {
	if len(b) < 4 {
		return nil, nil, false
	}
	n := binary.BigEndian.Uint32(b)
	if uint64(n) > uint64(len(b)-4) {
		return nil, nil, false
	}
	return b[4 : 4+n], b[4+n:], true
}

func appendString(dst, s []byte) []byte {
	dst = binary.BigEndian.AppendUint32(dst, uint32(len(s)))
	return append(dst, s...)
}

// ParseUserAuthRequest accepts only the exact, canonical encoding of a publickey
// user-authentication request for the ssh-connection service, and nothing else. Anything
// else an agent might be asked to sign (git commit signatures, SSHSIG blobs, host-based or
// keyboard-interactive auth, arbitrary bytes) is refused.
func ParseUserAuthRequest(data []byte) (*UserAuthRequest, error) {
	sid, rest, ok := readString(data)
	if !ok || len(sid) == 0 || len(sid) > 64 {
		return nil, errNotUserAuth
	}
	if len(rest) < 1 || rest[0] != msgUserAuthRequest {
		return nil, errNotUserAuth
	}
	rest = rest[1:]
	user, rest, ok := readString(rest)
	if !ok || len(user) == 0 {
		return nil, errNotUserAuth
	}
	service, rest, ok := readString(rest)
	if !ok || string(service) != serviceConnection {
		return nil, errNotUserAuth
	}
	method, rest, ok := readString(rest)
	if !ok || string(method) != methodPublicKey {
		return nil, errNotUserAuth
	}
	if len(rest) < 1 || rest[0] != 1 { // "has signature" must be TRUE
		return nil, errNotUserAuth
	}
	rest = rest[1:]
	algo, rest, ok := readString(rest)
	if !ok || len(algo) == 0 {
		return nil, errNotUserAuth
	}
	blob, rest, ok := readString(rest)
	if !ok || len(blob) == 0 || len(rest) != 0 {
		return nil, errNotUserAuth
	}
	return &UserAuthRequest{
		SessionID: bytes.Clone(sid),
		User:      string(user),
		Algorithm: string(algo),
		PublicKey: bytes.Clone(blob),
	}, nil
}

// Marshal is the inverse of ParseUserAuthRequest.
func (r *UserAuthRequest) Marshal() []byte {
	b := appendString(nil, r.SessionID)
	b = append(b, msgUserAuthRequest)
	b = appendString(b, []byte(r.User))
	b = appendString(b, []byte(serviceConnection))
	b = appendString(b, []byte(methodPublicKey))
	b = append(b, 1)
	b = appendString(b, []byte(r.Algorithm))
	return appendString(b, r.PublicKey)
}
