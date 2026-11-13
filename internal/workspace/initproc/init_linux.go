// Package initproc is the PID 1 of a workspace container: a minimal zombie reaper that sleeps
// until SIGTERM. It touches no files and no network, prints nothing, and works as uid 1000.
package initproc

import (
	"errors"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// grace is how long children get after SIGTERM before SIGKILL (a var so tests can shorten it).
var grace = 3 * time.Second

// Run reaps orphaned children until SIGTERM, then forwards SIGTERM to every other process,
// waits up to the grace period, sends SIGKILL and returns nil. SIGINT and SIGHUP are ignored and
// a child exiting never ends it. Handlers are installed with signal.Notify rather than
// SIG_IGN so children (which reset handlers on exec) do not inherit ignored signals.
func Run() error {
	ch := make(chan os.Signal, 64)
	signal.Notify(ch, syscall.SIGCHLD, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	reap()
	for sig := range ch {
		switch sig {
		case syscall.SIGCHLD:
			reap()
		case syscall.SIGTERM:
			shutdown(ch)
			return nil
		}
	}
	return nil
}

func shutdown(ch <-chan os.Signal) {
	signalAll(syscall.SIGTERM)
	deadline := time.NewTimer(grace)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		if !reap() {
			return // no children left
		}
		select {
		case <-ch:
		case <-tick.C:
		case <-deadline.C:
			signalAll(syscall.SIGKILL)
			reap()
			return
		}
	}
}

// signalAll signals every process but ourselves. As PID 1 of a pid namespace, kill(-1) does
// exactly that. Elsewhere (tests) it signals our process group, which the test makes private.
func signalAll(sig syscall.Signal) {
	if os.Getpid() == 1 {
		_ = syscall.Kill(-1, sig)
		return
	}
	_ = syscall.Kill(0, sig)
}

// reap collects every exited child without blocking and reports whether children remain.
func reap() bool {
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		switch {
		case errors.Is(err, syscall.EINTR):
			continue
		case err != nil: // ECHILD: nothing left
			return false
		case pid == 0: // children exist, none exited
			return true
		}
	}
}
