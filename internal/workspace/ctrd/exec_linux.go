//go:build linux

package ctrd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/runtime-spec/specs-go"

	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
)

const (
	drainWait     = 3 * time.Second // after a normal exit: time for the output FIFOs to reach EOF
	drainWaitKill = time.Second     // after a kill
	maxEnvVars    = 64
)

// Exec runs r.Argv inside the workspace's container as the workspace user (the identity and
// the capability, NoNewPrivileges, seccomp and AppArmor settings of the init process, which is
// all cloned from the container spec; they are re-pinned here as defence in depth).
//
// The context deadline is the timeout: when it passes the exec is killed (with its descendants,
// best effort) and the result has TimedOut set and a nil error. Cancellation kills it the same
// way and returns the context error. A command that cannot be started (not found, not
// executable, bad cwd) is reported like a shell would: exit code 127 or 126 and the reason on
// stderr. Processes that a command daemonised and that survive it stay in the container until
// the workspace is destroyed; the pids and memory limits of the container bound them.
//
// Stdout and stderr writers are never used after Exec returns, and writes to them are
// serialised, so a single non-thread-safe writer may be passed for both.
func (e *Executor) Exec(ctx context.Context, id workspace.ID, r workspace.ExecRequest) (workspace.ExecResult, error) {
	if !workspace.ValidID(id) {
		return workspace.ExecResult{}, refuse("invalid workspace id")
	}
	if err := workspace.ValidArgv(r.Argv); err != nil {
		return workspace.ExecResult{}, refuse("%v", err)
	}
	cwd := workspace.Root
	if r.Cwd != "" {
		c, err := workspace.CleanPath(r.Cwd)
		if err != nil {
			return workspace.ExecResult{}, refuse("cwd: %v", err)
		}
		cwd = c
	}
	reqEnv := r.Env
	if eenv := e.EgressEnv(id); eenv != nil {
		reqEnv = make(map[string]string, len(r.Env)+len(eenv))
		for k, v := range r.Env {
			reqEnv[k] = v
		}
		for k, v := range eenv { // proxy settings win over the caller's
			reqEnv[k] = v
		}
	}
	env, err := execEnv(reqEnv)
	if err != nil {
		return workspace.ExecResult{}, err
	}

	nctx := namespaces.WithNamespace(ctx, e.ns)
	ctr, err := e.client.LoadContainer(nctx, ctrName(id))
	if err != nil {
		if errdefs.IsNotFound(err) {
			return workspace.ExecResult{}, fmt.Errorf("%w: workspace %s", workspace.ErrNotFound, id)
		}
		return workspace.ExecResult{}, unavailable("look up container", err)
	}
	task, err := ctr.Task(nctx, nil)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return workspace.ExecResult{}, fmt.Errorf("%w: workspace %s is not running", workspace.ErrUnavailable, id)
		}
		return workspace.ExecResult{}, unavailable("look up task", err)
	}
	cspec, err := ctr.Spec(nctx)
	if err != nil {
		return workspace.ExecResult{}, unavailable("read container spec", err)
	}
	proc, err := execProcessSpec(cspec, r.Argv, cwd, env)
	if err != nil {
		return workspace.ExecResult{}, err
	}

	out := &gate{}
	stdout, stderr := r.Stdout, r.Stderr
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	ow, ew := out.wrap(stdout), out.wrap(stderr)
	// containerd always creates the stdin FIFO and its shim keeps a writer on it open until the
	// client calls CloseIO, so end of input is never seen by the command on its own. We call
	// CloseIO once the caller's reader hit EOF (or at once without stdin). Bytes already in the
	// FIFO are still delivered: the reader only sees EOF after draining it.
	in := &eofReader{r: strings.NewReader(""), done: make(chan struct{})}
	if r.Stdin != nil {
		in.r = r.Stdin
	} else {
		in.once.Do(func() { close(in.done) })
	}
	defer out.close() // nothing is written to the caller's writers after we return

	execID, err := newExecID()
	if err != nil {
		return workspace.ExecResult{}, err
	}
	// Everything after the exec exists must complete even when ctx is done.
	bg, stop := context.WithCancel(context.WithoutCancel(nctx))
	defer stop()

	p, err := task.Exec(bg, execID, proc, cio.NewCreator(cio.WithStreams(in, ow, ew)))
	if err != nil {
		return workspace.ExecResult{}, unavailable("create exec", err)
	}
	defer func() {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(bg), cleanupTime)
		defer cancel()
		// Nothing to return to; the exec id is unique so a leftover cannot collide.
		_, _ = p.Delete(dctx, containerd.WithProcessKill)
	}()
	exitC, err := p.Wait(bg)
	if err != nil {
		return workspace.ExecResult{}, unavailable("wait on exec", err)
	}

	start := time.Now()
	if err := p.Start(bg); err != nil {
		if code, msg, ok := startFailure(err); ok {
			_, _ = io.WriteString(ew, msg+"\n")
			return workspace.ExecResult{ExitCode: code, Duration: time.Since(start)}, nil
		}
		return workspace.ExecResult{}, unavailable("start exec", err)
	}
	go func() {
		select {
		case <-in.done:
			_ = p.CloseIO(bg, containerd.WithStdinCloser)
		case <-bg.Done():
		}
	}()

	var res workspace.ExecResult
	var retErr error
	select {
	case st := <-exitC:
		if st.Error() != nil {
			return workspace.ExecResult{}, unavailable("exec wait", st.Error())
		}
		res.ExitCode = int(st.ExitCode())
		drain(p, drainWait)
	case <-ctx.Done():
		res.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
		if !res.TimedOut {
			retErr = ctx.Err()
		}
		killTree(bg, p)
		select {
		case st := <-exitC:
			if st.Error() == nil {
				res.ExitCode = int(st.ExitCode())
			} else {
				res.ExitCode = 128 + int(syscall.SIGKILL)
			}
		case <-time.After(killWait):
			return workspace.ExecResult{TimedOut: res.TimedOut}, fmt.Errorf("%w: exec did not exit within %s after SIGKILL", workspace.ErrUnavailable, killWait)
		}
		drain(p, drainWaitKill)
	}
	res.Duration = time.Since(start)
	return res, retErr
}

