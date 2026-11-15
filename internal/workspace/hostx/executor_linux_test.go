//go:build linux

package hostx

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
)

func newTestExecutor(t *testing.T) (*Executor, workspace.ID, string) {
	t.Helper()
	base := t.TempDir()
	m, err := NewManager(SessionsConfig{Root: filepath.Join(base, "work"), StateDir: filepath.Join(base, "state"), DeleteAfter: -1})
	if err != nil {
		t.Fatal(err)
	}
	ex, err := NewExecutor(ExecutorConfig{Manager: m, Engine: newEngine(t, Config{}), StateDir: filepath.Join(base, "state")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ex.Close() })
	id := workspace.NewID()
	if err := ex.Create(context.Background(), workspace.Spec{ID: id}); err != nil {
		t.Fatal(err)
	}
	return ex, id, base
}

func fsOp(op, path string) transport.FSOp { return transport.FSOp{Op: op, Path: path} }

func fsWriteOp(path, content string) (transport.FSOp, []byte) {
	sum := sha256.Sum256([]byte(content))
	return transport.FSOp{Op: transport.FSWrite, Path: path, Length: int64(len(content)), SHA256: hex.EncodeToString(sum[:])}, []byte(content)
}

func TestExecutorFSConvertsResults(t *testing.T) {
	ex, id, _ := newTestExecutor(t)
	ctx := context.Background()

	w, body := fsWriteOp("a/x.txt", "one\ntwo\nthree\n")
	res, data, err := ex.FS(ctx, id, w, body)
	if err != nil || !res.NewFile || res.Size != 14 || res.WrittenBytes != 14 || res.Kind != transport.KindNone || len(data) != 0 {
		t.Fatalf("write: %+v %v", res, err)
	}
	res, _, err = ex.FS(ctx, id, fsOp(transport.FSRead, "a/x.txt"), nil)
	if err != nil || res.Kind != transport.KindText || !strings.Contains(res.Content, "two") || res.Lines != 3 || res.TotalLines != 3 {
		t.Fatalf("read: %+v %v", res, err)
	}
	res, _, err = ex.FS(ctx, id, transport.FSOp{Op: transport.FSEdit, Path: "a/x.txt", OldString: "two", NewString: "2"}, nil)
	if err != nil || res.Count != 1 || res.Kind != transport.KindNone {
		t.Fatalf("edit: %+v %v", res, err)
	}
	res, _, err = ex.FS(ctx, id, fsOp(transport.FSLS, "."), nil)
	if err != nil || res.Kind != transport.KindText || !strings.Contains(res.Content, "x.txt") || res.Count == 0 {
		t.Fatalf("ls: %+v %v", res, err)
	}
	res, _, err = ex.FS(ctx, id, transport.FSOp{Op: transport.FSGlob, Pattern: "**/*.txt"}, nil)
	if err != nil || res.Kind != transport.KindEntries || len(res.Entries) != 1 || res.Entries[0].Path != "a/x.txt" {
		t.Fatalf("glob: %+v %v", res, err)
	}
	res, _, err = ex.FS(ctx, id, transport.FSOp{Op: transport.FSGrep, Pattern: "three", OutputMode: "content", LineNumbers: true}, nil)
	if err != nil || res.Kind != transport.KindText || !strings.Contains(res.Content, "a/x.txt:3:three") || res.Count != 1 {
		t.Fatalf("grep: %+v %v", res, err)
	}
	res, _, err = ex.FS(ctx, id, fsOp(transport.FSStat, "a/x.txt"), nil)
	if err != nil || len(res.Entries) != 1 || res.Entries[0].Dir || res.Entries[0].Size != 12 {
		t.Fatalf("stat: %+v %v", res, err)
	}

	// An image comes back as bytes after the result, with Length set.
	var png1 bytes.Buffer
	if err := png.Encode(&png1, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	if _, err := ex.WriteFile(ctx, id, "/workspace/p.png", bytes.NewReader(png1.Bytes()), 0, true, 0o644); err != nil {
		t.Fatal(err)
	}
	res, data, err = ex.FS(ctx, id, fsOp(transport.FSRead, "p.png"), nil)
	if err != nil || res.Kind != transport.KindImage || res.MIME != "image/png" || res.Length != int64(len(data)) || !bytes.Equal(data, png1.Bytes()) {
		t.Fatalf("image: %+v len=%d %v", res, len(data), err)
	}

	// Errors keep their class.
	if _, _, err := ex.FS(ctx, id, fsOp(transport.FSRead, "nope.txt"), nil); !errors.Is(err, workspace.ErrNotFound) {
		t.Fatalf("missing file: %v", err)
	}
	if _, _, err := ex.FS(ctx, id, transport.FSOp{Op: transport.FSEdit, Path: "a/x.txt", OldString: "zzz", NewString: "y"}, nil); !errors.Is(err, workspace.ErrRefused) {
		t.Fatalf("failed edit: %v", err)
	}
	if _, _, err := ex.FS(ctx, workspace.NewID(), fsOp(transport.FSStat, "x"), nil); !errors.Is(err, workspace.ErrNotFound) {
		t.Fatalf("unknown workspace: %v", err)
	}
}

func TestExecutorReadWriteFileShareTheTree(t *testing.T) {
	ex, id, _ := newTestExecutor(t)
	ctx := context.Background()
	if _, err := ex.WriteFile(ctx, id, "/workspace/up/y.txt", strings.NewReader("hello world"), 0, true, 0o600); err != nil {
		t.Fatal(err)
	}
	// The file tools see what WriteFile wrote ...
	res, _, err := ex.FS(ctx, id, fsOp(transport.FSRead, "up/y.txt"), nil)
	if err != nil || !strings.Contains(res.Content, "hello world") {
		t.Fatalf("%+v %v", res, err)
	}
	// ... and ReadFile sees what the file tools wrote, with offset and length.
	w, body := fsWriteOp("z.txt", "0123456789")
	if _, _, err := ex.FS(ctx, id, w, body); err != nil {
		t.Fatal(err)
	}
	rc, info, err := ex.ReadFile(ctx, id, "/workspace/z.txt", 3, 4)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "3456" || info.Size != 10 {
		t.Fatalf("got %q size %d", got, info.Size)
	}
	if _, err := ex.WriteFile(ctx, id, "/workspace/z.txt", strings.NewReader("AB"), 2, false, 0); err != nil {
		t.Fatal(err)
	}
	rc, _, _ = ex.ReadFile(ctx, id, "/workspace/z.txt", 0, 0)
	got, _ = io.ReadAll(rc)
	rc.Close()
	if string(got) != "01AB456789" {
		t.Fatalf("%q", got)
	}
	if _, _, err := ex.ReadFile(ctx, id, "/workspace/missing", 0, 0); !errors.Is(err, workspace.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if _, _, err := ex.ReadFile(ctx, id, "/workspace/../../etc/passwd", 0, 0); err == nil {
		t.Fatal("escape not refused")
	}
}

func TestExecutorSessionHooksBashAndExec(t *testing.T) {
	ex, id, base := newTestExecutor(t)
	ctx := context.Background()

	dir, err := ex.SessionDir(id)
	if err != nil || !strings.HasPrefix(dir, filepath.Join(base, "state", "sessions")) {
		t.Fatalf("session dir %q %v", dir, err)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("session dir mode: %v %v", fi, err)
	}
	ex.SetSessionEnv(id, map[string]string{"MF_TOOL": "from-runner"})

	r, err := ex.Bash(ctx, id, `echo "$MF_TOOL"; echo err >&2; mkdir sub && cd sub`, 0, "req1")
	if err != nil {
		t.Fatal(err)
	}
	if r.Ref != "req1" || !strings.Contains(r.Stdout, "from-runner") || r.StdoutBytes != int64(len("from-runner\n")) ||
		r.StderrBytes != 4 || r.ExitCode != 0 || r.Omitted != 0 || r.Truncated {
		t.Fatalf("bash: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(dir, "out", "req1")); err != nil {
		t.Fatalf("output log: %v", err)
	}
	// The cwd persisted, and the output is paged by ref.
	r, _ = ex.Bash(ctx, id, "pwd", 0, "req2")
	if !strings.HasSuffix(strings.TrimSpace(r.Stdout), "/sub") {
		t.Fatalf("cwd not kept: %q", r.Stdout)
	}
	out, err := ex.OutputRead(ctx, id, transport.OutputOp{Ref: "req1"})
	if err != nil || !strings.Contains(out.Content, "from-runner") {
		t.Fatalf("output_read: %+v %v", out, err)
	}

	// Exec (argv) gets the same environment, over what the agent passes.
	var buf bytes.Buffer
	xr, err := ex.Exec(ctx, id, workspace.ExecRequest{Argv: []string{"sh", "-c", "echo $MF_TOOL $MF_OWN"},
		Env: map[string]string{"MF_TOOL": "agent", "MF_OWN": "mine"}, Stdout: &buf})
	if err != nil || xr.ExitCode != 0 || strings.TrimSpace(buf.String()) != "from-runner mine" {
		t.Fatalf("exec: %+v %q %v", xr, buf.String(), err)
	}

	// Cleared env.
	ex.SetSessionEnv(id, nil)
	r, _ = ex.Bash(ctx, id, `echo "[$MF_TOOL]"`, 0, "")
	if !strings.Contains(r.Stdout, "[]") {
		t.Fatalf("env not cleared: %q", r.Stdout)
	}
}

func TestExecutorJobsAndBusy(t *testing.T) {
	ex, id, _ := newTestExecutor(t)
	ctx := context.Background()
	j, err := ex.JobStart(ctx, id, transport.JobOp{Command: "echo job-out; sleep 300", Description: "d"})
	if err != nil || j.State != transport.JobRunning {
		t.Fatalf("start: %+v %v", j, err)
	}
	if ex.Busy() != 1 {
		t.Fatalf("busy = %d", ex.Busy())
	}
	var tail transport.JobResult
	for i := 0; i < 200 && !strings.Contains(tail.Output, "job-out"); i++ {
		time.Sleep(25 * time.Millisecond)
		x, err := ex.JobTail(ctx, id, j.Job, "")
		if err != nil {
			t.Fatal(err)
		}
		tail.Output += x.Output
	}
	if !strings.Contains(tail.Output, "job-out") {
		t.Fatalf("tail: %q", tail.Output)
	}
	all, err := ex.JobStatus(ctx, id, "")
	if err != nil || len(all) != 1 || all[0].Job != j.Job {
		t.Fatalf("status all: %+v %v", all, err)
	}
	one, err := ex.JobStatus(ctx, id, j.Job)
	if err != nil || len(one) != 1 {
		t.Fatalf("status one: %+v %v", one, err)
	}
	if _, err := ex.JobStatus(ctx, id, "nope"); err == nil {
		t.Fatal("unknown job")
	}
	c, err := ex.JobCancel(ctx, id, j.Job)
	if err != nil {
		t.Fatal(err)
	}
	if c.State != transport.JobCanceled && c.State != transport.JobRunning {
		t.Fatalf("cancel: %+v", c)
	}
	for i := 0; i < 200 && ex.Busy() > 0; i++ {
		time.Sleep(25 * time.Millisecond)
	}
	if ex.Busy() != 0 {
		t.Fatal("job still busy after cancel")
	}
}

func TestExecutorDestroyKeepsDirDestroyDeleteRemoves(t *testing.T) {
	ex, id, base := newTestExecutor(t)
	ctx := context.Background()
	w, body := fsWriteOp("keep.txt", "x")
	if _, _, err := ex.FS(ctx, id, w, body); err != nil {
		t.Fatal(err)
	}
	dir, _ := ex.SessionDir(id)
	work := filepath.Join(base, "work", Slug(string(id)))
	if err := ex.Destroy(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := ex.Destroy(ctx, id); err != nil { // idempotent
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(work, "keep.txt")); err != nil {
		t.Fatalf("plain destroy must keep the directory: %v", err)
	}
	if _, err := ex.SessionDir(id); !errors.Is(err, workspace.ErrNotFound) {
		t.Fatalf("destroyed: %v", err)
	}
	// Reattach, then destroy with delete.
	if err := ex.Create(ctx, workspace.Spec{ID: id}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ex.FS(ctx, id, fsOp(transport.FSRead, "keep.txt"), nil); err != nil {
		t.Fatalf("reattach lost the file: %v", err)
	}
	if err := ex.DestroyDelete(ctx, id); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{work, dir} {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("%s kept by destroy-delete", p)
		}
	}
	if err := ex.DestroyDelete(ctx, workspace.NewID()); err != nil { // unknown: nothing to do
		t.Fatal(err)
	}
}

func TestExecutorWorkdirLabelAndList(t *testing.T) {
	ex, _, base := newTestExecutor(t)
	ctx := context.Background()
	checkout := filepath.Join(base, "checkout")
	if err := os.Mkdir(checkout, 0o700); err != nil {
		t.Fatal(err)
	}
	id := workspace.NewID()
	if err := ex.Create(ctx, workspace.Spec{ID: id, Labels: map[string]string{LabelWorkdir: checkout}}); err != nil {
		t.Fatal(err)
	}
	w, body := fsWriteOp("f.txt", "in checkout")
	if _, _, err := ex.FS(ctx, id, w, body); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(checkout, "f.txt")); err != nil || string(b) != "in checkout" {
		t.Fatalf("%q %v", b, err)
	}
	r, _ := ex.Bash(ctx, id, "pwd", 0, "")
	if got, _ := filepath.EvalSymlinks(checkout); strings.TrimSpace(r.Stdout) != got {
		t.Fatalf("bash cwd %q, want %q", r.Stdout, got)
	}
	infos, err := ex.List(ctx)
	if err != nil || len(infos) != 2 || infos[0].State != workspace.StateRunning {
		t.Fatalf("list: %+v %v", infos, err)
	}
	if ex.Name() != "host" || !ex.HostMode() {
		t.Fatal("name / host mode")
	}
	// Deleting a workdir session never removes the checkout.
	if err := ex.DestroyDelete(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(checkout, "f.txt")); err != nil {
		t.Fatalf("checkout touched: %v", err)
	}
}
