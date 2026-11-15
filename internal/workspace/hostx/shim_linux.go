//go:build linux

package hostx

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// JobStatus is <jobdir>/status.json, written atomically by the shim and read by the runner
// (also after a runner restart). It sits in the agent's own user's directory, so it is
// informational: the runner verifies pid and start time before it acts on one.
type JobStatus struct {
	Job         string `json:"job"`
	State       string `json:"state"` // transport.Job* states
	ShimPid     int    `json:"shim_pid"`
	ShimTicks   uint64 `json:"shim_ticks"`
	Pgid        int    `json:"pgid"` // the command's process group (its leader's pid)
	ChildTicks  uint64 `json:"child_ticks"`
	ExitCode    *int   `json:"exit_code,omitempty"`
	StartedMs   int64  `json:"started_ms"`
	EndedMs     int64  `json:"ended_ms,omitempty"`
	Description string `json:"description,omitempty"`
}

const statusFile = "status.json"

func writeStatus(dir string, st *JobStatus) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".status.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, statusFile))
}

func readStatus(dir string) (*JobStatus, error) {
	b, err := os.ReadFile(filepath.Join(dir, statusFile))
	if err != nil {
		return nil, err
	}
	var st JobStatus
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// procTicks returns the start time (clock ticks since boot) of pid, and whether it is alive and
// not a zombie. A pid whose ticks differ from the recorded ones has been reused.
func procTicks(pid int) (ticks uint64, alive bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, false
	}
	f := strings.Fields(s[i+1:]) // f[0] is field 3 (state); starttime is field 22
	if len(f) < 20 || f[0] == "Z" || f[0] == "X" {
		return 0, false
	}
	t, err := strconv.ParseUint(f[19], 10, 64)
	return t, err == nil
}

// RunShim is `mirafleet host-shim job -dir D -cwd C [-timeout S] [-desc T]`: a tiny supervisor
// that runs the command in <D>/command in its own process group with stdout/stderr going straight
// to files in D, records status.json, enforces the timeout and the per-stream cap, and exits when
// the command does. The runner starts it in its own session, so it survives runner restarts.
func RunShim(args []string) error {
	if len(args) < 1 || args[0] != "job" {
		return errors.New("usage: host-shim job -dir DIR -cwd DIR [-timeout SECONDS] [-desc TEXT]")
	}
	fs := flag.NewFlagSet("host-shim job", flag.ContinueOnError)
	dir := fs.String("dir", "", "job directory")
	cwd := fs.String("cwd", "", "working directory")
	timeout := fs.Int("timeout", int(DefaultJobMax/time.Second), "seconds")
	desc := fs.String("desc", "", "description")
	capBytes := fs.Int64("cap", MaxJobStream, "per-stream byte cap")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if !filepath.IsAbs(*dir) || !filepath.IsAbs(*cwd) || *timeout <= 0 {
		return errors.New("host-shim: -dir and -cwd must be absolute and -timeout positive")
	}
	cmdText, err := os.ReadFile(filepath.Join(*dir, "command"))
	if err != nil {
		return err
	}
	fo, err := os.OpenFile(filepath.Join(*dir, "stdout"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	fe, err := os.OpenFile(filepath.Join(*dir, "stderr"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	st := &JobStatus{Job: filepath.Base(*dir), State: transport.JobRunning, ShimPid: os.Getpid(),
		StartedMs: time.Now().UnixMilli(), Description: *desc}
	st.ShimTicks, _ = procTicks(st.ShimPid)

	argv := shellArgv(`eval "$1"`, string(cmdText))
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = *cwd, fo, fe // Env: this process's, already clean
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(fe, "mirafleet: cannot start job: %v\n", err)
		c := 127
		st.State, st.ExitCode, st.EndedMs = transport.JobExited, &c, time.Now().UnixMilli()
		return writeStatus(*dir, st)
	}
	st.Pgid = cmd.Process.Pid
	st.ChildTicks, _ = procTicks(st.Pgid)
	if err := writeStatus(*dir, st); err != nil {
		_ = syscall.Kill(-st.Pgid, syscall.SIGKILL)
		return err
	}

	var reason atomic.Value // transport.JobCanceled | transport.JobTimedOut, set once
	stop := func(why, msg string) {
		if reason.CompareAndSwap(nil, why) {
			if msg != "" {
				fmt.Fprintf(fe, "\n[mirafleet: %s]\n", msg)
			}
			_ = syscall.Kill(-st.Pgid, syscall.SIGKILL)
		}
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	done := make(chan struct{})
	go func() {
		tm := time.NewTimer(time.Duration(*timeout) * time.Second)
		poll := time.NewTicker(time.Second)
		defer tm.Stop()
		defer poll.Stop()
		for {
			select {
			case <-sigs:
				if reason.CompareAndSwap(nil, transport.JobCanceled) {
					_ = syscall.Kill(-st.Pgid, syscall.SIGTERM)
					time.AfterFunc(2*time.Second, func() { _ = syscall.Kill(-st.Pgid, syscall.SIGKILL) })
				}
			case <-tm.C:
				stop(transport.JobTimedOut, "job timed out")
			case <-poll.C:
				for _, f := range []*os.File{fo, fe} {
					if fi, err := f.Stat(); err == nil && fi.Size() > *capBytes {
						stop(transport.JobCanceled, "job killed: output exceeded the per-stream cap")
					}
				}
			case <-done:
				return
			}
		}
	}()

	_ = cmd.Wait()
	close(done)
	_ = syscall.Kill(-st.Pgid, syscall.SIGKILL) // stragglers of a finished job
	st.EndedMs = time.Now().UnixMilli()
	code := exitCode(cmd.ProcessState)
	st.ExitCode = &code
	st.State = transport.JobExited
	if r, ok := reason.Load().(string); ok {
		st.State = r
	}
	return writeStatus(*dir, st)
}
