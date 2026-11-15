package service

import (
	"context"
	"strconv"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
)

// ActionAuditExport is the audit action written before an audit export streams.
const ActionAuditExport = "audit.export"

// ListAudit returns audit rows matching f. Admin only (double gate). Reads are not audited. A
// negative bound is bad_request; the store applies the default (100) and the cap (1000) to
// f.Limit.
func (s *Service) ListAudit(ctx context.Context, p auth.Principal, src Source, f store.AuditFilter) ([]store.AuditEvent, error) {
	if err := s.authorize(ctx, p, src, api.MethodAuditList, true); err != nil {
		return nil, err
	}
	if err := checkAuditBounds(f); err != nil {
		return nil, err
	}
	evs, err := s.st.QueryAudit(ctx, f)
	if err != nil {
		return nil, s.fail("query audit", err)
	}
	return evs, nil
}

// RecordAuditExport writes the audit.export row (filter and format as params) that must precede
// an export. Admin only. If it fails the caller must not stream.
func (s *Service) RecordAuditExport(ctx context.Context, p auth.Principal, src Source, f store.AuditFilter, format string) error {
	if err := s.authorize(ctx, p, src, ActionAuditExport, true); err != nil {
		return err
	}
	if err := checkAuditBounds(f); err != nil {
		return err
	}
	if format != "csv" && format != "jsonl" {
		return badRequest("format must be csv or jsonl")
	}
	for _, v := range []string{f.Fleet, f.Runner, f.Actor, f.Source, f.Outcome, f.Workspace, f.RequestID, f.Action} {
		if len(v) > maxAuditFilterString {
			return badRequest("audit filter value too long")
		}
	}
	ev := s.event(p, src, ActionAuditExport)
	ev.Params = filterParams(f)
	ev.Params["format"] = format
	if _, err := s.st.RecordAudit(ctx, ev); err != nil {
		return s.fail("audit export", err)
	}
	return nil
}

// maxAuditFilterString caps each text filter recorded in the audit.export row.
const maxAuditFilterString = 256

func checkAuditBounds(f store.AuditFilter) error {
	if f.AfterID < 0 || f.BeforeID < 0 || f.Limit < 0 {
		return badRequest("since_id, before_id and limit must be >= 0")
	}
	return nil
}

// filterParams renders the set fields of f as audit params.
func filterParams(f store.AuditFilter) map[string]string {
	m := map[string]string{}
	put := func(k, v string) {
		if v != "" {
			m[k] = v
		}
	}
	put("fleet", f.Fleet)
	put("runner", f.Runner)
	put("actor", f.Actor)
	put("source", f.Source)
	put("outcome", f.Outcome)
	put("workspace", f.Workspace)
	put("request_id", f.RequestID)
	put("action", f.Action)
	if f.ActionPrefix {
		m["action_prefix"] = "true"
	}
	if !f.From.IsZero() {
		m["from"] = f.From.UTC().Format(time.RFC3339)
	}
	if !f.To.IsZero() {
		m["to"] = f.To.UTC().Format(time.RFC3339)
	}
	if f.AfterID > 0 {
		m["after_id"] = strconv.FormatInt(f.AfterID, 10)
	}
	if f.BeforeID > 0 {
		m["before_id"] = strconv.FormatInt(f.BeforeID, 10)
	}
	return m
}
