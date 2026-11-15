package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

const decoyContent = "DECOY-FILE-CONTENT-0123456789"

type wsFn = func(context.Context, int64, transport.WorkspaceRequest, io.Reader, io.Writer) (*transport.WorkspaceResult, error)

func (e *env) hostFleet(name string, confirm bool) {
	e.t.Helper()
	if _, err := e.svc.CreateFleet(e.ctx, e.admin, SourceCLI, name, store.ModeWorkspace, confirm,
		&store.WorkspaceConfig{Executor: store.ExecutorHost}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) hostRunner(fleet string) store.Runner {
	return e.wsRunner(fleet, rspec{slots: 4, exec: "host"})
}

// hostEnv: one host fleet, one runner, ann with an open workspace.
func hostEnv(t *testing.T, guarded bool) (*env, auth.Principal, store.Workspace, string) {
	t.Helper()
	e := newEnv(t)
	e.hostFleet("vm", guarded)
	e.hostRunner("vm")
	op := e.operator("ann")
	confirm := ""
	if guarded {
		confirm = "vm"
	}
	w, _, err := e.svc.OpenWorkspace(e.ctx, op, SourceAPI, "vm", "s1", "", nil, confirm)
	if err != nil {
		t.Fatal(err)
	}
	return e, op, w, confirm
}

func TestHostFleetDefaultsAndOpen(t *testing.T) {
	e, _, w, _ := hostEnv(t, false)
	f, _ := e.db.GetFleetWorkspace(e.ctx, "vm")
	if f.Executor != store.ExecutorHost || f.DefaultImage != "" || len(f.Images) != 0 || f.CPUMilli != 0 || f.IdleTTL == 0 || f.ExecMax == 0 {
		t.Fatalf("host defaults: %+v", f)
	}
	if w.Image != hostImage || !strings.Contains(w.Spec, `"image":"host"`) {
		t.Fatalf("row: %+v", w)
	}
	op := e.operator("bob")
	if _, _, err := e.svc.OpenWorkspace(e.ctx, op, SourceAPI, "vm", "x", "docker.io/library/golang:1.27", nil, ""); code(t, err) != api.CodeBadRequest {
		t.Error("image on a host fleet accepted")
	}
	// A container fleet cannot be given host-only limits.
	if _, err := e.svc.CreateFleet(e.ctx, e.admin, SourceCLI, "bad", store.ModeWorkspace, false,
		&store.WorkspaceConfig{Executor: store.ExecutorHost, DefaultImage: imgGo}); err == nil {
		t.Error("host fleet with image accepted")
	}
}

func TestOpenWithWorkdir(t *testing.T) {
	e := newEnv(t)
	e.hostFleet("vm2", false)
	e.hostRunner("vm2")
	bob := e.operator("bob")
	w, _, err := e.svc.OpenWorkspaceIn(e.ctx, bob, SourceAPI, "vm2", "s9", "", nil, "", "shared-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(w.Spec, `"mirafleet.workdir":"shared-1"`) {
		t.Fatalf("spec lacks the workdir label: %s", w.Spec)
	}
	for _, bad := range []string{"../etc", "/abs", "A B"} {
		if _, _, err := e.svc.OpenWorkspaceIn(e.ctx, bob, SourceAPI, "vm2", "s10", "", nil, "", bad); code(t, err) != api.CodeBadRequest {
			t.Errorf("workdir %q accepted", bad)
		}
	}
}

func TestFSDispatchEveryOp(t *testing.T) {
	e, op, w, _ := hostEnv(t, false)
	var got []transport.WorkspaceRequest
	var body []byte
	e.live.wsFn = func(_ context.Context, _ int64, req transport.WorkspaceRequest, b io.Reader, out io.Writer) (*transport.WorkspaceResult, error) {
		got = append(got, req)
		if req.Op != transport.OpFS {
			return &transport.WorkspaceResult{Code: transport.CodeOK}, nil
		}
		if req.StdinLen > 0 {
			body, _ = io.ReadAll(b)
		}
		fr := &transport.FSResult{Kind: transport.KindText, Content: "ok"}
		if req.FS.Op == transport.FSRead && req.FS.Path == "pic.png" {
			png := []byte("\x89PNG-bytes")
			out.Write(png)
			fr = &transport.FSResult{Kind: transport.KindImage, MIME: "image/png", Length: int64(len(png))}
		}
		return &transport.WorkspaceResult{Code: transport.CodeOK, FS: fr}, nil
	}
	content := []byte(decoyContent)
	ops := []transport.FSOp{
		{Op: transport.FSRead, Path: "a.go", Limit: 10},
		{Op: transport.FSWrite, Path: "a.go", Length: int64(len(content)), SHA256: sum(content)},
		{Op: transport.FSEdit, Path: "a.go", OldString: decoyContent, NewString: "x"},
		{Op: transport.FSMultiEdit, Path: "a.go", Edits: []transport.FSEditItem{{OldString: "a", NewString: "b"}}},
		{Op: transport.FSNotebookEdit, Path: "n.ipynb", CellID: "c1", NewSource: decoyContent, EditMode: transport.NotebookReplace},
		{Op: transport.FSGrep, Pattern: "TODO"},
		{Op: transport.FSGlob, Pattern: "**/*.go"},
		{Op: transport.FSLS, Path: "."},
		{Op: transport.FSStat, Path: "a.go"},
	}
	for _, o := range ops {
		var rd io.Reader
		if o.Op == transport.FSWrite {
			rd = bytes.NewReader(content)
		}
		res, err := e.svc.FSWorkspace(e.ctx, op, SourceAPI, api.FSParams{Workspace: w.WsID, FSOp: o}, rd, nil)
		if err != nil || res.Kind != transport.KindText {
			t.Fatalf("%s: %+v %v", o.Op, res, err)
		}
		last := got[len(got)-1]
		if last.Op != transport.OpFS || last.ID != w.WsID || last.FS == nil || last.FS.Op != o.Op {
			t.Fatalf("%s: request %+v", o.Op, last)
		}
	}
	if string(body) != decoyContent {
		t.Fatalf("write body %q", body)
	}
	// Image: raw bytes reach out.
	var out bytes.Buffer
	res, err := e.svc.FSWorkspace(e.ctx, op, SourceAPI, api.FSParams{Workspace: w.WsID, FSOp: transport.FSOp{Op: transport.FSRead, Path: "pic.png"}}, nil, &out)
	if err != nil || res.Kind != transport.KindImage || out.String() != "\x89PNG-bytes" {
		t.Fatalf("image: %+v %v %q", res, err, out.String())
	}
	// Audit: paths and sizes, never content.
	if txt := e.auditText(); strings.Contains(txt, decoyContent) {
		t.Fatal("file content leaked into the audit log")
	}
	rows := e.wsAudit(api.MethodWorkspaceFS)
	if len(rows) != 2*(len(ops)+1) || rows[0].Outcome != OutcomeStart || rows[1].Outcome != OutcomeOK || rows[0].Params["op"] != "read" {
		t.Fatalf("audit rows: %d %+v", len(rows), rows[0])
	}
	if p := rows[2].Params; p["sha256"] != sum(content) || p["bytes"] != fmt.Sprint(len(content)) || p["path"] != "a.go" {
		t.Fatalf("write params: %+v", p)
	}
}

func TestFSWriteIntegrityAndInputs(t *testing.T) {
	e, op, w, _ := hostEnv(t, false)
	content := []byte("abc")
	e.live.log.reset()
	bad := api.FSParams{Workspace: w.WsID, FSOp: transport.FSOp{Op: transport.FSWrite, Path: "a", Length: 3, SHA256: sum([]byte("xyz"))}}
	if c := code(t, errOf(e.svc.FSWorkspace(e.ctx, op, SourceAPI, bad, bytes.NewReader(content), nil))); c != api.CodeBadRequest {
		t.Errorf("sha mismatch: %q", c)
	}
	short := api.FSParams{Workspace: w.WsID, FSOp: transport.FSOp{Op: transport.FSWrite, Path: "a", Length: 5, SHA256: sum(content)}}
	if c := code(t, errOf(e.svc.FSWorkspace(e.ctx, op, SourceAPI, short, bytes.NewReader(content), nil))); c != api.CodeBadRequest {
		t.Errorf("short body: %q", c)
	}
	unk := api.FSParams{Workspace: w.WsID, FSOp: transport.FSOp{Op: "rm", Path: "a"}}
	if c := code(t, errOf(e.svc.FSWorkspace(e.ctx, op, SourceAPI, unk, nil, nil))); c != api.CodeBadRequest {
		t.Errorf("unknown op: %q", c)
	}
	if n := len(e.wsCalls(transport.OpFS)); n != 0 {
		t.Fatalf("runner contacted %d times for refused calls", n)
	}
	// Missing result is an internal error, not an empty success.
	e.live.wsFn = func(context.Context, int64, transport.WorkspaceRequest, io.Reader, io.Writer) (*transport.WorkspaceResult, error) {
		return &transport.WorkspaceResult{Code: transport.CodeOK}, nil
	}
	ok := api.FSParams{Workspace: w.WsID, FSOp: transport.FSOp{Op: transport.FSStat, Path: "a"}}
	if c := code(t, errOf(e.svc.FSWorkspace(e.ctx, op, SourceAPI, ok, nil, nil))); c != api.CodeInternal {
		t.Errorf("no fs result: %q", c)
	}
	// Another operator cannot reach the workspace.
	bob := e.operator("bob")
	if c := code(t, errOf(e.svc.FSWorkspace(e.ctx, bob, SourceAPI, ok, nil, nil))); c != api.CodeNotFound {
		t.Errorf("non-owner: %q", c)
	}
}

func TestGuardedHostFleetConfirmOnly(t *testing.T) {
	e, op, w, confirm := hostEnv(t, true) // opened with confirm; no secure runtime needed
	e.live.wsFn = func(_ context.Context, _ int64, req transport.WorkspaceRequest, _ io.Reader, _ io.Writer) (*transport.WorkspaceResult, error) {
		switch req.Op {
		case transport.OpFS:
			return &transport.WorkspaceResult{Code: transport.CodeOK, FS: &transport.FSResult{Kind: transport.KindNone}}, nil
		case transport.OpJobStart, transport.OpJobTail, transport.OpJobCancel:
			return &transport.WorkspaceResult{Code: transport.CodeOK, Job: &transport.JobResult{Job: "j1", State: transport.JobRunning}}, nil
		case transport.OpJobStatus:
			return &transport.WorkspaceResult{Code: transport.CodeOK, Jobs: []transport.JobResult{{Job: "j1", State: transport.JobRunning}}}, nil
		}
		return &transport.WorkspaceResult{Code: transport.CodeOK}, nil
	}
	mut := []transport.FSOp{
		{Op: transport.FSWrite, Path: "a", Length: 0, SHA256: sum(nil)},
		{Op: transport.FSEdit, Path: "a", OldString: "a", NewString: "b"},
		{Op: transport.FSMultiEdit, Path: "a", Edits: []transport.FSEditItem{{OldString: "a", NewString: "b"}}},
		{Op: transport.FSNotebookEdit, Path: "n.ipynb", CellID: "c", NewSource: "x", EditMode: transport.NotebookReplace},
	}
	for _, o := range mut {
		var rd io.Reader
		p := api.FSParams{Workspace: w.WsID, FSOp: o}
		before := len(e.wsCalls(transport.OpFS))
		for _, bad := range []string{"", "other"} {
			p.Confirm = bad
			err := errOf(e.svc.FSWorkspace(e.ctx, op, SourceAPI, p, rd, nil))
			if code(t, err) != api.CodeBadRequest || !strings.Contains(err.Error(), api.MsgConfirmRequired) {
				t.Errorf("%s confirm %q: %v", o.Op, bad, err)
			}
		}
		if len(e.wsCalls(transport.OpFS)) != before {
			t.Errorf("%s reached the runner without confirm", o.Op)
		}
		p.Confirm = confirm
		if _, err := e.svc.FSWorkspace(e.ctx, op, SourceAPI, p, rd, nil); err != nil {
			t.Errorf("%s with confirm: %v", o.Op, err)
		}
	}
	for _, o := range []transport.FSOp{{Op: transport.FSRead, Path: "a"}, {Op: transport.FSGrep, Pattern: "x"},
		{Op: transport.FSGlob, Pattern: "*"}, {Op: transport.FSLS, Path: "."}, {Op: transport.FSStat, Path: "a"}} {
		if _, err := e.svc.FSWorkspace(e.ctx, op, SourceAPI, api.FSParams{Workspace: w.WsID, FSOp: o}, nil, nil); err != nil {
			t.Errorf("%s needs no confirm: %v", o.Op, err)
		}
	}
	// job_start is guarded; status, tail, cancel and output are not.
	js := api.JobStartParams{Workspace: w.WsID, Command: "make"}
	if c := code(t, errOf(e.svc.JobStart(e.ctx, op, SourceAPI, js))); c != api.CodeBadRequest {
		t.Errorf("job_start without confirm: %q", c)
	}
	js.Confirm = confirm
	if _, err := e.svc.JobStart(e.ctx, op, SourceAPI, js); err != nil {
		t.Errorf("job_start with confirm: %v", err)
	}
	if _, err := e.svc.JobStatus(e.ctx, op, SourceAPI, api.JobStatusParams{Workspace: w.WsID}); err != nil {
		t.Errorf("job_status: %v", err)
	}
	if _, err := e.svc.JobTail(e.ctx, op, SourceAPI, api.JobTailParams{Workspace: w.WsID, Job: "j1"}); err != nil {
		t.Errorf("job_tail: %v", err)
	}
	if _, err := e.svc.JobCancel(e.ctx, op, SourceAPI, api.JobCancelParams{Workspace: w.WsID, Job: "j1"}); err != nil {
		t.Errorf("job_cancel: %v", err)
	}
	// The refusals are audited as denied.
	denied := 0
	for _, a := range e.wsAudit(api.MethodWorkspaceFS) {
		if a.Outcome == OutcomeDenied {
			denied++
		}
	}
	if denied != 2*len(mut) {
		t.Errorf("%d denied rows, want %d", denied, 2*len(mut))
	}
}

func TestJobsAndOutputDispatch(t *testing.T) {
	e, op, w, _ := hostEnv(t, false)
	var got []transport.WorkspaceRequest
	e.live.wsFn = func(_ context.Context, _ int64, req transport.WorkspaceRequest, _ io.Reader, _ io.Writer) (*transport.WorkspaceResult, error) {
		got = append(got, req)
		j := &transport.JobResult{Job: "j1", State: transport.JobRunning, Output: "line"}
		switch req.Op {
		case transport.OpJobStatus:
			if req.Job.Job == "" {
				return &transport.WorkspaceResult{Code: transport.CodeOK, Jobs: []transport.JobResult{*j}}, nil
			}
		case transport.OpOutputRead:
			return &transport.WorkspaceResult{Code: transport.CodeOK, Output: &transport.OutputResult{Content: "page", Size: 4, EOF: true}}, nil
		}
		return &transport.WorkspaceResult{Code: transport.CodeOK, Job: j}, nil
	}
	later := t0.Add(2 * time.Hour)
	e.svc.now = func() time.Time { return later }
	st, err := e.svc.JobStart(e.ctx, op, SourceAPI, api.JobStartParams{Workspace: w.WsID, Command: "make -j8", Description: "build", TimeoutS: 600})
	if err != nil || st.Job.Job != "j1" || got[0].Op != transport.OpJobStart || got[0].Job.Command != "make -j8" || got[0].Job.TimeoutS != 600 {
		t.Fatalf("start: %+v %v %+v", st, err, got)
	}
	if ws, _ := e.db.GetWorkspace(e.ctx, w.WsID); !ws.LastUsedAt.Equal(later) {
		t.Fatalf("job_start did not touch: %v", ws.LastUsedAt)
	}
	one, err := e.svc.JobStatus(e.ctx, op, SourceAPI, api.JobStatusParams{Workspace: w.WsID, Job: "j1"})
	if err != nil || one.Job == nil || one.Jobs != nil {
		t.Fatalf("status one: %+v %v", one, err)
	}
	all, err := e.svc.JobStatus(e.ctx, op, SourceAPI, api.JobStatusParams{Workspace: w.WsID})
	if err != nil || len(all.Jobs) != 1 || all.Job != nil {
		t.Fatalf("status list: %+v %v", all, err)
	}
	tl, err := e.svc.JobTail(e.ctx, op, SourceAPI, api.JobTailParams{Workspace: w.WsID, Job: "j1", Filter: "err"})
	if err != nil || tl.Job.Output != "line" || got[3].Job.Filter != "err" {
		t.Fatalf("tail: %+v %v", tl, err)
	}
	cc, err := e.svc.JobCancel(e.ctx, op, SourceAPI, api.JobCancelParams{Workspace: w.WsID, Job: "j1"})
	if err != nil || cc.Job.Job != "j1" || got[4].Op != transport.OpJobCancel {
		t.Fatalf("cancel: %+v %v", cc, err)
	}
	or, err := e.svc.OutputRead(e.ctx, op, SourceAPI, api.OutputReadParams{Workspace: w.WsID, OutputOp: transport.OutputOp{Job: "j1", Stream: "stdout", TailLines: 5}})
	if err != nil || or.Content != "page" || got[5].Out.TailLines != 5 {
		t.Fatalf("output: %+v %v", or, err)
	}
	// Only start and cancel are audited when they succeed.
	if n := len(e.wsAudit(api.MethodWorkspaceJobStart)); n != 2 {
		t.Errorf("job_start rows: %d", n)
	}
	if n := len(e.wsAudit(api.MethodWorkspaceJobTail)) + len(e.wsAudit(api.MethodWorkspaceJobStatus)) + len(e.wsAudit(api.MethodWorkspaceOutputRead)); n != 0 {
		t.Errorf("polling reads audited %d rows", n)
	}
	// A failing poll is audited.
	e.live.wsFn = func(context.Context, int64, transport.WorkspaceRequest, io.Reader, io.Writer) (*transport.WorkspaceResult, error) {
		return &transport.WorkspaceResult{Code: transport.CodeNotFound, Message: "no such job"}, nil
	}
	if c := code(t, errOf(e.svc.JobTail(e.ctx, op, SourceAPI, api.JobTailParams{Workspace: w.WsID, Job: "zz"}))); c != api.CodeNotFound {
		t.Errorf("unknown job: %q", c)
	}
	if rows := e.wsAudit(api.MethodWorkspaceJobTail); len(rows) != 1 || rows[0].Outcome != OutcomeError {
		t.Errorf("failed tail rows: %+v", rows)
	}
}

func TestDevOpsRefusedOnContainerFleet(t *testing.T) {
	e, op, w := opsEnv(t, false, nil)
	if c := code(t, errOf(e.svc.FSWorkspace(e.ctx, op, SourceAPI, api.FSParams{Workspace: w.WsID, FSOp: transport.FSOp{Op: transport.FSStat, Path: "a"}}, nil, nil))); c != api.CodeBadRequest {
		t.Errorf("fs: %q", c)
	}
	if c := code(t, errOf(e.svc.JobStatus(e.ctx, op, SourceAPI, api.JobStatusParams{Workspace: w.WsID}))); c != api.CodeBadRequest {
		t.Errorf("job_status: %q", c)
	}
	if c := code(t, errOf(e.svc.CloseWorkspaceDelete(e.ctx, op, SourceAPI, w.WsID, true))); c != api.CodeBadRequest {
		t.Errorf("close -delete: %q", c)
	}
	if c := code(t, errOf(e.svc.KillWorkspaceDelete(e.ctx, e.admin, SourceCLI, w.WsID, true))); c != api.CodeBadRequest {
		t.Errorf("kill -delete: %q", c)
	}
}

func TestRunnerHeldByOneOperator(t *testing.T) {
	e := newEnv(t)
	e.hostFleet("vm", false)
	e.hostRunner("vm")
	ann, bob := e.operator("ann"), e.operator("bob")
	w1, _, err := e.svc.OpenWorkspace(e.ctx, ann, SourceAPI, "vm", "s1", "", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	w2, _, err := e.svc.OpenWorkspace(e.ctx, ann, SourceAPI, "vm", "s2", "", nil, "")
	if err != nil {
		t.Fatalf("same operator, second session: %v", err)
	}
	_, _, err = e.svc.OpenWorkspace(e.ctx, bob, SourceAPI, "vm", "b1", "", nil, "")
	if c := code(t, err); c != api.CodeConflict || err.(*api.Error).Message != "runner busy, in use by ann" {
		t.Fatalf("bob: %v", err)
	}
	if rows := e.wsAudit(api.MethodWorkspaceOpen); rows[len(rows)-1].Outcome != OutcomeDenied {
		t.Errorf("busy refusal not audited: %+v", rows[len(rows)-1])
	}
	// A second runner is free for bob; placement skips the held one.
	r2 := e.hostRunner("vm")
	wb, _, err := e.svc.OpenWorkspace(e.ctx, bob, SourceAPI, "vm", "b1", "", nil, "")
	if err != nil || wb.RunnerID != r2.ID {
		t.Fatalf("bob on the free runner: %+v %v", wb, err)
	}
	e.live.snap = e.live.snap[:1] // only the held runner remains
	// Close ann's workspaces: closing still holds until the destroy finished, then it releases.
	e.live.wsFn = func(context.Context, int64, transport.WorkspaceRequest, io.Reader, io.Writer) (*transport.WorkspaceResult, error) {
		return nil, fmt.Errorf("offline")
	}
	if c, err := e.svc.CloseWorkspace(e.ctx, ann, SourceAPI, w1.WsID); err != nil || c.State != store.WorkspaceClosing {
		t.Fatalf("close with offline runner: %+v %v", c, err)
	}
	e.live.wsFn = nil
	if _, _, err := e.svc.OpenWorkspace(e.ctx, bob, SourceAPI, "vm", "b2", "", nil, ""); code(t, err) != api.CodeConflict {
		t.Fatalf("closing workspaces must still hold the runner: %v", err)
	}
	for _, id := range []string{w1.WsID, w2.WsID} {
		if _, err := e.svc.CloseWorkspace(e.ctx, ann, SourceAPI, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := e.svc.OpenWorkspace(e.ctx, bob, SourceAPI, "vm", "b2", "", nil, ""); err != nil {
		t.Fatalf("released after ann's last workspace closed: %v", err)
	}
	// And ann is now the one refused.
	_, _, err = e.svc.OpenWorkspace(e.ctx, ann, SourceAPI, "vm", "s9", "", nil, "")
	if err == nil || err.(*api.Error).Message != "runner busy, in use by bob" {
		t.Fatalf("ann after bob took it: %v", err)
	}
}

func TestConcurrentOpensOnOneHostRunner(t *testing.T) {
	e := newEnv(t)
	e.hostFleet("vm", false)
	e.hostRunner("vm")
	ops := []auth.Principal{e.operator("ann"), e.operator("bob"), e.operator("cy"), e.operator("di")}
	errs := make([]error, len(ops))
	var wg sync.WaitGroup
	for i := range ops {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, errs[i] = e.svc.OpenWorkspace(e.ctx, ops[i], SourceAPI, "vm", "race", "", nil, "")
		}()
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		if err == nil {
			won++
		} else if !strings.HasPrefix(err.(*api.Error).Message, "runner busy, in use by ") {
			t.Errorf("loser: %v", err)
		}
	}
	if won != 1 {
		t.Fatalf("%d winners, want exactly 1", won)
	}
}

func TestKillAndCloseWithDelete(t *testing.T) {
	e, op, w, _ := hostEnv(t, false)
	var del []bool
	e.live.wsFn = func(_ context.Context, _ int64, req transport.WorkspaceRequest, _ io.Reader, _ io.Writer) (*transport.WorkspaceResult, error) {
		if req.Op == transport.OpDestroy {
			del = append(del, req.Delete)
		}
		return &transport.WorkspaceResult{Code: transport.CodeOK}, nil
	}
	k, err := e.svc.KillWorkspaceDelete(e.ctx, e.admin, SourceCLI, w.WsID, true)
	if err != nil || k.State != store.WorkspaceClosed {
		t.Fatalf("kill -delete: %+v %v", k, err)
	}
	if rows := e.wsAudit(api.MethodWorkspaceKill); rows[0].Params["delete"] != "true" {
		t.Errorf("kill start row: %+v", rows[0].Params)
	}
	// Operators may not kill; plain kill and close do not delete.
	if c := code(t, errOf(e.svc.KillWorkspaceDelete(e.ctx, op, SourceAPI, w.WsID, true))); c != api.CodeForbidden {
		t.Errorf("operator kill: %q", c)
	}
	w2, _, err := e.svc.OpenWorkspace(e.ctx, op, SourceAPI, "vm", "s2", "", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CloseWorkspace(e.ctx, op, SourceAPI, w2.WsID); err != nil {
		t.Fatal(err)
	}
	w3, _, _ := e.svc.OpenWorkspace(e.ctx, op, SourceAPI, "vm", "s3", "", nil, "")
	if _, err := e.svc.CloseWorkspaceDelete(e.ctx, op, SourceAPI, w3.WsID, true); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(del) != "[true false true]" {
		t.Fatalf("destroy delete flags: %v", del)
	}
}

func TestIngestRunnerEvents(t *testing.T) {
	e := newEnv(t)
	e.hostFleet("vm", false)
	r := e.hostRunner("vm")
	op := e.operator("ann")
	w, _, err := e.svc.OpenWorkspace(e.ctx, op, SourceAPI, "vm", "s1", "", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	b := &transport.EventBatch{Dropped: 7, Events: []transport.Event{
		{Type: transport.EventCredentialUse, Session: w.WsID, Credential: "gitlab-ssh", Host: "gitlab.corp", Status: transport.EventOK, Count: 12, WindowMs: 1000, WindowS: 60},
		{Type: transport.EventCredentialUse, Session: "s1", Credential: "reg", Host: "registry.corp:443", Status: transport.EventDenied, Count: 1, WindowS: 60},
	}}
	if err := e.svc.IngestRunnerEvents(e.ctx, r.ID, b); err != nil {
		t.Fatal(err)
	}
	use := e.wsAudit(ActionCredentialUse)
	if len(use) != 2 {
		t.Fatalf("credential.use rows: %+v", use)
	}
	a := use[0]
	if a.Source != "runner" || a.ActorKind != "runner" || a.Runner != r.Name || a.Fleet != "vm" || a.Subject != "gitlab-ssh" ||
		a.Outcome != OutcomeOK || a.Workspace != w.WsID || a.Params["count"] != "12" || a.Params["host"] != "gitlab.corp" {
		t.Fatalf("row: %+v", a)
	}
	if use[1].Outcome != OutcomeDenied || use[1].Workspace != "" || use[1].Params["session"] != "s1" {
		t.Fatalf("denied row: %+v", use[1])
	}
	if d := e.wsAudit(ActionEventsDropped); len(d) != 1 || d[0].Params["dropped"] != "7" || d[0].Source != "runner" {
		t.Fatalf("dropped row: %+v", d)
	}
	// Invalid batches write nothing; the batch's own fleet/runner claims do not exist to spoof.
	before := len(e.audit())
	for _, bad := range []*transport.EventBatch{nil, {}, {Events: []transport.Event{{Type: "x"}}}} {
		if c := code(t, e.svc.IngestRunnerEvents(e.ctx, r.ID, bad)); c != api.CodeBadRequest {
			t.Errorf("invalid batch: %q", c)
		}
	}
	if len(e.audit()) != before {
		t.Error("invalid batch wrote rows")
	}
	// A revoked runner is refused.
	if _, err := e.svc.Revoke(e.ctx, e.admin, SourceCLI, "vm/"+r.Name); err != nil {
		t.Fatal(err)
	}
	if c := code(t, e.svc.IngestRunnerEvents(e.ctx, r.ID, b)); c != api.CodeForbidden {
		t.Errorf("revoked runner: %q", c)
	}
}

func TestTouchBusy(t *testing.T) {
	e, _, w, _ := hostEnv(t, false)
	r, _ := e.db.GetWorkspace(e.ctx, w.WsID)
	later := t0.Add(10 * time.Minute)
	e.svc.now = func() time.Time { return later }
	e.svc.TouchBusy(e.ctx, r.RunnerID, 0)
	if g, _ := e.db.GetWorkspace(e.ctx, w.WsID); !g.LastUsedAt.Equal(t0) {
		t.Fatal("touched with busy 0")
	}
	e.svc.TouchBusy(e.ctx, r.RunnerID, 2)
	if g, _ := e.db.GetWorkspace(e.ctx, w.WsID); !g.LastUsedAt.Equal(later) {
		t.Fatalf("busy did not touch: %v", g.LastUsedAt)
	}
	e.svc.now = func() time.Time { return later.Add(time.Minute) }
	e.svc.TouchBusy(e.ctx, r.RunnerID, 2)
	if g, _ := e.db.GetWorkspace(e.ctx, w.WsID); !g.LastUsedAt.Equal(later) {
		t.Fatal("touched again within 5 minutes")
	}
}
