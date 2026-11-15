package orch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/opconf"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// devConn is a fake backend for the dev-workspace methods: it records every call and keeps a
// tiny in-memory file system for write_file and read_file.
type devConn struct {
	mu     sync.Mutex
	calls  []string
	params []json.RawMessage
	bodies [][]byte
	files  map[string][]byte
	image  []byte // returned (with Length) by an fs read
}

func newDevConn() *devConn { return &devConn{files: map[string][]byte{}} }

func (d *devConn) rec(method string, params any, body []byte) json.RawMessage {
	b, _ := json.Marshal(params)
	d.mu.Lock()
	d.calls = append(d.calls, method)
	d.params = append(d.params, b)
	d.bodies = append(d.bodies, body)
	d.mu.Unlock()
	return b
}

func (d *devConn) last() (string, json.RawMessage, []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	i := len(d.calls) - 1
	return d.calls[i], d.params[i], d.bodies[i]
}

func (d *devConn) Call(_ context.Context, method string, params, result any) error {
	d.rec(method, params, nil)
	switch r := result.(type) {
	case *api.JobStartResult:
		r.Job = api.JobResult{Job: "j1", State: transport.JobRunning}
	case *api.JobStatusResult:
		r.Jobs = []api.JobResult{{Job: "j1", State: transport.JobRunning}}
	case *api.JobTailResult:
		r.Job = api.JobResult{Job: "j1", State: transport.JobRunning, Output: "hi\n"}
	case *api.JobCancelResult:
		r.Job = api.JobResult{Job: "j1", State: transport.JobCanceled}
	case *api.OutputReadResult:
		r.Content = "page"
	case *api.FSResult:
		r.Kind, r.Content = transport.KindText, "text"
	}
	return nil
}

func (d *devConn) CallBody(_ context.Context, method string, params any, body []byte, result any) error {
	raw := d.rec(method, params, body)
	switch method {
	case api.MethodWorkspaceWriteFile:
		var p api.WorkspaceWriteFileParams
		_ = json.Unmarshal(raw, &p)
		sum := sha256.Sum256(body)
		if p.SHA256 != hex.EncodeToString(sum[:]) || p.Length != len(body) {
			return &api.Error{Code: api.CodeBadRequest, Message: "bad checksum"}
		}
		cur := d.files[p.Path]
		if p.Truncate {
			cur = nil
		}
		if int64(len(cur)) != p.Offset {
			return &api.Error{Code: api.CodeBadRequest, Message: "bad offset"}
		}
		d.files[p.Path] = append(cur, body...)
		result.(*api.WorkspaceWriteFileResult).Size = int64(len(d.files[p.Path]))
	case api.MethodWorkspaceFS:
		result.(*api.FSResult).Kind = transport.KindNone
	}
	return nil
}

func (d *devConn) CallReadBody(_ context.Context, method string, params, result any) ([]byte, error) {
	raw := d.rec(method, params, nil)
	var p api.WorkspaceReadFileParams
	_ = json.Unmarshal(raw, &p)
	data, ok := d.files[p.Path]
	if !ok {
		return nil, &api.Error{Code: api.CodeNotFound, Message: "no such file"}
	}
	end := p.Offset + int64(api.MaxFileChunk)
	if end > int64(len(data)) {
		end = int64(len(data))
	}
	chunk := data[p.Offset:end]
	*result.(*api.WorkspaceReadFileResult) = api.WorkspaceReadFileResult{
		Size: int64(len(data)), Offset: p.Offset, Length: len(chunk), Mode: 0o644, EOF: end == int64(len(data)),
	}
	return chunk, nil
}

func (d *devConn) CallReadBodyIf(_ context.Context, method string, params, result any, hasBody func() bool) ([]byte, error) {
	d.rec(method, params, nil)
	r := result.(*api.FSResult)
	if d.image != nil {
		r.Kind, r.Length = transport.KindImage, int64(len(d.image))
	} else {
		r.Kind, r.Content = transport.KindText, "     1\tx"
	}
	if hasBody() {
		return d.image, nil
	}
	return nil, nil
}

