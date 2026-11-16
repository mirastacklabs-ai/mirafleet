package service

// Central-held credentials: admin-only set/list/delete/assign/unassign/
// rotate, encryption at rest, delivery to runners and central-side GitLab self-rotation. The
// secret value exists in plaintext only inside Set (from the caller's buffer to the sealer), the
// delivery body and the rotation exchange; it is never audited, logged, returned or put in errors.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// Rotation and expiry timing.
const (
	// ExpiringWindow is how far ahead of expiry a credential reports "expiring".
	ExpiringWindow = 14 * 24 * time.Hour
	// RotateWindow is how far ahead of expiry central rotates a rotate=gitlab_self credential.
	RotateWindow = 7 * 24 * time.Hour
	// DefaultRotateLifetime is the lifetime requested for a rotated token.
	DefaultRotateLifetime = 30 * 24 * time.Hour
	rotateRetryAfter      = time.Hour
	rotateRespMax         = 64 << 10
	deliverTimeout        = 15 * time.Second
	// rotateTimeout bounds one whole rotation (GitLab call + store write + delivery start); it
	// runs detached from the caller's context so a client disconnect cannot lose a rotated token.
	rotateTimeout = 90 * time.Second
	// deliverWait is how long an admin call waits for delivery before answering; slower runners
	// finish in the background and are retried on their next heartbeat if they fail.
	deliverWait = 2 * time.Second
	// storeRetries/storeBackoff retry the store write after GitLab has already rotated.
	storeRetries = 3
	storeBackoff = 200 * time.Millisecond
)

// ErrCredentialNotHeld means a requested credential is not held by the runner.
var ErrCredentialNotHeld = errors.New("service: runner does not hold a requested credential")

// CredentialSender delivers a runner's complete credential set over its live SSH connection
// (creds_sync). The registry/central wiring implements it; it must return an error when the
// runner is not connected. Implementations must not log the body.
type CredentialSender interface {
	SyncCredentials(ctx context.Context, runnerID int64, body *transport.CredsSyncBody) error
}

// secretsEnvKey is the name of the key variable (secrets.EnvKey), repeated here so that this
// package, which every binary links, does not link internal/secrets (server-only).
const secretsEnvKey = "MIRAFLEET_SECRETS_KEY"

// KeyRing is the sealing key as the service sees it; cmd adapts a secrets.Key to it.
type KeyRing interface {
	Seal(plaintext, aad []byte) ([]byte, error)
	Open(sealed, aad []byte) ([]byte, error)
	Check() []byte
	VerifyCheck(stored []byte) bool
}

// CredentialsConfig configures the credential feature. The zero value disables it unless
// credentials or a key check already exist in the store (then New refuses to start).
type CredentialsConfig struct {
	// Key is the secrets key. Nil means none configured.
	Key KeyRing
	// Sender delivers creds_sync; nil disables delivery (set/list still work).
	Sender CredentialSender
	// RotateClient is the HTTP client for GitLab rotation (system pool + optional corporate CA);
	// nil means a default with a 30 s timeout. Redirects are never followed.
	RotateClient *http.Client
	// RotateBaseURL maps a credential host to the GitLab base URL; nil means "https://"+host.
	RotateBaseURL func(host string) string
	// RotateLifetime is the lifetime requested for a rotated token; 0 means 30 days.
	RotateLifetime time.Duration
}

type credState struct {
	key      KeyRing
	hasKey   bool
	sender   CredentialSender
	client   *http.Client
	baseURL  func(string) string
	lifetime time.Duration

	mu        sync.Mutex
	gen       int64
	delivered map[int64]map[string]int64  // runner id -> credential name -> version confirmed
	reported  map[int64]map[string]string // runner id -> credential name -> runner-reported status
	rotErr    map[string]string
	lastTry   map[string]time.Time
	rotating  map[string]bool       // credential names with a rotation in flight
	stale     map[int64]bool        // runners whose last delivery failed
	retrying  map[int64]bool        // runners with a heartbeat-triggered retry in flight
	runLocks  map[int64]*sync.Mutex // per-runner delivery lock: read, generation and send are one step

	// test seams
	wait    time.Duration
	backoff time.Duration
}

