package orch

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/handoff"
	"github.com/mirastacklabs-ai/mirafleet/internal/opconf"
)

func testFleets() func(context.Context, string, any) error {
	return fleetsHandler(
		api.FleetInfo{Name: "dev"}, api.FleetInfo{Name: "prod", Confirm: true},
		api.FleetInfo{Name: "ctxdef"}, api.FleetInfo{Name: "projf"},
	)
}

func TestResolvePrecedence(t *testing.T) {
	ctxs := []opconf.Context{
		{Name: "a", Addr: "127.0.0.1:1", DefaultFleet: "ctxdef"},
		{Name: "b", Addr: "127.0.0.1:2", DefaultFleet: "prod"},
		{Name: "c", Addr: "127.0.0.1:3"},
	}
	cases := []struct {
		name               string
		ctxArg, fleetArg   string
		flagCtx, flagFleet string
		project            string
		wantCtx, wantFleet string
		wantCS, wantFS     string
		wantErr            string
		guarded            bool
	}{
		{name: "argument beats everything", ctxArg: "c", fleetArg: "dev", flagCtx: "b", flagFleet: "prod",
			project: "fleet: projf\ncontext: a\n", wantCtx: "c", wantFleet: "dev", wantCS: SrcArgument, wantFS: SrcArgument},
		{name: "flag beats project and default", flagCtx: "b", flagFleet: "dev", project: "fleet: projf\ncontext: a\n",
			wantCtx: "b", wantFleet: "dev", wantCS: SrcFlag, wantFS: SrcFlag},
		{name: "project beats context default", project: "fleet: projf\ncontext: a\n",
			wantCtx: "a", wantFleet: "projf", wantCS: SrcProject, wantFS: SrcProject},
		{name: "context default", ctxArg: "a", wantCtx: "a", wantFleet: "ctxdef", wantCS: SrcArgument, wantFS: SrcCtxDefault},
		{name: "none", ctxArg: "c", wantCtx: "c", wantFleet: "", wantCS: SrcArgument, wantFS: SrcNone},
		{name: "ambiguous context", wantErr: "no context selected"},
		{name: "unknown context", ctxArg: "zz", wantErr: "unknown context"},
		{name: "guarded project default refused", ctxArg: "a", project: "fleet: prod\n", wantErr: "guarded", guarded: true},
		{name: "guarded context default refused", ctxArg: "b", wantErr: "guarded", guarded: true},
		{name: "guarded fleet by argument allowed", ctxArg: "b", fleetArg: "prod", wantCtx: "b", wantFleet: "prod", wantCS: SrcArgument, wantFS: SrcArgument},
		{name: "guarded fleet by flag allowed", ctxArg: "b", flagFleet: "prod", wantCtx: "b", wantFleet: "prod", wantCS: SrcArgument, wantFS: SrcFlag},
		{name: "unknown default fleet is not guarded", ctxArg: "a", project: "fleet: nosuch\n", wantCtx: "a", wantFleet: "nosuch", wantCS: SrcArgument, wantFS: SrcProject},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := setup(t, ctxs...)
			d := &dialer{mk: func(int) (Conn, error) { return &fakeConn{handler: testFleets()}, nil }}
			o := newOrch(t, dir, d, func(c *Config) { c.Context, c.Fleet = tc.flagCtx, tc.flagFleet })
			if tc.project != "" {
				writeProject(t, o.cfg.ProjectDir, tc.project)
			}
			got, err := o.Resolve(context.Background(), tc.ctxArg, tc.fleetArg)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				if tc.guarded && !errors.Is(err, ErrGuardedDefault) {
					t.Fatalf("err %v is not ErrGuardedDefault", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := Target{tc.wantCtx, tc.wantFleet, tc.wantCS, tc.wantFS}
			if got != want {
				t.Fatalf("got %+v want %+v", got, want)
			}
		})
	}
}

func TestResolveContextSources(t *testing.T) {
	dir := setup(t, opconf.Context{Name: "only", Addr: "127.0.0.1:1"})
	o := newOrch(t, dir, &dialer{mk: func(int) (Conn, error) { return &fakeConn{}, nil }})
	if got, err := o.Resolve(context.Background(), "", ""); err != nil || got.ContextSource != SrcOnly {
		t.Fatalf("got %+v %v", got, err)
	}
	// With two contexts and a current one, current wins; the project file beats current.
	dir = setup(t, opconf.Context{Name: "x", Addr: "127.0.0.1:1"}, opconf.Context{Name: "y", Addr: "127.0.0.1:2"})
	if err := opconf.UseContext(dir, "y"); err != nil {
		t.Fatal(err)
	}
	o = newOrch(t, dir, &dialer{mk: func(int) (Conn, error) { return &fakeConn{}, nil }})
	if got, _ := o.Resolve(context.Background(), "", ""); got.Context != "y" || got.ContextSource != SrcCurrent {
		t.Fatalf("got %+v", got)
	}
	writeProject(t, o.cfg.ProjectDir, "context: x\n")
	if got, _ := o.Resolve(context.Background(), "", ""); got.Context != "x" || got.ContextSource != SrcProject {
		t.Fatalf("got %+v", got)
	}
	// A hostile project file is an error with no path in it.
	writeProject(t, o.cfg.ProjectDir, "addr: evil:22\n")
	_, err := o.Resolve(context.Background(), "", "")
	if err == nil || strings.Contains(err.Error(), o.cfg.ProjectDir) {
		t.Fatalf("err = %v", err)
	}
}