func (d *devConn) Ping(context.Context) error { return nil }
func (d *devConn) Close() error               { return nil }

func devOrch(t *testing.T) (*Orch, *devConn) {
	t.Helper()
	dc := newDevConn()
	o := newOrch(t, setup(t, opconf.Context{Name: "a", Addr: "127.0.0.1:1"}),
		&dialer{mk: func(int) (Conn, error) { return dc, nil }})
	return o, dc
}

const wsID = "ws1"

func TestWorkspaceFSOps(t *testing.T) {
	o, dc := devOrch(t)
	ctx := context.Background()

	r, _, err := o.WorkspaceFS(ctx, "a", api.FSParams{Workspace: wsID, FSOp: transport.FSOp{Op: transport.FSStat, Path: "x"}}, nil)
	if err != nil || r.Kind != transport.KindText {
		t.Fatalf("%+v %v", r, err)
	}
	if m, _, _ := dc.last(); m != api.MethodWorkspaceFS {
		t.Fatal(m)
	}

	// write: Length and SHA256 are filled in and the body travels.
	body := []byte("hello")
	if _, _, err = o.WorkspaceFS(ctx, "a", api.FSParams{Workspace: wsID, FSOp: transport.FSOp{Op: transport.FSWrite, Path: "f"}}, body); err != nil {
		t.Fatal(err)
	}
	_, raw, got := dc.last()
	var p api.FSParams
	_ = json.Unmarshal(raw, &p)
	sum := sha256.Sum256(body)
	if string(got) != "hello" || p.Length != 5 || p.SHA256 != hex.EncodeToString(sum[:]) || p.Op != "write" {
		t.Fatalf("%s %q", raw, got)
	}

	// read of an image returns the raw bytes; a text read has no body.
	dc.image = []byte("PNG")
	r, b, err := o.WorkspaceFS(ctx, "a", api.FSParams{Workspace: wsID, FSOp: transport.FSOp{Op: transport.FSRead, Path: "i.png"}}, nil)
	if err != nil || string(b) != "PNG" || r.Kind != transport.KindImage {
		t.Fatalf("%+v %q %v", r, b, err)
	}
	dc.image = nil
	if _, b, err = o.WorkspaceFS(ctx, "a", api.FSParams{Workspace: wsID, FSOp: transport.FSOp{Op: transport.FSRead, Path: "t"}}, nil); err != nil || b != nil {
		t.Fatalf("%q %v", b, err)
	}

	// invalid calls never reach the wire.
	n := len(dc.calls)
	for name, c := range map[string]struct {
		p    api.FSParams
		data []byte
	}{
		"unknown op":   {api.FSParams{Workspace: wsID, FSOp: transport.FSOp{Op: "rm", Path: "x"}}, nil},
		"bad ws":       {api.FSParams{Workspace: "a b", FSOp: transport.FSOp{Op: transport.FSStat, Path: "x"}}, nil},
		"body on read": {api.FSParams{Workspace: wsID, FSOp: transport.FSOp{Op: transport.FSRead, Path: "x"}}, []byte("x")},
		"too big":      {api.FSParams{Workspace: wsID, FSOp: transport.FSOp{Op: transport.FSWrite, Path: "x"}}, make([]byte, api.MaxFileChunk+1)},
	} {
		if _, _, err := o.WorkspaceFS(ctx, "a", c.p, c.data); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if len(dc.calls) != n {
		t.Fatalf("invalid calls reached the backend: %v", dc.calls[n:])
	}
}

func TestJobAndOutputCalls(t *testing.T) {
	o, dc := devOrch(t)
	ctx := context.Background()
	want := func(m string) {
		t.Helper()
		if got, _, _ := dc.last(); got != m {
			t.Fatalf("method %q, want %q", got, m)
		}
	}
	if r, err := o.JobStart(ctx, "a", api.JobStartParams{Workspace: wsID, Command: "make", TimeoutS: 60}); err != nil || r.Job.Job != "j1" {
		t.Fatal(r, err)
	}
	want(api.MethodWorkspaceJobStart)
	if r, err := o.JobStatus(ctx, "a", api.JobStatusParams{Workspace: wsID}); err != nil || len(r.Jobs) != 1 {
		t.Fatal(r, err)
	}
	want(api.MethodWorkspaceJobStatus)
	if r, err := o.JobTail(ctx, "a", api.JobTailParams{Workspace: wsID, Job: "j1", Filter: "^E"}); err != nil || r.Job.Output != "hi\n" {
		t.Fatal(r, err)
	}
	want(api.MethodWorkspaceJobTail)
	if r, err := o.JobCancel(ctx, "a", api.JobCancelParams{Workspace: wsID, Job: "j1"}); err != nil || r.Job.State != transport.JobCanceled {
		t.Fatal(r, err)
	}
	want(api.MethodWorkspaceJobCancel)
	if r, err := o.OutputRead(ctx, "a", api.OutputReadParams{Workspace: wsID, OutputOp: transport.OutputOp{Ref: "r1"}}); err != nil || r.Content != "page" {
		t.Fatal(r, err)
	}
	want(api.MethodWorkspaceOutputRead)

	n := len(dc.calls)
	if _, err := o.JobStart(ctx, "a", api.JobStartParams{Workspace: wsID}); err == nil {
		t.Error("empty command accepted")
	}
	if _, err := o.JobTail(ctx, "a", api.JobTailParams{Workspace: wsID, Job: "j1", Filter: "("}); err == nil {
		t.Error("bad filter accepted")
	}
	if _, err := o.JobCancel(ctx, "a", api.JobCancelParams{Workspace: wsID}); err == nil {
		t.Error("cancel without job accepted")
	}
	if _, err := o.OutputRead(ctx, "a", api.OutputReadParams{Workspace: wsID}); err == nil {
		t.Error("output_read without ref or job accepted")
	}
	if len(dc.calls) != n {
		t.Fatal("invalid calls reached the backend")
	}
}

func TestOpenCredentialsAndCloseDelete(t *testing.T) {
	o, dc := devOrch(t)
	ctx := context.Background()
	if _, err := o.WorkspaceOpen(ctx, "a", api.WorkspaceOpenParams{Fleet: "f", Session: "s1", Credentials: []string{"gitlab", "ghcr"}}); err != nil {
		t.Fatal(err)
	}
	if _, raw, _ := dc.last(); !strings.Contains(string(raw), `"credentials":["gitlab","ghcr"]`) {
		t.Fatalf("%s", raw)
	}
	for _, bad := range [][]string{{"a b"}, {"x", "x"}, {""}} {
		if _, err := o.WorkspaceOpen(ctx, "a", api.WorkspaceOpenParams{Fleet: "f", Session: "s1", Credentials: bad}); err == nil {
			t.Errorf("credentials %q accepted", bad)
		}
	}
	if _, err := o.WorkspaceCloseWith(ctx, "a", api.WorkspaceCloseParams{Workspace: wsID, Delete: true}); err != nil {
		t.Fatal(err)
	}
	if m, raw, _ := dc.last(); m != api.MethodWorkspaceClose || !strings.Contains(string(raw), `"delete":true`) {
		t.Fatalf("%s %s", m, raw)
	}
	if _, err := o.WorkspaceClose(ctx, "a", wsID); err != nil {
		t.Fatal(err)
	}
	if _, raw, _ := dc.last(); strings.Contains(string(raw), "delete") {
		t.Fatalf("plain close must not delete: %s", raw)
	}
}

func setupOne(t *testing.T) string {
	return setup(t, opconf.Context{Name: "a", Addr: "127.0.0.1:1"})
}

// sizeLiar reports an oversize file on read_file.
type sizeLiar struct{ *devConn }

func (s *sizeLiar) CallReadBody(ctx context.Context, m string, p, res any) ([]byte, error) {
	b, err := s.devConn.CallReadBody(ctx, m, p, res)
	res.(*api.WorkspaceReadFileResult).Size = MaxTransferBytes + 1
	return b, err
}
