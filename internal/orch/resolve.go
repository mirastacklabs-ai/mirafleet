package orch

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/handoff"
	"github.com/mirastacklabs-ai/mirafleet/internal/opconf"
)

// Source labels of Target.ContextSource and Target.FleetSource.
const (
	SrcArgument   = "argument"
	SrcFlag       = "flag"
	SrcProject    = "project file"
	SrcCurrent    = "current context"
	SrcOnly       = "only context"
	SrcCtxDefault = "context default"
	SrcNone       = "none"
)

// Target is the resolved context and fleet of a call, with where each came from. Fleet may be
// empty (FleetSource is then SrcNone).
type Target struct {
	Context       string
	Fleet         string
	ContextSource string
	FleetSource   string
}

type guardEntry struct {
	at     time.Time
	fleets map[string]bool // name -> guarded
}

// Resolve applies the precedence tool argument > flags > .mirafleet.yaml > context default_fleet
// > none (docs/M5.md §1.5). contexts.yaml and the project file are re-read on every call. A
// guarded fleet taken from the project file or a context default is refused; it must be named
// explicitly (argument or flag). Guarded status comes from fleets.list (cached GuardedTTL).
func (o *Orch) Resolve(ctx context.Context, contextArg, fleetArg string) (Target, error) {
	cs, err := o.loadContexts()
	if err != nil {
		return Target{}, err
	}
	var proj opconf.Project
	if o.cfg.ProjectDir != "" {
		p, _, perr := opconf.LoadProject(o.cfg.ProjectDir, o.cfg.Home)
		if perr != nil {
			o.cfg.Logf("project file: %v", perr)
			return Target{}, errf(api.CodeBadRequest, nil, "the project file .mirafleet.yaml is invalid")
		}
		proj = p
	}

	var t Target
	switch {
	case contextArg != "":
		t.Context, t.ContextSource = contextArg, SrcArgument
	case o.cfg.Context != "":
		t.Context, t.ContextSource = o.cfg.Context, SrcFlag
	case proj.Context != "":
		t.Context, t.ContextSource = proj.Context, SrcProject
	case cs.Current != "":
		t.Context, t.ContextSource = cs.Current, SrcCurrent
	case len(cs.Contexts) == 1:
		t.Context, t.ContextSource = cs.Contexts[0].Name, SrcOnly
	default:
		return Target{}, errf(api.CodeBadRequest, nil,
			fmt.Sprintf("no context selected: pass `context` (one of %s) or run `mirafleet mcp context use NAME`", clean(strings.Join(cs.Names(), ", "))))
	}
	c, ok := cs.Find(t.Context)
	if !ok {
		return Target{}, errf(api.CodeNotFound,
			nil, fmt.Sprintf("unknown context %q (known: %s)", clean(t.Context), clean(strings.Join(cs.Names(), ", "))))
	}

	switch {
	case fleetArg != "":
		t.Fleet, t.FleetSource = fleetArg, SrcArgument
	case o.cfg.Fleet != "":
		t.Fleet, t.FleetSource = o.cfg.Fleet, SrcFlag
	case proj.Fleet != "":
		t.Fleet, t.FleetSource = proj.Fleet, SrcProject
	case c.DefaultFleet != "":
		t.Fleet, t.FleetSource = c.DefaultFleet, SrcCtxDefault
	default:
		t.FleetSource = SrcNone
	}
	if t.FleetSource == SrcProject || t.FleetSource == SrcCtxDefault {
		g, gerr := o.isGuarded(ctx, t.Context, c.Addr, t.Fleet)
		if gerr != nil {
			return Target{}, gerr
		}
		if g {
			return Target{}, errf(api.CodeBadRequest, ErrGuardedDefault, fmt.Sprintf(
				"fleet %q is guarded and was taken from the %s: name it explicitly (and ask the user before confirming)",
				clean(t.Fleet), t.FleetSource))
		}
	}
	return t, nil
}

// isGuarded reports whether fleet is guarded on the context. Unknown fleets are not guarded
// (central reports not_found itself). A failing fleets.list fails the resolution: a guarded
// fleet must never be used by default because central was unreachable.
func (o *Orch) isGuarded(ctx context.Context, cname, addr, fleet string) (bool, error) {
	key := guardKey(cname, addr) // a re-login to another central (new addr) never reuses the entry
	o.mu.Lock()
	e, ok := o.guarded[key]
	o.mu.Unlock()
	if ok && o.cfg.Now().Sub(e.at) <= GuardedTTL {
		return e.fleets[fleet], nil
	}
	e, err := o.refreshGuarded(ctx, cname, key)
	if err != nil {
		return false, err
	}
	return e.fleets[fleet], nil
}

func guardKey(cname, addr string) string { return cname + "\x00" + addr }

// guardFlight is one fleets.list refresh shared by every concurrent caller of the same key.
type guardFlight struct {
	done chan struct{}
	e    guardEntry
	err  error
}

// refreshGuarded runs one fleets.list per key at a time. The refresh runs detached from any one
// caller's context (bounded by the call's own time limit), so one caller giving up does not fail
// the others.
func (o *Orch) refreshGuarded(ctx context.Context, cname, key string) (guardEntry, error) {
	o.mu.Lock()
	f, ok := o.guardFlights[key]
	if !ok {
		if o.closed {
			o.mu.Unlock()
			return guardEntry{}, errClosed
		}
		f = &guardFlight{done: make(chan struct{})}
		o.guardFlights[key] = f
		o.wg.Add(1)
		start := o.cfg.Now()
		go func() {
			defer o.wg.Done()
			_, err := o.FleetsList(context.WithoutCancel(ctx), cname) // refreshes the cache
			o.mu.Lock()
			e, have := o.guarded[key]
			// The context changed under us (re-login: FleetsList wrote under the new address, so an
			// entry under this key is an older one): never read that as "not guarded".
			if err == nil && (!have || e.at.Before(start)) {
				err = errRetired
			}
			f.e, f.err = e, err
			delete(o.guardFlights, key)
			o.mu.Unlock()
			close(f.done)
		}()
	}
	o.mu.Unlock()
	select {
	case <-f.done:
		return f.e, f.err
	case <-ctx.Done():
		return guardEntry{}, o.mapErr(ctx, ctx.Err())
	}
}

func (o *Orch) noteFleets(cname, addr string, r *api.FleetsListResult) {
	m := make(map[string]bool, len(r.Fleets))
	for _, f := range r.Fleets {
		m[f.Name] = f.Confirm
	}
	o.mu.Lock()
	o.guarded[guardKey(cname, addr)] = guardEntry{at: o.cfg.Now(), fleets: m}
	o.mu.Unlock()
}

// SessionFor returns the workspace session id for t and where it came from ("flag", "handoff" or
// "process"). It is re-read at every call (docs/M5.md §1.4), so a hook that writes a new handoff
// file (after /clear) binds a new workspace on the next open. Every value passed
// api.ValidateSession.
func (o *Orch) SessionFor(t Target) (session, source string) {
	if s := o.cfg.Session; s != "" && api.ValidateSession(s) == nil {
		return s, "flag"
	}
	if o.cfg.StateDir != "" && o.cfg.PPID != 0 {
		k := handoff.Key{PID: o.cfg.PPID, Project: o.cfg.ProjectDir}
		if s, ok, err := handoff.Read(o.cfg.StateDir, k); err == nil && ok && api.ValidateSession(s) == nil {
			return s, "handoff"
		} else if err != nil {
			o.cfg.Logf("handoff: %v", err)
		}
	}
	return o.procSession, "process"
}
