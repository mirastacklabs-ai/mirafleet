// Package store defines the persistence boundary of central: domain types, validation, errors
// and the Store interface. It is pure Go with no SQL; internal/store/sqlite implements it.
package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mirastacklabs-ai/mirafleet/internal/keys"
)

// Errors returned by Store implementations. Callers match with errors.Is.
var (
	// ErrNotFound: the referenced fleet, runner, token, operator or setting does not exist. From
	// Enroll it also covers a wrong token secret (indistinguishable from an unknown id); the
	// service must map Enroll's ErrNotFound and ErrStale to one identical joiner-facing error.
	ErrNotFound = errors.New("store: not found")
	// ErrInvalid: input failed validation (name shape, mode, malformed token id or secret hash,
	// MaxUses < 1, expiry not after now, empty key or fingerprint or setting key/value, oversize
	// runner info, empty "from" status set, zero audit time). It describes the shape of the
	// caller's input, never a token's existence. The service maps it to bad_request.
	ErrInvalid = errors.New("store: invalid input")
	// ErrConflict: a UNIQUE/PRIMARY KEY or state precondition failed (duplicate name or key, revoked key
	// re-enrolling, status transition whose "from" set no longer matches).
	ErrConflict = errors.New("store: conflict")
	// ErrStale: the join token is expired, revoked or used up.
	ErrStale = errors.New("store: token expired, revoked or used up")
)

// Name rules. Runner names reuse the M1 rule; fleet names use the same shape.
var (
	runnerNameRE = keys.RunnerNameRE
	// FleetNameRE is the allowed shape of a fleet name.
	FleetNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	// OperatorNameRE is the allowed shape of an operator name.
	OperatorNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
)

// ValidFleetName reports whether s is an acceptable fleet name.
func ValidFleetName(s string) bool { return FleetNameRE.MatchString(s) }

// ValidRunnerName reports whether s is an acceptable runner name.
func ValidRunnerName(s string) bool { return runnerNameRE.MatchString(s) }

// ValidOperatorName reports whether s is an acceptable operator name.
func ValidOperatorName(s string) bool { return OperatorNameRE.MatchString(s) }

// Ref names a runner as fleet/runner.
type Ref struct{ Fleet, Runner string }

func (r Ref) String() string { return r.Fleet + "/" + r.Runner }

// maxEcho bounds how much user input an error message repeats.
const maxEcho = 64

