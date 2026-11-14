package osb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/wsconfig"
)

const (
	// Metadata keys that tie a sandbox to a mirafleet workspace and runner (docs/M3.md §1.14).
	// Ownership is decided by these two keys only.
	MetaWorkspace = "mirafleet.workspace"
	MetaRunner    = "mirafleet.runner"

	sandboxUID = 1000
	sandboxGID = 1000

	minSandboxTimeout = 60 * time.Second
	cleanupTimeout    = 30 * time.Second
	defaultMaxFile    = 64 << 20
)

// Tunables, variables so tests can shorten them.
var (
	createPoll = 500 * time.Millisecond
	createWait = 2 * time.Minute
)

type executor struct {
	c       *Client
	runner  string
	secure  string // declared secure runtime ("" = none)
	maxFile int64
	cpus    float64
	memory  int64

	createMu sync.Mutex
	creating map[workspace.ID]*idLock // per-ID create locks (C-15)
}

// idLock serialises Create for one workspace ID; refs counts holders and waiters so the map
// entry can be dropped.
type idLock struct {
	mu   sync.Mutex
	refs int
}

// lockCreate takes the per-ID create lock: two concurrent Creates for one ID would otherwise both
// see "not found" and make two sandboxes. It returns the unlock func.
func (e *executor) lockCreate(id workspace.ID) func() {
	e.createMu.Lock()
	if e.creating == nil {
		e.creating = map[workspace.ID]*idLock{}
	}
	l := e.creating[id]
	if l == nil {
		l = &idLock{}
		e.creating[id] = l
	}
	l.refs++
	e.createMu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		e.createMu.Lock()
		if l.refs--; l.refs == 0 {
			delete(e.creating, id)
		}
		e.createMu.Unlock()
	}
}

// NewExecutor builds the OpenSandbox executor from a workspace.yaml config. runnerName is the
// ownership tag: this executor only ever touches sandboxes whose metadata carries it.
func NewExecutor(cfg *wsconfig.Config, runnerName string) (workspace.Executor, error) {
	if cfg == nil || cfg.OpenSandbox == nil {
		return nil, errors.New("osb: opensandbox config is missing")
	}
	if runnerName == "" {
		return nil, errors.New("osb: runner name is required")
	}
	o := cfg.OpenSandbox
	c, err := NewClient(Config{
		BaseURL: o.Server, CAFile: o.CAFile, PinSHA256: o.PinSHA256,
		CertFile: o.ClientCert, KeyFile: o.ClientKey, APIKeyFile: o.APIKeyFile,
	})
	if err != nil {
		return nil, err
	}
	mf := int64(cfg.Limits.MaxFile)
	if mf <= 0 {
		mf = defaultMaxFile
	}
	return &executor{c: c, runner: runnerName, secure: o.SecureRuntime, maxFile: mf,
		cpus: cfg.Limits.CPUs, memory: int64(cfg.Limits.Memory)}, nil
}

func (e *executor) Name() string { return wsconfig.ExecOpenSandbox }

func (e *executor) Close() error {
	e.c.hc.CloseIdleConnections()
	return nil
}

func (e *executor) unavailable(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, workspace.ErrNotFound) || errors.Is(err, workspace.ErrRefused) || errors.Is(err, workspace.ErrTooLarge) {
		return err
	}
	return fmt.Errorf("%w: %s", workspace.ErrUnavailable, e.c.scrub(err.Error()))
}

// ---- ownership ----

func (e *executor) owned(sb Sandbox, id workspace.ID) bool {
	return sb.Metadata[MetaWorkspace] == string(id) && sb.Metadata[MetaRunner] == e.runner
}

