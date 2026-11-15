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
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/egress"
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
	// Workspace is the workspace configuration: required on CreateFleet when Mode is
	// workspace, forbidden (nil) otherwise. Populated by GetFleet, ListFleets and CreateFleet
	// for workspace-mode fleets.
	Workspace *WorkspaceConfig
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
	Workspace  string // ws_id of the workspace the event concerns, "" if none
}

// GUI admin statuses.
const (
	GUIAdminActive = "active"
	GUIAdminReset  = "reset"
)

// MaxGUIUserAgent bounds the stored user-agent summary, in bytes.
const MaxGUIUserAgent = 128

// GUIAdmin is the single admin account of the web GUI. PwHash is the PHC string; it never
// leaves the store/service boundary. Generation is the credential generation: a password
// change bumps it and every session bound to the old one dies.
type GUIAdmin struct {
	ID         int64
	Username   string
	PwHash     string
	Generation int64
	Status     string // GUIAdminActive or GUIAdminReset
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// GUISession is one browser session. Only SHA-256(token) is stored (TokenHash, 32 bytes);
// store methods return sessions with it nil unless stated.
type GUISession struct {
	ID           int64
	TokenHash    []byte
	AdminID      int64
	Generation   int64
	CreatedAt    time.Time
	LastActiveAt time.Time
	ExpiresAt    time.Time // absolute expiry
	Remote       string
	UserAgent    string
}

// AuditFilter selects audit rows for QueryAudit. Zero fields match everything. String fields
// are exact matches on their column (Action too, unless ActionPrefix). AfterID and BeforeID are
// keyset bounds (id > AfterID, id < BeforeID); From and To bound the time inclusively. Desc
// orders by id descending. Limit <= 0 means 100; it is capped at 1000. Desc with AfterID returns
// the newest rows overall, not the page just after AfterID: to page toward newer rows, query
// ascending with AfterID and reverse the result.
type AuditFilter struct {
	AfterID, BeforeID int64
	From, To          time.Time
	Fleet, Runner     string
	Actor, Source     string
	Outcome           string
	Workspace         string
	RequestID         string
	Action            string
	ActionPrefix      bool
	Desc              bool
	Limit             int
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
// by the store, inside the Enroll and RegisterOperator transactions in internal/store/sqlite.
type Store interface {
	// CreateFleet inserts a fleet and returns it with ID and CreatedAt (= now) set. ErrInvalid
	// if the mode is invalid or the name does not satisfy FleetNameRE, or if f.Workspace is
	// nil in workspace mode, non-nil in actions mode, or fails WorkspaceConfig.Validate. The
	// config is inserted in the same transaction (UpdatedAt = now). Atomic with ev.
	// ErrConflict if the name exists.
	CreateFleet(ctx context.Context, f Fleet, now time.Time, ev AuditEvent) (Fleet, error)
	// GetFleet returns the fleet by name, or ErrNotFound. RunnerCount is not populated;
	// Workspace is set for workspace-mode fleets.
	GetFleet(ctx context.Context, name string) (Fleet, error)
	// ListFleets returns all fleets ordered by name, each with RunnerCount (non-revoked
	// runners) and Workspace (workspace-mode fleets) populated.
	ListFleets(ctx context.Context) ([]Fleet, error)
	// GetFleetWorkspace returns the workspace config of fleet, or ErrNotFound if the fleet is
	// unknown or is not in workspace mode.
	GetFleetWorkspace(ctx context.Context, fleet string) (WorkspaceConfig, error)
	// PutFleetWorkspace replaces the workspace config of a workspace-mode fleet (UpdatedAt =
	// now) and returns it, atomically with ev. ErrInvalid if the config fails Validate or the
	// fleet is in actions mode; ErrNotFound if the fleet is unknown. Existing workspaces keep
	// the spec they were opened with.
	PutFleetWorkspace(ctx context.Context, fleet string, c WorkspaceConfig, now time.Time, ev AuditEvent) (WorkspaceConfig, error)

	// ReserveWorkspace places w (state is forced to opening, WsID must be 32 lowercase hex,
	// FleetID/RunnerID/OperatorID/Session/Image/Spec set by the caller; CreatedAt = LastUsedAt
	// = now, ExpiresAt from the caller) on its runner in one immediate transaction.
	//  1. If a live (opening or open) workspace exists for (operator, fleet, session), return it
	//     with existing=true and write nothing (the service audits the reuse).
	//  2. Else if the operator already holds maxPerOp live workspaces (opening, open or
	//     closing) -> ErrFull; maxPerOp <= 0 means no cap.
	//  3. Else if the runner already holds slots live workspaces (opening, open or closing)
	//     -> ErrFull.
	//  4. Else insert, atomically with ev (ev.Workspace is set to w.WsID).
	// ErrInvalid for a malformed w, slots < 1, a runner that is not in the fleet or is revoked; ErrNotFound for an unknown fleet, runner or
	// operator; ErrConflict on a duplicate WsID.
	ReserveWorkspace(ctx context.Context, w Workspace, slots, maxPerOp int, now time.Time, ev AuditEvent) (ws Workspace, existing bool, err error)
	// SetWorkspaceState moves wsID to state to with reason if its current state is in from and
	// CanTransitionWorkspace allows it, atomically with ev (ev.Workspace = wsID). closed_at =
	// now when to is closed, failed or lost. ErrInvalid if from is empty or holds an invalid
	// state, or to is invalid; ErrNotFound if wsID is unknown; ErrConflict if the current state
	// is not in from or the transition is not allowed.
	SetWorkspaceState(ctx context.Context, wsID string, from []WorkspaceState, to WorkspaceState, reason string, now time.Time, ev AuditEvent) (Workspace, error)
	// TouchWorkspace sets last_used_at = now on an open workspace. Not audited; ErrNotFound
	// if wsID is unknown; a workspace in another state is left unchanged (nil).
	TouchWorkspace(ctx context.Context, wsID string, now time.Time) error
	// GetWorkspace returns the workspace (any state) by ws_id, or ErrNotFound.
	GetWorkspace(ctx context.Context, wsID string) (Workspace, error)
	// ListWorkspaces returns workspaces matching the filter ordered by id.
	ListWorkspaces(ctx context.Context, f WorkspaceFilter) ([]Workspace, error)
	// CountLiveWorkspaces returns, for each given runner id, the number of workspaces in
	// opening, open or closing (the slots it holds). Runners with none map to 0.
	CountLiveWorkspaces(ctx context.Context, runnerIDs []int64) (map[int64]int, error)
	// CloseWorkspacesOf moves every opening and open workspace of the runner or operator to closing
	// (so a destroy is attempted even for an in-flight create) with reason, in one transaction, writing ev once (Params gains "count" and "ws_ids", the comma-joined affected ids; ev.Message defaults to the count).
	// It returns the affected ws_ids. ErrInvalid unless exactly one owner id is non-zero.
	CloseWorkspacesOf(ctx context.Context, who WorkspaceOwner, reason string, now time.Time, ev AuditEvent) ([]string, error)

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
	// QueryAudit returns events matching f (see AuditFilter), ordered by id (descending when
	// f.Desc), at most f.Limit rows (default 100, capped at 1000).
	QueryAudit(ctx context.Context, f AuditFilter) ([]AuditEvent, error)

	// SetFleetConfirm sets the fleet's guard flag (confirm) and returns the fleet, atomically
	// with ev. ErrNotFound if the fleet is unknown; ErrInvalid on a malformed name. Setting the
	// value it already has is not an error and still writes ev.
	SetFleetConfirm(ctx context.Context, name string, confirm bool, now time.Time, ev AuditEvent) (Fleet, error)

	// GetGUIAdmin returns the active GUI admin (PwHash included), or ErrNotFound.
	GetGUIAdmin(ctx context.Context) (GUIAdmin, error)
	// PutGUISetup upserts the single setup row (SHA-256 of the setup token, 32 bytes, and its
	// expiry; created_at = now), atomically with ev. ErrInvalid on a bad hash or an expiry not
	// after now; ErrConflict if an active admin exists.
	PutGUISetup(ctx context.Context, hash []byte, expires, now time.Time, ev AuditEvent) error
	// EnsureGUISetup is PutGUISetup that writes only if the setup row is missing or expired at
	// now, decided in the same transaction (token rotation without a check-then-act window).
	// created=false: a live token exists, nothing was written (no audit row), err is nil.
	// Same errors as PutGUISetup otherwise.
	EnsureGUISetup(ctx context.Context, hash []byte, expires, now time.Time, ev AuditEvent) (created bool, err error)
	// CompleteGUISetup, in one transaction: the setup row exists, hash matches it (constant
	// time), it has not expired at now and no active admin exists -> insert a (Username,
	// PwHash; generation 1, status active), delete the setup row, write ev. A missing, wrong
	// or expired token is ErrStale (the row is kept); an existing active admin is ErrConflict.
	// ErrInvalid on a bad hash, username (OperatorNameRE shape) or empty PwHash, checked first.
	CompleteGUISetup(ctx context.Context, hash []byte, a GUIAdmin, now time.Time, ev AuditEvent) (GUIAdmin, error)
	// SetGUIPassword, in one transaction: compare-and-swap the active admin's generation
	// (must equal generation), store hash, bump the generation, delete every session of the
	// admin and insert keep (AdminID and Generation are forced to the admin's and the new
	// generation), write ev. ErrConflict on a generation mismatch; ErrNotFound if id is not the
	// active admin; ErrInvalid on an empty hash or a bad keep. The returned GUIAdmin is the
	// updated one.
	SetGUIPassword(ctx context.Context, id, generation int64, hash string, keep GUISession, now time.Time, ev AuditEvent) (GUIAdmin, error)
	// ResetGUIAdmin, in one transaction: the active admin (if any) becomes status reset, every
	// session row is deleted, ev is written (also when there was no admin).
	ResetGUIAdmin(ctx context.Context, now time.Time, ev AuditEvent) error
	// CreateGUISession inserts s (token hash 32 bytes, user agent clipped to 128 bytes) for
	// the active admin s.AdminID, whose generation must equal s.Generation, then deletes that
	// admin's oldest rows beyond maxPerAdmin (must be >= 1, else ErrInvalid), atomically with ev. ErrNotFound
	// if the admin is not active; ErrConflict on a generation mismatch or duplicate token hash;
	// ErrInvalid on a malformed s. The result has ID set and TokenHash nil.
	CreateGUISession(ctx context.Context, s GUISession, maxPerAdmin int, now time.Time, ev AuditEvent) (GUISession, error)
	// GetGUISession returns the session with that token hash joined to its admin, which must be
	// active; otherwise ErrNotFound. It does not judge expiry or generation: the service does.
	// The returned session has TokenHash nil.
	GetGUISession(ctx context.Context, tokenHash []byte) (GUISession, GUIAdmin, error)
	// TouchGUISession sets last_active_at = now. Not audited; ErrNotFound if id is unknown.
	TouchGUISession(ctx context.Context, id int64, now time.Time) error
	// DeleteGUISession deletes session id if it belongs to adminID, atomically with ev (a
	// zero ev.Action writes no audit row, for housekeeping of dead rows). ErrNotFound if there
	// is no such row for that admin; nothing is written then.
	DeleteGUISession(ctx context.Context, adminID, id int64, now time.Time, ev AuditEvent) error
	// DeleteOtherGUISessions deletes every session of adminID except keepID and returns how
	// many, atomically with ev (written even when none; ErrInvalid if ev.Action is empty).
	DeleteOtherGUISessions(ctx context.Context, adminID, keepID int64, now time.Time, ev AuditEvent) (int, error)
	// ListGUISessions returns the admin's sessions, oldest first, with TokenHash nil.
	ListGUISessions(ctx context.Context, adminID int64) ([]GUISession, error)
	// PurgeGUISessions deletes sessions past their absolute expiry (expires_at <= now), idle
	// for idle or longer (last_active_at <= now-idle), or whose admin is not active or whose
	// generation differs from the admin's. Not audited. Returns the number deleted. ErrInvalid if
	// idle <= 0.
	PurgeGUISessions(ctx context.Context, idle time.Duration, now time.Time) (int, error)

	// Close releases resources. Further calls fail.
	Close() error
}

// SettingRegistrationSecret is the settings key holding the operator registration secret.
const SettingRegistrationSecret = "registration_secret"

// ErrFull: a workspace could not be reserved because every slot (or the per-operator cap) is
// taken. The service maps it to unavailable / the next runner.
var ErrFull = errors.New("store: no free workspace slot")

// MaxExecCeiling is the hard upper bound for a fleet's exec timeout (24 h); the per-fleet
// exec_max_s can be set up to it, never past it.
const MaxExecCeiling = 24 * time.Hour

// MaxWorkspaceTTL bounds a fleet's max_ttl. It mirrors transport.MaxWorkspaceAgeMs (the runner
// refuses a larger max_age_ms; a test asserts they are equal).
const MaxWorkspaceTTL = 365 * 24 * time.Hour

// Bounds on the list fields of a WorkspaceConfig.
const (
	maxConfigImages = 64
	maxConfigEgress = 256
	maxConfigEntry  = 512
)

// Executor names the runner-side workspace backend of a fleet.
type Executor string

const (
	ExecutorContainerd  Executor = "containerd"
	ExecutorOpenSandbox Executor = "opensandbox"
)

// Valid reports whether e is a known executor.
func (e Executor) Valid() bool { return e == ExecutorContainerd || e == ExecutorOpenSandbox }

// PullPolicy is when a runner pulls an image.
type PullPolicy string

const (
	PullIfNotPresent PullPolicy = "if-not-present"
	PullAlways       PullPolicy = "always"
	PullNever        PullPolicy = "never"
)

// Valid reports whether p is a known pull policy.
func (p PullPolicy) Valid() bool {
	return p == PullIfNotPresent || p == PullAlways || p == PullNever
}

// NetworkMode is a workspace's network policy.
type NetworkMode string

const (
	NetworkNone   NetworkMode = "none"
	NetworkEgress NetworkMode = "egress"
)

// Valid reports whether n is a known network mode.
func (n NetworkMode) Valid() bool { return n == NetworkNone || n == NetworkEgress }

// WorkspaceConfig is a workspace-mode fleet's admin-set configuration (M3.md 1.7, 1.10).
// Durations are stored in whole seconds.
type WorkspaceConfig struct {
	Executor     Executor
	DefaultImage string
	// Images is the allowlist: exact references, or patterns with one trailing '*'
	// (repo@sha256:*, repo:*). DefaultImage must match one entry.
	Images  []string
	Pull    PullPolicy
	Network NetworkMode
	// Egress is the proxy allowlist ("host[:port]", "*.domain:443", "cidr:port"); only
	// meaningful when Network is egress. Each entry is validated with egress.ParseRule.
	Egress      []string
	CPUMilli    int
	MemoryMB    int
	Pids        int
	TmpfsMB     int
	DiskMB      int
	IdleTTL     time.Duration // > 0
	MaxTTL      time.Duration // >= IdleTTL
	ExecDefault time.Duration // > 0, <= ExecMax
	ExecMax     time.Duration // <= MaxExecCeiling
	UpdatedAt   time.Time     // set by the store on write
}

// ImageAllowed reports whether ref is allowed by Images, using the one shared matcher
// (workspace.MatchImage): exact refs, repo:* and repo@sha256:*, normalised on both sides.
func (c WorkspaceConfig) ImageAllowed(ref string) bool {
	return workspace.MatchImage(c.Images, ref)
}

func badEntry(s string) bool {
	if s == "" || len(s) > maxConfigEntry {
		return true
	}
	for _, r := range s {
		if r <= ' ' || r == 0x7f {
			return true
		}
	}
	return false
}

// Upper bounds on the per-workspace resource limits (A-15), far above any real host and low enough
// that MB-to-byte conversions cannot overflow int64.
const (
	MaxCPUMilli = 1_024_000
	MaxMemoryMB = 16_777_216
	MaxPids     = 4_194_304
	MaxTmpfsMB  = 16_777_216
	MaxDiskMB   = 1_073_741_824
)

// Validate checks the configuration; every error wraps ErrInvalid.
func (c WorkspaceConfig) Validate() error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: workspace config: %s", ErrInvalid, fmt.Sprintf(format, a...))
	}
	if !c.Executor.Valid() {
		return bad("executor must be containerd or opensandbox")
	}
	if !c.Pull.Valid() {
		return bad("pull must be if-not-present, always or never")
	}
	if !c.Network.Valid() {
		return bad("network must be none or egress")
	}
	if _, err := workspace.NormalizeImage(c.DefaultImage); err != nil || badEntry(c.DefaultImage) || strings.Contains(c.DefaultImage, "*") {
		return bad("default image is empty, malformed or a pattern")
	}
	if len(c.Images) == 0 || len(c.Images) > maxConfigImages {
		return bad("images allowlist needs 1 to %d entries", maxConfigImages)
	}
	for _, p := range c.Images {
		if badEntry(p) {
			return bad("malformed image allowlist entry %q", echo(p))
		}
		if err := validImagePattern(p); err != nil {
			return bad("image allowlist entry %q: %v", echo(p), err)
		}
	}
	if !c.ImageAllowed(c.DefaultImage) {
		return bad("default image %q is not in the allowlist", echo(c.DefaultImage))
	}
	if len(c.Egress) > maxConfigEgress {
		return bad("at most %d egress rules", maxConfigEgress)
	}
	for _, e := range c.Egress {
		if badEntry(e) {
			return bad("malformed egress rule %q", echo(e))
		}
		if _, err := egress.ParseRule(e); err != nil {
			return bad("egress rule %q: %v", echo(e), err)
		}
	}
	if c.Network == NetworkNone && len(c.Egress) > 0 {
		return bad("egress rules require network egress")
	}
	for _, l := range []struct {
		name     string
		v, limit int
	}{{"cpu_milli", c.CPUMilli, MaxCPUMilli}, {"memory_mb", c.MemoryMB, MaxMemoryMB}, {"pids", c.Pids, MaxPids}, {"tmpfs_mb", c.TmpfsMB, MaxTmpfsMB}, {"disk_mb", c.DiskMB, MaxDiskMB}} {
		if l.v <= 0 || l.v > l.limit {
			return bad("%s must be 1 to %d", l.name, l.limit)
		}
	}
	if c.IdleTTL < time.Second || c.MaxTTL < c.IdleTTL {
		return bad("need 0 < idle ttl <= max ttl (whole seconds)")
	}
	if c.MaxTTL > MaxWorkspaceTTL {
		return bad("max ttl must be at most %s", MaxWorkspaceTTL)
	}
	if c.ExecDefault < time.Second || c.ExecMax < c.ExecDefault || c.ExecMax > MaxExecCeiling {
		return bad("need 0 < exec default <= exec max <= %s", MaxExecCeiling)
	}
	return nil
}

