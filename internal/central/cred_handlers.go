package central

import (
	"context"
	"io"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// Dev-workspace and credential handlers: the host-workspace operator methods (workspace.fs, job_*,
// output_read) on api@, and the credential admin methods on the admin socket. Like the M3
// handlers they decode strictly and call the service, which gates before any body is read.
// credential.set carries its secret as one body frame; it never appears in params or a result.

func handleWorkspaceFS(ctx context.Context, c *callCtx, req *api.Request) (any, error) {
	var p api.FSParams
	if e := api.DecodeParams(req.Params, &p); e != nil {
		return nil, e
	}
	if c.body == nil {
		return nil, api.Errorf(api.CodeBadRequest, "files are not supported on this channel")
	}
	var in io.Reader
	var lb *lazyFileBody
	if p.Op == transport.FSWrite {
		if p.Length < 0 || p.Length > api.MaxFileChunk {
			return nil, api.Errorf(api.CodeBadRequest, "invalid body length")
		}
		lb = &lazyFileBody{b: c.body, length: int(p.Length)}
		in = lb
	}
	out := &capWriter{max: api.MaxFileChunk}
	res, err := c.svc.FSWorkspace(ctx, c.p, c.src, p, in, out)
	if err != nil {
		if lb != nil && lb.err != nil {
			return nil, lb.err
		}
		return nil, err
	}
	if res.Length > 0 { // an image: its raw bytes follow the result frame
		c.body.out, c.body.hasOut = out.buf.Bytes(), true
	}
	return res, nil
}

func handleJobStart(ctx context.Context, c *callCtx, req *api.Request) (any, error) {
	var p api.JobStartParams
	if e := api.DecodeParams(req.Params, &p); e != nil {
		return nil, e
	}
	return c.svc.JobStart(ctx, c.p, c.src, p)
}

func handleJobStatus(ctx context.Context, c *callCtx, req *api.Request) (any, error) {
	var p api.JobStatusParams
	if e := api.DecodeParams(req.Params, &p); e != nil {
		return nil, e
	}
	return c.svc.JobStatus(ctx, c.p, c.src, p)
}

func handleJobTail(ctx context.Context, c *callCtx, req *api.Request) (any, error) {
	var p api.JobTailParams
	if e := api.DecodeParams(req.Params, &p); e != nil {
		return nil, e
	}
	return c.svc.JobTail(ctx, c.p, c.src, p)
}

func handleJobCancel(ctx context.Context, c *callCtx, req *api.Request) (any, error) {
	var p api.JobCancelParams
	if e := api.DecodeParams(req.Params, &p); e != nil {
		return nil, e
	}
	return c.svc.JobCancel(ctx, c.p, c.src, p)
}

func handleOutputRead(ctx context.Context, c *callCtx, req *api.Request) (any, error) {
	var p api.OutputReadParams
	if e := api.DecodeParams(req.Params, &p); e != nil {
		return nil, e
	}
	return c.svc.OutputRead(ctx, c.p, c.src, p)
}

// ---- credentials (admin socket) ----

func handleCredentialSet(ctx context.Context, c *callCtx, req *api.Request) (any, error) {
	var p api.CredentialSetParams
	if e := api.DecodeParams(req.Params, &p); e != nil {
		return nil, e
	}
	if e := p.Validate(); e != nil {
		return nil, e
	}
	if c.body == nil {
		return nil, api.Errorf(api.CodeBadRequest, "no body frame on this channel")
	}
	value, err := c.body.readBody(api.MaxCredentialValue)
	if err != nil {
		return nil, err
	}
	defer clear(value)
	return c.svc.SetCredential(ctx, c.p, c.src, p, value)
}

func handleCredentialList(ctx context.Context, c *callCtx, req *api.Request) (any, error) {
	var p api.CredentialListParams
	if e := api.DecodeParams(req.Params, &p); e != nil {
		return nil, e
	}
	if e := p.Validate(); e != nil {
		return nil, e
	}
	cs, err := c.svc.ListCredentials(ctx, c.p, c.src, p.Name)
	if err != nil {
		return nil, err
	}
	return api.CredentialListResult{Credentials: cs}, nil
}

func handleCredentialDelete(ctx context.Context, c *callCtx, req *api.Request) (any, error) {
	var p api.CredentialDeleteParams
	if e := api.DecodeParams(req.Params, &p); e != nil {
		return nil, e
	}
	if e := p.Validate(); e != nil {
		return nil, e
	}
	return struct{}{}, c.svc.DeleteCredential(ctx, c.p, c.src, p.Name)
}

func handleCredentialAssign(ctx context.Context, c *callCtx, req *api.Request) (any, error) {
	var p api.CredentialAssignParams
	if e := api.DecodeParams(req.Params, &p); e != nil {
		return nil, e
	}
	info, err := c.svc.AssignCredential(ctx, c.p, c.src, p)
	if err != nil {
		return nil, err
	}
	return api.CredentialResult{Credential: info}, nil
}

func handleCredentialUnassign(ctx context.Context, c *callCtx, req *api.Request) (any, error) {
	var p api.CredentialUnassignParams
	if e := api.DecodeParams(req.Params, &p); e != nil {
		return nil, e
	}
	info, err := c.svc.UnassignCredential(ctx, c.p, c.src, p)
	if err != nil {
		return nil, err
	}
	return api.CredentialResult{Credential: info}, nil
}

func handleCredentialRotate(ctx context.Context, c *callCtx, req *api.Request) (any, error) {
	var p api.CredentialRotateParams
	if e := api.DecodeParams(req.Params, &p); e != nil {
		return nil, e
	}
	if e := p.Validate(); e != nil {
		return nil, e
	}
	info, err := c.svc.RotateCredential(ctx, c.p, c.src, p.Name)
	if err != nil {
		return nil, err
	}
	return api.CredentialResult{Credential: info}, nil
}
