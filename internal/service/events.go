package service

// Runner event ingestion: the runner sends aggregated credential-use events on
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
	// H-03: a runner may only report credentials it holds; others are dropped, not audited.
	held, err := s.heldNames(ctx, runnerID)
	if err != nil {
		return err
	}
	for _, e := range b.Events {
		if !held[e.Credential] {
			continue
		}
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

// ActionRunnerReplaced is the audit action written when a runner's new connection replaces its
// live one (the only mitigation of the accepted enrolment-key gap).
const ActionRunnerReplaced = "runner.replaced"

// RunnerReplaced writes the runner.replaced audit row. Best effort: failures are logged.
func (s *Service) RunnerReplaced(ctx context.Context, runnerID int64) {
	r, err := s.st.GetRunnerByID(ctx, runnerID)
	if err != nil {
		s.logf("service: runner.replaced for %d: %v", runnerID, err)
		return
	}
	ev := store.AuditEvent{Time: s.now(), Source: SourceRunner, ActorKind: "runner", Actor: r.Name, Fleet: r.Fleet,
		Runner: r.Name, Action: ActionRunnerReplaced, Outcome: OutcomeOK, Subject: r.Name,
		Message: "a new connection replaced the live one"}
	_ = s.recordRunnerEvent(ctx, ev)
}

// ActionRunnerRefusedVersion is the audit action written when central refuses a runner for
// protocol version skew.
const ActionRunnerRefusedVersion = "runner.refused_version"

// RunnerRefusedVersion writes the runner.refused_version audit row; msg is the refusal text sent
// to the runner. Best effort: failures are logged.
func (s *Service) RunnerRefusedVersion(ctx context.Context, runnerID int64, msg string) {
	r, err := s.st.GetRunnerByID(ctx, runnerID)
	if err != nil {
		s.logf("service: runner.refused_version for %d: %v", runnerID, err)
		return
	}
	ev := store.AuditEvent{Time: s.now(), Source: SourceRunner, ActorKind: "runner", Actor: r.Name, Fleet: r.Fleet,
		Runner: r.Name, Action: ActionRunnerRefusedVersion, Outcome: OutcomeDenied, Subject: r.Name, Message: msg}
	_ = s.recordRunnerEvent(ctx, ev)
}

func (s *Service) recordRunnerEvent(ctx context.Context, ev store.AuditEvent) error {
	if _, err := s.st.RecordAudit(ctx, ev); err != nil {
		s.logf("service: audit of runner event %s failed: %v", ev.Action, err)
		return api.Errorf(api.CodeUnavailable, "audit unavailable")
	}
	return nil
}