// WorkspaceState is a workspace row's lifecycle state.
type WorkspaceState string

const (
	WorkspaceOpening WorkspaceState = "opening"
	WorkspaceOpen    WorkspaceState = "open"
	WorkspaceClosing WorkspaceState = "closing"
	WorkspaceClosed  WorkspaceState = "closed"
	WorkspaceFailed  WorkspaceState = "failed"
	WorkspaceLost    WorkspaceState = "lost"
)

// Valid reports whether s is a known workspace state.
func (s WorkspaceState) Valid() bool {
	switch s {
	case WorkspaceOpening, WorkspaceOpen, WorkspaceClosing, WorkspaceClosed, WorkspaceFailed, WorkspaceLost:
		return true
	}
	return false
}

// Terminal reports whether no transition leaves s.
func (s WorkspaceState) Terminal() bool {
	return s == WorkspaceClosed || s == WorkspaceFailed || s == WorkspaceLost
}

// Live reports whether the state holds a runner slot (opening, open or closing).
func (s WorkspaceState) Live() bool {
	return s == WorkspaceOpening || s == WorkspaceOpen || s == WorkspaceClosing
}

// CanTransitionWorkspace reports whether a workspace may move from one state to another:
// opening->open|closing|failed, open->closing|lost, closing->closed|lost|failed (B-12: a dead runner must not pin a slot forever). The rest are terminal.
func CanTransitionWorkspace(from, to WorkspaceState) bool {
	switch from {
	case WorkspaceOpening:
		return to == WorkspaceOpen || to == WorkspaceClosing || to == WorkspaceFailed
	case WorkspaceOpen:
		return to == WorkspaceClosing || to == WorkspaceLost
	case WorkspaceClosing:
		return to == WorkspaceClosed || to == WorkspaceLost || to == WorkspaceFailed
	}
	return false
}