// find returns the sandbox this runner owns for id, or workspace.ErrNotFound. The server-side
// filter is not trusted: the list is re-filtered by the client and the sandbox is fetched again
// and its metadata re-checked before anything acts on it.
func (e *executor) find(ctx context.Context, id workspace.ID) (Sandbox, error) {
	if !workspace.ValidID(id) {
		return Sandbox{}, workspace.ErrNotFound
	}
	list, err := e.c.ListSandboxes(ctx, ListOptions{Metadata: map[string]string{MetaWorkspace: string(id), MetaRunner: e.runner}})
	if err != nil {
		return Sandbox{}, e.unavailable(err)
	}
	for _, cand := range list {
		got, err := e.c.GetSandbox(ctx, cand.ID)
		if IsNotFound(err) {
			continue
		}
		if err != nil {
			return Sandbox{}, e.unavailable(err)
		}
		if got.ID == cand.ID && e.owned(got, id) {
			return got, nil
		}
	}
	return Sandbox{}, workspace.ErrNotFound
}

func running(sb Sandbox) bool { return strings.EqualFold(sb.Status.State, "running") }

func dead(sb Sandbox) bool {
	switch strings.ToLower(sb.Status.State) {
	case "failed", "terminated", "stopping":
		return true
	}
	return false
}

// ---- lifecycle ----

func (e *executor) Create(ctx context.Context, s workspace.Spec) error {
	if !workspace.ValidID(s.ID) {
		return fmt.Errorf("%w: invalid workspace id", workspace.ErrRefused)
	}
	if s.Image == "" {
		return fmt.Errorf("%w: image is required", workspace.ErrRefused)
	}
	// Normalise (docker.io/library/..., :latest) so the sandbox runs exactly the reference the
	// runner's allowlist matched, whatever defaults OpenSandbox itself would apply.
	norm, nerr := workspace.NormalizeImage(s.Image)
	if nerr != nil {
		return fmt.Errorf("%w: %v", workspace.ErrRefused, nerr)
	}
	s.Image = norm
	if s.RequireSecureRuntime && e.secure == "" {
		return fmt.Errorf("%w: guarded workspace needs a secure runtime (gVisor/Kata) and opensandbox.secure_runtime is not declared", workspace.ErrRefused)
	}
	if s.Pull == workspace.PullNever {
		return fmt.Errorf("%w: pull policy never cannot be honoured by opensandbox", workspace.ErrRefused)
	}
	switch s.Network.Mode {
	case "", workspace.NetNone, workspace.NetEgress:
	default:
		return fmt.Errorf("%w: unknown network mode", workspace.ErrRefused)
	}
	if s.Network.Mode == workspace.NetEgress {
		// OpenSandbox policies are host-only: an allowed host is reachable on every port, so a
		// rule that names another port cannot be honoured.
		for _, r := range s.Network.Allow {
			if r.Port != 443 && r.Port != 80 {
				return fmt.Errorf("%w: opensandbox network policy is host-only; egress rule %q port %d is not supported (only 443 and 80)", workspace.ErrRefused, r.Host, r.Port)
			}
		}
	}

	// Idempotent on ID, and serialised per ID so concurrent Creates cannot make two sandboxes.
	defer e.lockCreate(s.ID)()
	existing, err := e.find(ctx, s.ID)
	switch {
	case err == nil && !dead(existing):
		return e.waitRunning(ctx, existing.ID, s.ID)
	case err == nil: // dead leftover: replace it
		if derr := e.c.DeleteSandbox(ctx, existing.ID); derr != nil {
			return e.unavailable(derr)
		}
	case !errors.Is(err, workspace.ErrNotFound):
		return err
	}

	req := e.buildCreate(s)
	sb, err := e.c.CreateSandbox(ctx, req)
	if err != nil {
		// The server may have created it before the answer got lost: clean by metadata.
		e.cleanup(s.ID, "")
		return e.unavailable(err)
	}
	if err := e.waitRunning(ctx, sb.ID, s.ID); err != nil {
		e.cleanup(s.ID, sb.ID)
		return err
	}
	return nil
}

