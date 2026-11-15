package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
)

const wsSock = defaultCentralData + "/admin.sock"

func lastParams(t *testing.T, f *fakeAdmin, method string) map[string]any {
	t.Helper()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i].method == method {
			var m map[string]any
			if err := json.Unmarshal([]byte(f.calls[i].params), &m); err != nil {
				t.Fatal(err)
			}
			return m
		}
	}
	t.Fatalf("no %s call in %+v", method, f.calls)
	return nil
}

func TestFleetCreateWorkspaceFlags(t *testing.T) {
	f := &fakeAdmin{results: map[string]any{api.MethodFleetCreate: api.FleetCreateResult{Fleet: api.FleetInfo{Name: "dev", Mode: "workspace"}}}}
	err := f.cli().run([]string{"fleet", "create", "dev", "-mode", "workspace", "-executor", "containerd",
		"-image", "docker.io/library/golang:1.27", "-images", "docker.io/library/golang:*, ghcr.io/acme/dev@sha256:*",
		"-pull", "never", "-network", "egress", "-egress", "proxy.golang.org:443,*.golang.org:443",
		"-cpus", "1.5", "-memory", "4GiB", "-pids", "1024", "-tmpfs", "512MiB", "-disk", "10GiB",
		"-idle-ttl", "24h", "-max-ttl", "168h", "-exec-timeout", "2m", "-exec-max", "45m", "-confirm"})
	if err != nil {
		t.Fatal(err)
	}
	got := lastParams(t, f, api.MethodFleetCreate)
	want := `{"confirm":true,"mode":"workspace","name":"dev","workspace":{"cpu_milli":1500,"default_image":"docker.io/library/golang:1.27",` +
		`"disk_mb":10240,"egress":["proxy.golang.org:443","*.golang.org:443"],"exec_default_s":120,"exec_max_s":2700,"executor":"containerd",` +
		`"idle_ttl_s":86400,"images":["docker.io/library/golang:*","ghcr.io/acme/dev@sha256:*"],"max_ttl_s":604800,"memory_mb":4096,` +
		`"network":"egress","pids":1024,"pull":"never","tmpfs_mb":512}}`
	b, _ := json.Marshal(got)
	if string(b) != want {
		t.Errorf("params\n got %s\nwant %s", b, want)
	}
}

