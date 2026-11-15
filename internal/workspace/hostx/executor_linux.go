//go:build linux

package hostx

// Executor is the host-mode workspace executor (docs/M7.md section 3): it ties the session
// Manager (directories, file tools) to the Engine (bash, jobs). Besides workspace.Executor it
// implements the runner's optional interfaces structurally (runner.SessionHooks, FSExecutor,
// JobExecutor, BashExecutor, DestroyDeleter, HostModer); hostx never imports the runner.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/fsops"
)

// LabelWorkdir is the optional spec label naming an existing directory (a shared checkout) the
// session is confined to instead of its own directory.
const LabelWorkdir = "mirafleet.workdir"

// ExecutorConfig configures an Executor.
type ExecutorConfig struct {
	Manager *Manager
	Engine  *Engine
	// StateDir is the runner state directory; a session's state (sockets, CA, jobs, outputs)
	// lives in StateDir/sessions/<slug>.
	StateDir string
}

type hostWS struct {
	sd      *SessionDir
	sess    Session
	created time.Time
	labels  map[string]string
}

// Executor implements workspace.Executor for host mode. Safe for concurrent use.
type Executor struct {
	cfg ExecutorConfig

	mu sync.Mutex
	ws map[workspace.ID]*hostWS
}

// NewExecutor builds an Executor.
func NewExecutor(cfg ExecutorConfig) (*Executor, error) {
	if cfg.Manager == nil || cfg.Engine == nil || !filepath.IsAbs(cfg.StateDir) {
		return nil, errors.New("hostx: executor needs a Manager, an Engine and an absolute StateDir")
	}
	return &Executor{cfg: cfg, ws: map[workspace.ID]*hostWS{}}, nil
}

// Name implements workspace.Executor.
func (e *Executor) Name() string { return "host" }

// HostMode implements runner.HostModer.
func (e *Executor) HostMode() bool { return true }

func (e *Executor) get(id workspace.ID) (*hostWS, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	w := e.ws[id]
	if w == nil {
		return nil, workspace.ErrNotFound
	}
	return w, nil
}

// session returns the Engine view of w with the current tool environment.
func (e *Executor) session(id workspace.ID) (Session, *hostWS, error) {
	w, err := e.get(id)
	if err != nil {
		return Session{}, nil, err
	}
	e.mu.Lock()
	s := w.sess
	e.mu.Unlock()
	return s, w, nil
}

// Create implements workspace.Executor: open (or reattach) the session directory. Idempotent.
func (e *Executor) Create(ctx context.Context, sp workspace.Spec) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.ws[sp.ID]; ok {
		return nil
	}
	sd, err := e.cfg.Manager.Open(string(sp.ID), sp.Labels[LabelWorkdir])
	if err != nil {
		return fmt.Errorf("%w: %v", workspace.ErrRefused, err)
	}
	dir := filepath.Join(e.cfg.StateDir, "sessions", Slug(string(sp.ID)))
	s := Session{ID: string(sp.ID), Dir: dir, WorkDir: sd.Dir}
	if err := e.cfg.Engine.prepare(s); err != nil {
		sd.Close()
		return err
	}
	if _, err := e.cfg.Engine.Adopt(s); err != nil { // jobs that outlived a runner restart
		sd.Close()
		return err
	}
	e.ws[sp.ID] = &hostWS{sd: sd, sess: s, created: time.Now(), labels: sp.Labels}
	return nil
}

// SessionDir implements runner.SessionHooks: the session's state directory.
func (e *Executor) SessionDir(id workspace.ID) (string, error) {
	s, _, err := e.session(id)
	return s.Dir, err
}

// SetSessionEnv implements runner.SessionHooks. The env reaches every child of the session
// (foreground bash, jobs, Exec); nil or empty clears it.
func (e *Executor) SetSessionEnv(id workspace.ID, env map[string]string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if w := e.ws[id]; w != nil {
		w.sess.Env = nil
		if len(env) > 0 {
			w.sess.Env = make(map[string]string, len(env))
			for k, v := range env {
				w.sess.Env[k] = v
			}
		}
	}
}

// Bash implements runner.BashExecutor: a foreground command in the session's cwd, output kept
// on the runner under the returned Ref.
func (e *Executor) Bash(ctx context.Context, id workspace.ID, command string, timeout time.Duration, requestID string) (transport.WorkspaceExec, error) {
	s, _, err := e.session(id)
	if err != nil {
		return transport.WorkspaceExec{}, err
	}
	return e.cfg.Engine.Bash(ctx, s, BashRequest{Command: command, Timeout: timeout, RequestID: requestID})
}

// hostPath maps a container-style path (/workspace/x) to a path inside the session root.
func hostPath(p string) string {
	if p == workspace.Root {
		return "."
	}
	return strings.TrimPrefix(strings.TrimPrefix(p, workspace.Root+"/"), "/")
}