// drain waits (bounded) until the exec's output FIFOs are fully copied.
func drain(p containerd.Process, d time.Duration) {
	pio := p.IO()
	if pio == nil {
		return
	}
	done := make(chan struct{})
	go func() { pio.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
	}
}

// killTree SIGKILLs the exec process and, best effort, the descendants visible through
// /proc/<pid>/task/*/children (the runner shares the host pid namespace with containerd's
// shim). Descendants are killed first-found-first so the exec cannot respawn them.
func killTree(ctx context.Context, p containerd.Process) {
	kctx, cancel := context.WithTimeout(ctx, cleanupTime)
	defer cancel()
	pid := int(p.Pid())
	var tree []int
	if pid > 1 {
		tree = descendants(pid, 4096)
	}
	_ = p.Kill(kctx, syscall.SIGKILL)
	for _, c := range tree {
		_ = syscall.Kill(c, syscall.SIGKILL)
	}
}

func descendants(pid, limit int) []int {
	var out []int
	queue := []int{pid}
	for len(queue) > 0 && len(out) < limit {
		cur := queue[0]
		queue = queue[1:]
		tasks, err := os.ReadDir(filepath.Join("/proc", strconv.Itoa(cur), "task"))
		if err != nil {
			continue
		}
		for _, t := range tasks {
			b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(cur), "task", t.Name(), "children"))
			if err != nil {
				continue
			}
			for _, f := range strings.Fields(string(b)) {
				if n, err := strconv.Atoi(f); err == nil && n > 1 && !slices.Contains(out, n) {
					out = append(out, n)
					queue = append(queue, n)
				}
			}
		}
	}
	return out
}