func (e *executor) buildCreate(s workspace.Spec) CreateRequest {
	lim := map[string]string{}
	cpu := s.Limits.CPUMilli
	if cpu <= 0 && e.cpus > 0 {
		cpu = int64(e.cpus * 1000)
	}
	if cpu > 0 {
		lim["cpu"] = strconv.FormatInt(cpu, 10) + "m"
	}
	mem := s.Limits.MemoryBytes
	if mem <= 0 {
		mem = e.memory
	}
	if mem > 0 {
		lim["memory"] = strconv.FormatInt(mem, 10)
	}
	timeout := s.MaxAge
	if timeout < minSandboxTimeout {
		timeout = minSandboxTimeout
	}
	meta := map[string]string{}
	for k, v := range s.Labels {
		if strings.HasPrefix(k, "mirafleet.") {
			continue // reserved for ownership metadata
		}
		meta[k] = v
	}
	meta[MetaWorkspace] = string(s.ID)
	meta[MetaRunner] = e.runner

	// Always deny by default; only egress mode gets allow rules.
	np := &NetworkPolicy{DefaultAction: "deny"}
	if s.Network.Mode == workspace.NetEgress {
		seen := map[string]bool{}
		for _, r := range s.Network.Allow {
			if r.Host == "" || seen[r.Host] {
				continue
			}
			seen[r.Host] = true
			np.Egress = append(np.Egress, EgressRule{Action: "allow", Target: r.Host})
		}
		sort.Slice(np.Egress, func(i, j int) bool { return np.Egress[i].Target < np.Egress[j].Target })
	}
	return CreateRequest{
		Image:          Image{URI: s.Image},
		Entrypoint:     []string{"sleep", "infinity"},
		ResourceLimits: lim,
		Timeout:        int(timeout / time.Second),
		Metadata:       meta,
		NetworkPolicy:  np,
		Volumes: []Volume{{
			Name:      "ws",
			PVC:       &PVC{ClaimName: "mf-" + string(s.ID), CreateIfNotExists: true, DeleteOnSandboxTermination: true},
			MountPath: workspace.Root,
		}},
	}
}

func (e *executor) waitRunning(ctx context.Context, sbID string, id workspace.ID) error {
	deadline := time.Now().Add(createWait)
	for {
		sb, err := e.c.GetSandbox(ctx, sbID)
		if err != nil {
			return e.unavailable(err)
		}
		if running(sb) {
			return nil
		}
		if dead(sb) {
			return fmt.Errorf("%w: sandbox ended in state %s", workspace.ErrUnavailable, e.c.scrub(sb.Status.State))
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: sandbox did not become ready", workspace.ErrUnavailable)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(createPoll):
		}
	}
}

// cleanup removes partial state on a detached context. It only deletes sandboxes carrying our
// metadata (looked up again), never by a bare server id alone unless it was just created by us.
func (e *executor) cleanup(id workspace.ID, created string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), cleanupTimeout)
	defer cancel()
	if created != "" {
		_ = e.c.DeleteSandbox(ctx, created)
	}
	if sb, err := e.find(ctx, id); err == nil {
		_ = e.c.DeleteSandbox(ctx, sb.ID)
	}
}

