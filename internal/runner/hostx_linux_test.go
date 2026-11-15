//go:build linux

package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/hostx"
)

// hostxHarness runs the Runner over the real host executor (hostx), not the fake.
func hostxHarness(t *testing.T) (*wsHarness, *hostx.Executor, string) {
	t.Helper()
	old := systemBundle
	systemBundle = func() ([]byte, error) {
		return []byte("-----BEGIN CERTIFICATE-----\nSYSTEM\n-----END CERTIFICATE-----\n"), nil
	}
	t.Cleanup(func() { systemBundle = old })
	base, err := os.MkdirTemp("", "m7x") // short: unix socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	m, err := hostx.NewManager(hostx.SessionsConfig{Root: filepath.Join(base, "w"), StateDir: filepath.Join(base, "s"), DeleteAfter: -1})
	if err != nil {
		t.Fatal(err)
	}
	eng, err := hostx.New(hostx.Config{})
	if err != nil {
		t.Fatal(err)
	}
	ex, err := hostx.NewExecutor(hostx.ExecutorConfig{Manager: m, Engine: eng, StateDir: filepath.Join(base, "s")})
	if err != nil {
		t.Fatal(err)
	}
	h := newHarnessFor(t, wsYAML, ex, nil)
	return h, ex, base
}

// The interfaces the runner dispatches to are all implemented by the host executor.
var (
	_ SessionHooks   = (*hostx.Executor)(nil)
	_ FSExecutor     = (*hostx.Executor)(nil)
	_ JobExecutor    = (*hostx.Executor)(nil)
	_ BashExecutor   = (*hostx.Executor)(nil)
	_ DestroyDeleter = (*hostx.Executor)(nil)
	_ HostModer      = (*hostx.Executor)(nil)
)

