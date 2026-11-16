// Package fake is an in-memory workspace.Executor for unit tests. It runs no processes: Exec
// calls a pluggable function. It applies the same input validation the real executors must.
package fake

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"sync"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
)

// ExecFunc stands in for running a command. It must honour ctx.
type ExecFunc func(ctx context.Context, id workspace.ID, r workspace.ExecRequest) (workspace.ExecResult, error)

type file struct {
	data []byte
	mode fs.FileMode
	mod  time.Time
}

type ws struct {
	spec    workspace.Spec
	created time.Time
	files   map[string]*file
	dev     *devState // set when SessionRoot is configured (devops.go)
}

// Executor is the fake. Configure the exported fields before first use.
type Executor struct {
	// ExecFunc runs commands; nil means exit 0 with no output.
	ExecFunc ExecFunc
	// Images, when non-nil, restricts Create to refs matching these patterns (workspace.MatchImage).
	Images []string
	// SecureRuntime says whether the fake offers a secure runtime; specs that require one are
	// refused otherwise.
	SecureRuntime bool
	// MaxFile caps file size (0 = 64 MiB).
	MaxFile int64
	// Slots caps live workspaces (0 = unlimited); Create beyond it returns ErrUnavailable.
	Slots int
	// SessionRoot, when set, turns on the host-mode behaviour (devops.go): every workspace
	// gets a 0700 session directory under it with a "work" tree the file tools run on.
	SessionRoot string

	mu     sync.Mutex
	ws     map[workspace.ID]*ws
	closed bool
}

// New returns an empty fake executor.
func New() *Executor { return &Executor{ws: map[workspace.ID]*ws{}} }

var _ workspace.Executor = (*Executor)(nil)

func (e *Executor) Name() string { return "fake" }

func (e *Executor) get(ctx context.Context, id workspace.ID) (*ws, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if e.closed {
		return nil, workspace.ErrUnavailable
	}
	if !workspace.ValidID(id) {
		return nil, fmt.Errorf("%w: invalid id", workspace.ErrRefused)
	}
	w, ok := e.ws[id]
	if !ok {
		return nil, workspace.ErrNotFound
	}
	return w, nil
}

func (e *Executor) Create(ctx context.Context, s workspace.Spec) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ws == nil {
		e.ws = map[workspace.ID]*ws{}
	}
	if e.closed {
		return workspace.ErrUnavailable
	}
	if !workspace.ValidID(s.ID) {
		return fmt.Errorf("%w: invalid id", workspace.ErrRefused)
	}
	if _, ok := e.ws[s.ID]; ok {
		return nil // idempotent
	}
	if s.Image == "" || (e.Images != nil && !workspace.MatchImage(e.Images, s.Image)) {
		return fmt.Errorf("%w: image not allowed", workspace.ErrRefused)
	}
	if !s.Pull.Valid() {
		return fmt.Errorf("%w: bad pull policy", workspace.ErrRefused)
	}
	if s.RequireSecureRuntime && !e.SecureRuntime {
		return fmt.Errorf("%w: secure runtime required", workspace.ErrRefused)
	}
	if e.Slots > 0 && len(e.ws) >= e.Slots {
		return fmt.Errorf("%w: no free slot", workspace.ErrUnavailable)
	}
	labels := make(map[string]string, len(s.Labels))
	for k, v := range s.Labels {
		labels[k] = v
	}
	s.Labels = labels
	w := &ws{spec: s, created: time.Now(), files: map[string]*file{}}
	if e.SessionRoot != "" {
		d, err := newDevState(e.SessionRoot, s.ID)
		if err != nil {
			return fmt.Errorf("%w: %v", workspace.ErrUnavailable, err)
		}
		w.dev = d
	}
	e.ws[s.ID] = w
	return nil
}

func (e *Executor) Exec(ctx context.Context, id workspace.ID, r workspace.ExecRequest) (workspace.ExecResult, error) {
	e.mu.Lock()
	_, err := e.get(ctx, id)
	f := e.ExecFunc
	e.mu.Unlock()
	if err != nil {
		return workspace.ExecResult{}, err
	}
	if err := workspace.ValidArgv(r.Argv); err != nil {
		return workspace.ExecResult{}, fmt.Errorf("%w: %v", workspace.ErrRefused, err)
	}
	if _, err := workspace.CleanPath(r.Cwd); err != nil {
		return workspace.ExecResult{}, fmt.Errorf("%w: cwd: %v", workspace.ErrRefused, err)
	}
	r.Env = e.sessionEnvFor(id, r.Env)
	if f == nil {
		return workspace.ExecResult{}, nil
	}
	start := time.Now()
	res, err := f(ctx, id, r)
	if res.Duration == 0 {
		res.Duration = time.Since(start)
	}
	if ctx.Err() != nil {
		res.TimedOut = ctx.Err() == context.DeadlineExceeded
		if err == nil {
			err = ctx.Err()
		}
	}
	return res, err
}