// Exec implements workspace.Executor for argv commands (the bash tool uses Bash instead). The
// session's tool environment wins over r.Env.
func (e *Executor) Exec(ctx context.Context, id workspace.ID, r workspace.ExecRequest) (workspace.ExecResult, error) {
	s, w, err := e.session(id)
	if err != nil {
		return workspace.ExecResult{}, err
	}
	if len(r.Argv) == 0 {
		return workspace.ExecResult{}, fmt.Errorf("%w: empty argv", workspace.ErrRefused)
	}
	env := make(map[string]string, len(r.Env)+len(s.Env))
	for k, v := range r.Env {
		env[k] = v
	}
	for k, v := range s.Env {
		env[k] = v
	}
	s.Env = env
	envv, err := e.cfg.Engine.childEnv(s)
	if err != nil {
		return workspace.ExecResult{}, fmt.Errorf("%w: %v", workspace.ErrRefused, err)
	}
	dir := e.cfg.Engine.Cwd(s)
	if r.Cwd != "" && r.Cwd != workspace.Root {
		dir = filepath.Join(w.sd.Dir, hostPath(r.Cwd))
	}
	cmd := exec.CommandContext(ctx, r.Argv[0], r.Argv[1:]...)
	cmd.Dir, cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = dir, envv, r.Stdin, r.Stdout, r.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return workspace.ExecResult{}, fmt.Errorf("%w: %v", workspace.ErrUnavailable, err)
	}
	pgid := cmd.Process.Pid
	e.cfg.Engine.trackFG(s.ID, pgid, true)
	_ = cmd.Wait()
	e.cfg.Engine.trackFG(s.ID, pgid, false)
	return workspace.ExecResult{ExitCode: exitCode(cmd.ProcessState),
		TimedOut: errors.Is(ctx.Err(), context.DeadlineExceeded), Duration: time.Since(start)}, ctx.Err()
}

// ReadFile implements workspace.Executor on the session root (the same tree the file tools use).
func (e *Executor) ReadFile(ctx context.Context, id workspace.ID, p string, off, n int64) (io.ReadCloser, workspace.FileInfo, error) {
	_, w, err := e.session(id)
	if err != nil {
		return nil, workspace.FileInfo{}, err
	}
	if off < 0 {
		return nil, workspace.FileInfo{}, fmt.Errorf("%w: negative offset", workspace.ErrRefused)
	}
	f, err := w.files()
	if err != nil {
		return nil, workspace.FileInfo{}, err
	}
	fh, err := f.root.OpenFile(hostPath(p), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, workspace.FileInfo{}, fileErr(err)
	}
	fi, err := fh.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		fh.Close()
		return nil, workspace.FileInfo{}, fmt.Errorf("%w: not a regular file", workspace.ErrRefused)
	}
	info := workspace.FileInfo{Size: fi.Size(), Mode: fi.Mode().Perm(), ModTime: fi.ModTime()}
	end := info.Size
	if n > 0 && off+n < end {
		end = off + n
	}
	off = min(off, end)
	return struct {
		io.Reader
		io.Closer
	}{io.NewSectionReader(fh, off, end-off), fh}, info, nil
}

// WriteFile implements workspace.Executor on the session root.
func (e *Executor) WriteFile(ctx context.Context, id workspace.ID, p string, r io.Reader, off int64, truncate bool, mode fs.FileMode) (workspace.FileInfo, error) {
	_, w, err := e.session(id)
	if err != nil {
		return workspace.FileInfo{}, err
	}
	if off < 0 {
		return workspace.FileInfo{}, fmt.Errorf("%w: negative offset", workspace.ErrRefused)
	}
	f, err := w.files()
	if err != nil {
		return workspace.FileInfo{}, err
	}
	rel := hostPath(p)
	if d := filepath.Dir(rel); d != "." {
		if err := f.root.MkdirAll(d, 0o700); err != nil {
			return workspace.FileInfo{}, fileErr(err)
		}
	}
	if mode == 0 {
		mode = 0o644
	}
	flags := os.O_RDWR | os.O_CREATE | syscall.O_NOFOLLOW
	if truncate {
		flags |= os.O_TRUNC
	}
	fh, err := f.root.OpenFile(rel, flags, mode.Perm())
	if err != nil {
		return workspace.FileInfo{}, fileErr(err)
	}
	defer fh.Close()
	if fi, err := fh.Stat(); err != nil || !fi.Mode().IsRegular() {
		return workspace.FileInfo{}, fmt.Errorf("%w: not a regular file", workspace.ErrRefused)
	}
	if _, err := io.Copy(io.NewOffsetWriter(fh, off), r); err != nil {
		return workspace.FileInfo{}, err
	}
	fi, err := fh.Stat()
	if err != nil {
		return workspace.FileInfo{}, err
	}
	return workspace.FileInfo{Size: fi.Size(), Mode: fi.Mode().Perm(), ModTime: fi.ModTime()}, nil
}

func (w *hostWS) files() (*Files, error) {
	f, err := w.sd.Files()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", workspace.ErrUnavailable, err)
	}
	return f, nil
}

func fileErr(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return workspace.ErrNotFound
	}
	return fmt.Errorf("%w: %v", workspace.ErrRefused, err)
}