// initCredentials checks the key against the stored key check: with a key, the first start
// stores the check and later starts must match it; without a key, stored credentials or a stored
// check refuse the start.
func (s *Service) initCredentials(cfg CredentialsConfig) error {
	c := &credState{key: cfg.Key, sender: cfg.Sender, client: cfg.RotateClient, baseURL: cfg.RotateBaseURL,
		lifetime: cfg.RotateLifetime, delivered: map[int64]map[string]int64{},
		reported: map[int64]map[string]string{}, rotErr: map[string]string{}, lastTry: map[string]time.Time{},
		rotating: map[string]bool{}, stale: map[int64]bool{}, retrying: map[int64]bool{},
		runLocks: map[int64]*sync.Mutex{}, wait: deliverWait, backoff: storeBackoff}
	c.hasKey = cfg.Key != nil
	if c.client == nil {
		c.client = &http.Client{Timeout: 30 * time.Second}
	}
	if c.baseURL == nil {
		c.baseURL = func(h string) string { return "https://" + h }
	}
	if c.lifetime <= 0 {
		c.lifetime = DefaultRotateLifetime
	}
	s.cred = c
	ctx := context.Background()
	stored, err := s.st.GetSecretsKeyCheck(ctx)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("service: read secrets key check: %w", err)
	}
	have := err == nil
	if !c.hasKey {
		if have {
			return errors.New("service: stored credentials need " + secretsEnvKey + " (not set)")
		}
		list, lerr := s.st.ListCredentials(ctx)
		if lerr != nil {
			return fmt.Errorf("service: list credentials: %w", lerr)
		}
		if len(list) > 0 {
			return errors.New("service: stored credentials need " + secretsEnvKey + " (not set)")
		}
		return nil
	}
	if have {
		if !c.key.VerifyCheck(stored) {
			return errors.New("service: " + secretsEnvKey + " does not match the key that sealed the stored credentials")
		}
		return nil
	}
	if list, lerr := s.st.ListCredentials(ctx); lerr != nil {
		return fmt.Errorf("service: list credentials: %w", lerr)
	} else if len(list) > 0 {
		return errors.New("service: credentials exist without a key check; refusing to guess the key")
	}
	now := s.now()
	ev := store.AuditEvent{Time: now, Source: string(SourceSystem), ActorKind: "system", Actor: "central",
		Action: "secrets.keycheck", Outcome: OutcomeOK}
	if err := s.st.SetSecretsKeyCheck(ctx, c.key.Check(), now, ev); err != nil {
		return fmt.Errorf("service: store secrets key check: %w", err)
	}
	return nil
}

func (s *Service) needKey() error {
	if !s.cred.hasKey {
		return api.Errorf(api.CodeUnavailable, "credentials are not configured (set %s)", secretsEnvKey)
	}
	return nil
}

// ---- sealing ---------------------------------------------------------------------------------

// payload is what is sealed: the secret and the (non-secret) known_hosts lines of an ssh-key.
type payload struct {
	Value      string   `json:"v"`
	KnownHosts []string `json:"kh,omitempty"`
}

func credAAD(id, version int64) []byte {
	return []byte(fmt.Sprintf("mirafleet credential v1|%d|%d", id, version))
}

func (s *Service) sealer(pl payload) store.CredentialSealer {
	return func(id, version int64) ([]byte, []byte, error) {
		raw, err := json.Marshal(pl)
		if err != nil {
			return nil, nil, errors.New("encode credential")
		}
		sealed, err := s.cred.key.Seal(raw, credAAD(id, version))
		clear(raw) // H-05: no plaintext copy outlives the seal
		if err != nil {
			return nil, nil, errors.New("seal credential")
		}
		// The envelope (version, key id, nonce, ciphertext) is stored whole as the ciphertext;
		// the store also keeps a copy of the 12-byte nonce (envelope bytes 5..17).
		const nonceAt = 1 + 4
		if len(sealed) < nonceAt+12 {
			return nil, nil, errors.New("seal credential")
		}
		return append([]byte(nil), sealed[nonceAt:nonceAt+12]...), sealed, nil
	}
}

func (s *Service) open(sc store.SealedCredential) (payload, error) {
	raw, err := s.cred.key.Open(sc.Ciphertext, credAAD(sc.ID, sc.Version))
	if err != nil {
		return payload{}, errors.New("cannot open credential")
	}
	var pl payload
	err = json.Unmarshal(raw, &pl)
	clear(raw) // H-05
	if err != nil {
		return payload{}, errors.New("cannot decode credential")
	}
	return pl, nil
}

// ---- set -------------------------------------------------------------------------------------

