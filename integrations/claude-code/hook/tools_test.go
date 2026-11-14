package hook

import (
	"sort"
	"testing"

	"github.com/mirastacklabs-ai/mirafleet/internal/mcpserver"
)

// The hook hard-codes the tool names (it must not link the MCP server); this keeps the list in
// step with what internal/mcpserver registers.
func TestKnownToolsMatchMCPServer(t *testing.T) {
	var want []string
	want = append(want, mcpserver.ToolNames(mcpserver.Config{Tools: mcpserver.ListTools(nil)})...)
	want = append(want, mcpserver.ToolNames(mcpserver.Config{Tools: mcpserver.WorkspaceTools(nil)})...)
	var have []string
	for n := range knownTools {
		have = append(have, n)
	}
	sort.Strings(want)
	sort.Strings(have)
	if len(want) != len(have) {
		t.Fatalf("server tools %v, hook knows %v", want, have)
	}
	for i := range want {
		if want[i] != have[i] {
			t.Fatalf("server tools %v, hook knows %v", want, have)
		}
	}
}
