//go:build linux

// Package hostx is the host-mode workspace executor's exec side: start-up
// hardening, a clean child environment, foreground bash with a per-exec output log and a
// persisted cwd, and background jobs supervised by `mirafleet host-shim job` that outlive runner
// restarts. Every child runs as the runner's own unprivileged user.
package hostx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
)

// Limits.
const (
	DefaultTimeout = 120 * time.Second
	MaxTimeout     = 10 * time.Minute
	DefaultJobMax  = 12 * time.Hour
	MaxJobMax      = 24 * time.Hour
	DefaultMaxJobs = 8
	KeepOutputs    = 32
	MaxOutputFile  = 64 << 20  // one foreground output log
	MaxJobStream   = 256 << 20 // one job stream
	inlineBudget   = 32 << 10  // JSON-encoded inline text per stream
	maxCwdBytes    = 4096
)

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// baseEnv are always passed (when the runner has a value).
var baseEnv = []string{"HOME", "USER", "LOGNAME", "PATH", "LANG"}

// Config configures an Engine.
type Config struct {
	// EnvAllow names runner-environment variables passed on to children (workspace.yaml env_allow),
	// in addition to HOME, USER, LOGNAME, PATH and LANG. Nothing else of the runner's environment
	// ever reaches a child.
	EnvAllow []string
	// Shim is the binary run as `<Shim> host-shim job ...`; default os.Executable().
	Shim string
	// JobMax is the default and upper bound of a background job's duration (default 12h, max 24h).
	JobMax time.Duration
	// MaxJobs is the number of running background jobs per session (default 8).
	MaxJobs int
	// Environ is the runner environment to filter; default os.Environ(). Tests inject decoys here.
	Environ []string
}

// Session is what the Engine needs to know about one session. Dir is the session state dir
// (<state>/sessions/<sid>, 0700) holding out/, jobs/ and cwd; WorkDir is the initial cwd. Env are
// the runner-set tool variables (ssh-agent socket, CA bundle, proxy): the only way besides the
// allowlist to put a variable into a child.
type Session struct {
	ID      string
	Dir     string
	WorkDir string
	Env     map[string]string
}

// BashRequest is one foreground command. RequestID names the output log (the result's Ref).
type BashRequest struct {
	Command   string
	Timeout   time.Duration // 0 = DefaultTimeout; capped at MaxTimeout
	RequestID string
}

// Engine runs commands and jobs. It is safe for concurrent use.
type Engine struct {
	shim    string
	jobMax  time.Duration
	maxJobs int
	env     map[string]string // filtered runner env, snapshotted at New

	mu       sync.Mutex
	cwds     map[string]string           // sid -> cwd
	fg       map[string]map[int]struct{} // sid -> running foreground pgids
	sessions map[string]string           // sid -> Dir, every session seen (for Busy)
	startMu  map[string]*sync.Mutex      // sid -> serializes StartJob so job_max holds
}

// startLock returns the session's StartJob mutex.
func (e *Engine) startLock(sid string) *sync.Mutex {
	e.mu.Lock()
	defer e.mu.Unlock()
	m := e.startMu[sid]
	if m == nil {
		m = &sync.Mutex{}
		e.startMu[sid] = m
	}
	return m
}

// New builds an Engine. It snapshots the allowlisted part of the runner's environment now.
func New(cfg Config) (*Engine, error) {
	e := &Engine{
		shim: cfg.Shim, jobMax: cfg.JobMax, maxJobs: cfg.MaxJobs,
		cwds: map[string]string{}, fg: map[string]map[int]struct{}{}, sessions: map[string]string{},
		startMu: map[string]*sync.Mutex{},
	}
	if e.shim == "" {
		p, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("hostx: locate own binary: %w", err)
		}
		e.shim = p
	}
	if e.jobMax <= 0 {
		e.jobMax = DefaultJobMax
	}
	if e.jobMax > MaxJobMax {
		return nil, errors.New("hostx: job_max exceeds 24h")
	}
	if e.maxJobs <= 0 {
		e.maxJobs = DefaultMaxJobs
	}
	allow := map[string]bool{}
	for _, n := range append(append([]string{}, baseEnv...), cfg.EnvAllow...) {
		if !envNameRE.MatchString(n) {
			return nil, fmt.Errorf("hostx: bad env_allow name %q", n)
		}
		allow[n] = true
	}
	environ := cfg.Environ
	if environ == nil {
		environ = os.Environ()
	}
	e.env = map[string]string{}
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if ok && allow[k] && !strings.ContainsRune(v, 0) {
			e.env[k] = v
		}
	}
	if cfg.Environ == nil { // real runner: fill the always-present variables
		if e.env["HOME"] == "" {
			if h, err := os.UserHomeDir(); err == nil {
				e.env["HOME"] = h
			}
		}
		if u, err := user.Current(); err == nil {
			if e.env["USER"] == "" {
				e.env["USER"] = u.Username
			}
			if e.env["LOGNAME"] == "" {
				e.env["LOGNAME"] = u.Username
			}
		}
	}
	if e.env["PATH"] == "" {
		e.env["PATH"] = "/usr/local/bin:/usr/bin:/bin"
	}
	if e.env["LANG"] == "" {
		e.env["LANG"] = "C.UTF-8"
	}
	return e, nil
}