// SetCredential creates or replaces a credential. Admin only. value is the secret (the body
// frame); its length and SHA-256 must match p. The value is never echoed. The credential is
// delivered to every runner that holds it.
func (s *Service) SetCredential(ctx context.Context, p auth.Principal, src Source, params api.CredentialSetParams, value []byte) (api.CredentialSetResult, error) {
	if err := s.authorize(ctx, p, src, api.MethodCredentialSet, true); err != nil {
		return api.CredentialSetResult{}, err
	}
	if err := s.needKey(); err != nil {
		return api.CredentialSetResult{}, err
	}
	if ae := params.Validate(); ae != nil {
		return api.CredentialSetResult{}, ae
	}
	if len(value) != params.Length {
		return api.CredentialSetResult{}, badRequest("value length does not match length")
	}
	if sum := sha256.Sum256(value); hex.EncodeToString(sum[:]) != params.SHA256 {
		return api.CredentialSetResult{}, badRequest("value does not match sha256")
	}
	if params.Expires != nil && !params.Expires.After(s.now()) {
		return api.CredentialSetResult{}, badRequest("expires must be in the future")
	}
	fp, err := credFingerprint(params.Kind, value)
	if err != nil {
		return api.CredentialSetResult{}, err
	}
	c := store.Credential{Name: params.Name, Kind: store.CredentialKind(params.Kind), Hosts: params.Hosts,
		Fingerprint: fp, Meta: metaFromParams(params)}
	if params.Expires != nil {
		c.ExpiresAt = *params.Expires
	}
	now := s.now()
	ev := s.event(p, src, api.MethodCredentialSet)
	ev.Subject = params.Name
	ev.Params = map[string]string{"kind": params.Kind, "hosts": strings.Join(params.Hosts, ","), "fingerprint": fp}
	if params.Rotate != "" {
		ev.Params["rotate"] = params.Rotate
	}
	out, created, err := s.st.SetCredential(ctx, c, s.sealer(payload{Value: string(value), KnownHosts: params.KnownHosts}), now, ev)
	if err != nil {
		return api.CredentialSetResult{}, s.fail("set credential", err)
	}
	s.cred.mu.Lock()
	delete(s.cred.rotErr, params.Name)
	s.cred.mu.Unlock()
	s.deliverHolders(ctx, params.Name, nil)
	info, err := s.credInfo(ctx, out)
	if err != nil {
		return api.CredentialSetResult{}, err
	}
	return api.CredentialSetResult{Credential: info, Replaced: !created}, nil
}

func metaFromParams(p api.CredentialSetParams) map[string]string {
	m := map[string]string{}
	put := func(k, v string) {
		if v != "" {
			m[k] = v
		}
	}
	put("user", p.User)
	put("auth", p.Auth)
	put("header", p.Header)
	put("host_binding", p.HostBinding)
	put("rotate", p.Rotate)
	put("rotate_project", p.RotateProject)
	return m
}

// credFingerprint validates the value for its kind and returns the admin-visible fingerprint:
// SHA256 of the public key for ssh-key (an encrypted or malformed key is refused), SHA-256 of the
// value otherwise.
func credFingerprint(kind string, value []byte) (string, error) {
	switch kind {
	case transport.CredSSHKey:
		signer, err := ssh.ParsePrivateKey(value)
		if err != nil {
			return "", badRequest("value is not an unencrypted OpenSSH private key")
		}
		return ssh.FingerprintSHA256(signer.PublicKey()), nil
	default:
		if strings.IndexByte(string(value), 0) >= 0 || strings.TrimSpace(string(value)) == "" {
			return "", badRequest("invalid credential value")
		}
		sum := sha256.Sum256(value)
		return "SHA256:" + hex.EncodeToString(sum[:8]), nil
	}
}

// ---- list / delete / assign ------------------------------------------------------------------

// ListCredentials returns the masked views (metadata, fingerprint, expiry, assignments and
// per-runner delivery). Admin only; an empty name lists all.
func (s *Service) ListCredentials(ctx context.Context, p auth.Principal, src Source, name string) ([]api.CredentialInfo, error) {
	if err := s.authorize(ctx, p, src, api.MethodCredentialList, true); err != nil {
		return nil, err
	}
	var cs []store.Credential
	if name != "" {
		c, err := s.st.GetCredential(ctx, name)
		if err != nil {
			return nil, s.fail("get credential", err)
		}
		cs = []store.Credential{c}
	} else {
		var err error
		if cs, err = s.st.ListCredentials(ctx); err != nil {
			return nil, s.fail("list credentials", err)
		}
	}
	out := make([]api.CredentialInfo, 0, len(cs))
	for _, c := range cs {
		info, err := s.credInfo(ctx, c)
		if err != nil {
			return nil, err
		}
		out = append(out, info)
	}
	return out, nil
}

// DeleteCredential removes a credential and its assignments, then re-syncs the runners that held
// it so the value stops working at once. Admin only.
func (s *Service) DeleteCredential(ctx context.Context, p auth.Principal, src Source, name string) error {
	if err := s.authorize(ctx, p, src, api.MethodCredentialDelete, true); err != nil {
		return err
	}
	if !transport.ValidCredName(name) {
		return badRequest("invalid credential name")
	}
	holders, err := s.st.CredentialRunners(ctx, name)
	if err != nil {
		return s.fail("credential runners", err)
	}
	ev := s.event(p, src, api.MethodCredentialDelete)
	ev.Subject = name
	if err := s.st.DeleteCredential(ctx, name, s.now(), ev); err != nil {
		return s.fail("delete credential", err)
	}
	s.cred.mu.Lock()
	delete(s.cred.rotErr, name)
	delete(s.cred.lastTry, name)
	s.cred.mu.Unlock()
	s.deliverTo(ctx, holders)
	return nil
}

