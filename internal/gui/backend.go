package gui

import (
	"context"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
)

// Backend is the narrow set of service methods the GUI calls (docs/M4.md 2.1). *service.Service
// satisfies it; it is also the seam an enterprise build wraps for RBAC or SSO. The GUI always
// passes service.SourceGUI and an auth.GUIAdmin principal.
//
// ResetGUIAdmin is deliberately absent: it is reachable from the admin socket (CLI) only.
type Backend interface {
	// Admin account, setup and sessions (no Source: the service uses SourceGUI itself).
	EnsureGUISetup(ctx context.Context) (token string, created bool, err error)
	ReissueGUISetup(ctx context.Context) (token string, created bool, err error)
	CompleteGUISetup(ctx context.Context, remote, ua, token, username, password string) (string, auth.Principal, error)
	GUILogin(ctx context.Context, remote, ua, username, password string) (string, auth.Principal, error)
	GUIAuthenticate(ctx context.Context, remote, sessionToken string, passive bool) (auth.Principal, int64, error)
	ChangeGUIPassword(ctx context.Context, p auth.Principal, sessionID int64, current, next string) (string, error)
	GUILogout(ctx context.Context, p auth.Principal, sessionID int64) error
	ListGUISessions(ctx context.Context, p auth.Principal) ([]store.GUISession, error)
	RevokeGUISession(ctx context.Context, p auth.Principal, id int64) error
	RevokeOtherGUISessions(ctx context.Context, p auth.Principal, keepID int64) (int, error)
	PurgeGUISessions(ctx context.Context) (int, error)
	GUISetupOpen(ctx context.Context) (bool, error)
	RecordGUILockout(ctx context.Context, remote string) error

	// Audit.
	ListAudit(ctx context.Context, p auth.Principal, src service.Source, f store.AuditFilter) ([]store.AuditEvent, error)
	RecordAuditExport(ctx context.Context, p auth.Principal, src service.Source, f store.AuditFilter, format string) error

	// Fleet guard toggle and the all-status runner list.
	SetFleetConfirm(ctx context.Context, p auth.Principal, src service.Source, fleet string, confirm bool) (store.Fleet, error)
	ListRunnersAll(ctx context.Context, p auth.Principal, src service.Source, fleet string, includeRevoked bool) ([]service.RunnerView, error)

	// Fleets.
	CreateFleet(ctx context.Context, p auth.Principal, src service.Source, name string, mode store.Mode, confirm bool, ws *store.WorkspaceConfig) (store.Fleet, error)
	ListFleets(ctx context.Context, p auth.Principal, src service.Source) ([]store.Fleet, error)
	GetFleet(ctx context.Context, p auth.Principal, src service.Source, name string) (store.Fleet, error)
	SetFleetWorkspace(ctx context.Context, p auth.Principal, src service.Source, fleet string, c store.WorkspaceConfig) (store.WorkspaceConfig, error)

	// Runners.
	ListRunners(ctx context.Context, p auth.Principal, src service.Source, fleet string) ([]service.RunnerView, error)
	Lifecycle(ctx context.Context, p auth.Principal, src service.Source, ref, op string) (store.Runner, error)

	// Join tokens.
	CreateToken(ctx context.Context, p auth.Principal, src service.Source, tp service.TokenParams) (service.CreatedToken, error)
	RevokeToken(ctx context.Context, p auth.Principal, src service.Source, tokenID string) error
	ListTokens(ctx context.Context, p auth.Principal, src service.Source, fleet string) ([]store.JoinToken, error)

	// Operators.
	ListOperators(ctx context.Context, p auth.Principal, src service.Source, includeRevoked bool) ([]store.Operator, error)
	RevokeOperator(ctx context.Context, p auth.Principal, src service.Source, name string) (store.Operator, error)
	RotateRegistrationSecret(ctx context.Context, p auth.Principal, src service.Source) error
	OperatorCode(ctx context.Context, p auth.Principal, src service.Source) (api.OperatorCodeResult, error)

	// Workspaces.
	AdminListWorkspaces(ctx context.Context, p auth.Principal, src service.Source, fleet string) ([]store.Workspace, error)
	KillWorkspace(ctx context.Context, p auth.Principal, src service.Source, wsID string) (store.Workspace, error)
}

var _ Backend = (*service.Service)(nil)