// Workspace is one sandbox row. WsID is the 32-hex id central assigns.
type Workspace struct {
	ID         int64
	WsID       string
	FleetID    int64
	Fleet      string // denormalised on read
	RunnerID   int64
	Runner     string // denormalised on read
	OperatorID int64
	Operator   string // denormalised on read
	Session    string
	Image      string
	Spec       string // JSON
	State      WorkspaceState
	Reason     string
	CreatedAt  time.Time
	LastUsedAt time.Time
	ExpiresAt  time.Time
	ClosedAt   time.Time // zero while not closed
}

// WorkspaceFilter selects workspaces for ListWorkspaces; zero fields match everything.
type WorkspaceFilter struct {
	Fleet      string // fleet name
	OperatorID int64
	RunnerID   int64
	States     []WorkspaceState
	Limit      int // <= 0 means 100; implementations cap it
}

// WorkspaceOwner selects the workspaces CloseWorkspacesOf affects: exactly one of RunnerID and
// OperatorID is non-zero.
type WorkspaceOwner struct{ RunnerID, OperatorID int64 }

// validImagePattern accepts a well-formed exact reference, "repo:*" or "repo@sha256:*"; the
// semantics are those of workspace.MatchImage.
func validImagePattern(p string) error {
	base := p
	switch {
	case strings.HasSuffix(p, "@sha256:*"):
		base = strings.TrimSuffix(p, "@sha256:*")
	case strings.HasSuffix(p, ":*"):
		base = strings.TrimSuffix(p, ":*")
	}
	if strings.Contains(base, "*") {
		return errors.New("only repo:* and repo@sha256:* patterns are allowed")
	}
	if _, err := workspace.NormalizeImage(base); err != nil {
		return err
	}
	return nil
}
