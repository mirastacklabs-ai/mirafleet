//go:build linux

package hostx

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
)

const tailChunk = 32 << 10 // job_tail returns at most this much per stream per call

func (e *Engine) jobDir(s Session, id string) (string, error) {
	if !validName(id) || strings.Contains(id, ".") {
		return "", fmt.Errorf("%w: bad job id", workspace.ErrRefused)
	}
	return filepath.Join(s.Dir, "jobs", id), nil
}

// StartJob starts a background shell in the session's current cwd under `host-shim job`. The shim
// is in its own session, so the job survives a runner restart. It returns once the shim has
// recorded the command's process group.
func (e *Engine) StartJob(s Session, command, description string, timeout time.Duration) (transport.JobResult, error) {
	var zero transport.JobResult
	if command == "" || strings.ContainsRune(command, 0) {
		return zero, fmt.Errorf("%w: bad command", workspace.ErrRefused)
	}
	if err := e.prepare(s); err != nil {
		return zero, err
	}
	env, err := e.childEnv(s)
	if err != nil {
		return zero, fmt.Errorf("%w: %v", workspace.ErrRefused, err)
	}
	if timeout <= 0 || timeout > e.jobMax {
		timeout = e.jobMax
	}
	running := 0
	if jobs, err := e.ListJobs(s); err == nil {
		for _, j := range jobs {
			if j.State == transport.JobRunning {
				running++
			}
		}
	}
	if running >= e.maxJobs {
		return zero, fmt.Errorf("%w: %d background shells already running in this session", workspace.ErrBusy, running)
	}
	id := "j" + randHex(4)
	dir := filepath.Join(s.Dir, "jobs", id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return zero, err
	}
	if err := os.WriteFile(filepath.Join(dir, "command"), []byte(command), 0o600); err != nil {
		return zero, err
	}
	secs := int((timeout + time.Second - 1) / time.Second)
	args := []string{"host-shim", "job", "-dir", dir, "-cwd", e.Cwd(s), "-timeout", strconv.Itoa(secs)}
	if description != "" {
		args = append(args, "-desc", description)
	}
	cmd := exec.Command(e.shim, args...)
	cmd.Env = env
	cmd.Dir = "/" // the shim must not pin the session dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return zero, fmt.Errorf("%w: %v", workspace.ErrUnavailable, err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }() // reap while this runner lives
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if st, err := readStatus(dir); err == nil {
			return e.result(dir, st), nil
		}
		select {
		case <-exited:
			if st, err := readStatus(dir); err == nil {
				return e.result(dir, st), nil
			}
			return zero, fmt.Errorf("%w: job supervisor exited without a status", workspace.ErrUnavailable)
		case <-time.After(10 * time.Millisecond):
		}
	}
	return zero, fmt.Errorf("%w: job supervisor did not report in time", workspace.ErrUnavailable)
}

// result converts a status file into a JobResult, deciding "lost" when the supervisor of a job
// recorded as running is gone (pid gone, reused, or a zombie).
func (e *Engine) result(dir string, st *JobStatus) transport.JobResult {
	r := transport.JobResult{Job: st.Job, State: st.State, ExitCode: st.ExitCode, StartedMs: st.StartedMs,
		EndedMs: st.EndedMs, Description: st.Description}
	if st.State == transport.JobRunning && !shimAlive(st) {
		r.State = transport.JobLost
		r.ExitCode = nil
	}
	r.StdoutBytes = fileSize(filepath.Join(dir, "stdout"))
	r.StderrBytes = fileSize(filepath.Join(dir, "stderr"))
	return r
}

func shimAlive(st *JobStatus) bool {
	t, ok := procTicks(st.ShimPid)
	return ok && t == st.ShimTicks
}

func fileSize(p string) int64 {
	if fi, err := os.Stat(p); err == nil {
		return fi.Size()
	}
	return 0
}

