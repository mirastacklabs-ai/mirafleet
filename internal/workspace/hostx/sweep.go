//go:build linux

package hostx

import (
	"context"
	"time"
)

// SweepReport is the outcome of one sweep.
type SweepReport struct {
	Removed []Info // deleted (own directories) or forgotten (workdir records)
	Warned  []Info // inside the warning window, not yet deleted
	Errors  []error
}

// Sweep removes sessions unused for DeleteAfter and reports those about to expire. Sessions with
// a live handle, a live holder process or a running job are never touched. Own directories are
// deleted with their record; a workdir record is forgotten and the checkout left alone.
func (m *Manager) Sweep() SweepReport {
	m.mu.Lock()
	defer m.mu.Unlock()
	var rep SweepReport
	if m.cfg.DeleteAfter < 0 {
		return rep
	}
	infos, err := m.list(false)
	if err != nil {
		rep.Errors = append(rep.Errors, err)
		return rep
	}
	now := m.cfg.Now()
	for _, in := range infos {
		if in.Busy {
			continue
		}
		if now.Sub(in.LastUsed) >= m.cfg.DeleteAfter {
			mt, err := m.load(in.Slug)
			if err != nil {
				rep.Errors = append(rep.Errors, err)
				continue
			}
			if err := m.remove(in.Slug, mt); err != nil {
				rep.Errors = append(rep.Errors, err)
				continue
			}
			rep.Removed = append(rep.Removed, in)
		} else if in.Own && in.Warn {
			rep.Warned = append(rep.Warned, in)
		}
	}
	return rep
}

// RunSweeper sweeps once at start and then every interval (default daily) until ctx ends.
// report, if set, receives every report that removed or warned about something.
func (m *Manager) RunSweeper(ctx context.Context, interval time.Duration, report func(SweepReport)) {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	run := func() {
		r := m.Sweep()
		if report != nil && (len(r.Removed)+len(r.Warned)+len(r.Errors) > 0) {
			report(r)
		}
	}
	run()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}

// PruneOptions select what Prune removes.
type PruneOptions struct {
	OlderThan time.Duration // unused at least this long; 0 selects the configured DeleteAfter
	Yes       bool          // delete; false is a dry run
}

// PruneReport lists the sessions that matched; Removed is empty on a dry run.
type PruneReport struct {
	Candidates []Info
	Removed    []Info
	Skipped    []Info // matched but in use
	Errors     []error
}

// Prune is the core of `mirafleet runner host prune`: own session directories unused for
// OlderThan. In use sessions are skipped and reported. A workdir is never a candidate.
func (m *Manager) Prune(o PruneOptions) PruneReport {
	m.mu.Lock()
	defer m.mu.Unlock()
	var rep PruneReport
	if o.OlderThan <= 0 {
		o.OlderThan = m.cfg.DeleteAfter
		if o.OlderThan <= 0 {
			o.OlderThan = DefaultDeleteAfter
		}
	}
	infos, err := m.list(true)
	if err != nil {
		rep.Errors = append(rep.Errors, err)
		return rep
	}
	now := m.cfg.Now()
	for _, in := range infos {
		if !in.Own || now.Sub(in.LastUsed) < o.OlderThan {
			continue
		}
		if in.Busy {
			rep.Skipped = append(rep.Skipped, in)
			continue
		}
		rep.Candidates = append(rep.Candidates, in)
		if !o.Yes {
			continue
		}
		mt, err := m.load(in.Slug)
		if err == nil {
			err = m.remove(in.Slug, mt)
		}
		if err != nil {
			rep.Errors = append(rep.Errors, err)
			continue
		}
		rep.Removed = append(rep.Removed, in)
	}
	return rep
}
