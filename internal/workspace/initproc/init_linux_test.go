package initproc

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The test binary re-executes itself in roles selected by MF_INIT_ROLE:
//
//	init:     subreaper that spawns orphans, prints their pids, then runs Run (short grace)
//	spawner:  starts a quick, a polite and a stubborn child, prints their pids, exits
//	quick:    exits after 100ms (becomes a zombie unless the init reaps it)
//	polite:   sleeps (default SIGTERM action)
//	stubborn: ignores SIGTERM and sleeps
func TestMain(m *testing.M) {
	switch os.Getenv("MF_INIT_ROLE") {
	case "init":
		roleInit()
	case "spawner":
		roleSpawner()
	case "quick":
		time.Sleep(100 * time.Millisecond)
		os.Exit(0)
	case "polite":
		time.Sleep(time.Minute)
		os.Exit(0)
	case "stubborn":
		ignoreTerm()
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func ignoreTerm() { signal.Ignore(syscall.SIGTERM) }

func roleInit() {
	// PR_SET_CHILD_SUBREAPER = 36: orphans reparent to us as they would to PID 1.
	if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, 36, 1, 0); e != 0 {
		fmt.Fprintln(os.Stderr, "prctl:", e)
		os.Exit(3)
	}
	grace = 1500 * time.Millisecond
	sp := exec.Command(os.Args[0])
	sp.Env = append(os.Environ(), "MF_INIT_ROLE=spawner")
	out, _ := sp.StdoutPipe()
	if err := sp.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(3)
	}
	sc := bufio.NewScanner(out)
	for i := 0; i < 3 && sc.Scan(); i++ {
		fmt.Println(sc.Text())
	}
	if err := run(); err != nil { // bypasses the PID 1 check (test hook)
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestRunRefusesWhenNotPID1(t *testing.T) {
	if err := Run(); err == nil || !strings.Contains(err.Error(), "PID 1") {
		t.Fatalf("Run outside PID 1: %v", err)
	}
	old := pid1
	pid1 = func() int { return 1 }
	defer func() { pid1 = old }()
	if err := checkPID1(); err != nil {
		t.Fatalf("hook not honoured: %v", err)
	}
}

func roleSpawner() {
	for _, r := range []string{"quick", "polite", "stubborn"} {
		c := exec.Command(os.Args[0])
		c.Env = append(os.Environ(), "MF_INIT_ROLE="+r)
		if err := c.Start(); err != nil {
			os.Exit(3)
		}
		fmt.Println(c.Process.Pid)
	}
	os.Exit(0) // orphans the three children
}

type fakeInit struct {
	cmd                     *exec.Cmd
	quick, polite, stubborn int
	done                    chan error
	stdout                  *bufio.Reader
}

func startInit(t *testing.T) *fakeInit {
	t.Helper()
	c := exec.Command(os.Args[0])
	c.Env = append(os.Environ(), "MF_INIT_ROLE=init")
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := c.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	f := &fakeInit{cmd: c, done: make(chan error, 1)}
	waited := make(chan struct{})
	go func() { err := c.Wait(); f.done <- err; close(waited) }()
	t.Cleanup(func() {
		_ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
		<-waited
	})
	sc := bufio.NewScanner(out)
	var pids []int
	for len(pids) < 3 && sc.Scan() {
		n, err := strconv.Atoi(sc.Text())
		if err != nil {
			t.Fatalf("bad pid line %q", sc.Text())
		}
		pids = append(pids, n)
	}
	if len(pids) != 3 {
		t.Fatalf("got %d pids from init", len(pids))
	}
	f.quick, f.polite, f.stubborn = pids[0], pids[1], pids[2]
	return f
}

func gone(pid int) bool {
	_, err := os.Stat("/proc/" + strconv.Itoa(pid))
	return errors.Is(err, os.ErrNotExist)
}

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout: %s", what)
}

func TestReapsOrphansAndSurvivesChildExit(t *testing.T) {
	f := startInit(t)
	// The quick orphan exited; if it were an unreaped zombie /proc/<pid> would remain.
	waitFor(t, "quick orphan reaped", 3*time.Second, func() bool { return gone(f.quick) })
	select {
	case err := <-f.done:
		t.Fatalf("init exited on child exit: %v", err)
	default:
	}
	// Terminated orphans must be reaped too, and SIGINT/SIGHUP must not end the init.
	_ = f.cmd.Process.Signal(syscall.SIGINT)
	_ = f.cmd.Process.Signal(syscall.SIGHUP)
	time.Sleep(200 * time.Millisecond)
	select {
	case err := <-f.done:
		t.Fatalf("init exited on SIGINT/SIGHUP: %v", err)
	default:
	}
	if gone(f.polite) || gone(f.stubborn) {
		t.Fatal("untouched orphans died")
	}
}

func TestSIGTERMTerminatesCleanly(t *testing.T) {
	f := startInit(t)
	waitFor(t, "quick orphan reaped", 3*time.Second, func() bool { return gone(f.quick) })
	start := time.Now()
	if err := f.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	// TERM goes to the process group (shared with the stubborn child that ignores it), so the
	// init waits out the grace and then SIGKILLs the group; init itself is in that group in this
	// non-PID-1 test, so a SIGKILL exit is also acceptable. As PID 1, kill(-1) spares it.
	select {
	case err := <-f.done:
		var ee *exec.ExitError
		if err != nil && !(errors.As(err, &ee) && ee.Sys().(syscall.WaitStatus).Signal() == syscall.SIGKILL) {
			t.Fatalf("init exit: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("init did not terminate")
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Fatalf("took %v", d)
	}
	waitFor(t, "polite orphan gone", 2*time.Second, func() bool { return dead(f.polite) })
	waitFor(t, "stubborn orphan gone", 2*time.Second, func() bool { return dead(f.stubborn) })
}

// dead reports a pid that is gone or a zombie (after the init itself died in the non-PID-1
// test, the kernel hands the orphans to whatever PID 1 the test runs under, which may not reap).
func dead(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	i := bytes.LastIndexByte(b, ')')
	return i >= 0 && i+2 < len(b) && b[i+2] == 'Z'
}