// echo truncates user input to its first maxEcho bytes (cut at a rune boundary) plus "…".
func echo(s string) string {
	if len(s) <= maxEcho {
		return s
	}
	n := maxEcho
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// ParseRef parses "fleet/runner", validating both names. Errors echo at most the first 64
// bytes of the offending input.
func ParseRef(s string) (Ref, error) {
	f, r, ok := strings.Cut(s, "/")
	if !ok {
		return Ref{}, fmt.Errorf("invalid runner reference %q: want fleet/runner", echo(s))
	}
	if !ValidFleetName(f) {
		return Ref{}, fmt.Errorf("invalid fleet name %q (must match %s)", echo(f), FleetNameRE)
	}
	if !ValidRunnerName(r) {
		return Ref{}, fmt.Errorf("invalid runner name %q (must match %s)", echo(r), runnerNameRE)
	}
	return Ref{Fleet: f, Runner: r}, nil
}

// Mode is a fleet's operating mode.
type Mode string

const (
	ModeActions   Mode = "actions"
	ModeWorkspace Mode = "workspace"
)

// Valid reports whether m is a known mode.
func (m Mode) Valid() bool { return m == ModeActions || m == ModeWorkspace }

// Status is a runner's lifecycle state.
type Status string

const (
	StatusPending  Status = "pending"
	StatusActive   Status = "active"
	StatusDisabled Status = "disabled"
	StatusRevoked  Status = "revoked"
)

// Valid reports whether s is a known runner status.
func (s Status) Valid() bool {
	switch s {
	case StatusPending, StatusActive, StatusDisabled, StatusRevoked:
		return true
	}
	return false
}

// CanTransition reports whether a runner may move from one status to another:
// pending->active, active<->disabled, any non-revoked->revoked; revoked is terminal.
func CanTransition(from, to Status) bool {
	if !from.Valid() || !to.Valid() || from == StatusRevoked {
		return false
	}
	switch to {
	case StatusRevoked:
		return true
	case StatusActive:
		return from == StatusPending || from == StatusDisabled
	case StatusDisabled:
		return from == StatusActive
	}
	return false
}

// OperatorStatus is an operator's state. There is no pending state: operators register with a
// rotating code and are active immediately.
type OperatorStatus string

const (
	OperatorActive  OperatorStatus = "active"
	OperatorRevoked OperatorStatus = "revoked"
)

// Valid reports whether s is a known operator status.
func (s OperatorStatus) Valid() bool { return s == OperatorActive || s == OperatorRevoked }

// Fleet groups runners.
type Fleet struct {
	ID        int64
	Name      string
	Mode      Mode
	Confirm   bool // guarded: action.run must carry confirm == fleet name
	CreatedAt time.Time
	// RunnerCount is the number of non-revoked runners. Populated by ListFleets only; zero
	// from GetFleet and CreateFleet.
	RunnerCount int
}

// Runner is an enrolled host. PubKey is the SSH wire-format key; Fingerprint its SHA256 form.
type Runner struct {
	ID          int64
	FleetID     int64
	Fleet       string // fleet name (denormalised on read)
	Name        string
	PubKey      []byte
	Fingerprint string
	Status      Status
	Labels      map[string]string
	Info        map[string]string
	JoinTokenID *int64 // nil once the token is deleted
	CreatedAt   time.Time
	UpdatedAt   time.Time
	LastSeenAt  time.Time // zero if never seen
}

// Ref returns the runner's fleet/name reference.
func (r Runner) Ref() Ref { return Ref{r.Fleet, r.Name} }

// JoinToken is a bearer credential for runner enrolment. Only the SHA-256 of the secret is stored.
type JoinToken struct {
	ID          int64
	TokenID     string // public 16-hex id
	SecretHash  []byte
	FleetID     int64
	Fleet       string // fleet name (denormalised on read)
	Labels      map[string]string
	AutoApprove bool
	MaxUses     int // >= 1
	Uses        int
	ExpiresAt   time.Time
	RevokedAt   time.Time // zero if not revoked
	CreatedBy   string
	CreatedAt   time.Time
}

// Usable reports whether the token can still be consumed at now (a pre-check; Enroll re-checks
// atomically).
func (t JoinToken) Usable(now time.Time) bool {
	return t.RevokedAt.IsZero() && t.Uses < t.MaxUses && now.Before(t.ExpiresAt)
}

// Operator is a human/agent identity allowed to use api@mirafleet.
type Operator struct {
	ID          int64
	Name        string
	PubKey      []byte
	Fingerprint string
	Status      OperatorStatus
	CreatedAt   time.Time
	UpdatedAt   time.Time
	LastSeenAt  time.Time
}

// AuditEvent is one row of the append-only audit table (§1.7). Names are denormalised; there
// are no foreign keys, so rows survive deletion of what they describe. ID is assigned by the
// store and ignored on write.
type AuditEvent struct {
	ID         int64
	Time       time.Time
	Source     string // cli api gui enroll system
	ActorKind  string // admin operator runner system
	Actor      string
	Remote     string
	Fleet      string
	Runner     string
	Subject    string
	Action     string
	Params     map[string]string
	Outcome    string
	ExitCode   *int // nil = no exit code (stored as NULL)
	DurationMS int64
	Message    string
	RequestID  string
}

// Store is the persistence interface. All methods are safe for concurrent use. Every
// state-changing method writes its audit event in the same transaction as the change: either
// both commit or neither does. The event's ID is ignored; a zero ev.Time is set to the
// method's now. Secrets (token secrets, registration secret) must never appear in events.
//
// Time: every mutator takes now and implementations never call time.Now(); the service owns
// the clock (injectable in tests).
//
// Key disjointness (a key may belong to a runner or an operator, never both, §4a) is enforced
// in the service on enrol and register, not in the store.
type Store interface {
	// CreateFleet inserts a fleet and returns it with ID and CreatedAt (= now) set. ErrInvalid
	// if the mode is invalid or the name does not satisfy FleetNameRE. Atomic with ev.
	// ErrConflict if the name exists.
	CreateFleet(ctx context.Context, f Fleet, now time.Time, ev AuditEvent) (Fleet, error)
	// GetFleet returns the fleet by name, or ErrNotFound. RunnerCount is not populated.
	GetFleet(ctx context.Context, name string) (Fleet, error)
	// ListFleets returns all fleets ordered by name, each with RunnerCount (non-revoked
	// runners) populated.
	ListFleets(ctx context.Context) ([]Fleet, error)

	// ListRunners returns runners of a fleet ordered by name, including revoked ones when
	// includeRevoked is set. ErrNotFound if the fleet does not exist.
	ListRunners(ctx context.Context, fleet string, includeRevoked bool) ([]Runner, error)
	// GetRunner returns the non-revoked runner fleet/name, or ErrNotFound.
	GetRunner(ctx context.Context, ref Ref) (Runner, error)
	// GetRunnerByKey returns the runner (any status) with that SSH wire key, or ErrNotFound.
	GetRunnerByKey(ctx context.Context, pubKey []byte) (Runner, error)
	// GetRunnerByID returns the runner (any status) by id, or ErrNotFound.
	GetRunnerByID(ctx context.Context, id int64) (Runner, error)
	// TouchRunner records last_seen_at (= seen) and info for a runner. Not audited (high
	// frequency); ErrNotFound if the id does not exist. Never changes status. ErrInvalid if the info JSON exceeds 16 KiB.
	TouchRunner(ctx context.Context, id int64, seen time.Time, info map[string]string) error

	// Enroll consumes one use of the join token and inserts newRunner in a single transaction.
	// Order, fixed so that timing does not leak which check failed:
	//  1. look up the token by tokenID;
	//  2. compare SHA-256(secret) (secretHash) with the stored hash in constant time, against a
	//     fixed dummy hash when the id is unknown, so unknown id and wrong secret cost the same;
	//  3. staleness: revoked, expired at now, or used up -> ErrStale (the conditional
	//     uses<max_uses update must affect one row);
	//  4. key disjointness: a key present in operators (any status) -> ErrConflict. This runs only
	//     after steps 1-3, so a caller with a bad token gets the token error, never a conflict;
	//  5. insert.
	// Unknown id and wrong secret both return ErrNotFound. The service maps ErrNotFound and
	// ErrStale to one identical joiner-facing error. newRunner.FleetID/Fleet/Labels/Status are
	// taken from the token (active if AutoApprove, else pending); the caller supplies Name,
	// PubKey, Fingerprint. ErrInvalid on a bad runner name, an empty PubKey or Fingerprint, or a
	// secretHash that is not 32 bytes (checked before any lookup). ErrConflict if the name is taken by
	// a non-revoked runner in the fleet or the key is already known (including revoked runner
	// keys, which never re-enrol) or belongs to an operator; nothing is consumed on any error.
	// Writes ev with ev.Fleet set to the token's fleet name; the returned Runner.Fleet is that name.
	Enroll(ctx context.Context, tokenID string, secretHash []byte, now time.Time, newRunner Runner, ev AuditEvent) (Runner, error)
	// SetRunnerStatus moves runner id to status to if its current status is in from and
	// CanTransition allows it, atomically with ev; UpdatedAt = now. ErrInvalid if from is empty
	// or holds an invalid status, or to is invalid. ErrNotFound if id is unknown; ErrConflict
	// if the current status is not in from or the transition is not allowed.
	SetRunnerStatus(ctx context.Context, id int64, from []Status, to Status, now time.Time, ev AuditEvent) (Runner, error)

	// CreateToken inserts a join token (SecretHash, TokenID, Fleet name, MaxUses and ExpiresAt
	// set by the caller) and returns it with ID and CreatedAt (= now). ErrInvalid unless SecretHash is 32 bytes,
	// TokenID is 16 lowercase hex chars, MaxUses >= 1 and ExpiresAt is after now. The returned
	// token has SecretHash zeroed (nil). Atomic with ev. ErrNotFound if the fleet is unknown; ErrConflict on
	// duplicate TokenID.
	CreateToken(ctx context.Context, t JoinToken, now time.Time, ev AuditEvent) (JoinToken, error)
	// RevokeToken sets revoked_at (= now) for tokenID if not already set, atomically with ev.
	// ErrNotFound if unknown; revoking an already revoked token is a no-op returning nil
	// (no event written).
	RevokeToken(ctx context.Context, tokenID string, now time.Time, ev AuditEvent) error
	// ListTokens returns tokens ordered by id; fleet is "" for all fleets. SecretHash is always
	// zeroed (nil) in the result.
	ListTokens(ctx context.Context, fleet string) ([]JoinToken, error)

	// RegisterOperator inserts an active operator, idempotent per key: if the key is already an
	// active operator, that operator is returned unchanged (created=false) even when the
	// requested name differs (the service compares names); this path writes NO audit row, so
	// the service must RecordAudit it. ErrInvalid on a bad name. ErrConflict if the key belongs
	// to a revoked operator, to a runner (any status; checked inside the transaction), or the
	// name is held by a different active operator. When created,
	// atomic with ev.
	RegisterOperator(ctx context.Context, name string, pubKey []byte, fingerprint string, now time.Time, ev AuditEvent) (op Operator, created bool, err error)
	// GetOperatorByKey returns the operator (any status) with that wire key, or ErrNotFound.
	GetOperatorByKey(ctx context.Context, pubKey []byte) (Operator, error)
	// GetOperatorByID returns the operator (any status) by id, or ErrNotFound.
	GetOperatorByID(ctx context.Context, id int64) (Operator, error)
	// GetOperatorByName returns the non-revoked operator named name, or ErrNotFound.
	GetOperatorByName(ctx context.Context, name string) (Operator, error)
	// TouchOperator records last_seen_at (= seen) for an operator. Not audited; ErrNotFound if
	// the id does not exist. Never changes status.
	TouchOperator(ctx context.Context, id int64, seen time.Time) error
	// RevokeOperator marks the non-revoked operator named name revoked (UpdatedAt = now),
	// atomically with ev. ErrNotFound if there is no such non-revoked operator.
	RevokeOperator(ctx context.Context, name string, now time.Time, ev AuditEvent) (Operator, error)
	// ListOperators returns operators ordered by id, optionally including revoked ones.
	ListOperators(ctx context.Context, includeRevoked bool) ([]Operator, error)

	// GetSetting returns a setting value or ErrNotFound. Values may be secret: never log them.
	GetSetting(ctx context.Context, key string) ([]byte, error)
	// CreateSetting inserts a setting atomically with ev; ErrConflict if the key is present.
	// Used by `central init`.
	CreateSetting(ctx context.Context, key string, value []byte, now time.Time, ev AuditEvent) error
	// PutSetting upserts (explicitly overwrites) a setting atomically with ev, e.g.
	// `operator rotate-secret`.
	PutSetting(ctx context.Context, key string, value []byte, now time.Time, ev AuditEvent) error

	// RecordAudit appends ev (for events with no accompanying state change, e.g. exec start
	// and outcome rows) and returns its assigned id. ev.Time must be set by the caller
	// (ErrInvalid if zero): there is no now to default from.
	RecordAudit(ctx context.Context, ev AuditEvent) (int64, error)
	// ListAudit returns events with id > sinceID in ascending id order, at most limit rows
	// (implementations cap limit).
	ListAudit(ctx context.Context, sinceID int64, limit int) ([]AuditEvent, error)

	// Close releases resources. Further calls fail.
	Close() error
}

// SettingRegistrationSecret is the settings key holding the operator registration secret.
const SettingRegistrationSecret = "registration_secret"
