// Package policy decides whether a principal may perform an action. Deny by default.
package policy

import (
	"context"
	"errors"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
)

// ErrForbidden is returned when the policy denies an action.
var ErrForbidden = errors.New("policy: forbidden")

// Resource describes what an action touches. The policy cannot look anything up: the caller
// loads current state from the store and fills it in.
type Resource struct {
	Fleet  string
	Runner string
	// ActorStatus is the principal's CURRENT status, loaded by the caller (not the status
	// captured at SSH authentication time). Operators need "active"; empty means unknown
	// and is denied.
	ActorStatus string
}

// PolicyEngine authorizes actions. Enterprise builds can replace it (RBAC seam).
type PolicyEngine interface {
	Authorize(ctx context.Context, p auth.Principal, action string, res Resource) error
}

// TablePolicy is the OSS policy: a fixed table by principal kind.
type TablePolicy struct{}

// Authorize implements PolicyEngine.
func (TablePolicy) Authorize(_ context.Context, p auth.Principal, action string, res Resource) error {
	switch p.Kind {
	case auth.KindAdmin:
		return nil
	case auth.KindOperator:
		if res.ActorStatus != string(store.OperatorActive) {
			return ErrForbidden
		}
		switch action {
		case api.MethodHello, api.MethodFleetsList, api.MethodRunnersList, api.MethodActionRun:
			return nil
		}
	case auth.KindEnroll:
		if api.IsEnrollMethod(action) {
			return nil
		}
	}
	return ErrForbidden
}