func TestFleetCreateWorkspaceFlagErrors(t *testing.T) {
	for name, args := range map[string][]string{
		"workspace flag on actions fleet": {"fleet", "create", "x", "-image", "a"},
		"bad memory":                      {"fleet", "create", "x", "-mode", "workspace", "-memory", "lots"},
		"memory with KB":                  {"fleet", "create", "x", "-mode", "workspace", "-memory", "4GB"},
		"zero memory":                     {"fleet", "create", "x", "-mode", "workspace", "-memory", "0"},
		"zero cpus":                       {"fleet", "create", "x", "-mode", "workspace", "-cpus", "0"},
		"huge cpus":                       {"fleet", "create", "x", "-mode", "workspace", "-cpus", "1e9"},
		"tiny cpus":                       {"fleet", "create", "x", "-mode", "workspace", "-cpus", "0.0004"},
		"just below 0.001":                {"fleet", "create", "x", "-mode", "workspace", "-cpus", "0.00099"},
		"negative cpus":                   {"fleet", "create", "x", "-mode", "workspace", "-cpus", "-1"},
		"above store cap":                 {"fleet", "create", "x", "-mode", "workspace", "-cpus", "1024.001"},
		"nan cpus":                        {"fleet", "create", "x", "-mode", "workspace", "-cpus", "NaN"},
		"inf cpus":                        {"fleet", "create", "x", "-mode", "workspace", "-cpus", "Inf"},
		"negative pids":                   {"fleet", "create", "x", "-mode", "workspace", "-pids", "-1"},
		"sub-second ttl":                  {"fleet", "create", "x", "-mode", "workspace", "-idle-ttl", "500ms"},
		"fractional second":               {"fleet", "create", "x", "-mode", "workspace", "-exec-max", "1500ms"},
		"zero exec max":                   {"fleet", "create", "x", "-mode", "workspace", "-exec-max", "0s"},
	} {
		f := &fakeAdmin{}
		err := f.cli().run(args)
		if !errors.Is(err, errAdminUsage) || len(f.calls) != 0 {
			t.Errorf("%s: err=%v calls=%d", name, err, len(f.calls))
		}
	}
	// A workspace fleet without flags still sends no config (the server rejects the missing image).
	f := &fakeAdmin{}
	if err := f.cli().run([]string{"fleet", "create", "w", "-mode", "workspace"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := lastParams(t, f, api.MethodFleetCreate)["workspace"]; ok {
		t.Error("an empty workspace config was sent")
	}
}

func TestFleetCreateCPUBoundaries(t *testing.T) {
	if maxCLICPUs != store.MaxCPUMilli/1000 {
		t.Fatalf("CLI cpu cap %d != store cap %d", maxCLICPUs, store.MaxCPUMilli/1000)
	}
	for in, want := range map[string]float64{"0.001": 1, "0.0015": 2, "1": 1000, "1024": 1_024_000} {
		f := &fakeAdmin{}
		if err := f.cli().run([]string{"fleet", "create", "x", "-mode", "workspace", "-cpus", in}); err != nil {
			t.Errorf("-cpus %s refused: %v", in, err)
			continue
		}
		ws, _ := lastParams(t, f, api.MethodFleetCreate)["workspace"].(map[string]any)
		if ws["cpu_milli"] != want {
			t.Errorf("-cpus %s: cpu_milli=%v want %v", in, ws["cpu_milli"], want)
		}
	}
}

func currentFleet() api.FleetsListResult {
	return api.FleetsListResult{Fleets: []api.FleetInfo{
		{Name: "act", Mode: "actions"},
		{Name: "dev", Mode: "workspace", Workspace: &api.FleetWorkspaceInfo{DefaultImage: "a:1", Images: []string{"a:1"}, Network: "egress",
			Config: &api.WorkspaceConfig{Executor: "containerd", DefaultImage: "a:1", Images: []string{"a:1", "b:*"}, Pull: "always",
				Network: "egress", Egress: []string{"x.org:443"}, CPUMilli: 2000, MemoryMB: 4096, Pids: 1024, TmpfsMB: 512, DiskMB: 10240,
				IdleTTLS: 86400, MaxTTLS: 604800, ExecDefaultS: 120, ExecMaxS: 1800}}},
	}}
}

func TestFleetWorkspaceSetSendsFullConfig(t *testing.T) {
	f := &fakeAdmin{results: map[string]any{api.MethodFleetsList: currentFleet(),
		api.MethodFleetWorkspaceSet: api.FleetWorkspaceSetResult{Fleet: api.FleetInfo{Name: "dev"}}}}
	// Change memory and clear egress (empty value); everything else must be carried over.
	if err := f.cli().run([]string{"fleet", "workspace", "set", "dev", "-memory", "8GiB", "-egress", "", "-network", "none"}); err != nil {
		t.Fatal(err)
	}
	if f.calls[0].method != api.MethodFleetsList || f.calls[1].method != api.MethodFleetWorkspaceSet || len(f.calls) != 2 {
		t.Fatalf("calls: %+v", f.calls)
	}
	var p api.FleetWorkspaceSetParams
	if err := json.Unmarshal([]byte(f.calls[1].params), &p); err != nil {
		t.Fatal(err)
	}
	w := p.Workspace
	if p.Fleet != "dev" || w.MemoryMB != 8192 || len(w.Egress) != 0 || w.Network != "none" {
		t.Errorf("changes not applied: %+v", p)
	}
	if w.Executor != "containerd" || w.DefaultImage != "a:1" || len(w.Images) != 2 || w.Pull != "always" || w.CPUMilli != 2000 ||
		w.Pids != 1024 || w.TmpfsMB != 512 || w.DiskMB != 10240 || w.IdleTTLS != 86400 || w.MaxTTLS != 604800 ||
		w.ExecDefaultS != 120 || w.ExecMaxS != 1800 {
		t.Errorf("unchanged fields were not carried over: %+v", w)
	}
}

func TestFleetWorkspaceSetErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"no flags":      {[]string{"fleet", "workspace", "set", "dev"}, "at least one flag"},
		"no name":       {[]string{"fleet", "workspace", "set", "-memory", "1GiB"}, "one fleet NAME"},
		"unknown fleet": {[]string{"fleet", "workspace", "set", "nope", "-memory", "1GiB"}, "not found"},
		"actions fleet": {[]string{"fleet", "workspace", "set", "act", "-memory", "1GiB"}, "not a workspace fleet"},
		"bad value":     {[]string{"fleet", "workspace", "set", "dev", "-memory", "x"}, "size"},
		"no sub":        {[]string{"fleet", "workspace"}, "missing subcommand"},
		"bad sub":       {[]string{"fleet", "workspace", "get", "dev"}, "unknown subcommand"},
	} {
		f := &fakeAdmin{results: map[string]any{api.MethodFleetsList: currentFleet()}}
		err := f.cli().run(tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
		for _, c := range f.calls {
			if c.method == api.MethodFleetWorkspaceSet {
				t.Errorf("%s: a set was sent", name)
			}
		}
	}
}

func TestWorkspaceListKill(t *testing.T) {
	created := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	f := &fakeAdmin{results: map[string]any{api.MethodWorkspaceAdminList: api.WorkspaceAdminListResult{Workspaces: []api.WorkspaceInfo{
		{ID: strings.Repeat("a", 32), Fleet: "dev", Runner: "r1", Operator: "alice", Session: "s1", Image: "a:1", State: "open", Created: created, LastUsed: created, Expires: created.Add(time.Hour)}}}}}
	if err := f.cli().run([]string{"workspace", "list", "-fleet", "dev"}); err != nil {
		t.Fatal(err)
	}
	if lastParams(t, f, api.MethodWorkspaceAdminList)["fleet"] != "dev" {
		t.Errorf("params: %+v", f.calls)
	}
	out := f.out.String()
	for _, w := range []string{"ID", "OPERATOR", strings.Repeat("a", 32), "alice", "s1", "open", "2026-11-13T11:00:00Z"} {
		if !strings.Contains(out, w) {
			t.Errorf("list output lacks %q:\n%s", w, out)
		}
	}
	f.out.Reset()
	if err := f.cli().run([]string{"workspace", "list", "-json"}); err != nil || !json.Valid(f.out.Bytes()) || !strings.Contains(f.out.String(), `"operator": "alice"`) {
		t.Errorf("json: %v %s", err, f.out.String())
	}
	if err := f.cli().run([]string{"workspace", "kill", strings.Repeat("a", 32)}); err != nil {
		t.Fatal(err)
	}
	if lastParams(t, f, api.MethodWorkspaceKill)["id"] != strings.Repeat("a", 32) {
		t.Errorf("kill params: %+v", f.calls)
	}
	if v, ok := lastParams(t, f, api.MethodWorkspaceKill)["delete"]; ok && v != false {
		t.Errorf("delete must be off by default: %+v", f.calls)
	}
	if err := f.cli().run([]string{"workspace", "kill", "-delete", strings.Repeat("a", 32)}); err != nil {
		t.Fatal(err)
	}
	if lastParams(t, f, api.MethodWorkspaceKill)["delete"] != true {
		t.Errorf("kill -delete params: %+v", f.calls)
	}
	for _, args := range [][]string{{"workspace"}, {"workspace", "kill"}, {"workspace", "kill", "a", "b"}, {"workspace", "list", "x"}, {"workspace", "nope"}} {
		if err := (&fakeAdmin{}).cli().run(args); !errors.Is(err, errAdminUsage) {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func TestParseSizeMB(t *testing.T) {
	for in, want := range map[string]int{"512MiB": 512, "4GiB": 4096, "1TiB": 1 << 20, "100": 100, " 2GiB ": 2048} {
		if got, err := parseSizeMB("x", in); err != nil || got != want {
			t.Errorf("%q = %d, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "0", "-1GiB", "1.5GiB", "4GB", "4gib", "9999999999GiB", "99999999999"} {
		if _, err := parseSizeMB("x", in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}
