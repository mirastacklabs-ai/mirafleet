package fake

// M7 host-mode behaviour of the fake executor, on only when Executor.SessionRoot is set: a session
// directory per workspace, the runner-set tool environment, file tools (internal/workspace/fsops
// on <session>/work), background jobs run through ExecFunc, and saved-output paging. It lets the
// runner and TestM7Done exercise the same ops the host executor serves.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/fsops"
)

const maxFakeJobs = 8

type job struct {
	id      string
	desc    string
	state   string
	exit    *int
	started time.Time
	ended   time.Time
	out     bytes.Buffer
	cursor  int
	cancel  context.CancelFunc
}

type devState struct {
	dir, work string
	fsx       *fsops.FS

	mu   sync.Mutex
	env  map[string]string
	jobs map[string]*job
	seq  int
	outs map[string][]byte
}

func newDevState(root string, id workspace.ID) (*devState, error) {
	dir := filepath.Join(root, string(id))
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := fsops.Open(work, fsops.Options{})
	if err != nil {
		return nil, err
	}
	return &devState{dir: dir, work: work, fsx: f, jobs: map[string]*job{}, outs: map[string][]byte{}}, nil
}

func (d *devState) stop() {
	d.mu.Lock()
	for _, j := range d.jobs {
		if j.state == transport.JobRunning {
			j.cancel()
		}
	}
	d.mu.Unlock()
	d.fsx.Close()
}

func (d *devState) abs(cp string) string {
	return filepath.Join(d.work, strings.TrimPrefix(cp, workspace.Root))
}

func (d *devState) readFile(cp string, off, n int64) (io.ReadCloser, workspace.FileInfo, error) {
	if off < 0 {
		return nil, workspace.FileInfo{}, fmt.Errorf("%w: negative offset", workspace.ErrRefused)
	}
	data, err := os.ReadFile(d.abs(cp))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, workspace.FileInfo{}, workspace.ErrNotFound
		}
		return nil, workspace.FileInfo{}, fmt.Errorf("%w: %v", workspace.ErrRefused, err)
	}
	fi, _ := os.Stat(d.abs(cp))
	info := workspace.FileInfo{Size: int64(len(data)), Mode: fi.Mode().Perm(), ModTime: fi.ModTime()}
	off = min(off, info.Size)
	end := info.Size
	if n > 0 && n < end-off {
		end = off + n
	}
	return io.NopCloser(bytes.NewReader(data[off:end])), info, nil
}

func (d *devState) writeFile(cp string, body []byte, off int64, truncate bool, mode fs.FileMode) (workspace.FileInfo, error) {
	p := d.abs(cp)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return workspace.FileInfo{}, fmt.Errorf("%w: %v", workspace.ErrRefused, err)
	}
	flags := os.O_RDWR | os.O_CREATE
	if truncate {
		flags |= os.O_TRUNC
	}
	if mode == 0 {
		mode = 0o644
	}
	f, err := os.OpenFile(p, flags, mode.Perm())
	if err != nil {
		return workspace.FileInfo{}, fmt.Errorf("%w: %v", workspace.ErrRefused, err)
	}
	defer f.Close()
	if _, err := f.WriteAt(body, off); err != nil {
		return workspace.FileInfo{}, err
	}
	fi, err := f.Stat()
	if err != nil {
		return workspace.FileInfo{}, err
	}
	return workspace.FileInfo{Size: fi.Size(), Mode: fi.Mode().Perm(), ModTime: fi.ModTime()}, nil
}

// dev returns the workspace's dev state or a typed error.
func (e *Executor) dev(ctx context.Context, id workspace.ID) (*devState, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	w, err := e.get(ctx, id)
	if err != nil {
		return nil, err
	}
	if w.dev == nil {
		return nil, workspace.ErrUnsupported
	}
	return w.dev, nil
}

// HostMode reports whether the fake behaves like a host executor.
func (e *Executor) HostMode() bool { return e.SessionRoot != "" }