// AssignCredential assigns name to a runner (fleet+runner) or a whole fleet and syncs holders.
// Admin only.
func (s *Service) AssignCredential(ctx context.Context, p auth.Principal, src Source, params api.CredentialAssignParams) (api.CredentialInfo, error) {
	if ae := params.Validate(); ae != nil {
		return api.CredentialInfo{}, ae
	}
	return s.assign(ctx, p, src, api.MethodCredentialAssign, params.Name, store.CredentialTarget{Fleet: params.Fleet, Runner: params.Runner}, true)
}

// UnassignCredential removes one assignment and syncs the affected runners. Admin only.
func (s *Service) UnassignCredential(ctx context.Context, p auth.Principal, src Source, params api.CredentialUnassignParams) (api.CredentialInfo, error) {
	if ae := params.Validate(); ae != nil {
		return api.CredentialInfo{}, ae
	}
	return s.assign(ctx, p, src, api.MethodCredentialUnassign, params.Name, store.CredentialTarget{Fleet: params.Fleet, Runner: params.Runner}, false)
}

func (s *Service) assign(ctx context.Context, p auth.Principal, src Source, action, name string, t store.CredentialTarget, add bool) (api.CredentialInfo, error) {
	if err := s.authorize(ctx, p, src, action, true); err != nil {
		return api.CredentialInfo{}, err
	}
	before, err := s.st.CredentialRunners(ctx, name)
	if err != nil {
		return api.CredentialInfo{}, s.fail("credential runners", err)
	}
	ev := s.event(p, src, action)
	ev.Subject, ev.Fleet, ev.Runner = name, t.Fleet, t.Runner
	if add {
		_, err = s.st.AssignCredential(ctx, name, t, s.now(), ev)
	} else {
		err = s.st.UnassignCredential(ctx, name, t, s.now(), ev)
	}
	if err != nil {
		return api.CredentialInfo{}, s.fail(action, err)
	}
	s.deliverHolders(ctx, name, before)
	c, err := s.st.GetCredential(ctx, name)
	if err != nil {
		return api.CredentialInfo{}, s.fail("get credential", err)
	}
	return s.credInfo(ctx, c)
}

// ---- views -----------------------------------------------------------------------------------

func credStatus(c store.Credential, now time.Time) string {
	switch {
	case c.ExpiresAt.IsZero():
		return transport.CredOK
	case !c.ExpiresAt.After(now):
		return transport.CredExpired
	case c.ExpiresAt.Sub(now) < ExpiringWindow:
		return transport.CredExpiring
	}
	return transport.CredOK
}

// credInfo builds the admin view: metadata, assignments and per-runner delivery. No value.
func (s *Service) credInfo(ctx context.Context, c store.Credential) (api.CredentialInfo, error) {
	info := api.CredentialInfo{Name: c.Name, Kind: string(c.Kind), Hosts: c.Hosts, User: c.Meta["user"],
		Auth: c.Meta["auth"], Header: c.Meta["header"], HostBinding: c.Meta["host_binding"],
		Rotate: c.Meta["rotate"], RotateProject: c.Meta["rotate_project"], Version: c.Version,
		Fingerprint: c.Fingerprint, Created: c.CreatedAt, Updated: c.CreatedAt}
	if !c.RotatedAt.IsZero() {
		info.Updated = c.RotatedAt
	}
	if !c.ExpiresAt.IsZero() {
		e := c.ExpiresAt
		info.Expires = &e
	}
	for _, f := range c.Fleets {
		info.Assignments = append(info.Assignments, api.CredentialAssignment{Fleet: f})
	}
	for _, r := range c.Runners {
		info.Assignments = append(info.Assignments, api.CredentialAssignment{Fleet: r.Fleet, Runner: r.Runner})
	}
	holders, err := s.st.CredentialRunners(ctx, c.Name)
	if err != nil {
		return info, s.fail("credential runners", err)
	}
	s.cred.mu.Lock()
	defer s.cred.mu.Unlock()
	info.RotateError = s.cred.rotErr[c.Name]
	for _, r := range holders {
		st := "pending"
		if s.cred.delivered[r.ID][c.Name] == c.Version {
			if rep, ok := s.cred.reported[r.ID][c.Name]; ok {
				st = rep
			} else {
				st = credStatus(c, s.now())
			}
		}
		info.Delivery = append(info.Delivery, api.CredentialDelivery{Fleet: r.Fleet, Runner: r.Name, Status: st, Version: c.Version})
	}
	return info, nil
}

// RunnerCredentials is the operator view of what the runner fleet/name holds: name, kind, hosts
// and status only (Q32). Allowed for active operators and admins (policy action runners.list).
func (s *Service) RunnerCredentials(ctx context.Context, p auth.Principal, src Source, ref store.Ref) ([]api.RunnerCredential, error) {
	if err := s.authorize(ctx, p, src, api.MethodRunnersList, false); err != nil {
		return nil, err
	}
	r, err := s.st.GetRunner(ctx, ref)
	if err != nil {
		return nil, s.fail("get runner", err)
	}
	return s.RunnerCredentialViews(ctx, r.ID)
}

