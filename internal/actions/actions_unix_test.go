//go:build unix

package actions

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A backgrounded grandchild inherits the output pipes. Run must still report the real exit
// code (not -1 / ErrWaitDelay), not flag a timeout, and kill the grandchild afterwards.
func TestGrandchildHoldingPipes(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	s, err := Parse([]byte(`{actions: {a: {timeout: 20s, run: ["/bin/sh", "-c", "/bin/sleep 60 & echo $! > ` + pidFile + `; echo out; exit 3"]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Run(context.Background(), "a", nil)
	if err != nil || r.ExitCode != 3 || r.TimedOut || strings.TrimSpace(r.Stdout) != "out" {
		t.Fatalf("%+v %v", r, err)
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	deadline := time.Now().Add(3 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d outlived the action", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// alive reports whether pid is a live process. A zombie (state Z/X, e.g. under a PID 1 that
// does not reap orphans) counts as dead; kill(pid, 0) alone succeeds on zombies.
func alive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	if runtime.GOOS != "linux" {
		return true
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 || i+2 >= len(s) {
		return true
	}
	return s[i+2] != 'Z' && s[i+2] != 'X'
}
