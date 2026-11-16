//go:build linux

package hostx

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
)

func waitJob(t *testing.T, e *Engine, s Session, id, state string) transport.JobResult {
	t.Helper()
	var r transport.JobResult
	for i := 0; i < 500; i++ {
		var err error
		if r, err = e.JobInfo(s, id); err != nil {
			t.Fatal(err)
		}
		if r.State == state {
			return r
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job %s: state %s, want %s", id, r.State, state)
	return r
}

func TestJobSurvivesRunnerRestartAndIsReadopted(t *testing.T) {
	e1 := newEngine(t, Config{})
	s := newSession(t)
	bash(t, e1, s, "mkdir sub && cd sub", 0)
	j, err := e1.StartJob(s, `pwd; echo first; sleep 1; echo second; sleep 300`, "build", 0)
	if err != nil {
		t.Fatal(err)
	}
	if j.State != transport.JobRunning || j.Description != "build" {
		t.Fatalf("%+v", j)
	}
	st, err := readStatus(filepath.Join(s.Dir, "jobs", j.Job))
	if err != nil {
		t.Fatal(err)
	}

	// "restart": drop the engine, build a new one over the same state dir.
	e1 = nil
	e2 := newEngine(t, Config{})
	jobs, err := e2.Adopt(s)
	if err != nil || len(jobs) != 1 || jobs[0].Job != j.Job || jobs[0].State != transport.JobRunning {
		t.Fatalf("adopt: %+v %v", jobs, err)
	}
	if e2.Busy() != 1 {
		t.Errorf("busy = %d", e2.Busy())
	}
	if !alive(st.Pgid) {
		t.Fatal("job process gone after runner restart")
	}
	// incremental tail: only new bytes each call
	var got strings.Builder
	for i := 0; i < 300 && !strings.Contains(got.String(), "second"); i++ {
		tl, err := e2.TailJob(s, j.Job, "")
		if err != nil {
			t.Fatal(err)
		}
		got.WriteString(tl.Output)
		time.Sleep(20 * time.Millisecond)
	}
	if want := filepath.Join(s.WorkDir, "sub") + "\nfirst\nsecond\n"; got.String() != want {
		t.Fatalf("tail = %q, want %q", got.String(), want)
	}
	if tl, _ := e2.TailJob(s, j.Job, ""); tl.Output != "" || tl.Omitted != 0 {
		t.Errorf("second tail not empty: %+v", tl)
	}
	// cancel kills the whole group
	c, err := e2.CancelJob(s, j.Job)
	if err != nil || c.State != transport.JobCanceled {
		t.Fatalf("cancel: %+v %v", c, err)
	}
	for i := 0; i < 100 && (alive(st.Pgid) || alive(st.ShimPid)); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if alive(st.Pgid) || alive(st.ShimPid) {
		t.Error("job or supervisor survived cancel")
	}
	if e2.Busy() != 0 {
		t.Errorf("busy after cancel = %d", e2.Busy())
	}
	if c2, _ := e2.CancelJob(s, j.Job); c2.State != transport.JobCanceled {
		t.Errorf("second cancel: %+v", c2)
	}
}

func TestJobExitTimeoutAndFilter(t *testing.T) {
	e := newEngine(t, Config{})
	s := newSession(t)
	j, err := e.StartJob(s, `echo keep-1; echo drop; echo keep-2 >&2; exit 4`, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	r := waitJob(t, e, s, j.Job, transport.JobExited)
	if r.ExitCode == nil || *r.ExitCode != 4 || r.StdoutBytes != int64(len("keep-1\ndrop\n")) || r.StderrBytes != 7 {
		t.Fatalf("%+v", r)
	}
	tl, err := e.TailJob(s, j.Job, "^keep")
	if err != nil || tl.Output != "keep-1\n[stderr]\nkeep-2\n" {
		t.Fatalf("filtered tail %q %v", tl.Output, err)
	}
	if _, err := e.TailJob(s, j.Job, "("); err == nil {
		t.Error("bad filter accepted")
	}

	k, err := e.StartJob(s, `sleep 300 & wait`, "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	waitJob(t, e, s, k.Job, transport.JobTimedOut)
	st, _ := readStatus(filepath.Join(s.Dir, "jobs", k.Job))
	for i := 0; i < 100 && alive(st.Pgid); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if alive(st.Pgid) {
		t.Error("timed-out job still running")
	}
	if all, _ := e.ListJobs(s); len(all) != 2 || all[0].Job != k.Job {
		t.Errorf("list: %+v", all)
	}
	if _, err := e.JobInfo(s, "nosuch"); !errors.Is(err, workspace.ErrNotFound) {
		t.Errorf("missing job: %v", err)
	}
	if _, err := e.JobInfo(s, "../x"); err == nil {
		t.Error("bad job id accepted")
	}
}

func TestJobLostWhenSupervisorGone(t *testing.T) {
	e := newEngine(t, Config{})
	s := newSession(t)
	j, err := e.StartJob(s, `sleep 300`, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := readStatus(filepath.Join(s.Dir, "jobs", j.Job))
	_ = syscall.Kill(st.ShimPid, syscall.SIGKILL) // the supervisor dies, the command lives
	var r transport.JobResult
	for i := 0; i < 200; i++ {
		if r, _ = e.JobInfo(s, j.Job); r.State == transport.JobLost {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if r.State != transport.JobLost {
		t.Fatalf("state %s", r.State)
	}
	c, err := e.CancelJob(s, j.Job) // still cleans up the orphaned group
	if err != nil || c.State != transport.JobCanceled {
		t.Fatalf("cancel: %+v %v", c, err)
	}
	for i := 0; i < 100 && alive(st.Pgid); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if alive(st.Pgid) {
		t.Error("orphaned command survived cancel")
	}
}

func TestJobLimitAndJobOutputRead(t *testing.T) {
	e := newEngine(t, Config{MaxJobs: 2})
	s := newSession(t)
	a, err := e.StartJob(s, `printf 'l1\nl2\nl3\n'; sleep 300`, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.StartJob(s, `sleep 300`, "", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.StartJob(s, `sleep 300`, "", 0); !errors.Is(err, workspace.ErrBusy) {
		t.Fatalf("third job: %v", err)
	}
	for i := 0; i < 200; i++ {
		if r, _ := e.JobInfo(s, a.Job); r.StdoutBytes == 9 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	o, err := e.ReadOutput(s, transport.OutputOp{Job: a.Job, TailLines: 2})
	if err != nil || o.Content != "l2\nl3\n" {
		t.Fatalf("%+v %v", o, err)
	}
	if _, err := e.ReadOutput(s, transport.OutputOp{Job: a.Job, Stream: "stderr"}); err != nil {
		t.Fatal(err)
	}
	e.KillSession(s)
}

func TestJobOutputCapKillsJob(t *testing.T) {
	e := newEngine(t, Config{})
	s := newSession(t)
	if err := e.prepare(s); err != nil {
		t.Fatal(err)
	}
	// Drive the supervisor directly with a tiny cap.
	dir := filepath.Join(s.Dir, "jobs", "jcap")
	_ = os.Mkdir(dir, 0o700)
	_ = os.WriteFile(filepath.Join(dir, "command"), []byte("yes | head -c 100000; sleep 300"), 0o600)
	if err := RunShim([]string{"job", "-dir", dir, "-cwd", s.WorkDir, "-timeout", "60", "-cap", "1000"}); err != nil {
		t.Fatal(err)
	}
	r, err := e.JobInfo(s, "jcap")
	if err != nil || r.State != transport.JobCanceled {
		t.Fatalf("%+v %v", r, err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "stderr"))
	if !strings.Contains(string(b), "output exceeded") {
		t.Errorf("no cap note: %q", b)
	}
}

// Concurrent job_start cannot exceed MaxJobs.
func TestConcurrentStartJobHonoursMax(t *testing.T) {
	e := newEngine(t, Config{MaxJobs: 2})
	s := newSession(t)
	var wg sync.WaitGroup
	var ok, busy atomic.Int32
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.StartJob(s, "sleep 300", "", 0)
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, workspace.ErrBusy):
				busy.Add(1)
			default:
				t.Errorf("unexpected: %v", err)
			}
		}()
	}
	wg.Wait()
	t.Cleanup(func() { e.KillSession(s) })
	if ok.Load() != 2 || busy.Load() != 6 {
		t.Fatalf("started %d, refused %d; want 2 and 6", ok.Load(), busy.Load())
	}
}