// RunnerCredentialViews is RunnerCredentials without authorisation, for callers that have
// already authorised (the runners.list builder).
func (s *Service) RunnerCredentialViews(ctx context.Context, runnerID int64) ([]api.RunnerCredential, error) {
	cs, err := s.st.EffectiveCredentials(ctx, runnerID)
	if err != nil {
		return nil, s.fail("effective credentials", err)
	}
	s.cred.mu.Lock()
	defer s.cred.mu.Unlock()
	out := make([]api.RunnerCredential, 0, len(cs))
	for _, c := range cs {
		st, ok := s.cred.reported[runnerID][c.Name]
		if !ok {
			st = credStatus(c, s.now())
		}
		out = append(out, api.RunnerCredential{Name: c.Name, Kind: string(c.Kind), Hosts: c.Hosts, Status: st})
	}
	return out, nil
}

// ReportCredentialStatus records the credential statuses a runner reported in its heartbeat.
func (s *Service) ReportCredentialStatus(runnerID int64, hb []transport.HeartbeatCredential) {
	m := make(map[string]string, len(hb))
	for _, c := range hb {
		m[c.Name] = c.Status
	}
	s.cred.mu.Lock()
	s.cred.reported[runnerID] = m
	s.cred.mu.Unlock()
}

// ---- placement -------------------------------------------------------------------------------

// ResolveCredentials returns the credential names a workspace on the runner will use: all the
// runner holds when requested is empty, otherwise exactly requested, which must be a subset of
// what it holds (narrowing never widens: ErrCredentialNotHeld otherwise).
func (s *Service) ResolveCredentials(ctx context.Context, runnerID int64, requested []string) ([]string, error) {
	held, err := s.heldNames(ctx, runnerID)
	if err != nil {
		return nil, err
	}
	if len(requested) == 0 {
		names := make([]string, 0, len(held))
		for n := range held {
			names = append(names, n)
		}
		sort.Strings(names)
		return names, nil
	}
	for _, n := range requested {
		if !held[n] {
			return nil, ErrCredentialNotHeld
		}
	}
	return append([]string(nil), requested...), nil
}

