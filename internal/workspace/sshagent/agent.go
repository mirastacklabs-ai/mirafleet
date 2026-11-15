// Package sshagent is a restricted ssh-agent for one workspace session. It lists only the
// keys the session was assigned and signs only SSH public key user-authentication requests
// for the credential's SSH user. With host binding on (the default) it also requires the
// client to prove, via OpenSSH's session-bind@openssh.com extension, which server host key
// the session is talking to, and refuses unless that key is pinned in the credential's
// known_hosts. Add, remove, lock and unlock are refused. See docs/M7.md section 4.3.
package sshagent

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

const sessionBindExt = "session-bind@openssh.com"

var errRefused = errors.New("sshagent: operation refused")

// Identity is one key the session may use.
type Identity struct {
	Signer      ssh.Signer
	Comment     string
	User        string          // the only SSH user this key may authenticate as
	HostKeys    []ssh.PublicKey // pinned server host keys (from the credential's known_hosts)
	HostBinding bool            // require a session-bind to a pinned host key before signing
}

// Provider supplies the session's identities. It is called on every request, so a
// credential set replaced by the runner takes effect at once. Implementations are
// in-memory and safe for concurrent use.
type Provider interface {
	Identities() []Identity
}

// ParseKnownHosts returns the host keys in OpenSSH known_hosts data. Revoked and
// cert-authority lines are ignored: only plain pinned host keys count.
func ParseKnownHosts(data []byte) ([]ssh.PublicKey, error) {
	var keys []ssh.PublicKey
	rest := data
	for len(bytes.TrimSpace(rest)) > 0 {
		marker, _, key, _, r, err := ssh.ParseKnownHosts(rest)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("sshagent: known_hosts: %w", err)
		}
		rest = r
		if marker == "" {
			keys = append(keys, key)
		}
	}
	return keys, nil
}

// connAgent serves one client connection. Session binds are per connection, as in OpenSSH.
type connAgent struct {
	p     Provider
	mu    sync.Mutex
	binds map[string][]byte // session id -> bound host key blob
}

func newConnAgent(p Provider) *connAgent {
	return &connAgent{p: p, binds: map[string][]byte{}}
}

var (
	_ agent.ExtendedAgent = (*connAgent)(nil)
)

func (a *connAgent) List() ([]*agent.Key, error) {
	var out []*agent.Key
	for _, id := range a.p.Identities() {
		pk := id.Signer.PublicKey()
		out = append(out, &agent.Key{Format: pk.Type(), Blob: pk.Marshal(), Comment: id.Comment})
	}
	return out, nil
}

func (a *connAgent) Sign(key ssh.PublicKey, data []byte) (*ssh.Signature, error) {
	return a.SignWithFlags(key, data, 0)
}

func (a *connAgent) SignWithFlags(key ssh.PublicKey, data []byte, flags agent.SignatureFlags) (*ssh.Signature, error) {
	req, err := ParseUserAuthRequest(data)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(req.PublicKey, key.Marshal()) {
		return nil, errors.New("sshagent: request key does not match signing key")
	}
	var id *Identity
	for _, c := range a.p.Identities() {
		if bytes.Equal(c.Signer.PublicKey().Marshal(), key.Marshal()) {
			c := c
			id = &c
			break
		}
	}
	if id == nil {
		return nil, errors.New("sshagent: unknown key")
	}
	if req.User != id.User {
		return nil, fmt.Errorf("sshagent: user %q is not allowed for this key", req.User)
	}
	if id.HostBinding {
		if err := a.checkBound(req.SessionID, id.HostKeys); err != nil {
			return nil, err
		}
	}
	algo := ""
	switch {
	case flags&agent.SignatureFlagRsaSha512 != 0:
		algo = ssh.KeyAlgoRSASHA512
	case flags&agent.SignatureFlagRsaSha256 != 0:
		algo = ssh.KeyAlgoRSASHA256
	}
	if as, ok := id.Signer.(ssh.AlgorithmSigner); ok && algo != "" {
		return as.SignWithAlgorithm(randReader, data, algo)
	}
	return id.Signer.Sign(randReader, data)
}

func (a *connAgent) checkBound(sessionID []byte, pinned []ssh.PublicKey) error {
	a.mu.Lock()
	hk, ok := a.binds[string(sessionID)]
	a.mu.Unlock()
	if !ok {
		return errors.New("sshagent: host binding required but this session was not bound (OpenSSH >= 8.9 needed, or turn host binding off for the credential)")
	}
	for _, p := range pinned {
		if bytes.Equal(p.Marshal(), hk) {
			return nil
		}
	}
	return errors.New("sshagent: server host key is not pinned in the credential's known_hosts")
}

// Extension implements session-bind@openssh.com (OpenSSH PROTOCOL.agent): the client
// sends the server host key, the session id and the server's signature over the session id.
func (a *connAgent) Extension(extensionType string, contents []byte) ([]byte, error) {
	if extensionType != sessionBindExt {
		return nil, agent.ErrExtensionUnsupported
	}
	var m struct {
		HostKey      []byte
		SessionID    []byte
		Signature    []byte
		IsForwarding bool
	}
	if err := ssh.Unmarshal(contents, &m); err != nil {
		return nil, fmt.Errorf("sshagent: bad session-bind: %w", err)
	}
	if m.IsForwarding {
		return nil, errors.New("sshagent: forwarded session-bind refused")
	}
	if len(m.SessionID) == 0 || len(m.SessionID) > 64 {
		return nil, errors.New("sshagent: bad session id")
	}
	hk, err := ssh.ParsePublicKey(m.HostKey)
	if err != nil {
		return nil, fmt.Errorf("sshagent: bad host key: %w", err)
	}
	var sig struct {
		Format string
		Blob   []byte
	}
	if err := ssh.Unmarshal(m.Signature, &sig); err != nil {
		return nil, fmt.Errorf("sshagent: bad signature: %w", err)
	}
	if err := hk.Verify(m.SessionID, &ssh.Signature{Format: sig.Format, Blob: sig.Blob}); err != nil {
		return nil, fmt.Errorf("sshagent: session-bind signature invalid: %w", err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.binds) >= 64 { // a connection never legitimately binds this many sessions
		return nil, errors.New("sshagent: too many session binds")
	}
	a.binds[string(m.SessionID)] = hk.Marshal()
	return nil, nil
}

func (a *connAgent) Add(agent.AddedKey) error       { return errRefused }
func (a *connAgent) Remove(ssh.PublicKey) error     { return errRefused }
func (a *connAgent) RemoveAll() error               { return errRefused }
func (a *connAgent) Lock([]byte) error              { return errRefused }
func (a *connAgent) Unlock([]byte) error            { return errRefused }
func (a *connAgent) Signers() ([]ssh.Signer, error) { return nil, errRefused }