func (e *executor) Destroy(ctx context.Context, id workspace.ID) error {
	sb, err := e.find(ctx, id)
	if errors.Is(err, workspace.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return e.unavailable(e.c.DeleteSandbox(ctx, sb.ID))
}

func mapState(s string) workspace.State {
	switch strings.ToLower(s) {
	case "running":
		return workspace.StateRunning
	case "pending":
		return workspace.StateCreated
	case "paused", "pausing", "stopping", "terminated", "failed":
		return workspace.StateStopped
	}
	return workspace.StateUnknown
}

func (e *executor) List(ctx context.Context) ([]workspace.Info, error) {
	list, err := e.c.ListSandboxes(ctx, ListOptions{Metadata: map[string]string{MetaRunner: e.runner}})
	if err != nil {
		return nil, e.unavailable(err)
	}
	out := make([]workspace.Info, 0, len(list))
	for _, sb := range list {
		id := workspace.ID(sb.Metadata[MetaWorkspace])
		if sb.Metadata[MetaRunner] != e.runner || !workspace.ValidID(id) {
			continue
		}
		created, _ := time.Parse(time.RFC3339, sb.CreatedAt)
		labels := make(map[string]string, len(sb.Metadata))
		for k, v := range sb.Metadata {
			if strings.HasPrefix(k, "mirafleet.") {
				continue // ownership metadata is not a user label
			}
			labels[k] = v
		}
		out = append(out, workspace.Info{ID: id, State: mapState(sb.Status.State), Created: created, Labels: labels})
	}
	return out, nil
}

// ---- exec ----

// endpoint resolves the owned, running sandbox's execd route.
func (e *executor) endpoint(ctx context.Context, id workspace.ID) (*Endpoint, error) {
	sb, err := e.find(ctx, id)
	if err != nil {
		return nil, err
	}
	if !running(sb) {
		return nil, fmt.Errorf("%w: sandbox is %s", workspace.ErrUnavailable, e.c.scrub(sb.Status.State))
	}
	ep, err := e.c.GetEndpoint(ctx, sb.ID)
	if err != nil {
		return nil, e.unavailable(err)
	}
	return ep, nil
}

func (e *executor) Exec(ctx context.Context, id workspace.ID, r workspace.ExecRequest) (workspace.ExecResult, error) {
	if r.Stdin != nil {
		return workspace.ExecResult{}, fmt.Errorf("%w: opensandbox exec has no stdin", workspace.ErrUnsupported)
	}
	if err := workspace.ValidArgv(r.Argv); err != nil {
		return workspace.ExecResult{}, fmt.Errorf("%w: %v", workspace.ErrRefused, err)
	}
	cwd := r.Cwd
	if cwd == "" {
		cwd = workspace.Root
	} else {
		var err error
		if cwd, err = workspace.CleanPath(cwd); err != nil {
			return workspace.ExecResult{}, fmt.Errorf("%w: %v", workspace.ErrRefused, err)
		}
	}
	start := time.Now()
	ep, err := e.endpoint(ctx, id)
	if err != nil {
		if ctx.Err() != nil {
			return e.ctxResult(ctx, start)
		}
		return workspace.ExecResult{}, err
	}
	uid, gid := sandboxUID, sandboxGID
	req := CommandRequest{Argv: r.Argv, Cwd: cwd, Envs: r.Env, UID: &uid, GID: &gid}
	dl, hasDL := ctx.Deadline()
	if hasDL {
		ms := time.Until(dl).Milliseconds()
		if ms < 1 {
			ms = 1
		}
		req.Timeout = ms
	}
	st, err := e.c.RunCommand(ctx, ep, req)
	if err != nil {
		if ctx.Err() != nil {
			return e.ctxResult(ctx, start)
		}
		return workspace.ExecResult{}, e.unavailable(err)
	}
	defer st.Close()

	var cmdID string
	var exit *int
	finish := func(code int) (workspace.ExecResult, error) {
		res := workspace.ExecResult{ExitCode: code, Duration: time.Since(start)}
		if hasDL && code != 0 && !time.Now().Before(dl) {
			res.TimedOut = true
		}
		return res, nil
	}
	for {
		ev, err := st.Next()
		if err != nil {
			if ctx.Err() != nil {
				e.cancelCommand(ep, cmdID)
				return e.ctxResult(ctx, start)
			}
			if errors.Is(err, io.EOF) && exit != nil {
				return finish(*exit)
			}
			if errors.Is(err, io.EOF) {
				err = errors.New("command stream ended without a result")
			}
			e.cancelCommand(ep, cmdID)
			return workspace.ExecResult{}, e.unavailable(err)
		}
		switch ev.Type {
		case EventInit:
			cmdID = ev.SessionID()
		case EventStdout:
			if r.Stdout != nil {
				_, _ = io.WriteString(r.Stdout, ev.Text)
			}
		case EventStderr:
			if r.Stderr != nil {
				_, _ = io.WriteString(r.Stderr, ev.Text)
			}
		case EventError:
			switch {
			case ev.Error != nil && ev.Error.ExitCode != nil:
				c := *ev.Error.ExitCode
				exit = &c
			case ev.ExitCode != nil:
				c := *ev.ExitCode
				exit = &c
			default:
				name, val := "", ""
				if ev.Error != nil {
					name, val = ev.Error.Name, ev.Error.Value
				}
				return workspace.ExecResult{}, e.unavailable(fmt.Errorf("command failed: %s %s", trunc(name), trunc(val)))
			}
		case EventComplete:
			switch {
			case exit != nil:
			case ev.ExitCode != nil:
				c := *ev.ExitCode
				exit = &c
			default:
				zero := 0
				exit = &zero
			}
			return finish(*exit)
		}
	}
}

func trunc(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return s
}

func (e *executor) ctxResult(ctx context.Context, start time.Time) (workspace.ExecResult, error) {
	res := workspace.ExecResult{ExitCode: -1, Duration: time.Since(start)}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		res.TimedOut = true
		return res, nil
	}
	return res, ctx.Err()
}

