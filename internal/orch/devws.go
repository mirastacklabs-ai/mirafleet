package orch

// Typed calls for the dev-workspace methods (docs/M7.md §3.5-§3.7). Each validates its params
// locally (the same Validate central runs) so a bad call never leaves the machine.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

func validErr(e *api.Error) error {
	if e == nil {
		return nil
	}
	return &Error{Code: e.Code, Message: clean(e.Message)}
}

// WorkspaceFS runs one file-tool call. For op write, data is the file content (at most
// api.MaxFileChunk bytes); Length and SHA256 are set from it. For op read of an image the raw
// bytes come back as the second value. Never retried (a write may already have taken effect).
func (o *Orch) WorkspaceFS(ctx context.Context, cname string, p api.FSParams, data []byte) (*api.FSResult, []byte, error) {
	if len(data) > api.MaxFileChunk {
		return nil, nil, errf(api.CodeTooLarge, nil, fmt.Sprintf("a write is at most %d bytes", api.MaxFileChunk))
	}
	isWrite := p.Op == transport.FSWrite
	if !isWrite && len(data) > 0 {
		return nil, nil, errf(api.CodeBadRequest, nil, "only write takes a body")
	}
	if isWrite {
		sum := sha256.Sum256(data)
		p.Length, p.SHA256 = int64(len(data)), hex.EncodeToString(sum[:])
	}
	if e := p.Validate(); e != nil {
		return nil, nil, validErr(e)
	}
	var r api.FSResult
	x := &callIO{result: &r}
	if isWrite {
		x.body, x.hasBody = data, true
	} else if p.Op == transport.FSRead {
		x.bodyIf = func() bool { return r.Length > 0 }
	}
	if err := o.call(ctx, cname, api.MethodWorkspaceFS, p, x); err != nil {
		return nil, nil, err
	}
	return &r, x.got, nil
}

// JobStart starts a background shell. Never retried.
func (o *Orch) JobStart(ctx context.Context, cname string, p api.JobStartParams) (*api.JobStartResult, error) {
	if e := p.Validate(); e != nil {
		return nil, validErr(e)
	}
	var r api.JobStartResult
	if err := o.call(ctx, cname, api.MethodWorkspaceJobStart, p, &callIO{result: &r}); err != nil {
		return nil, err
	}
	return &r, nil
}

// JobStatus describes one job, or lists the session's jobs when p.Job is empty.
func (o *Orch) JobStatus(ctx context.Context, cname string, p api.JobStatusParams) (*api.JobStatusResult, error) {
	if e := p.Validate(); e != nil {
		return nil, validErr(e)
	}
	var r api.JobStatusResult
	if err := o.call(ctx, cname, api.MethodWorkspaceJobStatus, p, &callIO{result: &r}); err != nil {
		return nil, err
	}
	return &r, nil
}

// JobTail returns the output a job produced since the previous tail. Never retried: a tail
// advances the cursor.
func (o *Orch) JobTail(ctx context.Context, cname string, p api.JobTailParams) (*api.JobTailResult, error) {
	if e := p.Validate(); e != nil {
		return nil, validErr(e)
	}
	var r api.JobTailResult
	if err := o.call(ctx, cname, api.MethodWorkspaceJobTail, p, &callIO{result: &r}); err != nil {
		return nil, err
	}
	return &r, nil
}

// JobCancel kills a job's process group. Never retried.
func (o *Orch) JobCancel(ctx context.Context, cname string, p api.JobCancelParams) (*api.JobCancelResult, error) {
	if e := p.Validate(); e != nil {
		return nil, validErr(e)
	}
	var r api.JobCancelResult
	if err := o.call(ctx, cname, api.MethodWorkspaceJobCancel, p, &callIO{result: &r}); err != nil {
		return nil, err
	}
	return &r, nil
}

// OutputRead pages saved output by ref or job.
func (o *Orch) OutputRead(ctx context.Context, cname string, p api.OutputReadParams) (*api.OutputReadResult, error) {
	if e := p.Validate(); e != nil {
		return nil, validErr(e)
	}
	var r api.OutputReadResult
	if err := o.call(ctx, cname, api.MethodWorkspaceOutputRead, p, &callIO{result: &r}); err != nil {
		return nil, err
	}
	return &r, nil
}
