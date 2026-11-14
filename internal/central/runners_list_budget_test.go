package central

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// runners.list with many near-max action lists stays under MaxResponseBytes: the action budget
// runs out, later runners are listed without actions and flagged, order is preserved.
func TestRunnersListActionBudget(t *testing.T) {
	var acts []transport.ActionInfo
	for i := 0; transport.ActionsEncodedSize(acts) < transport.MaxActionsBytes-400; i++ {
		d := strings.Repeat("d", 200)
		acts = append(acts, transport.ActionInfo{Name: fmt.Sprintf("act%03d", i), Params: []transport.ActionParam{
			{Name: "p", Pattern: strings.Repeat("x", 200), Default: &d}}})
	}
	if transport.ActionsEncodedSize(acts) > transport.MaxActionsBytes {
		t.Fatal("test actions over the cap")
	}
	const n = 300
	vs := make([]service.RunnerView, n)
	for i := range vs {
		vs[i] = service.RunnerView{Runner: store.Runner{Name: fmt.Sprintf("r%04d", i)}, Actions: acts}
	}
	out := buildRunnersList(vs)
	b, err := json.Marshal(api.RunnersListResult(out))
	if err != nil {
		t.Fatal(err)
	}
	if len(b) >= api.MaxResponseBytes {
		t.Fatalf("response %d >= %d", len(b), api.MaxResponseBytes)
	}
	withActs, omitted := 0, 0
	for i, r := range out.Runners {
		if r.Name != vs[i].Name {
			t.Fatalf("order changed at %d", i)
		}
		switch {
		case len(r.Actions) > 0 && !r.ActionsOmitted:
			if omitted > 0 {
				t.Fatalf("runner %d has actions after an omitted one", i)
			}
			withActs++
		case len(r.Actions) == 0 && r.ActionsOmitted:
			omitted++
		default:
			t.Fatalf("runner %d inconsistent: %+v", i, r)
		}
	}
	if withActs == 0 || omitted == 0 {
		t.Fatalf("with=%d omitted=%d", withActs, omitted)
	}
}