// Destroy implements workspace.Executor: kill the session's processes and release it. The
// directory stays (docs/M7.md section 3.4). Idempotent.
func (e *Executor) Destroy(ctx context.Context, id workspace.ID) error {
	e.mu.Lock()
	w := e.ws[id]
	delete(e.ws, id)
	e.mu.Unlock()
	if w == nil {
		return nil
	}
	e.cfg.Engine.KillSession(w.sess)
	return w.sd.Close()
}

// DestroyDelete implements runner.DestroyDeleter: destroy, then delete the session directory
// (never a workdir) and its state.
func (e *Executor) DestroyDelete(ctx context.Context, id workspace.ID) error {
	e.mu.Lock()
	w := e.ws[id]
	e.mu.Unlock()
	state := filepath.Join(e.cfg.StateDir, "sessions", Slug(string(id)))
	if w != nil {
		state = w.sess.Dir
	}
	if err := e.Destroy(ctx, id); err != nil {
		return err
	}
	if err := e.cfg.Manager.Delete(string(id)); err != nil && !errors.Is(err, ErrNoSession) {
		return err
	}
	return os.RemoveAll(state)
}

// List implements workspace.Executor: the sessions currently open.
func (e *Executor) List(ctx context.Context) ([]workspace.Info, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]workspace.Info, 0, len(e.ws))
	for id, w := range e.ws {
		out = append(out, workspace.Info{ID: id, Image: "host", State: workspace.StateRunning, Created: w.created,
			Labels: w.labels})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Close implements workspace.Executor: releases the session handles. Jobs keep running.
func (e *Executor) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for id, w := range e.ws {
		w.sd.Close()
		delete(e.ws, id)
	}
	return nil
}

// ---- jobs (runner.JobExecutor) ----

// JobStart implements runner.JobExecutor.
func (e *Executor) JobStart(ctx context.Context, id workspace.ID, op transport.JobOp) (transport.JobResult, error) {
	s, _, err := e.session(id)
	if err != nil {
		return transport.JobResult{}, err
	}
	return e.cfg.Engine.StartJob(s, op.Command, op.Description, time.Duration(op.TimeoutS)*time.Second)
}

// JobStatus implements runner.JobExecutor: one job, or all when job is empty.
func (e *Executor) JobStatus(ctx context.Context, id workspace.ID, job string) ([]transport.JobResult, error) {
	s, _, err := e.session(id)
	if err != nil {
		return nil, err
	}
	if job == "" {
		return e.cfg.Engine.ListJobs(s)
	}
	r, err := e.cfg.Engine.JobInfo(s, job)
	if err != nil {
		return nil, err
	}
	return []transport.JobResult{r}, nil
}

// JobTail implements runner.JobExecutor.
func (e *Executor) JobTail(ctx context.Context, id workspace.ID, job, filter string) (transport.JobResult, error) {
	s, _, err := e.session(id)
	if err != nil {
		return transport.JobResult{}, err
	}
	return e.cfg.Engine.TailJob(s, job, filter)
}

// JobCancel implements runner.JobExecutor.
func (e *Executor) JobCancel(ctx context.Context, id workspace.ID, job string) (transport.JobResult, error) {
	s, _, err := e.session(id)
	if err != nil {
		return transport.JobResult{}, err
	}
	return e.cfg.Engine.CancelJob(s, job)
}

// OutputRead implements runner.JobExecutor.
func (e *Executor) OutputRead(ctx context.Context, id workspace.ID, op transport.OutputOp) (transport.OutputResult, error) {
	s, _, err := e.session(id)
	if err != nil {
		return transport.OutputResult{}, err
	}
	return e.cfg.Engine.ReadOutput(s, op)
}

// Busy implements runner.JobExecutor: running jobs across all sessions.
func (e *Executor) Busy() int { return e.cfg.Engine.Busy() }

// ---- file tools (runner.FSExecutor) ----

// FS implements runner.FSExecutor: runs the op on the session's Files and converts the result.
func (e *Executor) FS(ctx context.Context, id workspace.ID, op transport.FSOp, body []byte) (transport.FSResult, []byte, error) {
	w, err := e.get(id)
	if err != nil {
		return transport.FSResult{}, nil, err
	}
	raw, warning, err := w.sd.Do(ctx, op, body)
	if err != nil {
		return transport.FSResult{}, nil, mapFSErr(err)
	}
	res, data, err := ConvertFS(raw)
	if err == nil && warning != "" {
		res.Content = warning + "\n" + res.Content
	}
	return res, data, err
}

func mapFSErr(err error) error {
	switch {
	case errors.Is(err, fsops.ErrNotFound):
		return fmt.Errorf("%w: %v", workspace.ErrNotFound, err)
	case errors.Is(err, fsops.ErrTooLarge):
		return fmt.Errorf("%w: %v", workspace.ErrTooLarge, err)
	case errors.Is(err, fsops.ErrRefused), errors.Is(err, fsops.ErrInvalid):
		return fmt.Errorf("%w: %v", workspace.ErrRefused, err)
	}
	return err
}