// JobInfo returns one job's status.
func (e *Engine) JobInfo(s Session, id string) (transport.JobResult, error) {
	dir, err := e.jobDir(s, id)
	if err != nil {
		return transport.JobResult{}, err
	}
	st, err := readStatus(dir)
	if err != nil {
		return transport.JobResult{}, fmt.Errorf("%w: job %s", workspace.ErrNotFound, id)
	}
	return e.result(dir, st), nil
}

// ListJobs reads the session's jobs from disk, newest first (at most transport.MaxJobList). This
// is also how jobs are re-adopted after a runner restart: nothing is held in memory.
func (e *Engine) ListJobs(s Session) ([]transport.JobResult, error) {
	root := filepath.Join(s.Dir, "jobs")
	ents, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []transport.JobResult
	for _, d := range ents {
		if !d.IsDir() {
			continue
		}
		dir := filepath.Join(root, d.Name())
		if st, err := readStatus(dir); err == nil && st.Job == d.Name() {
			out = append(out, e.result(dir, st))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedMs > out[j].StartedMs })
	if len(out) > transport.MaxJobList {
		out = out[:transport.MaxJobList]
	}
	return out, nil
}

// Adopt lists the session's jobs and registers the session, so Busy counts its running jobs.
// Call it for each known session after a runner restart.
func (e *Engine) Adopt(s Session) ([]transport.JobResult, error) {
	e.mu.Lock()
	e.sessions[s.ID] = s.Dir
	e.mu.Unlock()
	return e.ListJobs(s)
}

// Busy is the number of running background jobs across the sessions this Engine knows.
func (e *Engine) Busy() int {
	e.mu.Lock()
	dirs := make([]string, 0, len(e.sessions))
	for _, d := range e.sessions {
		dirs = append(dirs, d)
	}
	e.mu.Unlock()
	n := 0
	for _, d := range dirs {
		jobs, _ := e.ListJobs(Session{Dir: d})
		for _, j := range jobs {
			if j.State == transport.JobRunning {
				n++
			}
		}
	}
	return n
}

// CancelJob stops a running job: SIGTERM to the supervisor (which kills the command's group and
// records "canceled"), then SIGKILL of the group if it has not finished within 5 s. A job that is
// already finished is returned unchanged.
func (e *Engine) CancelJob(s Session, id string) (transport.JobResult, error) {
	dir, err := e.jobDir(s, id)
	if err != nil {
		return transport.JobResult{}, err
	}
	st, err := readStatus(dir)
	if err != nil {
		return transport.JobResult{}, fmt.Errorf("%w: job %s", workspace.ErrNotFound, id)
	}
	if st.State != transport.JobRunning {
		return e.result(dir, st), nil
	}
	if shimAlive(st) {
		_ = syscall.Kill(st.ShimPid, syscall.SIGTERM)
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if cur, err := readStatus(dir); err == nil && cur.State != transport.JobRunning {
				return e.result(dir, cur), nil
			}
			if !shimAlive(st) {
				break
			}
		}
	}
	// The supervisor is gone or stuck: kill the group directly (only if its leader is the very
	// process recorded, never a reused pid) and record the outcome ourselves.
	if t, ok := procTicks(st.Pgid); ok && t == st.ChildTicks && st.Pgid > 1 {
		_ = syscall.Kill(-st.Pgid, syscall.SIGKILL)
	}
	if shimAlive(st) {
		_ = syscall.Kill(st.ShimPid, syscall.SIGKILL)
	}
	if cur, err := readStatus(dir); err == nil && cur.State == transport.JobRunning {
		cur.State, cur.EndedMs = transport.JobCanceled, time.Now().UnixMilli()
		_ = writeStatus(dir, cur)
		st = cur
	} else if err == nil {
		st = cur
	}
	return e.result(dir, st), nil
}

type cursor struct {
	Out int64 `json:"out"`
	Err int64 `json:"err"`
}