func TestHostExecutorOpsAreNotUnsupported(t *testing.T) {
	h, ex, base := hostxHarness(t)
	id := h.create()
	sid := string(id)

	// Heartbeat advertises fs, jobs and host.
	hb := lastHeartbeat(h, func(hb transport.Heartbeat) bool { return len(hb.Features) > 0 })
	have := map[string]bool{}
	for _, f := range hb.Features {
		have[f] = true
	}
	if !have[transport.FeatureFS] || !have[transport.FeatureJobs] || !have[transport.FeatureHost] || hb.Executor != "host" {
		t.Fatalf("heartbeat: %+v", hb)
	}

	// fs write / read / edit.
	req, body := fsWrite("x.txt", "a\nb\nc\n")
	req.ID = sid
	if res := h.want(req, body, transport.CodeOK); res.FS == nil || !res.FS.NewFile || res.FS.Size != 6 {
		t.Fatalf("fs write: %+v", res.FS)
	}
	res := h.want(transport.WorkspaceRequest{Op: transport.OpFS, ID: sid, FS: &transport.FSOp{Op: transport.FSRead, Path: "x.txt"}}, nil, transport.CodeOK)
	if res.FS.Kind != transport.KindText || !strings.Contains(res.FS.Content, "b") {
		t.Fatalf("fs read: %+v", res.FS)
	}
	h.want(transport.WorkspaceRequest{Op: transport.OpFS, ID: sid, FS: &transport.FSOp{Op: transport.FSEdit, Path: "x.txt", OldString: "b", NewString: "B"}}, nil, transport.CodeOK)
	h.want(transport.WorkspaceRequest{Op: transport.OpFS, ID: sid, FS: &transport.FSOp{Op: transport.FSEdit, Path: "x.txt", OldString: "zzz", NewString: "y"}}, nil, transport.CodeRefused)
	h.want(transport.WorkspaceRequest{Op: transport.OpFS, ID: sid, FS: &transport.FSOp{Op: transport.FSRead, Path: "nope"}}, nil, transport.CodeNotFound)

	// read_file / write_file see the same tree.
	_, data := h.do(transport.WorkspaceRequest{Op: transport.OpRead, ID: sid, Path: "/workspace/x.txt"}, nil)
	if string(data) != "a\nB\nc\n" {
		t.Fatalf("read_file: %q", data)
	}
	h.want(transport.WorkspaceRequest{Op: transport.OpWrite, ID: sid, Path: "up/y.txt", Length: 2, Truncate: true}, []byte("hi"), transport.CodeOK)
	res = h.want(transport.WorkspaceRequest{Op: transport.OpFS, ID: sid, FS: &transport.FSOp{Op: transport.FSGrep, Pattern: "hi", OutputMode: "content", LineNumbers: true}}, nil, transport.CodeOK)
	if !strings.Contains(res.FS.Content, "up/y.txt:1:hi") {
		t.Fatalf("grep: %q", res.FS.Content)
	}

	// Foreground bash goes through the host engine: ref, byte counts and a persisted cwd.
	exec := func(cmd string) *transport.WorkspaceExec {
		t.Helper()
		r := h.want(transport.WorkspaceRequest{Op: transport.OpExec, ID: sid, Argv: []string{"bash", "-lc", cmd}}, nil, transport.CodeOK)
		return r.Exec
	}
	e1 := exec("echo out; echo err >&2; mkdir d && cd d")
	if e1.Ref == "" || e1.StdoutBytes != 4 || e1.StderrBytes != 4 || e1.ExitCode != 0 || !strings.Contains(e1.Stdout, "out") {
		t.Fatalf("bash: %+v", e1)
	}
	if e2 := exec("pwd"); !strings.HasSuffix(strings.TrimSpace(e2.Stdout), "/d") {
		t.Fatalf("cwd not kept: %+v", e2)
	}
	if e3 := exec("exit 3"); e3.ExitCode != 3 {
		t.Fatalf("exit code: %+v", e3)
	}
	res = h.want(transport.WorkspaceRequest{Op: transport.OpOutputRead, ID: sid, Out: &transport.OutputOp{Ref: e1.Ref}}, nil, transport.CodeOK)
	if !strings.Contains(res.Output.Content, "out") {
		t.Fatalf("output_read: %+v", res.Output)
	}
	// A non-bash argv still runs, through Exec.
	r := h.want(transport.WorkspaceRequest{Op: transport.OpExec, ID: sid, Argv: []string{"echo", "plain"}}, nil, transport.CodeOK)
	if strings.TrimSpace(r.Exec.Stdout) != "plain" || r.Exec.Ref != "" {
		t.Fatalf("exec: %+v", r.Exec)
	}

	// Jobs are served (no unsupported); status lists none yet.
	res = h.want(transport.WorkspaceRequest{Op: transport.OpJobStatus, ID: sid, Job: &transport.JobOp{}}, nil, transport.CodeOK)
	if len(res.Jobs) != 0 {
		t.Fatalf("jobs: %+v", res.Jobs)
	}
	h.want(transport.WorkspaceRequest{Op: transport.OpJobCancel, ID: sid, Job: &transport.JobOp{Job: "nope"}}, nil, transport.CodeNotFound)

	// The runner's session state lives in the executor's session dir.
	dir, err := ex.SessionDir(workspace.ID(sid))
	if err != nil || !strings.HasPrefix(dir, filepath.Join(base, "s", "sessions")) {
		t.Fatalf("session dir %q %v", dir, err)
	}

	// close keeps the directory; close with delete removes it.
	work := filepath.Join(base, "w", hostx.Slug(sid))
	id2 := h.create()
	h.want(transport.WorkspaceRequest{Op: transport.OpDestroy, ID: string(id2)}, nil, transport.CodeOK)
	if _, err := os.Stat(filepath.Join(base, "w", hostx.Slug(string(id2)))); err != nil {
		t.Fatalf("plain destroy must keep the dir: %v", err)
	}
	h.want(transport.WorkspaceRequest{Op: transport.OpDestroy, ID: sid, Delete: true}, nil, transport.CodeOK)
	if _, err := os.Stat(work); err == nil {
		t.Fatal("destroy delete kept the dir")
	}
}
