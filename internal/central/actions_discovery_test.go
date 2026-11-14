//go:build unix

package central

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// S5: runners.list shows the actions of an actions-mode runner (names and param shapes only);
// a runner that sends none (M2/M3 style heartbeat) lists without the field.
func TestRunnersListShowsActions(t *testing.T) {
	e := newEnv(t, "r1", "old")
	e.startRunner("r1")
	e.waitOnline("r1")

	// An M2/M3-style runner: raw heartbeat with no actions field.
	cl, err := e.dial(transport.UserRunner, "old")
	must(t, err)
	defer cl.Close()
	b, _ := json.Marshal(transport.Heartbeat{Hostname: "h"})
	if ok, _, err := cl.SendRequest(transport.ReqHeartbeat, true, b); err != nil || !ok {
		t.Fatalf("heartbeat without actions refused: %v", err)
	}
	e.waitOnline("old")

	sock := adminSocket(t, e)
	var rl api.RunnersListResult
	must(t, AdminCall(sock, api.MethodRunnersList, api.RunnersListParams{Fleet: testFleet}, &rl))
	byName := map[string]api.RunnerInfo{}
	for _, r := range rl.Runners {
		byName[r.Name] = r
	}
	got := byName["r1"].Actions
	if len(got) != 4 || got[0].Name != "fail" || got[1].Name != "hello" || got[2].Name != "long" || got[3].Name != "slow" {
		t.Fatalf("actions: %+v", got)
	}
	if p := got[1].Params; len(p) != 1 || p[0].Name != "name" || p[0].Pattern != "[a-z]{1,8}" || p[0].Default != nil {
		t.Fatalf("hello params: %+v", got[1].Params)
	}
	if len(byName["old"].Actions) != 0 {
		t.Fatalf("old runner has actions: %+v", byName["old"])
	}
	raw, _ := json.Marshal(rl)
	for _, bad := range []string{"/bin/", "sleep", "argv", "timeout"} {
		if strings.Contains(string(raw), bad) {
			t.Fatalf("runners.list leaks %q: %s", bad, raw)
		}
	}
	if strings.Contains(string(mustMarshal(byName["old"])), "actions") {
		t.Fatal("empty actions not omitted")
	}
}

func mustMarshal(v any) []byte { b, _ := json.Marshal(v); return b }