// TailJob returns the output produced since the previous TailJob call (the cursor is a file, so it
// survives a runner restart), at most 32 KiB per stream; JobResult.Omitted is what is still
// pending for the next call. filter (RE2) keeps only matching lines of what is returned; the
// cursor advances over everything returned either way.
func (e *Engine) TailJob(s Session, id, filter string) (transport.JobResult, error) {
	dir, err := e.jobDir(s, id)
	if err != nil {
		return transport.JobResult{}, err
	}
	res, err := e.JobInfo(s, id)
	if err != nil {
		return res, err
	}
	var re *regexp.Regexp
	if filter != "" {
		if re, err = regexp.Compile(filter); err != nil {
			return res, fmt.Errorf("%w: bad filter", workspace.ErrRefused)
		}
	}
	var cur cursor
	if b, err := os.ReadFile(filepath.Join(dir, "cursor")); err == nil {
		_ = json.Unmarshal(b, &cur)
	}
	// Read the exit state after the files so that "exited" implies the output is complete.
	outB, outPending := readFrom(filepath.Join(dir, "stdout"), cur.Out)
	errB, errPending := readFrom(filepath.Join(dir, "stderr"), cur.Err)
	cur.Out += int64(len(outB))
	cur.Err += int64(len(errB))
	var sb strings.Builder
	sb.WriteString(filterLines(string(outB), re))
	if len(errB) > 0 {
		if t := filterLines(string(errB), re); t != "" {
			if sb.Len() > 0 && !strings.HasSuffix(sb.String(), "\n") {
				sb.WriteByte('\n')
			}
			sb.WriteString("[stderr]\n" + t)
		}
	}
	res.Output = sb.String()
	res.Omitted = outPending + errPending
	cb, _ := json.Marshal(cur)
	if err := os.WriteFile(filepath.Join(dir, "cursor"), cb, 0o600); err != nil {
		return res, err
	}
	return res, nil
}

// readFrom reads up to tailChunk bytes of path from off (not ending inside a UTF-8 sequence) and
// returns them with the count of bytes still unread.
func readFrom(path string, off int64) ([]byte, int64) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.Size() <= off {
		return nil, 0
	}
	n := fi.Size() - off
	if n > tailChunk {
		n = tailChunk
	}
	b := make([]byte, n)
	m, _ := f.ReadAt(b, off)
	b = b[:m]
	b = b[:len(b)-incompleteTail(b)]
	return b, fi.Size() - off - int64(len(b))
}

// incompleteTail is the length of an unfinished UTF-8 sequence at the end of b.
func incompleteTail(b []byte) int {
	for i := 1; i <= utf8.UTFMax && i <= len(b); i++ {
		if c := b[len(b)-i]; c&0xC0 != 0x80 { // a lead byte (or ASCII)
			if c >= 0x80 && !utf8.FullRune(b[len(b)-i:]) {
				return i
			}
			return 0
		}
	}
	return 0
}

func filterLines(s string, re *regexp.Regexp) string {
	if re == nil || s == "" {
		return s
	}
	var sb strings.Builder
	for _, ln := range strings.SplitAfter(s, "\n") {
		if ln != "" && re.MatchString(strings.TrimRight(ln, "\n")) {
			sb.WriteString(ln)
			if !strings.HasSuffix(ln, "\n") {
				sb.WriteByte('\n')
			}
		}
	}
	return sb.String()
}

// ---- output_read -----------------------------------------------------------------------------

