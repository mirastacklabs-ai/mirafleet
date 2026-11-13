package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
)

type fakeCall struct {
	sock, method, params string
}

// fakeAdmin is the AdminCall seam: it records calls and answers from canned results.
type fakeAdmin struct {
	calls   []fakeCall
	results map[string]any // by method
	err     error
	out     bytes.Buffer
	errw    bytes.Buffer
	exitCh  []int
}

func (f *fakeAdmin) cli() *adminCLI {
	return &adminCLI{
		out: &f.out, err: &f.errw,
		exit: func(c int) { f.exitCh = append(f.exitCh, c) },
		call: func(sock, method string, params, result any) error {
			pj := ""
			if params != nil {
				b, _ := json.Marshal(params)
				pj = string(b)
			}
			f.calls = append(f.calls, fakeCall{sock, method, pj})
			if f.err != nil {
				return f.err
			}
			if r, ok := f.results[method]; ok && result != nil {
				b, _ := json.Marshal(r)
				return json.Unmarshal(b, result)
			}
			return nil
		},
	}
}

func TestAdminCLIParsing(t *testing.T) {
	const sock = defaultCentralData + "/admin.sock"
	tests := []struct {
		name   string
		args   []string
		method string // expected single call; "" = expect a usage error and no call
		params string
		sock   string
	}{
		{"fleet create default", []string{"fleet", "create", "web"}, api.MethodFleetCreate, `{"name":"web","mode":"actions"}`, sock},
		{"fleet create flags after name", []string{"fleet", "create", "web", "-mode", "workspace", "-confirm"}, api.MethodFleetCreate, `{"name":"web","mode":"workspace","confirm":true}`, sock},
		{"fleet create flags before name", []string{"fleet", "create", "-confirm", "web"}, api.MethodFleetCreate, `{"name":"web","mode":"actions","confirm":true}`, sock},
		{"fleet create custom data", []string{"fleet", "create", "web", "-data", "/d"}, api.MethodFleetCreate, `{"name":"web","mode":"actions"}`, "/d/admin.sock"},
		{"fleet create no name", []string{"fleet", "create"}, "", "", ""},
		{"fleet create two names", []string{"fleet", "create", "a", "b"}, "", "", ""},
		{"fleet list", []string{"fleet", "list", "-json"}, api.MethodFleetsList, "", sock},
		{"token create full", []string{"token", "create", "-fleet", "web", "-labels", "env=prod,tier=1", "-approve", "-uses", "3", "-ttl", "90m"},
			api.MethodTokenCreate, `{"fleet":"web","labels":{"env":"prod","tier":"1"},"approve":true,"uses":3,"ttl_s":5400}`, sock},
		{"token create minimal", []string{"token", "create", "-fleet", "web"}, api.MethodTokenCreate, `{"fleet":"web"}`, sock},
		{"token create no fleet", []string{"token", "create"}, "", "", ""},
		{"token create bad label", []string{"token", "create", "-fleet", "web", "-labels", "novalue"}, "", "", ""},
		{"token create sub-second ttl", []string{"token", "create", "-fleet", "web", "-ttl", "500ms"}, "", "", ""},
		{"token create negative uses", []string{"token", "create", "-fleet", "web", "-uses", "-1"}, "", "", ""},
		{"token create unknown flag", []string{"token", "create", "-fleet", "web", "-nope"}, "", "", ""},
		{"token list", []string{"token", "list", "-fleet", "web"}, api.MethodTokenList, `{"fleet":"web"}`, sock},
		{"token list all", []string{"token", "list"}, api.MethodTokenList, `{}`, sock},
		{"token revoke", []string{"token", "revoke", "abc123"}, api.MethodTokenRevoke, `{"id":"abc123"}`, sock},
		{"token revoke no id", []string{"token", "revoke"}, "", "", ""},
		{"runner list fleet", []string{"runner", "list", "-fleet", "web"}, api.MethodRunnersList, `{"fleet":"web"}`, sock},
		{"runner approve", []string{"runner", "approve", "web/r1"}, api.MethodRunnerLifecycle, `{"fleet":"web","runner":"r1","op":"approve"}`, sock},
		{"runner disable", []string{"runner", "disable", "web/r1"}, api.MethodRunnerLifecycle, `{"fleet":"web","runner":"r1","op":"disable"}`, sock},
		{"runner enable", []string{"runner", "enable", "web/r1"}, api.MethodRunnerLifecycle, `{"fleet":"web","runner":"r1","op":"enable"}`, sock},
		{"runner revoke", []string{"runner", "revoke", "web/r1"}, api.MethodRunnerLifecycle, `{"fleet":"web","runner":"r1","op":"revoke"}`, sock},
		{"runner revoke bad ref", []string{"runner", "revoke", "r1"}, "", "", ""},
		{"operator code", []string{"operator", "code"}, api.MethodOperatorCode, "", sock},
		{"operator rotate", []string{"operator", "rotate-secret"}, api.MethodOperatorRotateSecret, "", sock},
		{"operator list", []string{"operator", "list"}, api.MethodOperatorList, "", sock},
		{"operator revoke", []string{"operator", "revoke", "alice"}, api.MethodOperatorRevoke, `{"name":"alice"}`, sock},
		{"operator revoke no name", []string{"operator", "revoke"}, "", "", ""},
		{"exec", []string{"exec", "web/r1", "uptime", "n=1", "-confirm", "web"}, api.MethodActionRun,
			`{"fleet":"web","runner":"r1","action":"uptime","params":{"n":"1"},"timeout_s":600,"confirm":"web"}`, sock},
		{"exec bad param", []string{"exec", "web/r1", "uptime", "oops"}, "", "", ""},
		{"exec too few", []string{"exec", "web/r1"}, "", "", ""},
		{"audit", []string{"audit", "-since", "5", "-limit", "10"}, api.MethodAuditList, `{"since_id":5,"limit":10}`, sock},
		{"audit default", []string{"audit"}, api.MethodAuditList, `{}`, sock},
		{"audit negative", []string{"audit", "-limit", "-2"}, "", "", ""},
		{"unknown sub", []string{"frobnicate"}, "", "", ""},
		{"group without sub", []string{"fleet"}, "", "", ""},
		{"unknown group sub", []string{"token", "frob"}, "", "", ""},
		{"no args", nil, "", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeAdmin{results: map[string]any{}}
			err := f.cli().run(tc.args)
			if tc.method == "" {
				if !errors.Is(err, errAdminUsage) {
					t.Fatalf("want usage error, got %v", err)
				}
				if len(f.calls) != 0 {
					t.Fatalf("usage error must not call central: %+v", f.calls)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(f.calls) != 1 {
				t.Fatalf("calls: %+v", f.calls)
			}
			c := f.calls[0]
			if c.method != tc.method || c.params != tc.params || c.sock != tc.sock {
				t.Fatalf("got %+v, want method=%s params=%s sock=%s", c, tc.method, tc.params, tc.sock)
			}
		})
	}
}

func TestAdminCLITokenCreateStdoutIsOnlyTheToken(t *testing.T) {
	f := &fakeAdmin{results: map[string]any{api.MethodTokenCreate: api.TokenCreateResult{
		ID: "0123456789abcdef", Token: "mfj1.id.secret.hash", Expires: time.Unix(1700000000, 0)}}}
	if err := f.cli().run([]string{"token", "create", "-fleet", "web"}); err != nil {
		t.Fatal(err)
	}
	if f.out.String() != "mfj1.id.secret.hash\n" {
		t.Fatalf("stdout = %q", f.out.String())
	}
	if strings.Contains(f.errw.String(), "mfj1.") || !strings.Contains(f.errw.String(), "0123456789abcdef") {
		t.Fatalf("stderr = %q (metadata only, never the token)", f.errw.String())
	}
}

func TestAdminCLIRunnerListAllFleets(t *testing.T) {
	f := &fakeAdmin{results: map[string]any{
		api.MethodFleetsList:  api.FleetsListResult{Fleets: []api.FleetInfo{{Name: "a"}, {Name: "b"}}},
		api.MethodRunnersList: api.RunnersListResult{Runners: []api.RunnerInfo{{Name: "r", Status: "active", Online: true}}},
	}}
	if err := f.cli().run([]string{"runner", "list"}); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 3 || f.calls[1].params != `{"fleet":"a"}` || f.calls[2].params != `{"fleet":"b"}` {
		t.Fatalf("calls: %+v", f.calls)
	}
	if !strings.Contains(f.out.String(), "a/r") || !strings.Contains(f.out.String(), "b/r") {
		t.Fatalf("table: %s", f.out.String())
	}
}

func TestAdminCLIJSONIsTheDTO(t *testing.T) {
	want := api.FleetsListResult{Fleets: []api.FleetInfo{{Name: "a", Mode: "actions", Runners: 2}}}
	f := &fakeAdmin{results: map[string]any{api.MethodFleetsList: want}}
	if err := f.cli().run([]string{"fleet", "list", "-json"}); err != nil {
		t.Fatal(err)
	}
	var got api.FleetsListResult
	if err := json.Unmarshal(f.out.Bytes(), &got); err != nil || len(got.Fleets) != 1 || got.Fleets[0] != want.Fleets[0] {
		t.Fatalf("json output %q: %v", f.out.String(), err)
	}
}

func TestAdminCLIErrorsKeepTheAPICode(t *testing.T) {
	f := &fakeAdmin{err: api.Errorf(api.CodeConflict, "fleet exists")}
	err := f.cli().run([]string{"fleet", "create", "web"})
	if err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("err = %v", err)
	}
}

