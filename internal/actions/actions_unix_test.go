//go:build unix

package actions

import (
	"context"
	"os"
	"path/filepath"
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
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d outlived the action", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