func (e *executor) cancelCommand(ep *Endpoint, id string) {
	if id == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 10*time.Second)
	defer cancel()
	_ = e.c.CancelCommand(ctx, ep, id)
}

// ---- files ----

func octalMode(m int) fs.FileMode {
	v, err := strconv.ParseUint(strconv.Itoa(m), 8, 32)
	if err != nil {
		return 0o644
	}
	return fs.FileMode(v) & fs.ModePerm
}

func toInfo(fi FileInfo) workspace.FileInfo {
	t, _ := time.Parse(time.RFC3339, fi.ModifiedAt)
	return workspace.FileInfo{Size: fi.Size, Mode: octalMode(fi.Mode), ModTime: t}
}

func (e *executor) stat(ctx context.Context, ep *Endpoint, p string) (FileInfo, error) {
	fi, err := e.c.StatFile(ctx, ep, sandboxUID, sandboxGID, p)
	if IsNotFound(err) {
		return FileInfo{}, workspace.ErrNotFound
	}
	if err != nil {
		return FileInfo{}, e.unavailable(err)
	}
	if fi.Type != "" && fi.Type != "file" {
		return FileInfo{}, fmt.Errorf("%w: not a regular file", workspace.ErrRefused)
	}
	return fi, nil
}

func (e *executor) ReadFile(ctx context.Context, id workspace.ID, p string, off, n int64) (io.ReadCloser, workspace.FileInfo, error) {
	p, err := workspace.CleanPath(p)
	if err != nil {
		return nil, workspace.FileInfo{}, fmt.Errorf("%w: %v", workspace.ErrRefused, err)
	}
	if off < 0 {
		return nil, workspace.FileInfo{}, fmt.Errorf("%w: negative offset", workspace.ErrRefused)
	}
	ep, err := e.endpoint(ctx, id)
	if err != nil {
		return nil, workspace.FileInfo{}, err
	}
	fi, err := e.stat(ctx, ep, p)
	if err != nil {
		return nil, workspace.FileInfo{}, err
	}
	info := toInfo(fi)
	avail := fi.Size - off
	if avail < 0 {
		avail = 0
	}
	want := avail
	if n > 0 && n < want {
		want = n
	}
	if want > e.maxFile {
		return nil, info, fmt.Errorf("%w: read of %d bytes exceeds the %d byte file limit", workspace.ErrTooLarge, want, e.maxFile)
	}
	if want == 0 {
		return io.NopCloser(bytes.NewReader(nil)), info, nil
	}
	fb, err := e.c.DownloadFile(ctx, ep, sandboxUID, sandboxGID, p, off, off+want-1)
	if err != nil {
		if IsNotFound(err) {
			return nil, info, workspace.ErrNotFound
		}
		return nil, info, e.unavailable(err)
	}
	if fb.Status == 200 && off > 0 { // server ignored Range
		if _, err := io.CopyN(io.Discard, fb, off); err != nil {
			fb.Close()
			return nil, info, e.unavailable(errors.New("short download"))
		}
	}
	return &limitedBody{Reader: io.LimitReader(fb, want), c: fb}, info, nil
}