// SessionDir implements runner.SessionHooks.
func (e *Executor) SessionDir(id workspace.ID) (string, error) {
	d, err := e.dev(context.Background(), id)
	if err != nil {
		return "", err
	}
	return d.dir, nil
}

// SetSessionEnv implements runner.SessionHooks; the env is merged into every Exec and job.
func (e *Executor) SetSessionEnv(id workspace.ID, env map[string]string) {
	d, err := e.dev(context.Background(), id)
	if err != nil {
		return
	}
	d.mu.Lock()
	d.env = nil
	if len(env) > 0 {
		d.env = make(map[string]string, len(env))
		for k, v := range env {
			d.env[k] = v
		}
	}
	d.mu.Unlock()
}

// SessionEnv returns a copy of the tool environment the runner last set (for tests).
func (e *Executor) SessionEnv(id workspace.ID) map[string]string {
	d, err := e.dev(context.Background(), id)
	if err != nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]string, len(d.env))
	for k, v := range d.env {
		out[k] = v
	}
	return out
}

// SetOutput stores saved output under ref, as a host executor's foreground bash would.
func (e *Executor) SetOutput(id workspace.ID, ref string, data []byte) {
	if d, err := e.dev(context.Background(), id); err == nil {
		d.mu.Lock()
		d.outs[ref] = append([]byte(nil), data...)
		d.mu.Unlock()
	}
}

// DestroyDelete implements runner.DestroyDeleter: destroy and delete the session directory.
func (e *Executor) DestroyDelete(ctx context.Context, id workspace.ID) error {
	var dir string
	if d, err := e.dev(ctx, id); err == nil {
		dir = d.dir
	}
	if err := e.Destroy(ctx, id); err != nil {
		return err
	}
	if dir != "" {
		return os.RemoveAll(dir)
	}
	return nil
}

// sessionEnvFor merges the session env over r (the runner-set names win).
func (e *Executor) sessionEnvFor(id workspace.ID, r map[string]string) map[string]string {
	d, err := e.dev(context.Background(), id)
	if err != nil {
		return r
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.env) == 0 {
		return r
	}
	out := make(map[string]string, len(r)+len(d.env))
	for k, v := range r {
		out[k] = v
	}
	for k, v := range d.env {
		out[k] = v
	}
	return out
}

// ---- file tools ----

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