// FilterRunnersByCredentials keeps the runners that hold every requested credential (placement,
// Q33). No requested names keeps all.
func (s *Service) FilterRunnersByCredentials(ctx context.Context, runners []store.Runner, requested []string) ([]store.Runner, error) {
	if len(requested) == 0 {
		return runners, nil
	}
	var out []store.Runner
	for _, r := range runners {
		held, err := s.heldNames(ctx, r.ID)
		if err != nil {
			return nil, err
		}
		ok := true
		for _, n := range requested {
			if !held[n] {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *Service) heldNames(ctx context.Context, runnerID int64) (map[string]bool, error) {
	cs, err := s.st.EffectiveCredentials(ctx, runnerID)
	if err != nil {
		return nil, s.fail("effective credentials", err)
	}
	m := make(map[string]bool, len(cs))
	for _, c := range cs {
		m[c.Name] = true
	}
	return m, nil
}

// ---- delivery --------------------------------------------------------------------------------

// RunnerConnected must be called when a runner's session is established: it forgets the runner's
// delivery state and sends its full effective set.
func (s *Service) RunnerConnected(ctx context.Context, runnerID int64) error {
	s.cred.mu.Lock()
	delete(s.cred.delivered, runnerID)
	delete(s.cred.reported, runnerID)
	s.cred.mu.Unlock()
	return s.DeliverRunner(ctx, runnerID)
}

// ClaimDeliveryRetry reports whether the runner's last delivery failed and no retry is already
// running; when true the caller owns the retry and must call RetryDelivery. Cheap: called on
// every heartbeat.
func (s *Service) ClaimDeliveryRetry(runnerID int64) bool {
	if s.cred == nil {
		return false
	}
	s.cred.mu.Lock()
	defer s.cred.mu.Unlock()
	if !s.cred.stale[runnerID] || s.cred.retrying[runnerID] {
		return false
	}
	s.cred.retrying[runnerID] = true
	return true
}

// RetryDelivery re-sends the runner's set after ClaimDeliveryRetry returned true.
func (s *Service) RetryDelivery(ctx context.Context, runnerID int64) error {
	defer func() {
		s.cred.mu.Lock()
		delete(s.cred.retrying, runnerID)
		s.cred.mu.Unlock()
	}()
	return s.DeliverRunner(ctx, runnerID)
}

// DeliverRunner builds the runner's effective set (sealed -> plaintext) and hands it to the
// sender. Nothing is sent when no key or sender is configured, or the runner is not active. A
// failure marks the runner stale so its next heartbeat retries.
func (s *Service) DeliverRunner(ctx context.Context, runnerID int64) error {
	// H-01: one delivery per runner at a time, so the store read and the generation taken after
	// it cannot interleave with another delivery and give a stale set the higher generation.
	l := s.runnerLock(runnerID)
	l.Lock()
	defer l.Unlock()
	err := s.deliverRunner(ctx, runnerID)
	s.cred.mu.Lock()
	if err != nil {
		s.cred.stale[runnerID] = true
	} else {
		delete(s.cred.stale, runnerID)
	}
	s.cred.mu.Unlock()
	return err
}

func (s *Service) runnerLock(id int64) *sync.Mutex {
	s.cred.mu.Lock()
	defer s.cred.mu.Unlock()
	l := s.cred.runLocks[id]
	if l == nil {
		l = &sync.Mutex{}
		s.cred.runLocks[id] = l
	}
	return l
}

// withdrawRunner sends a disabled runner an empty creds_sync so it drops the secrets it holds
// (H-02), and forgets the delivery state. Best effort: a failure is logged; the runner is
// re-synced from the store when it reconnects.
func (s *Service) withdrawRunner(ctx context.Context, runnerID int64) {
	if s.cred == nil || !s.cred.hasKey || s.cred.sender == nil {
		return
	}
	l := s.runnerLock(runnerID)
	l.Lock()
	defer l.Unlock()
	s.cred.mu.Lock()
	delete(s.cred.delivered, runnerID)
	delete(s.cred.reported, runnerID)
	delete(s.cred.stale, runnerID)
	s.cred.mu.Unlock()
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deliverTimeout)
	defer cancel()
	body := &transport.CredsSyncBody{Credentials: []transport.CredSecret{}, Generation: s.nextGen()}
	if err := s.cred.sender.SyncCredentials(dctx, runnerID, body); err != nil {
		s.logf("service: empty creds_sync to runner %d failed: %v", runnerID, errors.New("send failed"))
	}
}

// MarkDeliveryStale forgets the runner's delivery state and marks it for a heartbeat retry
// instead of delivering now: used for a reconnect inside the edge debounce (H-06).
func (s *Service) MarkDeliveryStale(runnerID int64) {
	if s.cred == nil {
		return
	}
	s.cred.mu.Lock()
	delete(s.cred.delivered, runnerID)
	delete(s.cred.reported, runnerID)
	s.cred.stale[runnerID] = true
	s.cred.mu.Unlock()
}

func (s *Service) deliverRunner(ctx context.Context, runnerID int64) error {
	if !s.cred.hasKey || s.cred.sender == nil {
		return nil
	}
	r, err := s.st.GetRunnerByID(ctx, runnerID)
	if err != nil {
		return s.fail("get runner", err)
	}
	if r.Status != store.StatusActive {
		return nil
	}
	scs, err := s.st.SealedCredentialsFor(ctx, runnerID)
	if err != nil {
		return s.fail("sealed credentials", err)
	}
	if len(scs) > transport.MaxSyncCreds {
		s.logf("service: runner %d holds %d credentials (max %d); not delivering", runnerID, len(scs), transport.MaxSyncCreds)
		return errors.New("too many credentials for one runner")
	}
	body := &transport.CredsSyncBody{Credentials: make([]transport.CredSecret, 0, len(scs))}
	versions := map[string]int64{}
	for _, sc := range scs {
		pl, err := s.open(sc)
		if err != nil {
			s.logf("service: credential %q cannot be opened; skipped", sc.Name)
			continue
		}
		cs := transport.CredSecret{
			CredMeta: transport.CredMeta{Name: sc.Name, Kind: string(sc.Kind), Hosts: sc.Hosts, User: sc.Meta["user"],
				Auth: sc.Meta["auth"], Header: sc.Meta["header"], KnownHosts: pl.KnownHosts, HostBinding: sc.Meta["host_binding"]},
			Version: sc.Version, Value: pl.Value}
		if !sc.ExpiresAt.IsZero() {
			cs.ExpiresMs = sc.ExpiresAt.UnixMilli()
		}
		body.Credentials = append(body.Credentials, cs)
		versions[sc.Name] = sc.Version
	}
	body.Generation = s.nextGen()
	if err := body.Validate(); err != nil {
		s.logf("service: creds_sync for runner %d is invalid: %v", runnerID, err)
		return errors.New("invalid credential set")
	}
	dctx, cancel := context.WithTimeout(ctx, deliverTimeout)
	defer cancel()
	if err := s.cred.sender.SyncCredentials(dctx, runnerID, body); err != nil {
		s.logf("service: creds_sync to runner %d failed: %v", runnerID, errors.New("send failed"))
		return errors.New("credential delivery failed")
	}
	s.cred.mu.Lock()
	s.cred.delivered[runnerID] = versions
	s.cred.mu.Unlock()
	return nil
}

func (s *Service) nextGen() int64 {
	s.cred.mu.Lock()
	defer s.cred.mu.Unlock()
	g := s.now().UnixMilli()
	if g <= s.cred.gen {
		g = s.cred.gen + 1
	}
	s.cred.gen = g
	return g
}

// deliverHolders re-syncs every current holder of name plus extra (holders from before the
// change, which may have lost it).
func (s *Service) deliverHolders(ctx context.Context, name string, extra []store.Runner) {
	cur, err := s.st.CredentialRunners(ctx, name)
	if err != nil {
		s.logf("service: credential runners: %v", errors.New("lookup failed"))
	}
	s.deliverTo(ctx, append(cur, extra...))
}

// deliverTo delivers to each runner in its own goroutine, detached from the caller's context
// (a client disconnect must not abort delivery) and bounded by deliverTimeout. It waits at most
// cred.wait for all of them, so a hung runner never holds up the admin response or the others.
func (s *Service) deliverTo(ctx context.Context, rs []store.Runner) {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deliverTimeout)
	var wg sync.WaitGroup
	seen := map[int64]bool{}
	for _, r := range rs {
		if seen[r.ID] {
			continue
		}
		seen[r.ID] = true
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			if err := s.DeliverRunner(dctx, id); err != nil {
				s.logf("service: deliver credentials to runner %d: %v", id, err)
			}
		}(r.ID)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); cancel(); close(done) }()
	t := time.NewTimer(s.cred.wait)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
	}
}

