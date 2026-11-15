package mcpserver

import (
	"context"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/orch"
)

var _ ProjectDirer = (*fakeBackend)(nil)

func (f *fakeBackend) ProjectDir() string { return f.ProjDir }

func (f *fakeBackend) WorkspaceCloseWith(ctx context.Context, cname string, p api.WorkspaceCloseParams) (*api.WorkspaceCloseResult, error) {
	f.rec("WorkspaceCloseWith", p)
	if f.WorkspaceCloseWithFn != nil {
		return f.WorkspaceCloseWithFn(ctx, cname, p)
	}
	return &api.WorkspaceCloseResult{}, nil
}

func (f *fakeBackend) WorkspaceFS(ctx context.Context, cname string, p api.FSParams, data []byte) (*api.FSResult, []byte, error) {
	f.rec("WorkspaceFS", p)
	if f.WorkspaceFSFn != nil {
		return f.WorkspaceFSFn(ctx, cname, p, data)
	}
	return &api.FSResult{Kind: "none"}, nil, nil
}

func (f *fakeBackend) JobStart(ctx context.Context, cname string, p api.JobStartParams) (*api.JobStartResult, error) {
	f.rec("JobStart", p)
	if f.JobStartFn != nil {
		return f.JobStartFn(ctx, cname, p)
	}
	return &api.JobStartResult{}, nil
}

func (f *fakeBackend) JobStatus(ctx context.Context, cname string, p api.JobStatusParams) (*api.JobStatusResult, error) {
	f.rec("JobStatus", p)
	if f.JobStatusFn != nil {
		return f.JobStatusFn(ctx, cname, p)
	}
	return &api.JobStatusResult{}, nil
}

func (f *fakeBackend) JobTail(ctx context.Context, cname string, p api.JobTailParams) (*api.JobTailResult, error) {
	f.rec("JobTail", p)
	if f.JobTailFn != nil {
		return f.JobTailFn(ctx, cname, p)
	}
	return &api.JobTailResult{}, nil
}

func (f *fakeBackend) JobCancel(ctx context.Context, cname string, p api.JobCancelParams) (*api.JobCancelResult, error) {
	f.rec("JobCancel", p)
	if f.JobCancelFn != nil {
		return f.JobCancelFn(ctx, cname, p)
	}
	return &api.JobCancelResult{}, nil
}

func (f *fakeBackend) OutputRead(ctx context.Context, cname string, p api.OutputReadParams) (*api.OutputReadResult, error) {
	f.rec("OutputRead", p)
	if f.OutputReadFn != nil {
		return f.OutputReadFn(ctx, cname, p)
	}
	return &api.OutputReadResult{}, nil
}

func (f *fakeBackend) Upload(ctx context.Context, cname, projectDir string, p orch.UploadParams) (*orch.TransferResult, error) {
	f.rec("Upload", p)
	if f.UploadFn != nil {
		return f.UploadFn(ctx, cname, projectDir, p)
	}
	return &orch.TransferResult{}, nil
}

func (f *fakeBackend) Download(ctx context.Context, cname, projectDir string, p orch.DownloadParams) (*orch.TransferResult, error) {
	f.rec("Download", p)
	if f.DownloadFn != nil {
		return f.DownloadFn(ctx, cname, projectDir, p)
	}
	return &orch.TransferResult{}, nil
}