// FS implements runner.FSExecutor.
func (e *Executor) FS(ctx context.Context, id workspace.ID, op transport.FSOp, body []byte) (transport.FSResult, []byte, error) {
	d, err := e.dev(ctx, id)
	if err != nil {
		return transport.FSResult{}, nil, err
	}
	if err := op.Validate(); err != nil {
		return transport.FSResult{}, nil, fmt.Errorf("%w: %v", workspace.ErrRefused, err)
	}
	var res transport.FSResult
	var data []byte
	switch op.Op {
	case transport.FSRead:
		r, err := d.fsx.Read(op.Path, fsops.ReadOptions{Offset: int(op.Offset), Limit: op.Limit})
		if err != nil {
			return res, nil, mapFSErr(err)
		}
		res = transport.FSResult{Kind: string(r.Kind), Content: r.Text, Size: r.Size, TotalLines: r.TotalLines,
			Truncated: r.More, MIME: r.MIMEType, SHA256: r.SHA256}
		if r.EndLine >= r.StartLine && r.StartLine > 0 {
			res.Lines = r.EndLine - r.StartLine + 1
		}
		if len(r.Images) > 0 {
			raw, derr := base64.StdEncoding.DecodeString(r.Images[0].Data)
			if derr != nil {
				return res, nil, derr
			}
			res.Kind, res.MIME, res.Length, data = transport.KindImage, r.Images[0].MIME, int64(len(raw)), raw
		}
	case transport.FSWrite:
		if int64(len(body)) != op.Length {
			return res, nil, fmt.Errorf("%w: body length mismatch", workspace.ErrRefused)
		}
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != op.SHA256 {
			return res, nil, fmt.Errorf("%w: body sha256 mismatch", workspace.ErrRefused)
		}
		w, err := d.fsx.Write(op.Path, string(body))
		if err != nil {
			return res, nil, mapFSErr(err)
		}
		res = transport.FSResult{Kind: transport.KindNone, NewFile: w.Created, Size: w.Size, SHA256: w.SHA256, WrittenBytes: w.Size}
	case transport.FSEdit, transport.FSMultiEdit:
		var r *fsops.EditResult
		if op.Op == transport.FSEdit {
			r, err = d.fsx.Edit(op.Path, fsops.EditOp{OldString: op.OldString, NewString: op.NewString, ReplaceAll: op.ReplaceAll})
		} else {
			ops := make([]fsops.EditOp, len(op.Edits))
			for i, x := range op.Edits {
				ops[i] = fsops.EditOp{OldString: x.OldString, NewString: x.NewString, ReplaceAll: x.ReplaceAll}
			}
			r, err = d.fsx.MultiEdit(op.Path, ops)
		}
		if err != nil {
			return res, nil, mapFSErr(err)
		}
		res = transport.FSResult{Kind: transport.KindNone, Count: r.Replacements, Size: r.Size, SHA256: r.SHA256,
			NewFile: r.Created, Content: r.Snippet}
	case transport.FSNotebookEdit:
		r, err := d.fsx.NotebookEdit(op.Path, fsops.NotebookEditOp{CellID: op.CellID, NewSource: op.NewSource, CellType: op.CellType, EditMode: op.EditMode})
		if err != nil {
			return res, nil, mapFSErr(err)
		}
		res = transport.FSResult{Kind: transport.KindNone, Count: r.Cells, Size: r.Size, SHA256: r.SHA256, Content: r.CellID}
	case transport.FSGrep:
		root, err := os.OpenRoot(d.work)
		if err != nil {
			return res, nil, err
		}
		defer root.Close()
		g, err := fsops.Grep(ctx, root, fsops.GrepOptions{Pattern: op.Pattern, Path: op.Path, Glob: op.Glob, Type: op.Type,
			Mode: op.OutputMode, IgnoreCase: op.CaseInsensitive, LineNumbers: op.LineNumbers, After: op.After,
			Before: op.Before, Context: op.Context, Multiline: op.Multiline, HeadLimit: op.HeadLimit})
		if err != nil {
			return res, nil, mapFSErr(err)
		}
		var lines []string
		switch {
		case len(g.Lines) > 0:
			lines = g.Lines
		case len(g.Files) > 0:
			lines = g.Files
		default:
			for _, c := range g.Counts {
				lines = append(lines, fmt.Sprintf("%s:%d", c.Path, c.Count))
			}
		}
		res = transport.FSResult{Kind: transport.KindText, Content: strings.Join(lines, "\n"), Count: len(lines), Truncated: g.Truncated}
	case transport.FSGlob:
		root, err := os.OpenRoot(d.work)
		if err != nil {
			return res, nil, err
		}
		defer root.Close()
		g, err := fsops.Glob(ctx, root, fsops.GlobOptions{Pattern: op.Pattern, Path: op.Path})
		if err != nil {
			return res, nil, mapFSErr(err)
		}
		res = transport.FSResult{Kind: transport.KindEntries, Count: len(g.Paths), Truncated: g.Truncated}
		for _, p := range g.Paths {
			res.Entries = append(res.Entries, transport.FSEntry{Path: p})
		}
	case transport.FSLS:
		r, err := d.fsx.LS(op.Path, op.Ignore)
		if err != nil {
			return res, nil, mapFSErr(err)
		}
		res = transport.FSResult{Kind: transport.KindText, Content: r.Text, Count: r.Count, Truncated: r.Truncated}
	case transport.FSStat:
		r, err := d.fsx.Stat(op.Path)
		if err != nil {
			return res, nil, mapFSErr(err)
		}
		res = transport.FSResult{Kind: transport.KindEntries, Count: 1, Entries: []transport.FSEntry{{Path: r.Path, Size: r.Size,
			Mode: uint32(r.Mode.Perm()), MTimeMs: r.ModTime.UnixMilli(), Dir: r.Type == "dir"}}}
	}
	return res, data, nil
}