type limitedBody struct {
	io.Reader
	c io.Closer
}

func (l *limitedBody) Close() error { return l.c.Close() }

// WriteFile uploads the file through execd's multipart upload. It is NOT atomic: the OpenSandbox
// execd API has no rename or temp-file primitive, so a reader can see a partially written file
// and a failed upload may leave a truncated one (C-15). Offset and append writes are
// read-modify-write and are not safe against concurrent writers of the same path.
func (e *executor) WriteFile(ctx context.Context, id workspace.ID, p string, r io.Reader, off int64, truncate bool, mode fs.FileMode) (workspace.FileInfo, error) {
	p, err := workspace.CleanPath(p)
	if err != nil {
		return workspace.FileInfo{}, fmt.Errorf("%w: %v", workspace.ErrRefused, err)
	}
	if p == workspace.Root {
		return workspace.FileInfo{}, fmt.Errorf("%w: path is a directory", workspace.ErrRefused)
	}
	if off < 0 {
		return workspace.FileInfo{}, fmt.Errorf("%w: negative offset", workspace.ErrRefused)
	}
	data, err := io.ReadAll(io.LimitReader(r, e.maxFile+1))
	if err != nil {
		return workspace.FileInfo{}, fmt.Errorf("%w: reading body failed", workspace.ErrUnavailable)
	}
	if off+int64(len(data)) > e.maxFile {
		return workspace.FileInfo{}, fmt.Errorf("%w: file would exceed %d bytes", workspace.ErrTooLarge, e.maxFile)
	}
	ep, err := e.endpoint(ctx, id)
	if err != nil {
		return workspace.FileInfo{}, err
	}
	body := data
	if off > 0 || !truncate {
		// execd uploads replace the whole file, so splice into the current content.
		var cur []byte
		if !truncate {
			cur, err = e.slurp(ctx, ep, p)
			if err != nil {
				return workspace.FileInfo{}, err
			}
		}
		end := off + int64(len(data))
		size := int64(len(cur))
		if end > size {
			size = end
		}
		buf := make([]byte, size)
		copy(buf, cur)
		copy(buf[off:], data)
		body = buf
	}
	perm := mode & fs.ModePerm
	if perm == 0 {
		perm = 0o644
	}
	m, _ := strconv.Atoi(strconv.FormatUint(uint64(perm), 8))
	meta := UploadMeta{Path: p, Owner: strconv.Itoa(sandboxUID), Group: strconv.Itoa(sandboxGID), Mode: m}
	if err := e.c.UploadFile(ctx, ep, sandboxUID, sandboxGID, meta, bytes.NewReader(body)); err != nil {
		return workspace.FileInfo{}, e.unavailable(err)
	}
	fi, err := e.stat(ctx, ep, p)
	if err != nil {
		return workspace.FileInfo{}, err
	}
	return toInfo(fi), nil
}

// slurp returns the current content of p (nil when absent), bounded by the file limit.
func (e *executor) slurp(ctx context.Context, ep *Endpoint, p string) ([]byte, error) {
	if _, err := e.stat(ctx, ep, p); err != nil {
		if errors.Is(err, workspace.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	fb, err := e.c.DownloadFile(ctx, ep, sandboxUID, sandboxGID, p, 0, -1)
	if err != nil {
		if IsNotFound(err) {
			return nil, nil
		}
		return nil, e.unavailable(err)
	}
	defer fb.Close()
	b, err := io.ReadAll(io.LimitReader(fb, e.maxFile+1))
	if err != nil {
		return nil, e.unavailable(errors.New("download failed"))
	}
	if int64(len(b)) > e.maxFile {
		return nil, fmt.Errorf("%w: existing file exceeds %d bytes", workspace.ErrTooLarge, e.maxFile)
	}
	return b, nil
}