// ReadOutput pages saved output: a foreground command's log (Ref) or a job stream ("" is stdout).
// Offset and Limit are bytes (Limit 0 = transport.MaxBodyBytes); TailLines returns the last N
// lines instead; Grep keeps matching lines of everything from Offset (not resumable: narrow the
// pattern when Size is large). A page never splits a UTF-8 sequence at its end.
func (e *Engine) ReadOutput(s Session, op transport.OutputOp) (transport.OutputResult, error) {
	var zero transport.OutputResult
	if (op.Ref == "") == (op.Job == "") {
		return zero, fmt.Errorf("%w: exactly one of ref and job", workspace.ErrRefused)
	}
	var path string
	if op.Ref != "" {
		if !validName(op.Ref) {
			return zero, fmt.Errorf("%w: bad ref", workspace.ErrRefused)
		}
		path = filepath.Join(s.Dir, "out", op.Ref)
	} else {
		dir, err := e.jobDir(s, op.Job)
		if err != nil {
			return zero, err
		}
		switch op.Stream {
		case "", "stdout":
			path = filepath.Join(dir, "stdout")
		case "stderr":
			path = filepath.Join(dir, "stderr")
		default:
			return zero, fmt.Errorf("%w: bad stream", workspace.ErrRefused)
		}
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return zero, fmt.Errorf("%w: no such output", workspace.ErrNotFound)
		}
		return zero, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return zero, fmt.Errorf("%w: no such output", workspace.ErrNotFound)
	}
	size := fi.Size()
	limit := op.Limit
	if limit <= 0 || limit > transport.MaxBodyBytes {
		limit = transport.MaxBodyBytes
	}
	res := transport.OutputResult{Size: size}
	var re *regexp.Regexp
	if op.Grep != "" {
		if re, err = regexp.Compile(op.Grep); err != nil {
			return zero, fmt.Errorf("%w: bad grep", workspace.ErrRefused)
		}
	}
	switch {
	case re != nil:
		return grepOutput(f, op.Offset, size, limit, re, res)
	case op.TailLines > 0:
		start, err := tailStart(f, size, op.TailLines)
		if err != nil {
			return zero, err
		}
		if size-start > limit {
			start = size - limit
		}
		return readPage(f, start, size, limit, res)
	default:
		return readPage(f, op.Offset, size, limit, res)
	}
}

func readPage(f *os.File, off, size, limit int64, res transport.OutputResult) (transport.OutputResult, error) {
	res.Offset = off
	if off >= size {
		res.EOF = true
		return res, nil
	}
	n := size - off
	if n > limit {
		n = limit
	}
	b := make([]byte, n)
	m, err := f.ReadAt(b, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return res, err
	}
	b = b[:m]
	if off+int64(m) < size { // do not end a page inside a sequence
		b = b[:len(b)-incompleteTail(b)]
	}
	res.Content = strings.ToValidUTF8(string(b), "�")
	res.EOF = off+int64(len(b)) >= size
	res.Lines = strings.Count(res.Content, "\n")
	return res, nil
}

// tailStart finds the byte offset where the last n lines begin.
func tailStart(f *os.File, size int64, n int) (int64, error) {
	const blk = 64 << 10
	buf := make([]byte, blk)
	lines := 0
	pos := size
	first := true
	for pos > 0 {
		step := int64(blk)
		if pos < step {
			step = pos
		}
		pos -= step
		if _, err := f.ReadAt(buf[:step], pos); err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		for i := step - 1; i >= 0; i-- {
			if buf[i] != '\n' {
				continue
			}
			if first && pos+i == size-1 { // the final newline ends the last line
				continue
			}
			lines++
			if lines == n {
				return pos + i + 1, nil
			}
		}
		first = false
	}
	return 0, nil
}

func grepOutput(f *os.File, off, size, limit int64, re *regexp.Regexp, res transport.OutputResult) (transport.OutputResult, error) {
	res.Offset = off
	if off >= size {
		res.EOF = true
		return res, nil
	}
	r := bufio.NewReaderSize(io.NewSectionReader(f, off, size-off), 64<<10)
	var out bytes.Buffer
	res.EOF = true
	for {
		line, err := r.ReadString('\n')
		if line != "" {
			// Very long lines are matched on a bounded prefix.
			probe := line
			if len(probe) > 1<<20 {
				probe = probe[:1<<20]
			}
			if re.MatchString(strings.TrimRight(probe, "\n")) {
				if int64(out.Len()+len(line)) > limit {
					res.EOF = false
					break
				}
				out.WriteString(line)
				if !strings.HasSuffix(line, "\n") {
					out.WriteByte('\n')
				}
				res.Lines++
			}
		}
		if err != nil {
			break
		}
	}
	res.Content = strings.ToValidUTF8(out.String(), "�")
	return res, nil
}
