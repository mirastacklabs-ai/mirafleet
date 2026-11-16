package mcpserver

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/orch"
)

// fakeBackend is a configurable Backend for tool tests. Set the *Fn fields to override a method;
// unset methods return zero results. Every call is recorded in Calls (method name, params).
type fakeBackend struct {
	mu    sync.Mutex
	Calls []fakeCall

	Contexts []string
	Cap      int // 0 = 30 KiB
	Session  string

	ResolveFn      func(ctx context.Context, contextArg, fleetArg string) (orch.Target, error)
	ContextNamesFn func() ([]string, error)
	FleetsListFn   func(ctx context.Context, cname string) (*api.FleetsListResult, error)
	RunnersListFn  func(ctx context.Context, cname, fleet string) (*api.RunnersListResult, error)

	// Dev-workspace calls; the methods live in fake_dev_test.go.
	WorkspaceCloseWithFn func(ctx context.Context, cname string, p api.WorkspaceCloseParams) (*api.WorkspaceCloseResult, error)
	WorkspaceFSFn        func(ctx context.Context, cname string, p api.FSParams, data []byte) (*api.FSResult, []byte, error)
	JobStartFn           func(ctx context.Context, cname string, p api.JobStartParams) (*api.JobStartResult, error)
	JobStatusFn          func(ctx context.Context, cname string, p api.JobStatusParams) (*api.JobStatusResult, error)
	JobTailFn            func(ctx context.Context, cname string, p api.JobTailParams) (*api.JobTailResult, error)
	JobCancelFn          func(ctx context.Context, cname string, p api.JobCancelParams) (*api.JobCancelResult, error)
	OutputReadFn         func(ctx context.Context, cname string, p api.OutputReadParams) (*api.OutputReadResult, error)
	UploadFn             func(ctx context.Context, cname, projectDir string, p orch.UploadParams) (*orch.TransferResult, error)
	DownloadFn           func(ctx context.Context, cname, projectDir string, p orch.DownloadParams) (*orch.TransferResult, error)
	ProjDir              string
	ActionRunFn          func(ctx context.Context, cname string, p api.ActionRunParams) (*api.ActionRunResult, error)
	WorkspaceOpenFn      func(ctx context.Context, cname string, p api.WorkspaceOpenParams) (*api.WorkspaceOpenResult, error)
	WorkspaceExecFn      func(ctx context.Context, cname string, p api.WorkspaceExecParams, stdin []byte) (*api.WorkspaceExecResult, error)
	WorkspaceReadFileFn  func(ctx context.Context, cname string, p api.WorkspaceReadFileParams) (*api.WorkspaceReadFileResult, []byte, error)
	WorkspaceWriteFileFn func(ctx context.Context, cname string, p api.WorkspaceWriteFileParams, data []byte) (*api.WorkspaceWriteFileResult, error)
	WorkspaceCloseFn     func(ctx context.Context, cname, id string) (*api.WorkspaceCloseResult, error)
	WorkspaceListFn      func(ctx context.Context, cname, fleet string) (*api.WorkspaceListResult, error)
}

type fakeCall struct {
	Method string
	Params any
}

var _ Backend = (*fakeBackend)(nil)

func (f *fakeBackend) rec(m string, p any) {
	f.mu.Lock()
	f.Calls = append(f.Calls, fakeCall{m, p})
	f.mu.Unlock()
}

// calls returns the recorded params of method, in order.
func (f *fakeBackend) calls(method string) []any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []any
	for _, c := range f.Calls {
		if c.Method == method {
			out = append(out, c.Params)
		}
	}
	return out
}

func (f *fakeBackend) Resolve(ctx context.Context, contextArg, fleetArg string) (orch.Target, error) {
	f.rec("Resolve", [2]string{contextArg, fleetArg})
	if f.ResolveFn != nil {
		return f.ResolveFn(ctx, contextArg, fleetArg)
	}
	t := orch.Target{Context: contextArg, ContextSource: orch.SrcArgument, Fleet: fleetArg, FleetSource: orch.SrcArgument}
	if t.Context == "" {
		t.Context, t.ContextSource = "main", orch.SrcOnly
	}
	if t.Fleet == "" {
		t.FleetSource = orch.SrcNone
	}
	return t, nil
}

func (f *fakeBackend) SessionFor(orch.Target) (string, string) {
	if f.Session != "" {
		return f.Session, "flag"
	}
	return "p:0123456789abcdef0123456789abcdef", "process"
}

func (f *fakeBackend) ContextNames() ([]string, error) {
	if f.ContextNamesFn != nil {
		return f.ContextNamesFn()
	}
	return f.Contexts, nil
}

func (f *fakeBackend) OutputCap() int {
	if f.Cap > 0 {
		return f.Cap
	}
	return 30 << 10
}

