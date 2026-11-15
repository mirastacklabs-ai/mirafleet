package service

// Runner event ingestion (docs/M7.md 4.5): the runner sends aggregated credential-use events on
// the events@mirafleet channel; central validates the batch and writes one audit row per event
// with source "runner". The row carries names, hosts, status and counts, never a secret value.

import (
	"context"
	"strconv"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
)

// Audit actions and source of ingested runner events.
const (
	SourceRunner        = "runner"
	ActionCredentialUse = transport.EventCredentialUse
	ActionEventsDropped = "events.dropped"
)

var eventOutcome = map[string]string{
	transport.EventOK:      OutcomeOK,
	transport.EventDenied:  OutcomeDenied,
	transport.EventError:   OutcomeError,
	transport.EventExpired: OutcomeError,
}

// IngestRunnerEvents records a batch sent by runnerID. A revoked runner's batch is refused.
// Events become audit rows (fleet and runner from the store, never from the batch); Dropped > 0
// becomes one events.dropped row. The first row that cannot be written fails the call.
func (s *Service) IngestRunnerEvents(ctx context.Context, runnerID int64, b *transport.EventBatch) error {
	if b == nil {
		return badRequest("empty batch")
	}
	if err := b.Validate(); err != nil {
		return badRequest("invalid event batch")
	}
	r, err := s.st.GetRunnerByID(ctx, runnerID)
	if err != nil {
		return s.fail("get runner", err)
	}
	if r.Status == store.StatusRevoked {
		return api.Errorf(api.CodeForbidden, "forbidden")
	}
	base := store.AuditEvent{Source: SourceRunner, ActorKind: "runner", Actor: r.Name, Fleet: r.Fleet, Runner: r.Name}
	for _, e := range b.Events {
		ev := base
		ev.Time = s.now()
		ev.Action = ActionCredentialUse
		ev.Subject = e.Credential
		ev.Outcome = eventOutcome[e.Status]
		ev.Message = e.Status
		ev.Params = map[string]string{"session": e.Session, "credential": e.Credential, "host": e.Host,
			"status": e.Status, "count": strconv.FormatInt(e.Count, 10),
			"window_ms": strconv.FormatInt(e.WindowMs, 10), "window_s": strconv.Itoa(e.WindowS)}
		if workspace.ValidID(workspace.ID(e.Session)) {
			ev.Workspace = e.Session
		}
		if err := s.recordRunnerEvent(ctx, ev); err != nil {
			return err
		}
	}
	if b.Dropped > 0 {
		ev := base
		ev.Time = s.now()
		ev.Action = ActionEventsDropped
		ev.Outcome = OutcomeError
		ev.Message = "runner dropped events"
		ev.Params = map[string]string{"dropped": strconv.FormatInt(b.Dropped, 10)}
		if err := s.recordRunnerEvent(ctx, ev); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) recordRunnerEvent(ctx context.Context, ev store.AuditEvent) error {
	if _, err := s.st.RecordAudit(ctx, ev); err != nil {
		s.logf("service: audit of runner event %s failed: %v", ev.Action, err)
		return api.Errorf(api.CodeUnavailable, "audit unavailable")
	}
	return nil
}
