package service

import (
	"context"
	"strconv"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
)

// CreateFleet creates a fleet. Admin only. An empty mode means "actions". ws is required for
// mode workspace and must be nil otherwise; the store validates it (ErrInvalid).
func (s *Service) CreateFleet(ctx context.Context, p auth.Principal, src Source, name string, mode store.Mode, confirm bool, ws *store.WorkspaceConfig) (store.Fleet, error) {
	if err := s.authorize(ctx, p, src, api.MethodFleetCreate, true); err != nil {
		return store.Fleet{}, err
	}
	if mode == "" {
		mode = store.ModeActions
	}
	if !store.ValidFleetName(name) {
		return store.Fleet{}, badRequest("invalid fleet name")
	}
	if !mode.Valid() {
		return store.Fleet{}, badRequest("invalid fleet mode")
	}
	if mode == store.ModeWorkspace {
		// Omitted (zero) fields take the documented defaults; the default image stays required.
		var c store.WorkspaceConfig
		if ws != nil {
			c = *ws
		}
		c = WorkspaceDefaults(c)
		if err := c.Validate(); err != nil {
			return store.Fleet{}, configInvalid(err)
		}
		ws = &c
	}
	now := s.now()
	ev := s.event(p, src, api.MethodFleetCreate)
	ev.Fleet = name
	ev.Subject = name
	ev.Params = map[string]string{"mode": string(mode), "confirm": strconv.FormatBool(confirm)}
	f, err := s.st.CreateFleet(ctx, store.Fleet{Name: name, Mode: mode, Confirm: confirm, Workspace: ws}, now, ev)
	if err != nil {
		return store.Fleet{}, s.fail("create fleet", err)
	}
	return f, nil
}

// ListFleets lists fleets with non-revoked runner counts. Allowed for admins and active
// operators (policy action fleets.list).
func (s *Service) ListFleets(ctx context.Context, p auth.Principal, src Source) ([]store.Fleet, error) {
	if err := s.authorize(ctx, p, src, api.MethodFleetsList, false); err != nil {
		return nil, err
	}
	fs, err := s.st.ListFleets(ctx)
	if err != nil {
		return nil, s.fail("list fleets", err)
	}
	return fs, nil
}

// GetFleet returns one fleet by name (policy action fleets.list).
func (s *Service) GetFleet(ctx context.Context, p auth.Principal, src Source, name string) (store.Fleet, error) {
	if err := s.authorize(ctx, p, src, api.MethodFleetsList, false); err != nil {
		return store.Fleet{}, err
	}
	if !store.ValidFleetName(name) {
		return store.Fleet{}, badRequest("invalid fleet name")
	}
	f, err := s.st.GetFleet(ctx, name)
	if err != nil {
		return store.Fleet{}, s.fail("get fleet", err)
	}
	return f, nil
}
