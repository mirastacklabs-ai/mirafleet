package runner

// Credentials on the runner (docs/M7.md section 4.2, Q8). creds_sync delivers the runner's whole
// effective set; the holder keeps it in memory only and replaces it atomically. Nothing here is
// ever written to disk, logged or put in an environment. The only view that leaves the holder is
// the heartbeat's name/kind/hosts/status list.

import (
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/broker"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/sshagent"
)

// credExpiringWithin is how long before expiry a credential reports "expiring" (docs/M7.md 4.5).
const credExpiringWithin = 14 * 24 * time.Hour

// credEntry is one held credential, prepared for use. It is immutable once built.
type credEntry struct {
	meta      transport.CredMeta
	version   int64
	expiresMs int64

	bc       broker.Credential  // https-token and registry
	identity *sshagent.Identity // ssh-key (Signer is the raw signer; sessions wrap it)
	bad      bool               // the value could not be turned into a usable credential
}

func (e *credEntry) isSSH() bool { return e.meta.Kind == transport.CredSSHKey }

func (e *credEntry) expired(now time.Time) bool {
	return e.expiresMs > 0 && e.expiresMs <= now.UnixMilli()
}

func (e *credEntry) status(now time.Time) string {
	switch {
	case e.bad:
		return transport.CredInvalid
	case e.expired(now):
		return transport.CredExpired
	case e.expiresMs > 0 && time.UnixMilli(e.expiresMs).Sub(now) < credExpiringWithin:
		return transport.CredExpiring
	}
	return transport.CredOK
}

// usable reports whether sessions may use the credential now.
func (e *credEntry) usable(now time.Time) bool { return !e.bad && !e.expired(now) }

func newCredEntry(c transport.CredSecret) *credEntry {
	e := &credEntry{meta: c.CredMeta, version: c.Version, expiresMs: c.ExpiresMs}
	e.meta.Hosts = append([]string(nil), c.Hosts...)
	e.meta.KnownHosts = append([]string(nil), c.KnownHosts...)
	if e.isSSH() {
		id, err := buildIdentity(c)
		if err != nil {
			e.bad = true
		} else {
			e.identity = id
		}
		return e
	}
	bc := broker.Credential{Name: c.Name, Hosts: e.meta.Hosts, User: c.User, Secret: c.Value}
	switch {
	case c.Kind == transport.CredRegistry:
		bc.Kind = broker.Registry
	case c.Auth == transport.AuthBasic:
		bc.Kind = broker.Basic
	case c.Auth == transport.AuthBearer:
		bc.Kind = broker.Bearer
	case c.Auth == transport.AuthHeader:
		bc.Kind, bc.HeaderName = broker.Header, c.Header
	}
	if bc.Validate() != nil {
		e.bad = true
		return e
	}
	e.bc = bc
	return e
}

func buildIdentity(c transport.CredSecret) (*sshagent.Identity, error) {
	raw, err := ssh.ParseRawPrivateKey([]byte(c.Value))
	if err != nil {
		return nil, errors.New("unusable ssh key") // never wrap err: it can quote the key
	}
	signer, err := ssh.NewSignerFromKey(raw)
	if err != nil {
		return nil, errors.New("unusable ssh key")
	}
	hk, err := sshagent.ParseKnownHosts([]byte(strings.Join(c.KnownHosts, "\n") + "\n"))
	if err != nil || len(hk) == 0 {
		return nil, errors.New("no usable known_hosts line")
	}
	user := c.User
	if user == "" {
		user = "git"
	}
	return &sshagent.Identity{Signer: signer, Comment: c.Name, User: user, HostKeys: hk,
		HostBinding: c.HostBinding != transport.BindOff}, nil
}

// credHolder is the runner's in-memory credential set.
type credHolder struct {
	mu   sync.Mutex
	gen  int64
	have bool
	m    map[string]*credEntry
}

func newCredHolder() *credHolder { return &credHolder{m: map[string]*credEntry{}} }

// apply replaces the whole set atomically. A sync older than the one held is ignored (applied is
// false); an equal or newer generation replaces. The caller zeroes its copy of the request body.
func (h *credHolder) apply(b *transport.CredsSyncBody) (applied bool, gen int64, n int) {
	m := make(map[string]*credEntry, len(b.Credentials))
	for _, c := range b.Credentials {
		m[c.Name] = newCredEntry(c)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.have && b.Generation < h.gen {
		return false, h.gen, len(h.m)
	}
	h.m, h.gen, h.have = m, b.Generation, true // the old entries are dropped with their references
	return true, h.gen, len(m)
}

// entries returns the held entries sorted by name (the entries are immutable).
func (h *credHolder) entries() []*credEntry {
	h.mu.Lock()
	out := make([]*credEntry, 0, len(h.m))
	for _, e := range h.m {
		out = append(out, e)
	}
	h.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].meta.Name < out[j].meta.Name })
	return out
}

// has reports whether a credential of that name is held.
func (h *credHolder) has(name string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.m[name]
	return ok
}

// heartbeat is the operator-visible view: name, kind, hosts and status. Never a value.
func (h *credHolder) heartbeat(now time.Time) []transport.HeartbeatCredential {
	es := h.entries()
	out := make([]transport.HeartbeatCredential, 0, len(es))
	for _, e := range es {
		if len(out) >= transport.MaxHeartbeatCredentials {
			break
		}
		out = append(out, transport.HeartbeatCredential{Name: e.meta.Name, Kind: e.meta.Kind,
			Hosts: append([]string(nil), e.meta.Hosts...), Status: e.status(now)})
	}
	return out
}

// signature identifies the usable subset (names, versions): sessions refresh when it changes,
// for instance when a credential expires between two syncs.
func (h *credHolder) signature(now time.Time) string {
	var sb strings.Builder
	for _, e := range h.entries() {
		if e.usable(now) {
			sb.WriteString(e.meta.Name)
			sb.WriteByte(':')
			sb.WriteString(strconv.FormatInt(e.version, 10))
			sb.WriteByte(' ')
		}
	}
	return sb.String()
}
