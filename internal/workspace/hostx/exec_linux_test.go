//go:build linux

package hostx

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// TestMain lets the test binary act as the `mirafleet host-shim` binary.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "host-shim" {
		if err := RunShim(os.Args[2:]); err != nil {
			os.Stderr.WriteString("host-shim: " + err.Error() + "\n")
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

const decoy = "DECOY-SECRET-5f1c9b"

func testEnviron(extra ...string) []string {
	return append([]string{"PATH=" + os.Getenv("PATH"), "HOME=/nonexistent-home", "USER=dev", "LANG=C.UTF-8",
		"MIRAFLEET_ENROL_KEY=" + decoy, "AWS_SECRET_ACCESS_KEY=" + decoy, "TOKEN=" + decoy}, extra...)
}

func newEngine(t *testing.T, cfg Config) *Engine {
	t.Helper()
	if cfg.Shim == "" {
		cfg.Shim = os.Args[0]
	}
	if cfg.Environ == nil {
		cfg.Environ = testEnviron()
	}
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func newSession(t *testing.T) Session {
	t.Helper()
	root := t.TempDir()
	work := filepath.Join(root, "work")
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	return Session{ID: "s1", Dir: filepath.Join(root, "sess"), WorkDir: work}
}

func bash(t *testing.T, e *Engine, s Session, cmd string, to time.Duration) transport.WorkspaceExec {
	t.Helper()
	r, err := e.Bash(context.Background(), s, BashRequest{Command: cmd, Timeout: to})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func alive(pid int) bool { _, ok := procTicks(pid); return ok }

func TestChildEnvironmentIsClean(t *testing.T) {
	e := newEngine(t, Config{
		EnvAllow: []string{"EXTRA_OK"},
		Environ:  testEnviron("EXTRA_OK=yes", "EXTRA_NOT=no"),
	})
	s := newSession(t)
	s.Env = map[string]string{"SSH_AUTH_SOCK": "/run/s/agent.sock"}
	r := bash(t, e, s, `env; cat /proc/self/environ | tr '\0' '\n'`, 0)
	if r.ExitCode != 0 {
		t.Fatalf("exit %d: %s", r.ExitCode, r.Stderr)
	}
	if strings.Contains(r.Stdout+r.Stderr, decoy) {
		t.Fatalf("decoy secret reached the child:\n%s", r.Stdout)
	}
	for _, want := range []string{"HOME=/nonexistent-home", "USER=dev", "LANG=C.UTF-8", "EXTRA_OK=yes", "SSH_AUTH_SOCK=/run/s/agent.sock"} {
		if !strings.Contains(r.Stdout, want) {
			t.Errorf("missing %s", want)
		}
	}
	for _, bad := range []string{"EXTRA_NOT", "MIRAFLEET_", "AWS_", "TOKEN="} {
		if strings.Contains(r.Stdout, bad) {
			t.Errorf("leaked %s", bad)
		}
	}
	// A background job gets the same environment.
	j, err := e.StartJob(s, "env > envdump", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	waitJob(t, e, s, j.Job, transport.JobExited)
	b, err := os.ReadFile(filepath.Join(s.WorkDir, "envdump"))
	if err != nil || strings.Contains(string(b), decoy) || !strings.Contains(string(b), "EXTRA_OK=yes") {
		t.Fatalf("job env: %v %q", err, b)
	}
	if _, err := New(Config{EnvAllow: []string{"BAD NAME"}}); err == nil {
		t.Error("bad env_allow name accepted")
	}
}

func TestNoInheritedDescriptors(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	marker := filepath.Join(t.TempDir(), "held-open")
	f, err := os.Create(marker)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	e := newEngine(t, Config{})
	r := bash(t, e, newSession(t), `for f in /proc/$$/fd/*; do readlink "$f"; done`, 0)
	if strings.Contains(r.Stdout, "socket:") || strings.Contains(r.Stdout, marker) {
		t.Fatalf("inherited descriptors:\n%s", r.Stdout)
	}
}

func TestCwdPersists(t *testing.T) {
	e := newEngine(t, Config{})
	s := newSession(t)
	bash(t, e, s, "mkdir sub && cd sub", 0)
	want := filepath.Join(s.WorkDir, "sub")
	if got := strings.TrimSpace(bash(t, e, s, "pwd", 0).Stdout); got != want {
		t.Fatalf("cwd = %q, want %q", got, want)
	}
	// env does not persist
	bash(t, e, s, "export FOO=1", 0)
	if r := bash(t, e, s, `echo "[$FOO]"`, 0); strings.TrimSpace(r.Stdout) != "[]" {
		t.Errorf("env persisted: %q", r.Stdout)
	}
	// survives a runner restart (new Engine, same session dir)
	e2 := newEngine(t, Config{})
	if got := strings.TrimSpace(bash(t, e2, s, "pwd", 0).Stdout); got != want {
		t.Errorf("after restart cwd = %q", got)
	}
	// a failing command still reports the cwd; a vanished dir falls back to WorkDir
	bash(t, e, s, "cd /; exit 3", 0)
	if got := strings.TrimSpace(bash(t, e, s, "pwd", 0).Stdout); got != "/" {
		t.Errorf("cwd after exit = %q", got)
	}
	bash(t, e, s, "cd "+s.WorkDir+" && mkdir gone && cd gone", 0)
	_ = os.Remove(filepath.Join(s.WorkDir, "gone"))
	if got := strings.TrimSpace(bash(t, e, s, "pwd", 0).Stdout); got != s.WorkDir {
		t.Errorf("fallback cwd = %q", got)
	}
}

func TestTimeoutKillsWholeGroup(t *testing.T) {
	e := newEngine(t, Config{})
	s := newSession(t)
	start := time.Now()
	r := bash(t, e, s, `sleep 300 & echo $! > child.pid; sleep 300 & wait`, time.Second)
	if !r.TimedOut || r.ExitCode != 137 {
		t.Fatalf("timed_out=%v exit=%d", r.TimedOut, r.ExitCode)
	}
	if time.Since(start) > 15*time.Second {
		t.Fatalf("took %v", time.Since(start))
	}
	b, err := os.ReadFile(filepath.Join(s.WorkDir, "child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100 && alive(pid); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if alive(pid) {
		t.Errorf("grandchild %d survived the timeout", pid)
	}
}

func TestCancelKillsGroup(t *testing.T) {
	e := newEngine(t, Config{})
	s := newSession(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	r, err := e.Bash(ctx, s, BashRequest{Command: "sleep 300 & wait"})
	if err == nil || r.TimedOut || r.ExitCode != 137 {
		t.Fatalf("err=%v timed_out=%v exit=%d", err, r.TimedOut, r.ExitCode)
	}
}

func TestLargeOutputIsKeptAndPaged(t *testing.T) {
	e := newEngine(t, Config{})
	s := newSession(t)
	const n = 3 << 20
	const errLen = int64(len("to-stderr\n"))
	r := bash(t, e, s, `head -c 3145728 /dev/zero | tr '\0' 'a'; echo; sleep 0.5; echo "to-stderr" >&2`, 30*time.Second)
	if r.StdoutBytes != n+1 || r.StderrBytes != errLen {
		t.Fatalf("counts %d/%d", r.StdoutBytes, r.StderrBytes)
	}
	if !r.Truncated || r.Omitted <= 0 || r.Ref == "" || len(r.Stdout) > 40<<10 {
		t.Fatalf("truncated=%v omitted=%d ref=%q len=%d", r.Truncated, r.Omitted, r.Ref, len(r.Stdout))
	}
	if r.Stderr != "to-stderr\n" {
		t.Errorf("stderr = %q", r.Stderr)
	}
	// page the whole log with a small limit and rebuild it exactly
	var total int64
	for {
		o, err := e.ReadOutput(s, transport.OutputOp{Ref: r.Ref, Offset: total, Limit: 1 << 18})
		if err != nil {
			t.Fatal(err)
		}
		if o.Size != n+1+errLen || o.Offset != total {
			t.Fatalf("page offset %d size %d", o.Offset, o.Size)
		}
		total += int64(len(o.Content))
		if o.EOF {
			break
		}
	}
	if total != n+1+errLen {
		t.Fatalf("paged %d bytes", total)
	}
	tail, err := e.ReadOutput(s, transport.OutputOp{Ref: r.Ref, TailLines: 1})
	if err != nil || tail.Content != "to-stderr\n" || !tail.EOF {
		t.Fatalf("tail %+v %v", tail, err)
	}
	g, err := e.ReadOutput(s, transport.OutputOp{Ref: r.Ref, Grep: "^to-"})
	if err != nil || g.Content != "to-stderr\n" {
		t.Fatalf("grep %+v %v", g, err)
	}
	for _, bad := range []string{"..", ".", "../x", "a/b"} {
		if _, err := e.ReadOutput(s, transport.OutputOp{Ref: bad}); err == nil {
			t.Errorf("ref %q accepted", bad)
		}
	}
	if _, err := e.ReadOutput(s, transport.OutputOp{Ref: "nosuch"}); err == nil {
		t.Error("missing ref found")
	}
}

func TestSmallOutputNotMarkedAndLogsPruned(t *testing.T) {
	e := newEngine(t, Config{})
	s := newSession(t)
	r := bash(t, e, s, "echo hi; echo oops >&2; exit 7", 0)
	if r.ExitCode != 7 || r.Stdout != "hi\n" || r.Stderr != "oops\n" || r.Truncated || r.Omitted != 0 {
		t.Fatalf("%+v", r)
	}
	for i := 0; i < KeepOutputs+5; i++ {
		bash(t, e, s, "true", 0)
	}
	ents, _ := os.ReadDir(filepath.Join(s.Dir, "out"))
	if len(ents) != KeepOutputs {
		t.Errorf("%d logs kept", len(ents))
	}
	if _, err := e.Bash(context.Background(), s, BashRequest{Command: "true", RequestID: "../evil"}); err == nil {
		t.Error("bad request id accepted")
	}
}

func TestKillSession(t *testing.T) {
	e := newEngine(t, Config{})
	s := newSession(t)
	j, err := e.StartJob(s, "sleep 300", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = e.Bash(context.Background(), s, BashRequest{Command: "sleep 300", Timeout: 60 * time.Second})
	}()
	time.Sleep(500 * time.Millisecond)
	e.KillSession(s)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("foreground command survived KillSession")
	}
	if got, _ := e.JobInfo(s, j.Job); got.State != transport.JobCanceled {
		t.Errorf("job state %s", got.State)
	}
}

// `server &` in a foreground command does not survive the call.
func TestForegroundBackgroundChildDoesNotSurvive(t *testing.T) {
	e := newEngine(t, Config{})
	s := newSession(t)
	r := bash(t, e, s, `sleep 300 >/dev/null 2>&1 & echo $! > child.pid`, 0)
	if r.ExitCode != 0 {
		t.Fatalf("%+v", r)
	}
	b, err := os.ReadFile(filepath.Join(s.WorkDir, "child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100 && alive(pid); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if alive(pid) {
		t.Errorf("background child %d survived the foreground call", pid)
	}
}