// ---- jobs and saved output ----

type lockedBuf struct {
	d *devState
	j *job
}

func (b lockedBuf) Write(p []byte) (int, error) {
	b.d.mu.Lock()
	defer b.d.mu.Unlock()
	return b.j.out.Write(p)
}

func (j *job) result() transport.JobResult {
	return transport.JobResult{Job: j.id, State: j.state, ExitCode: j.exit, StartedMs: j.started.UnixMilli(),
		EndedMs: j.ended.UnixMilli() * boolToInt(!j.ended.IsZero()), Description: j.desc, StdoutBytes: int64(j.out.Len())}
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// JobStart implements runner.JobExecutor: the command runs through ExecFunc as bash -lc.
func (e *Executor) JobStart(ctx context.Context, id workspace.ID, op transport.JobOp) (transport.JobResult, error) {
	d, err := e.dev(ctx, id)
	if err != nil {
		return transport.JobResult{}, err
	}
	e.mu.Lock()
	f := e.ExecFunc
	e.mu.Unlock()
	d.mu.Lock()
	running := 0
	for _, j := range d.jobs {
		if j.state == transport.JobRunning {
			running++
		}
	}
	if running >= maxFakeJobs {
		d.mu.Unlock()
		return transport.JobResult{}, fmt.Errorf("%w: %d background shells already running", workspace.ErrBusy, maxFakeJobs)
	}
	d.seq++
	jctx, cancel := context.WithCancel(context.Background())
	if op.TimeoutS > 0 {
		jctx, cancel = context.WithTimeout(context.Background(), time.Duration(op.TimeoutS)*time.Second)
	}
	j := &job{id: fmt.Sprintf("j%d", d.seq), desc: op.Description, state: transport.JobRunning, started: time.Now(), cancel: cancel}
	d.jobs[j.id] = j
	res := j.result()
	d.mu.Unlock()
	env := e.sessionEnvFor(id, nil)
	go func() {
		defer cancel()
		code := 0
		if f != nil {
			w := lockedBuf{d, j}
			r, _ := f(jctx, id, workspace.ExecRequest{Argv: []string{"bash", "-lc", op.Command}, Env: env, Stdout: w, Stderr: w})
			code = r.ExitCode
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		j.ended = time.Now()
		switch {
		case j.state != transport.JobRunning: // canceled
		case jctx.Err() == context.DeadlineExceeded:
			j.state = transport.JobTimedOut
		default:
			j.state, j.exit = transport.JobExited, &code
		}
	}()
	return res, nil
}

func (d *devState) find(job string) (*job, error) {
	j := d.jobs[job]
	if j == nil {
		return nil, workspace.ErrNotFound
	}
	return j, nil
}

// JobStatus implements runner.JobExecutor.
func (e *Executor) JobStatus(ctx context.Context, id workspace.ID, jobID string) ([]transport.JobResult, error) {
	d, err := e.dev(ctx, id)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if jobID != "" {
		j, err := d.find(jobID)
		if err != nil {
			return nil, err
		}
		return []transport.JobResult{j.result()}, nil
	}
	ids := make([]string, 0, len(d.jobs))
	for k := range d.jobs {
		ids = append(ids, k)
	}
	sort.Strings(ids)
	var out []transport.JobResult
	for _, k := range ids {
		out = append(out, d.jobs[k].result())
	}
	return out, nil
}

// JobTail implements runner.JobExecutor: output since the previous tail call.
func (e *Executor) JobTail(ctx context.Context, id workspace.ID, jobID, filter string) (transport.JobResult, error) {
	d, err := e.dev(ctx, id)
	if err != nil {
		return transport.JobResult{}, err
	}
	var re *regexp.Regexp
	if filter != "" {
		if re, err = regexp.Compile(filter); err != nil {
			return transport.JobResult{}, fmt.Errorf("%w: bad filter", workspace.ErrRefused)
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	j, err := d.find(jobID)
	if err != nil {
		return transport.JobResult{}, err
	}
	fresh := string(j.out.Bytes()[j.cursor:])
	j.cursor = j.out.Len()
	if re != nil {
		var keep []string
		for _, l := range strings.SplitAfter(fresh, "\n") {
			if l != "" && re.MatchString(strings.TrimSuffix(l, "\n")) {
				keep = append(keep, l)
			}
		}
		fresh = strings.Join(keep, "")
	}
	r := j.result()
	r.Output = fresh
	return r, nil
}

// JobCancel implements runner.JobExecutor.
func (e *Executor) JobCancel(ctx context.Context, id workspace.ID, jobID string) (transport.JobResult, error) {
	d, err := e.dev(ctx, id)
	if err != nil {
		return transport.JobResult{}, err
	}
	d.mu.Lock()
	j, err := d.find(jobID)
	if err != nil {
		d.mu.Unlock()
		return transport.JobResult{}, err
	}
	if j.state == transport.JobRunning {
		j.state = transport.JobCanceled
		j.cancel()
	}
	r := j.result()
	d.mu.Unlock()
	return r, nil
}

// OutputRead implements runner.JobExecutor: pages saved output (a ref or a job's stream).
func (e *Executor) OutputRead(ctx context.Context, id workspace.ID, op transport.OutputOp) (transport.OutputResult, error) {
	d, err := e.dev(ctx, id)
	if err != nil {
		return transport.OutputResult{}, err
	}
	d.mu.Lock()
	var data []byte
	if op.Ref != "" {
		b, ok := d.outs[op.Ref]
		if !ok {
			d.mu.Unlock()
			return transport.OutputResult{}, workspace.ErrNotFound
		}
		data = b
	} else {
		j, err := d.find(op.Job)
		if err != nil {
			d.mu.Unlock()
			return transport.OutputResult{}, err
		}
		if op.Stream != "stderr" {
			data = append([]byte(nil), j.out.Bytes()...)
		}
	}
	d.mu.Unlock()
	if op.Grep != "" {
		re, err := regexp.Compile(op.Grep)
		if err != nil {
			return transport.OutputResult{}, fmt.Errorf("%w: bad grep", workspace.ErrRefused)
		}
		var keep []byte
		for _, l := range bytes.SplitAfter(data, []byte("\n")) {
			if len(l) > 0 && re.Match(bytes.TrimSuffix(l, []byte("\n"))) {
				keep = append(keep, l...)
			}
		}
		data = keep
	}
	size := int64(len(data))
	off := op.Offset
	if op.TailLines > 0 {
		lines := bytes.SplitAfter(data, []byte("\n"))
		if n := len(lines); n > 0 && len(lines[n-1]) == 0 {
			lines = lines[:n-1]
		}
		start := max(len(lines)-op.TailLines, 0)
		off = size - int64(len(bytes.Join(lines[start:], nil)))
	}
	off = min(off, size)
	limit := op.Limit
	if limit <= 0 || limit > transport.MaxBodyBytes {
		limit = transport.MaxBodyBytes
	}
	end := min(off+limit, size)
	page := data[off:end]
	return transport.OutputResult{Content: string(page), Offset: off, Size: size, EOF: end >= size,
		Lines: bytes.Count(page, []byte("\n"))}, nil
}

// Busy implements runner.JobExecutor: running jobs across all workspaces.
func (e *Executor) Busy() int {
	e.mu.Lock()
	ds := make([]*devState, 0, len(e.ws))
	for _, w := range e.ws {
		if w.dev != nil {
			ds = append(ds, w.dev)
		}
	}
	e.mu.Unlock()
	n := 0
	for _, d := range ds {
		d.mu.Lock()
		for _, j := range d.jobs {
			if j.state == transport.JobRunning {
				n++
			}
		}
		d.mu.Unlock()
	}
	return n
}

// FinishJobs waits until no job is running or ctx ends (for tests).
func (e *Executor) FinishJobs(ctx context.Context) {
	for e.Busy() > 0 && ctx.Err() == nil {
		time.Sleep(5 * time.Millisecond)
	}
}