func TestAdminCLIExecPropagatesExitCode(t *testing.T) {
	f := &fakeAdmin{results: map[string]any{api.MethodActionRun: api.ActionRunResult{ExitCode: 3, Stdout: "o", Stderr: "e"}}}
	if err := f.cli().run([]string{"exec", "web/r1", "x"}); err != nil {
		t.Fatal(err)
	}
	if len(f.exitCh) != 1 || f.exitCh[0] != 3 || f.out.String() != "o" || f.errw.String() != "e" {
		t.Fatalf("exit=%v out=%q err=%q", f.exitCh, f.out.String(), f.errw.String())
	}
}

func TestMCPCallLimit(t *testing.T) {
	run := api.MethodActionRun
	cases := []struct {
		name, method, params string
		want                 time.Duration
	}{
		{"other method", api.MethodFleetsList, `{"timeout_s":600}`, mcpCallTimeout},
		{"run default", run, ``, api.DefaultActionTimeout + mcpRunMargin},
		{"run empty params", run, `{}`, api.DefaultActionTimeout + mcpRunMargin},
		{"run explicit", run, `{"timeout_s":300}`, 300*time.Second + mcpRunMargin},
		{"run max", run, `{"timeout_s":600}`, api.MaxActionTimeout + mcpRunMargin},
		{"run over max capped", run, `{"timeout_s":99999999999}`, api.MaxActionTimeout + mcpRunMargin},
		{"run malformed", run, `[1]`, api.DefaultActionTimeout + mcpRunMargin},
	}
	for _, c := range cases {
		if got := mcpCallLimit(c.method, json.RawMessage(c.params)); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}