// ---- rotation --------------------------------------------------------------------------------

var systemPrincipal = auth.Principal{Kind: "system", Name: "central"}

// RotateCredential rotates a rotate=gitlab_self credential now. Admin only.
func (s *Service) RotateCredential(ctx context.Context, p auth.Principal, src Source, name string) (api.CredentialInfo, error) {
	if err := s.authorize(ctx, p, src, api.MethodCredentialRotate, true); err != nil {
		return api.CredentialInfo{}, err
	}
	if !transport.ValidCredName(name) {
		return api.CredentialInfo{}, badRequest("invalid credential name")
	}
	if err := s.needKey(); err != nil {
		return api.CredentialInfo{}, err
	}
	return s.rotate(ctx, p, src, name)
}

// RotateDue rotates every rotate=gitlab_self credential with less than RotateWindow left, at
// most once an hour per credential after a failure. Failures are audited and kept for the list
// view. It returns the number rotated.
func (s *Service) RotateDue(ctx context.Context) int {
	if !s.cred.hasKey {
		return 0
	}
	cs, err := s.st.ListCredentials(ctx)
	if err != nil {
		s.logf("service: list credentials for rotation: %v", errors.New("lookup failed"))
		return 0
	}
	n := 0
	for _, c := range cs {
		if c.Meta["rotate"] != api.RotateGitLabSelf || c.ExpiresAt.IsZero() || c.ExpiresAt.Sub(s.now()) >= RotateWindow {
			continue
		}
		s.cred.mu.Lock()
		last, busy := s.cred.lastTry[c.Name], s.cred.rotating[c.Name]
		s.cred.mu.Unlock()
		if busy {
			continue
		}
		if !last.IsZero() && s.now().Sub(last) < rotateRetryAfter {
			continue
		}
		if _, err := s.rotate(ctx, systemPrincipal, SourceSystem, c.Name); err == nil {
			n++
		}
	}
	return n
}