func (f *fakeBackend) FleetsList(ctx context.Context, cname string) (*api.FleetsListResult, error) {
	f.rec("FleetsList", cname)
	if f.FleetsListFn != nil {
		return f.FleetsListFn(ctx, cname)
	}
	return &api.FleetsListResult{}, nil
}

func (f *fakeBackend) RunnersList(ctx context.Context, cname, fleet string) (*api.RunnersListResult, error) {
	f.rec("RunnersList", [2]string{cname, fleet})
	if f.RunnersListFn != nil {
		return f.RunnersListFn(ctx, cname, fleet)
	}
	return &api.RunnersListResult{}, nil
}

func (f *fakeBackend) ActionRun(ctx context.Context, cname string, p api.ActionRunParams) (*api.ActionRunResult, error) {
	f.rec("ActionRun", p)
	if f.ActionRunFn != nil {
		return f.ActionRunFn(ctx, cname, p)
	}
	return &api.ActionRunResult{}, nil
}

func (f *fakeBackend) WorkspaceOpen(ctx context.Context, cname string, p api.WorkspaceOpenParams) (*api.WorkspaceOpenResult, error) {
	f.rec("WorkspaceOpen", p)
	if f.WorkspaceOpenFn != nil {
		return f.WorkspaceOpenFn(ctx, cname, p)
	}
	return &api.WorkspaceOpenResult{}, nil
}

func (f *fakeBackend) WorkspaceExec(ctx context.Context, cname string, p api.WorkspaceExecParams, stdin []byte) (*api.WorkspaceExecResult, error) {
	f.rec("WorkspaceExec", p)
	if f.WorkspaceExecFn != nil {
		return f.WorkspaceExecFn(ctx, cname, p, stdin)
	}
	return &api.WorkspaceExecResult{}, nil
}

func (f *fakeBackend) WorkspaceReadFile(ctx context.Context, cname string, p api.WorkspaceReadFileParams) (*api.WorkspaceReadFileResult, []byte, error) {
	f.rec("WorkspaceReadFile", p)
	if f.WorkspaceReadFileFn != nil {
		return f.WorkspaceReadFileFn(ctx, cname, p)
	}
	return &api.WorkspaceReadFileResult{}, nil, nil
}

func (f *fakeBackend) WorkspaceWriteFile(ctx context.Context, cname string, p api.WorkspaceWriteFileParams, data []byte) (*api.WorkspaceWriteFileResult, error) {
	f.rec("WorkspaceWriteFile", p)
	if f.WorkspaceWriteFileFn != nil {
		return f.WorkspaceWriteFileFn(ctx, cname, p, data)
	}
	return &api.WorkspaceWriteFileResult{}, nil
}

func (f *fakeBackend) WorkspaceClose(ctx context.Context, cname, id string) (*api.WorkspaceCloseResult, error) {
	f.rec("WorkspaceClose", id)
	if f.WorkspaceCloseFn != nil {
		return f.WorkspaceCloseFn(ctx, cname, id)
	}
	return &api.WorkspaceCloseResult{}, nil
}

func (f *fakeBackend) WorkspaceList(ctx context.Context, cname, fleet string) (*api.WorkspaceListResult, error) {
	f.rec("WorkspaceList", [2]string{cname, fleet})
	if f.WorkspaceListFn != nil {
		return f.WorkspaceListFn(ctx, cname, fleet)
	}
	return &api.WorkspaceListResult{}, nil
}

// startTools serves tools over pipes and returns the SDK client harness.
func startTools(t *testing.T, tools []Tool) *harness {
	t.Helper()
	return start(t, Config{Name: "mirafleet", Version: "test", Tools: tools})
}

// callJSON calls a tool with raw JSON arguments and returns the raw result.
func callJSON(t *testing.T, h *harness, name, args string) *mcp.CallToolResult {
	t.Helper()
	var a any
	if args != "" {
		a = json.RawMessage(args)
	}
	res, err := h.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: a})
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}
	return res
}

// okJSON asserts a success result and decodes its text content into v; it returns the text.
func okJSON(t *testing.T, res *mcp.CallToolResult, v any) string {
	t.Helper()
	if res.IsError {
		t.Fatalf("unexpected error result: %s", resText(res))
	}
	s := resText(res)
	if v != nil {
		if err := json.Unmarshal([]byte(s), v); err != nil {
			t.Fatalf("decode %q: %v", s, err)
		}
	}
	return s
}

func resText(res *mcp.CallToolResult) string {
	if len(res.Content) == 0 {
		return ""
	}
	if tc, ok := res.Content[0].(*mcp.TextContent); ok {
		return tc.Text
	}
	return ""
}
