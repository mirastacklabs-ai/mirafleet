package mcpserver

import (
	"context"
	"errors"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/orch"
)

// Backend is what the tools need from the orchestrator layer; *orch.Orch implements it.
type Backend interface {
	Resolve(ctx context.Context, contextArg, fleetArg string) (orch.Target, error)
	SessionFor(t orch.Target) (session, source string)
	ContextNames() ([]string, error)
	OutputCap() int
	FleetsList(ctx context.Context, cname string) (*api.FleetsListResult, error)
	RunnersList(ctx context.Context, cname, fleet string) (*api.RunnersListResult, error)
	ActionRun(ctx context.Context, cname string, p api.ActionRunParams) (*api.ActionRunResult, error)
	WorkspaceOpen(ctx context.Context, cname string, p api.WorkspaceOpenParams) (*api.WorkspaceOpenResult, error)
	WorkspaceExec(ctx context.Context, cname string, p api.WorkspaceExecParams, stdin []byte) (*api.WorkspaceExecResult, error)
	WorkspaceReadFile(ctx context.Context, cname string, p api.WorkspaceReadFileParams) (*api.WorkspaceReadFileResult, []byte, error)
	WorkspaceWriteFile(ctx context.Context, cname string, p api.WorkspaceWriteFileParams, data []byte) (*api.WorkspaceWriteFileResult, error)
	WorkspaceClose(ctx context.Context, cname, id string) (*api.WorkspaceCloseResult, error)
	WorkspaceList(ctx context.Context, cname, fleet string) (*api.WorkspaceListResult, error)

	// Dev-workspace calls (tools_fs.go, tools_bash.go).
	WorkspaceCloseWith(ctx context.Context, cname string, p api.WorkspaceCloseParams) (*api.WorkspaceCloseResult, error)
	WorkspaceFS(ctx context.Context, cname string, p api.FSParams, data []byte) (*api.FSResult, []byte, error)
	JobStart(ctx context.Context, cname string, p api.JobStartParams) (*api.JobStartResult, error)
	JobStatus(ctx context.Context, cname string, p api.JobStatusParams) (*api.JobStatusResult, error)
	JobTail(ctx context.Context, cname string, p api.JobTailParams) (*api.JobTailResult, error)
	JobCancel(ctx context.Context, cname string, p api.JobCancelParams) (*api.JobCancelResult, error)
	OutputRead(ctx context.Context, cname string, p api.OutputReadParams) (*api.OutputReadResult, error)
	Upload(ctx context.Context, cname, projectDir string, p orch.UploadParams) (*orch.TransferResult, error)
	Download(ctx context.Context, cname, projectDir string, p orch.DownloadParams) (*orch.TransferResult, error)
}

// ProjectDirer is optionally implemented by a Backend that knows the local project directory
// (where .mirafleet.yaml lives); upload and download are confined to it. Without it the
// server's working directory is used.
type ProjectDirer interface{ ProjectDir() string }

var _ Backend = (*orch.Orch)(nil)

// where names the context and fleet a call went to and where each came from. Every tool result
// that resolved a target carries it: a cloned repo's .mirafleet.yaml can steer unqualified calls,
// so the user must be able to see where a call ran.
type where struct {
	Context       string `json:"context"`
	ContextSource string `json:"context_source"`
	Fleet         string `json:"fleet"`
	FleetSource   string `json:"fleet_source"`
}

func whereOf(t orch.Target) where {
	return where{Context: t.Context, ContextSource: t.ContextSource, Fleet: t.Fleet, FleetSource: t.FleetSource}
}

// whereSchema is the JSON Schema of [where], for output schemas.
const whereSchema = `{"type":"object","properties":{"context":{"type":"string"},"context_source":{"type":"string"},` +
	`"fleet":{"type":"string"},"fleet_source":{"type":"string"}},"required":["context","context_source","fleet","fleet_source"]}`

// toolError maps any error to one toResult presents correctly: public errors (orch and api
// errors) pass through, context errors become canceled/timeout, everything else is left for
// toResult to log and report as internal.
func toolError(err error) error {
	if err == nil {
		return nil
	}
	var pe PublicError
	switch {
	case errors.As(err, &pe):
		return err
	case errors.Is(err, context.Canceled):
		return &Error{Code: "canceled", Message: "request canceled"}
	case errors.Is(err, context.DeadlineExceeded):
		return &Error{Code: api.CodeTimeout, Message: "the call timed out"}
	}
	return err
}
