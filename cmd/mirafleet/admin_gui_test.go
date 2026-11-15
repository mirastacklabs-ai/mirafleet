package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
)

func TestAuditFilterFlags(t *testing.T) {
	f := &fakeAdmin{}
	err := f.cli().run([]string{"audit", "-since", "5", "-before", "90", "-limit", "10", "-fleet", "web",
		"-runner", "r1", "-actor", "admin", "-source", "gui", "-outcome", "ok", "-action", "gui.", "-action-prefix",
		"-workspace", "ws1", "-request-id", "req1", "-from", "2026-11-01", "-to", "2026-11-02T10:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || f.calls[0].method != api.MethodAuditList {
		t.Fatalf("calls: %+v", f.calls)
	}
	var got api.AuditListParams
	if err := json.Unmarshal([]byte(f.calls[0].params), &got); err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	want := api.AuditListParams{SinceID: 5, BeforeID: 90, Limit: 10, Fleet: "web", Runner: "r1", Actor: "admin",
		Source: "gui", Outcome: "ok", Action: "gui.", ActionPrefix: true, Workspace: "ws1", RequestID: "req1",
		From: &from, To: &to}
	if got.From == nil || !got.From.Equal(from) || got.To == nil || !got.To.Equal(to) {
		t.Fatalf("times: %+v", got)
	}
	got.From, got.To, want.From, want.To = nil, nil, nil, nil
	if got != want {
		t.Fatalf("params:\n got %+v\nwant %+v", got, want)
	}
}

func TestAuditFilterFlagErrors(t *testing.T) {
	for _, args := range [][]string{
		{"audit", "-from", "yesterday"},
		{"audit", "-from", "2026-11-02", "-to", "2026-11-01"},
		{"audit", "-action-prefix"},
		{"audit", "-before", "-1"},
	} {
		f := &fakeAdmin{}
		if err := f.cli().run(args); !errors.Is(err, errAdminUsage) || len(f.calls) != 0 {
			t.Errorf("%v: err %v calls %d", args, err, len(f.calls))
		}
	}
}

func TestFleetSetAndResetAdminParsing(t *testing.T) {
	f := &fakeAdmin{results: map[string]any{
		api.MethodFleetSetConfirm: api.FleetSetConfirmResult{Fleet: api.FleetInfo{Name: "web", Confirm: false}},
		api.MethodGUIResetAdmin:   api.GUIResetAdminResult{SetupToken: "mfs1.secret"},
	}}
	if err := f.cli().run([]string{"fleet", "set", "web", "-confirm=false"}); err != nil {
		t.Fatal(err)
	}
	if c := f.calls[0]; c.method != api.MethodFleetSetConfirm || c.params != `{"fleet":"web","confirm":false}` {
		t.Fatalf("call: %+v", c)
	}
	for _, args := range [][]string{{"fleet", "set", "web"}, {"fleet", "set", "-confirm=true"}, {"gui"}, {"gui", "nope"},
		{"gui", "reset-admin", "extra"}} {
		g := &fakeAdmin{}
		if err := g.cli().run(args); !errors.Is(err, errAdminUsage) || len(g.calls) != 0 {
			t.Errorf("%v: err %v calls %d", args, err, len(g.calls))
		}
	}
	f.out.Reset()
	f.errw.Reset()
	if err := f.cli().run([]string{"gui", "reset-admin"}); err != nil {
		t.Fatal(err)
	}
	if f.out.String() != "mfs1.secret\n" || strings.Contains(f.errw.String(), "mfs1.secret") {
		t.Fatalf("stdout %q stderr %q", f.out.String(), f.errw.String())
	}
}
