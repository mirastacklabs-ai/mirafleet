package hook

import (
	"testing"

	"github.com/mirastacklabs-ai/mirafleet/internal/mcpserver"
)

// The hook hard-codes the tool names (it must not link the MCP server); every tool the server
// registers must be known, or a confirm on it would get the generic ask. knownTools may list
// remote tools (docs/M7.md §3.7) a little before the server registers them.
func TestKnownToolsMatchMCPServer(t *testing.T) {
	var want []string
	want = append(want, mcpserver.ToolNames(mcpserver.Config{Tools: mcpserver.ListTools(nil)})...)
	want = append(want, mcpserver.ToolNames(mcpserver.Config{Tools: mcpserver.WorkspaceTools(nil)})...)
	for _, n := range want {
		if !knownTools[n] {
			t.Errorf("server tool %q is not in the hook's knownTools", n)
		}
	}
}

// Every local tool the hook denies maps to a distinct, known remote tool.
func TestLocalToolsMapToKnownRemoteTools(t *testing.T) {
	if len(localTools) != 9 {
		t.Fatalf("localTools: %v", localTools)
	}
	seen := map[string]bool{}
	for l, r := range localTools {
		if !knownTools[r] || seen[r] {
			t.Errorf("%s -> %q: unknown or duplicate remote tool", l, r)
		}
		seen[r] = true
	}
}
