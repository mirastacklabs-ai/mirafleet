package orch

import (
	"encoding/json"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
)

// Client-side limits (moved from cmd/mirafleet mcpCallLimit; semantics unchanged).
const (
	// CallTimeout is the client-side limit of every call not listed in CallLimit.
	CallTimeout = 2 * time.Minute
	// RunMargin is added to an action.run or workspace.exec timeout for dial, queueing and reply latency.
	RunMargin = 30 * time.Second

	workspaceOpenLimit = 5*time.Minute + RunMargin
	workspaceFileLimit = 100 * time.Second
)

// CallLimit is the client-side limit for one call: action.run gets its requested timeout_s
// (server default when omitted, server max at most) plus RunMargin; every other method keeps
// CallTimeout.
//
// Workspace methods (docs/M3.md §1.9, decision 5): workspace.exec gets its requested timeout_s
// plus the margin, capped at the server's 24 h ceiling; without timeout_s the fleet default
// applies, which this client cannot see, so the cap is the ceiling (the server enforces the real
// deadline). workspace.open gets the 5 min open timeout plus the margin; file calls 100 s.
func CallLimit(method string, params json.RawMessage) time.Duration {
	switch method {
	case api.MethodActionRun:
		var p struct {
			TimeoutS int `json:"timeout_s"`
		}
		_ = json.Unmarshal(params, &p) // unparsable: the server rejects it; use the default
		return api.ActionTimeout(p.TimeoutS) + RunMargin
	case api.MethodWorkspaceExec:
		var p struct {
			TimeoutS int `json:"timeout_s"`
		}
		_ = json.Unmarshal(params, &p)
		ceil := api.HardWorkspaceExecCeiling
		if p.TimeoutS > 0 && time.Duration(p.TimeoutS) < ceil/time.Second {
			ceil = time.Duration(p.TimeoutS) * time.Second
		}
		return ceil + RunMargin
	case api.MethodWorkspaceOpen:
		return workspaceOpenLimit
	case api.MethodWorkspaceReadFile, api.MethodWorkspaceWriteFile:
		return workspaceFileLimit
	}
	return CallTimeout
}