// childEnv is the complete environment of a child of session s: the filtered runner env, then the
// session's tool variables.
func (e *Engine) childEnv(s Session) ([]string, error) {
	m := make(map[string]string, len(e.env)+len(s.Env))
	for k, v := range e.env {
		m[k] = v
	}
	for k, v := range s.Env {
		if !envNameRE.MatchString(k) || strings.ContainsRune(v, 0) {
			return nil, fmt.Errorf("hostx: bad session env var %q", k)
		}
		m[k] = v
	}
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out, nil
}

// prepare creates the session's subdirectories (0700) and remembers the session.
func (e *Engine) prepare(s Session) error {
	if s.ID == "" || !filepath.IsAbs(s.Dir) || !filepath.IsAbs(s.WorkDir) {
		return errors.New("hostx: session needs an id and absolute Dir and WorkDir")
	}
	for _, d := range []string{s.Dir, filepath.Join(s.Dir, "out"), filepath.Join(s.Dir, "jobs")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	e.mu.Lock()
	e.sessions[s.ID] = s.Dir
	e.mu.Unlock()
	return nil
}

// Cwd returns the session's current directory: the last one a command ended in (kept in memory
// and in <Dir>/cwd so a runner restart keeps it), or WorkDir when it is unset or gone.
func (e *Engine) Cwd(s Session) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cwdLocked(s)
}

func (e *Engine) cwdLocked(s Session) string {
	c, ok := e.cwds[s.ID]
	if !ok {
		if b, err := os.ReadFile(filepath.Join(s.Dir, "cwd")); err == nil {
			c = strings.TrimSpace(string(b))
		}
	}
	if !validDir(c) {
		c = s.WorkDir
	}
	e.cwds[s.ID] = c
	return c
}

func validDir(c string) bool {
	if c == "" || len(c) > maxCwdBytes || !filepath.IsAbs(c) || strings.ContainsRune(c, 0) {
		return false
	}
	fi, err := os.Stat(c)
	return err == nil && fi.IsDir()
}

func (e *Engine) setCwd(s Session, c string) {
	if !validDir(c) {
		return
	}
	e.mu.Lock()
	e.cwds[s.ID] = c
	e.mu.Unlock()
	_ = os.WriteFile(filepath.Join(s.Dir, "cwd"), []byte(c+"\n"), 0o600)
}

