// Package service is the single code path for CLI, api, GUI and MCP: it authorises through the
// policy, validates input, performs the store operation (whose audit row commits in the same
// transaction) and maps failures to api error codes. Nothing here calls time.Now(): the clock
// is injected.
package service

import (
	"context"
	"errors"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/central/registry"
	"github.com/mirastacklabs-ai/mirafleet/internal/policy"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
)

// Source says which front end a call came from; it is recorded in the audit row.
type Source string

const (
	SourceCLI    Source = "cli"
	SourceAPI    Source = "api"
	SourceGUI    Source = "gui"
	SourceEnroll Source = "enroll"
	SourceSystem Source = "system"
)

// Valid reports whether s is a known source.
func (s Source) Valid() bool {
	switch s {
	case SourceCLI, SourceAPI, SourceGUI, SourceEnroll, SourceSystem:
		return true
	}
	return false
}

// Audit outcomes written by this package.
const (
	OutcomeOK     = "ok"
	OutcomeDenied = "denied"
)

// Config holds the injected dependencies. All but Logf are required.
type Config struct {
	Store  store.Store
	Policy policy.PolicyEngine
	Live   registry.Live
	// HostKey is central's SSH host public key; join tokens pin its hash.
	HostKey ssh.PublicKey
	Now     func() time.Time
	// OperatorConns drops a revoked operator's live connections. Required.
	OperatorConns OperatorConns
	// Logf receives server-side diagnostics. It never receives secrets. Optional.
	Logf func(format string, args ...any)
	// MaxWorkspacesPerOperator caps live workspaces per operator; 0 means the default (8),
	// negative means no cap.
	MaxWorkspacesPerOperator int
	// WorkspaceOpenTimeout bounds a workspace create (image pull); 0 means 5 minutes.
	WorkspaceOpenTimeout time.Duration
	// ReaperInterval is the workspace reaper sweep period; 0 means 60 seconds.
	ReaperInterval time.Duration

	// Credentials; see credentials.go. All optional.
	Credentials CredentialsConfig
}

// Service implements the central operations. Safe for concurrent use.
type Service struct {
	st      store.Store
	pol     policy.PolicyEngine
	live    registry.Live
	hostKey ssh.PublicKey
	now     func() time.Time
	logf    func(format string, args ...any)
	opConns OperatorConns

	maxPerOp    int
	openTimeout time.Duration
	reapEvery   time.Duration
	recon       sync.Map      // runner ids with a reconcile in flight
	openMargin  time.Duration // slack on top of openTimeout before reconcile destroys an opening row

	mu        sync.Mutex     // guards startedAt and inflightOps
	startedAt time.Time      // when this central process started its workspace housekeeping
	inflight  map[string]int // in-flight ops per workspace id (exec, read, write)

	cred *credState
}

// New validates cfg and returns a Service.
func New(cfg Config) (*Service, error) {
	switch {
	case cfg.Store == nil:
		return nil, errors.New("service: Store required")
	case cfg.Policy == nil:
		return nil, errors.New("service: Policy required")
	case cfg.Live == nil:
		return nil, errors.New("service: Live required")
	case cfg.HostKey == nil:
		return nil, errors.New("service: HostKey required")
	case cfg.Now == nil:
		return nil, errors.New("service: Now required")
	case cfg.OperatorConns == nil:
		return nil, errors.New("service: OperatorConns required")
	}
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	maxPerOp, openTimeout := cfg.MaxWorkspacesPerOperator, cfg.WorkspaceOpenTimeout
	if maxPerOp == 0 {
		maxPerOp = DefaultMaxWorkspacesPerOperator
	}
	if openTimeout <= 0 {
		openTimeout = DefaultWorkspaceOpenTimeout
	}
	reapEvery := cfg.ReaperInterval
	if reapEvery <= 0 {
		reapEvery = DefaultReaperInterval
	}
	s := &Service{st: cfg.Store, pol: cfg.Policy, live: cfg.Live, hostKey: cfg.HostKey, now: cfg.Now, logf: logf,
		opConns: cfg.OperatorConns, maxPerOp: maxPerOp, openTimeout: openTimeout, reapEvery: reapEvery, openMargin: DefaultOpenMargin}
	if err := s.initCredentials(cfg.Credentials); err != nil {
		return nil, err
	}
	return s, nil
}

var errForbidden = api.Errorf(api.CodeForbidden, "forbidden")

func badRequest(format string, args ...any) *api.Error {
	return api.Errorf(api.CodeBadRequest, format, args...)
}

// ToAPIError maps any error to a wire error. Messages are fixed per code, so no internal detail
// (SQL, paths, secrets) can leak. nil stays nil. store.ErrStale maps to a generic conflict: the
// enrol paths (Join, Register) never use ToAPIError; they return their own identical refusal.
func ToAPIError(err error) *api.Error {
	if err == nil {
		return nil
	}
	var ae *api.Error
	switch {
	case errors.As(err, &ae):
		return ae
	case errors.Is(err, policy.ErrForbidden), errors.Is(err, auth.ErrDenied):
		return errForbidden
	case errors.Is(err, store.ErrNotFound):
		return api.Errorf(api.CodeNotFound, "not found")
	case errors.Is(err, store.ErrConflict):
		return api.Errorf(api.CodeConflict, "conflict")
	case errors.Is(err, store.ErrStale):
		return api.Errorf(api.CodeConflict, "conflict")
	case errors.Is(err, store.ErrInvalid):
		return api.Errorf(api.CodeBadRequest, "invalid input")
	}
	return api.Internal(err)
}

// fail logs unexpected errors server-side and returns the wire error.
func (s *Service) fail(op string, err error) error {
	ae := ToAPIError(err)
	if ae.Code == api.CodeInternal {
		s.logf("service: %s: %v", op, err)
	}
	return ae
}

// event builds an audit event for p at the current time.
func (s *Service) event(p auth.Principal, src Source, action string) store.AuditEvent {
	return store.AuditEvent{
		Time:      s.now(),
		Source:    string(src),
		ActorKind: string(p.Kind),
		Actor:     p.Name,
		Remote:    p.Remote,
		Action:    action,
		Outcome:   OutcomeOK,
	}
}

// authorize checks the policy (loading an operator's current status first). If adminOnly, it
// also requires p.Kind == admin directly, independent of the policy (double gate). A denial is
// audited with outcome "denied" and returned as a forbidden api error.
func (s *Service) authorize(ctx context.Context, p auth.Principal, src Source, action string, adminOnly bool) error {
	if !src.Valid() {
		return badRequest("invalid source")
	}
	res := policy.Resource{}
	if p.Kind == auth.KindOperator {
		if op, err := s.st.GetOperatorByID(ctx, p.ID); err == nil {
			res.ActorStatus = string(op.Status)
		} else if !errors.Is(err, store.ErrNotFound) {
			return s.fail("load operator", err)
		}
	}
	denied := s.pol.Authorize(ctx, p, action, res) != nil
	if adminOnly && p.Kind != auth.KindAdmin {
		denied = true
	}
	if !denied {
		return nil
	}
	return s.deny(ctx, p, src, action)
}

// deny audits a denial of action and returns the forbidden error.
func (s *Service) deny(ctx context.Context, p auth.Principal, src Source, action string) error {
	ev := s.event(p, src, action)
	ev.Outcome = OutcomeDenied
	if _, err := s.st.RecordAudit(ctx, ev); err != nil {
		s.logf("service: audit denial of %s failed: %v", action, err)
	}
	return errForbidden
}