func TestGuardedCacheTTLAndFailClosed(t *testing.T) {
	dir := setup(t, opconf.Context{Name: "a", Addr: "127.0.0.1:1", DefaultFleet: "dev"})
	var fc *fakeConn
	d := &dialer{mk: func(int) (Conn, error) { fc = &fakeConn{handler: testFleets()}; return fc, nil }}
	now := time.Unix(1_700_000_000, 0)
	o := newOrch(t, dir, d, func(c *Config) { c.Now = func() time.Time { return now } })
	count := func() int {
		n := 0
		for _, m := range fc.methods() {
			if m == api.MethodFleetsList {
				n++
			}
		}
		return n
	}
	for i := 0; i < 3; i++ {
		if _, err := o.Resolve(context.Background(), "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if count() != 1 {
		t.Fatalf("fleets.list called %d times within the TTL, want 1", count())
	}
	now = now.Add(GuardedTTL + time.Second)
	if _, err := o.Resolve(context.Background(), "", ""); err != nil {
		t.Fatal(err)
	}
	if count() != 2 {
		t.Fatalf("fleets.list called %d times after the TTL, want 2", count())
	}

	// Central unreachable: a defaulted fleet cannot be verified, so it is refused.
	dir = setup(t, opconf.Context{Name: "a", Addr: "127.0.0.1:1", DefaultFleet: "dev"})
	o2 := newOrch(t, dir, &dialer{mk: func(int) (Conn, error) { return nil, errors.New("boom /secret/path") }})
	if _, err := o2.Resolve(context.Background(), "", ""); err == nil || strings.Contains(err.Error(), "/secret") {
		t.Fatalf("err = %v", err)
	}
	// An explicit fleet needs no check.
	if got, err := o2.Resolve(context.Background(), "", "dev"); err != nil || got.Fleet != "dev" {
		t.Fatalf("got %+v %v", got, err)
	}
}

var procSessionRE = regexp.MustCompile(`^p:[0-9a-f]{32}$`)

func TestSessionOrder(t *testing.T) {
	dir := setup(t, opconf.Context{Name: "a", Addr: "127.0.0.1:1"})
	d := &dialer{mk: func(int) (Conn, error) { return &fakeConn{}, nil }}
	o := newOrch(t, dir, d)
	tgt := Target{Context: "a"}

	s1, src := o.SessionFor(tgt)
	if src != "process" || !procSessionRE.MatchString(s1) || api.ValidateSession(s1) != nil {
		t.Fatalf("got %q %q", s1, src)
	}
	if s2, _ := o.SessionFor(tgt); s2 != s1 {
		t.Fatal("process session must be stable")
	}
	o2 := newOrch(t, dir, d)
	if s3, _ := o2.SessionFor(tgt); s3 == s1 {
		t.Fatal("process sessions must differ between processes")
	}

	// Handoff beats process, and is re-read per call (/clear writes a new one).
	k := handoff.Key{PID: o.cfg.PPID, Project: o.cfg.ProjectDir}
	now := time.Now()
	if err := handoff.Write(o.cfg.StateDir, k, "cc:one", now); err != nil {
		t.Fatal(err)
	}
	if s, src := o.SessionFor(tgt); s != "cc:one" || src != "handoff" {
		t.Fatalf("got %q %q", s, src)
	}
	if err := handoff.Write(o.cfg.StateDir, k, "cc:two", now); err != nil {
		t.Fatal(err)
	}
	if s, _ := o.SessionFor(tgt); s != "cc:two" {
		t.Fatalf("got %q, want the new handoff", s)
	}
	// A handoff for another parent PID is not ours.
	other := newOrch(t, dir, d, func(c *Config) { c.StateDir, c.ProjectDir, c.PPID = o.cfg.StateDir, o.cfg.ProjectDir, 7 })
	if _, src := other.SessionFor(tgt); src != "process" {
		t.Fatalf("src = %q", src)
	}
	// Removing the handoff falls back to the process session.
	if err := handoff.Remove(o.cfg.StateDir, k); err != nil {
		t.Fatal(err)
	}
	if s, src := o.SessionFor(tgt); s != s1 || src != "process" {
		t.Fatalf("got %q %q", s, src)
	}

	// The flag beats the handoff.
	if err := handoff.Write(o.cfg.StateDir, k, "cc:three", now); err != nil {
		t.Fatal(err)
	}
	f := newOrch(t, dir, d, func(c *Config) {
		c.StateDir, c.ProjectDir, c.PPID, c.Session = o.cfg.StateDir, o.cfg.ProjectDir, o.cfg.PPID, "flag.sess"
	})
	if s, src := f.SessionFor(tgt); s != "flag.sess" || src != "flag" {
		t.Fatalf("got %q %q", s, src)
	}
	if _, err := New(Config{Session: "bad session!"}); err == nil {
		t.Fatal("an invalid -session must be rejected")
	}
}