// RunRotation calls RotateDue every interval (default 1 hour) until ctx ends.
func (s *Service) RunRotation(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = time.Hour
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		s.RotateDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// errRotateLost is returned when GitLab rotated the token but the new value could not be stored:
// the old token is revoked and the new one is gone, so the admin must issue a fresh token.
const errRotateLost = "GitLab issued a new token but it could not be stored; the old token is revoked"

func (s *Service) rotate(ctx context.Context, p auth.Principal, src Source, name string) (api.CredentialInfo, error) {
	// One rotation per credential at a time: a second one would present the already-rotated
	// token and make GitLab revoke the whole token family.
	s.cred.mu.Lock()
	if s.cred.rotating[name] {
		s.cred.mu.Unlock()
		return api.CredentialInfo{}, api.Errorf(api.CodeConflict, "rotation in progress")
	}
	s.cred.rotating[name] = true
	s.cred.mu.Unlock()
	defer func() {
		s.cred.mu.Lock()
		delete(s.cred.rotating, name)
		s.cred.mu.Unlock()
	}()
	// Detached from the caller (admin socket close, browser abort): once GitLab has rotated, the
	// store write must complete.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rotateTimeout)
	defer cancel()
	sc, err := s.st.GetSealedCredential(ctx, name)
	if err != nil {
		return api.CredentialInfo{}, s.fail("get credential", err)
	}
	if sc.Meta["rotate"] != api.RotateGitLabSelf {
		return api.CredentialInfo{}, badRequest("credential is not set to rotate")
	}
	now := s.now()
	s.cred.mu.Lock()
	s.cred.lastTry[name] = now
	s.cred.mu.Unlock()
	fail := func(msg string) (api.CredentialInfo, error) {
		ev := s.event(p, src, api.MethodCredentialRotate)
		ev.Subject, ev.Outcome, ev.Message = name, "error", msg
		if _, aerr := s.st.RecordAudit(ctx, ev); aerr != nil {
			s.logf("service: audit rotation failure: %v", errors.New("audit failed"))
		}
		s.cred.mu.Lock()
		s.cred.rotErr[name] = msg
		s.cred.mu.Unlock()
		return api.CredentialInfo{}, api.Errorf(api.CodeUnavailable, "rotation failed: %s", msg)
	}
	pl, err := s.open(sc)
	if err != nil {
		return fail("stored value cannot be opened")
	}
	tok, exp, msg := s.gitlabRotate(ctx, sc.Hosts[0], sc.Meta["rotate_project"], pl.Value, now)
	if msg != "" {
		return fail(msg)
	}
	next := sc.Credential
	next.Fingerprint = "SHA256:" + func() string { h := sha256.Sum256([]byte(tok)); return hex.EncodeToString(h[:8]) }()
	next.ExpiresAt = exp
	ev := s.event(p, src, api.MethodCredentialRotate)
	ev.Subject = name
	ev.Params = map[string]string{"from_version": fmt.Sprint(sc.Version), "fingerprint": next.Fingerprint}
	inner := s.sealer(payload{Value: tok, KnownHosts: pl.KnownHosts})
	guard := func(id, version int64) ([]byte, []byte, error) {
		if version != sc.Version+1 {
			return nil, nil, errors.New("credential changed during rotation")
		}
		return inner(id, version)
	}
	var out store.Credential
	for i := 0; ; i++ {
		out, _, err = s.st.SetCredential(ctx, next, guard, now, ev)
		if err == nil {
			break
		}
		if i+1 >= storeRetries {
			ev := s.event(p, src, api.MethodCredentialRotate)
			ev.Subject, ev.Outcome, ev.Message = name, "error", errRotateLost
			if _, aerr := s.st.RecordAudit(ctx, ev); aerr != nil {
				s.logf("service: audit rotation failure: %v", errors.New("audit failed"))
			}
			s.cred.mu.Lock()
			s.cred.rotErr[name] = errRotateLost
			s.cred.mu.Unlock()
			return api.CredentialInfo{}, api.Errorf(api.CodeUnavailable, "%s", errRotateLost)
		}
		select {
		case <-ctx.Done():
		case <-time.After(s.cred.backoff * time.Duration(i+1)):
		}
	}
	s.cred.mu.Lock()
	delete(s.cred.rotErr, name)
	s.cred.mu.Unlock()
	s.deliverHolders(ctx, name, nil)
	return s.credInfo(ctx, out)
}

// gitlabRotate calls POST /api/v4/projects/:id/access_tokens/self/rotate with the current token
// and returns the new token and expiry. On failure it returns a fixed message (no response
// body, no URL, no token).
func (s *Service) gitlabRotate(ctx context.Context, host, project, token string, now time.Time) (string, time.Time, string) {
	base, err := url.Parse(s.cred.baseURL(host))
	if err != nil || project == "" {
		return "", time.Time{}, "bad gitlab address"
	}
	want := now.Add(s.cred.lifetime).UTC()
	if max := now.Add(365 * 24 * time.Hour); want.After(max) {
		want = max.UTC()
	}
	u := *base
	u.Path = strings.TrimRight(base.Path, "/") + "/api/v4/projects/" + project + "/access_tokens/self/rotate"
	u.RawPath = strings.TrimRight(base.EscapedPath(), "/") + "/api/v4/projects/" + url.PathEscape(project) + "/access_tokens/self/rotate"
	form := url.Values{"expires_at": {want.Format("2006-01-02")}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, "bad gitlab address"
	}
	req.Header.Set("PRIVATE-TOKEN", token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	cl := *s.cred.client
	cl.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := cl.Do(req)
	if err != nil {
		return "", time.Time{}, "gitlab unreachable"
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, rotateRespMax))
	if err != nil {
		return "", time.Time{}, "gitlab response unreadable"
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", time.Time{}, fmt.Sprintf("gitlab returned status %d", resp.StatusCode)
	}
	var r struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if json.Unmarshal(body, &r) != nil || r.Token == "" || len(r.Token) > transport.MaxCredentialData ||
		strings.IndexByte(r.Token, 0) >= 0 || strings.TrimSpace(r.Token) != r.Token {
		return "", time.Time{}, "gitlab response invalid"
	}
	exp := want
	if t, err := time.Parse("2006-01-02", r.ExpiresAt); err == nil {
		exp = t.Add(24*time.Hour - time.Second).UTC() // expires at the end of that day
	}
	if !exp.After(now) {
		return "", time.Time{}, "gitlab response invalid"
	}
	return r.Token, exp, ""
}
