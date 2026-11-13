// Package auth authenticates SSH peers and carries their identity to handlers.
//
// Identity travels only in ssh.Permissions.Extensions, never in closure state shared across
// connections (CVE-2024-45337 class of bugs: the callback may be invoked for several keys).
package auth

import (
	"context"
	"errors"
	"strconv"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/store"
)

// Kind is the role of an authenticated principal.
type Kind string

const (
	KindRunner   Kind = "runner"
	KindOperator Kind = "operator"
	KindEnroll   Kind = "enroll"
	KindAdmin    Kind = "admin" // local admin socket only; never from SSH
)

// Principal is an authenticated identity.
type Principal struct {
	Kind        Kind
	ID          int64 // runner or operator id; 0 for enroll and admin
	Name        string
	Fingerprint string
	Remote      string
	// Status is the runner/operator status at authentication time (store.Status or
	// store.OperatorStatus text). It is informational: callers reload it before deciding.
	Status string
}

// ErrDenied is returned for every authentication failure (no detail leaks to the peer).
var ErrDenied = errors.New("auth: access denied")

// AuthProvider authenticates a public-key attempt. Enterprise builds can swap it.
type AuthProvider interface {
	Authenticate(meta ssh.ConnMetadata, key ssh.PublicKey) (Principal, error)
}

// KeyLookup is the subset of store.Store that KeyAuth needs.
type KeyLookup interface {
	GetRunnerByKey(ctx context.Context, pubKey []byte) (store.Runner, error)
	GetOperatorByKey(ctx context.Context, pubKey []byte) (store.Operator, error)
}

// KeyAuth is the OSS provider: keys registered in the store, SSH user binds the role.
type KeyAuth struct {
	Store KeyLookup
	// Logf, if set, receives store errors other than not-found (server-side only; the peer
	// always sees ErrDenied).
	Logf func(format string, args ...any)
}

// lookupTimeout bounds one store lookup during the SSH handshake.
const lookupTimeout = 5 * time.Second

func (a *KeyAuth) storeErr(kind string, err error) {
	if a.Logf != nil && !errors.Is(err, store.ErrNotFound) {
		a.Logf("auth: %s key lookup failed: %v", kind, err)
	}
}

// NewKeyAuth returns a KeyAuth over s.
func NewKeyAuth(s KeyLookup) *KeyAuth { return &KeyAuth{Store: s} }

// Authenticate implements AuthProvider.
func (a *KeyAuth) Authenticate(meta ssh.ConnMetadata, key ssh.PublicKey) (Principal, error) {
	if key == nil || key.Type() != ssh.KeyAlgoED25519 {
		return Principal{}, ErrDenied
	}
	remote := ""
	if ra := meta.RemoteAddr(); ra != nil {
		remote = ra.String()
	}
	fp := ssh.FingerprintSHA256(key)
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	switch meta.User() {
	case string(KindRunner):
		r, err := a.Store.GetRunnerByKey(ctx, key.Marshal())
		if err != nil {
			a.storeErr("runner", err)
			return Principal{}, ErrDenied
		}
		if r.Status == store.StatusRevoked {
			return Principal{}, ErrDenied
		}
		return Principal{Kind: KindRunner, ID: r.ID, Name: r.Name, Fingerprint: fp, Remote: remote, Status: string(r.Status)}, nil
	case string(KindOperator):
		o, err := a.Store.GetOperatorByKey(ctx, key.Marshal())
		if err != nil {
			a.storeErr("operator", err)
			return Principal{}, ErrDenied
		}
		if o.Status == store.OperatorRevoked {
			return Principal{}, ErrDenied
		}
		return Principal{Kind: KindOperator, ID: o.ID, Name: o.Name, Fingerprint: fp, Remote: remote, Status: string(o.Status)}, nil
	case string(KindEnroll):
		return Principal{Kind: KindEnroll, Fingerprint: fp, Remote: remote}, nil
	}
	return Principal{}, ErrDenied
}

// LocalAdmin is the principal for the local admin socket, identified by peer UID.
func LocalAdmin(uid int) Principal {
	return Principal{Kind: KindAdmin, Name: "uid:" + strconv.Itoa(uid), Remote: "local"}
}

const (
	extKind   = "mirafleet-kind"
	extID     = "mirafleet-id"
	extName   = "mirafleet-name"
	extFP     = "mirafleet-fp"
	extRemote = "mirafleet-remote"
	extStatus = "mirafleet-status"
)

// Encode puts p into ssh.Permissions.Extensions.
func Encode(p Principal) *ssh.Permissions {
	return &ssh.Permissions{Extensions: map[string]string{
		extKind:   string(p.Kind),
		extID:     strconv.FormatInt(p.ID, 10),
		extName:   p.Name,
		extFP:     p.Fingerprint,
		extRemote: p.Remote,
		extStatus: p.Status,
	}}
}

// Decode recovers a Principal; it fails closed on missing, unknown or garbled values. The
// admin kind is never accepted from SSH permissions.
func Decode(perm *ssh.Permissions) (Principal, error) {
	if perm == nil || perm.Extensions == nil {
		return Principal{}, ErrDenied
	}
	e := perm.Extensions
	for _, k := range []string{extKind, extID, extName, extFP, extRemote, extStatus} {
		if _, ok := e[k]; !ok {
			return Principal{}, ErrDenied
		}
	}
	k := Kind(e[extKind])
	id, err := strconv.ParseInt(e[extID], 10, 64)
	if err != nil || id < 0 || strconv.FormatInt(id, 10) != e[extID] || e[extFP] == "" {
		return Principal{}, ErrDenied
	}
	switch k {
	case KindRunner, KindOperator:
		if id == 0 || e[extName] == "" {
			return Principal{}, ErrDenied
		}
	case KindEnroll:
		if id != 0 {
			return Principal{}, ErrDenied
		}
	default:
		return Principal{}, ErrDenied
	}
	return Principal{Kind: k, ID: id, Name: e[extName], Fingerprint: e[extFP], Remote: e[extRemote], Status: e[extStatus]}, nil
}

// PublicKeyCallback adapts ap to ssh.ServerConfig.PublicKeyCallback. Errors are opaque.
func PublicKeyCallback(ap AuthProvider) func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
	return func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		p, err := ap.Authenticate(meta, key)
		if err != nil {
			return nil, ErrDenied
		}
		if p.Kind == KindAdmin {
			return nil, ErrDenied
		}
		return Encode(p), nil
	}
}