// startFailure maps runc's "cannot start the process" errors to shell-like exit codes.
func startFailure(err error) (code int, msg string, ok bool) {
	s := err.Error()
	switch {
	case strings.Contains(s, "executable file not found"), strings.Contains(s, "no such file or directory"):
		return 127, "mirafleet: " + shortStartErr(s), true
	case strings.Contains(s, "permission denied"), strings.Contains(s, "exec format error"), strings.Contains(s, "not a directory"):
		return 126, "mirafleet: " + shortStartErr(s), true
	}
	return 0, "", false
}

func shortStartErr(s string) string {
	if i := strings.LastIndex(s, "unable to start container process: "); i >= 0 {
		s = s[i+len("unable to start container process: "):]
	}
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

// execProcessSpec derives the exec's OCI process from the container's init process, so identity,
// capabilities, seccomp and AppArmor are exactly the container's, and pins the security fields
// again.
func execProcessSpec(cspec *specs.Spec, argv []string, cwd string, env []string) (*specs.Process, error) {
	if cspec == nil || cspec.Process == nil {
		return nil, unavailable("container spec", errors.New("has no process"))
	}
	p := *cspec.Process // shallow copy; slices below are replaced, never mutated
	p.Args = slices.Clone(argv)
	p.Cwd = cwd
	p.Env = env
	p.Terminal = false
	p.ConsoleSize = nil
	p.NoNewPrivileges = true
	p.Capabilities = &specs.LinuxCapabilities{
		Bounding: []string{}, Effective: []string{}, Inheritable: []string{},
		Permitted: []string{}, Ambient: []string{},
	}
	if p.User.UID == 0 || p.User.GID == 0 {
		return nil, unavailable("container spec", errors.New("init runs as root; refusing to exec"))
	}
	p.Rlimits = append(slices.Clone(cspec.Process.Rlimits),
		specs.POSIXRlimit{Type: "RLIMIT_CORE", Hard: 0, Soft: 0})
	return &p, nil
}

// execEnv builds the exec environment: the container's fixed PATH and HOME, then the request's
// variables (already filtered by the runner; names and sizes are re-checked here).
func execEnv(req map[string]string) ([]string, error) {
	if len(req) > maxEnvVars {
		return nil, refuse("too many environment variables")
	}
	m := map[string]string{"PATH": "/usr/local/bin:/usr/bin:/bin", "HOME": workspace.Root}
	for k, v := range req {
		if k == "" || strings.ContainsAny(k, "=\x00") || strings.IndexByte(v, 0) >= 0 || len(k) > 256 || len(v) > 64<<10 {
			return nil, refuse("invalid environment variable %q", k)
		}
		m[k] = v
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	env := make([]string, 0, len(keys))
	for _, k := range keys {
		env = append(env, k+"="+m[k])
	}
	return env, nil
}

func newExecID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("ctrd: exec id: %w", err)
	}
	return "x" + hex.EncodeToString(b[:]), nil
}

// gate serialises writes to the caller's writers and drops writes after close, so a late copy
// goroutine of containerd can never touch a writer (for example a Capture) after Exec returned.
type gate struct {
	mu     sync.Mutex
	closed bool
}

func (g *gate) wrap(w io.Writer) io.Writer { return &gatedWriter{g: g, w: w} }

func (g *gate) close() {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
}

type gatedWriter struct {
	g *gate
	w io.Writer
}

func (w *gatedWriter) Write(p []byte) (int, error) {
	w.g.mu.Lock()
	defer w.g.mu.Unlock()
	if w.g.closed {
		return len(p), nil
	}
	if _, err := w.w.Write(p); err != nil {
		return len(p), nil // a failing sink must not stall or kill the command
	}
	return len(p), nil
}

// eofReader closes done the first time the wrapped reader returns an error (EOF included).
type eofReader struct {
	r    io.Reader
	once sync.Once
	done chan struct{}
}

func (e *eofReader) Read(b []byte) (int, error) {
	n, err := e.r.Read(b)
	if err != nil {
		e.once.Do(func() { close(e.done) })
	}
	return n, err
}