// shellArgv is `bash -lc script name arg`, falling back to `sh -c`.
func shellArgv(script, arg string) []string {
	if p, err := exec.LookPath("bash"); err == nil {
		return []string{p, "-lc", script, "bash", arg}
	}
	return []string{"sh", "-c", script, "sh", arg}
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("hostx: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// Bash runs one foreground command in a fresh shell in the session's cwd, with the whole
// process group killed on timeout or cancellation. Full output goes to <Dir>/out/<Ref>; the
// result carries head+tail of each stream, the true byte counts and the omitted count.
func (e *Engine) Bash(ctx context.Context, s Session, r BashRequest) (transport.WorkspaceExec, error) {
	var res transport.WorkspaceExec
	if r.Command == "" || strings.ContainsRune(r.Command, 0) {
		return res, fmt.Errorf("%w: bad command", workspace.ErrRefused)
	}
	if err := e.prepare(s); err != nil {
		return res, err
	}
	env, err := e.childEnv(s)
	if err != nil {
		return res, fmt.Errorf("%w: %v", workspace.ErrRefused, err)
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	if timeout > MaxTimeout {
		timeout = MaxTimeout
	}
	sink, err := openSink(filepath.Join(s.Dir, "out"), r.RequestID)
	if err != nil {
		return res, err
	}
	defer sink.close()
	pr, pw, err := os.Pipe()
	if err != nil {
		return res, err
	}
	defer pr.Close()

	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	argv := shellArgv(`trap 'pwd >&3' EXIT; eval "$1"`, r.Command)
	cmd := exec.CommandContext(tctx, argv[0], argv[1:]...)
	cmd.Dir = e.Cwd(s)
	cmd.Env = env
	cmd.ExtraFiles = []*os.File{pw} // fd 3; everything else the runner holds is close-on-exec
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	outC, errC := workspace.NewCapture(inlineBudget), workspace.NewCapture(inlineBudget)
	so, se := &streamW{sink: sink, c: outC}, &streamW{sink: sink, c: errC}
	cmd.Stdout, cmd.Stderr = so, se

	start := time.Now()
	if err := cmd.Start(); err != nil {
		pw.Close()
		return res, fmt.Errorf("%w: %v", workspace.ErrUnavailable, err)
	}
	pw.Close()
	pgid := cmd.Process.Pid
	e.trackFG(s.ID, pgid, true)
	werr := cmd.Wait()
	e.trackFG(s.ID, pgid, false)
	// A foreground command leaves nothing behind: `server &` must not outlive the call (a
	// background shell is job_start's job). The group id stays ours until the shell is reaped.
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	_ = werr // non-zero exit and WaitDelay are reported through the result, not as errors

	e.setCwd(s, readCwd(pr))
	res.DurationMs = time.Since(start).Milliseconds()
	res.ExitCode = exitCode(cmd.ProcessState)
	res.TimedOut = errors.Is(tctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
	res.Ref = sink.ref
	var t1, t2 bool
	res.Stdout, t1 = outC.Result()
	res.Stderr, t2 = errC.Result()
	res.Truncated = t1 || t2
	res.StdoutBytes, res.StderrBytes = outC.Total(), errC.Total()
	res.Omitted = outC.Dropped() + errC.Dropped()
	res.Cwd = e.Cwd(s)
	pruneOutputs(filepath.Join(s.Dir, "out"), sink.ref)
	return res, ctx.Err()
}

func exitCode(ps *os.ProcessState) int {
	if ps == nil {
		return -1
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}

// readCwd reads what the shell wrote to fd 3 without blocking (a background process may still
// hold the write end).
func readCwd(pr *os.File) string {
	fd := int(pr.Fd())
	if err := syscall.SetNonblock(fd, true); err != nil {
		return ""
	}
	buf := make([]byte, 0, 256)
	tmp := make([]byte, 512)
	for len(buf) <= maxCwdBytes {
		n, err := syscall.Read(fd, tmp)
		if n <= 0 || err != nil {
			break
		}
		buf = append(buf, tmp[:n]...)
	}
	s := strings.TrimSpace(string(buf))
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:] // the last line wins
	}
	return s
}

func (e *Engine) trackFG(sid string, pgid int, add bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if add {
		if e.fg[sid] == nil {
			e.fg[sid] = map[int]struct{}{}
		}
		e.fg[sid][pgid] = struct{}{}
		return
	}
	delete(e.fg[sid], pgid)
}

// KillSession kills the session's running foreground process groups and cancels its background
// jobs (workspace_close, idle reap). Its directory stays.
func (e *Engine) KillSession(s Session) {
	e.mu.Lock()
	for pg := range e.fg[s.ID] {
		_ = syscall.Kill(-pg, syscall.SIGKILL)
	}
	e.mu.Unlock()
	if jobs, err := e.ListJobs(s); err == nil {
		for _, j := range jobs {
			if j.State == transport.JobRunning {
				_, _ = e.CancelJob(s, j.Job)
			}
		}
	}
}

// ---- output log ------------------------------------------------------------------------------

type sink struct {
	mu     sync.Mutex
	f      *os.File
	ref    string
	n      int64
	capped bool
}

func openSink(dir, ref string) (*sink, error) {
	if ref != "" && (!validName(ref) || len(ref) > transport.MaxRefBytes) {
		return nil, fmt.Errorf("%w: bad request id", workspace.ErrRefused)
	}
	for i := 0; i < 4; i++ {
		name := ref
		if name == "" || i > 0 {
			name = randHex(8)
			if ref != "" && len(ref)+9 <= transport.MaxRefBytes {
				name = ref + "-" + randHex(4)
			}
		}
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			return &sink{f: f, ref: name}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	return nil, errors.New("hostx: cannot allocate an output log")
}

// validName accepts the ref/job-id alphabet but never a dot-leading name (no "." or "..").
func validName(s string) bool {
	if s == "" || s[0] == '.' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}

func (k *sink) write(p []byte) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.capped {
		return
	}
	if room := MaxOutputFile - k.n; int64(len(p)) > room {
		p = p[:room]
		defer func() {
			k.capped = true
			_, _ = k.f.WriteString("\n[mirafleet: output log capped at 64 MiB; later output is not kept]\n")
		}()
	}
	n, _ := k.f.Write(p)
	k.n += int64(n)
}

func (k *sink) close() { _ = k.f.Close() }

// streamW tees one stream into the shared log and its head+tail capture.
type streamW struct {
	sink *sink
	c    *workspace.Capture
}

func (w *streamW) Write(p []byte) (int, error) {
	w.sink.write(p)
	return w.c.Write(p)
}

// pruneOutputs keeps the newest KeepOutputs logs (and always keep).
func pruneOutputs(dir, keep string) {
	ents, err := os.ReadDir(dir)
	if err != nil || len(ents) <= KeepOutputs {
		return
	}
	type fi struct {
		name string
		mt   time.Time
	}
	var all []fi
	for _, d := range ents {
		if info, err := d.Info(); err == nil && info.Mode().IsRegular() {
			all = append(all, fi{d.Name(), info.ModTime()})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].mt.After(all[j].mt) })
	for i, f := range all {
		if i >= KeepOutputs && f.name != keep {
			_ = os.Remove(filepath.Join(dir, f.name))
		}
	}
}