func (e *Executor) maxFile() int64 {
	if e.MaxFile > 0 {
		return e.MaxFile
	}
	return 64 << 20
}

func (e *Executor) ReadFile(ctx context.Context, id workspace.ID, p string, off, n int64) (io.ReadCloser, workspace.FileInfo, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	w, err := e.get(ctx, id)
	if err != nil {
		return nil, workspace.FileInfo{}, err
	}
	cp, err := workspace.CleanPath(p)
	if err != nil {
		return nil, workspace.FileInfo{}, fmt.Errorf("%w: %v", workspace.ErrRefused, err)
	}
	if w.dev != nil {
		return w.dev.readFile(cp, off, n)
	}
	if off < 0 {
		return nil, workspace.FileInfo{}, fmt.Errorf("%w: negative offset", workspace.ErrRefused)
	}
	f, ok := w.files[cp]
	if !ok {
		return nil, workspace.FileInfo{}, workspace.ErrNotFound
	}
	info := workspace.FileInfo{Size: int64(len(f.data)), Mode: f.mode, ModTime: f.mod}
	if off > info.Size {
		off = info.Size
	}
	end := info.Size
	if n > 0 && n < end-off { // not off+n: that overflows for a huge n
		end = off + n
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), f.data[off:end]...))), info, nil
}

func (e *Executor) WriteFile(ctx context.Context, id workspace.ID, p string, r io.Reader, off int64, truncate bool, mode fs.FileMode) (workspace.FileInfo, error) {
	// Read the body before taking the lock so a slow reader cannot block other callers.
	max := e.maxFile()
	if off < 0 || off > max {
		return workspace.FileInfo{}, fmt.Errorf("%w: bad offset", workspace.ErrRefused)
	}
	body, err := io.ReadAll(io.LimitReader(&ctxReader{ctx, r}, max-off+1))
	if err != nil {
		return workspace.FileInfo{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	w, err := e.get(ctx, id)
	if err != nil {
		return workspace.FileInfo{}, err
	}
	cp, err := workspace.CleanPath(p)
	if err != nil || cp == workspace.Root {
		return workspace.FileInfo{}, fmt.Errorf("%w: bad path", workspace.ErrRefused)
	}
	if w.dev != nil {
		return w.dev.writeFile(cp, body, off, truncate, mode)
	}
	f := w.files[cp]
	if f == nil {
		f = &file{mode: mode.Perm()}
	}
	data := f.data
	if truncate {
		data = nil
	}
	if int64(len(body))+off > max {
		return workspace.FileInfo{}, workspace.ErrTooLarge
	}
	if need := off + int64(len(body)); need > int64(len(data)) {
		data = append(data, make([]byte, need-int64(len(data)))...)
	}
	copy(data[off:], body)
	f.data, f.mod = data, time.Now()
	if mode != 0 {
		f.mode = mode.Perm()
	}
	w.files[cp] = f
	return workspace.FileInfo{Size: int64(len(f.data)), Mode: f.mode, ModTime: f.mod}, nil
}

func (e *Executor) Destroy(ctx context.Context, id workspace.ID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !workspace.ValidID(id) {
		return fmt.Errorf("%w: invalid id", workspace.ErrRefused)
	}
	if w := e.ws[id]; w != nil && w.dev != nil {
		w.dev.stop()
	}
	delete(e.ws, id)
	return nil
}

func (e *Executor) List(ctx context.Context) ([]workspace.Info, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, workspace.ErrUnavailable
	}
	out := make([]workspace.Info, 0, len(e.ws))
	for id, w := range e.ws {
		var disk int64
		for _, f := range w.files {
			disk += int64(len(f.data))
		}
		labels := make(map[string]string, len(w.spec.Labels))
		for k, v := range w.spec.Labels {
			labels[k] = v
		}
		out = append(out, workspace.Info{ID: id, Image: w.spec.Image, State: workspace.StateRunning,
			Created: w.created, DiskBytes: disk, Labels: labels})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (e *Executor) Close() error {
	e.mu.Lock()
	e.closed = true
	e.mu.Unlock()
	return nil
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
