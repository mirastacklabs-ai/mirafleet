package central

import (
	"reflect"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
)

func TestWorkspaceFromAPIMapsEveryField(t *testing.T) {
	if workspaceFromAPI(nil) != nil {
		t.Fatal("nil must stay nil")
	}
	in := &api.WorkspaceConfig{
		Executor: "opensandbox", DefaultImage: "img:1", Images: []string{"img:1", "r/x:*"},
		Pull: "always", Network: "egress", Egress: []string{"a.example:443"},
		CPUMilli: 1, MemoryMB: 2, Pids: 3, TmpfsMB: 4, DiskMB: 5,
		IdleTTLS: 6, MaxTTLS: 7, ExecDefaultS: 8, ExecMaxS: 9,
	}
	// Every api field must be non-zero so a forgotten mapping cannot hide.
	iv := reflect.ValueOf(*in)
	for i := 0; i < iv.NumField(); i++ {
		if iv.Field(i).IsZero() {
			t.Fatalf("test fixture leaves api field %s zero", iv.Type().Field(i).Name)
		}
	}
	got := workspaceFromAPI(in)
	want := &store.WorkspaceConfig{
		Executor: "opensandbox", DefaultImage: "img:1", Images: []string{"img:1", "r/x:*"},
		Pull: "always", Network: "egress", Egress: []string{"a.example:443"},
		CPUMilli: 1, MemoryMB: 2, Pids: 3, TmpfsMB: 4, DiskMB: 5,
		IdleTTL: 6 * time.Second, MaxTTL: 7 * time.Second, ExecDefault: 8 * time.Second, ExecMax: 9 * time.Second,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	gv := reflect.ValueOf(*got)
	for i := 0; i < gv.NumField(); i++ {
		if n := gv.Type().Field(i).Name; n != "UpdatedAt" && gv.Field(i).IsZero() {
			t.Errorf("store field %s not mapped", n)
		}
	}
}

func TestCheckWorkspaceSeconds(t *testing.T) {
	if checkWorkspaceSeconds(nil) != nil {
		t.Fatal("nil must pass")
	}
	if err := checkWorkspaceSeconds(&api.WorkspaceConfig{IdleTTLS: 3600, ExecMaxS: maxConfigSeconds}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []api.WorkspaceConfig{{IdleTTLS: maxConfigSeconds + 1}, {MaxTTLS: -maxConfigSeconds - 1},
		{ExecDefaultS: 1 << 60}, {ExecMaxS: 1 << 62}} {
		if checkWorkspaceSeconds(&c) == nil {
			t.Errorf("accepted %+v", c)
		}
	}
}
