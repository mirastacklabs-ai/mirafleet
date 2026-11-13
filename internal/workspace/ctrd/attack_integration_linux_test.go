//go:build linux && integration

package ctrd

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/images"

	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
)

// ---- the in-container tool ----
//
// The test image holds one binary (this test binary, as /bin/init). Exec'd as
// `/bin/init mf-tool <cmd> ...` it acts as a tiny coreutils/attacker for the tests below.

func init() {
	if len(os.Args) < 2 || os.Args[1] != "mf-tool" {
		return
	}
	runtime.LockOSThread() // Pdeathsig of the children below is tied to the forking thread
	os.Exit(runTool(os.Args[2:]))
}

func runTool(a []string) int {
	if len(a) == 0 {
		return 2
	}
	fail := func(err error) int { fmt.Println("error:", err); return 1 }
	switch a[0] {
	case "id":
		fmt.Printf("uid=%d gid=%d\n", os.Getuid(), os.Getgid())
	case "cat":
		b, err := os.ReadFile(a[1])
		if err != nil {
			return fail(err)
		}
		os.Stdout.Write(b)
	case "catstdin":
		if _, err := io.Copy(os.Stdout, os.Stdin); err != nil {
			return fail(err)
		}
	case "touch":
		if err := os.WriteFile(a[1], []byte("x"), 0o644); err != nil {
			return fail(err)
		}
	case "mkdir":
		if err := os.MkdirAll(a[1], 0o755); err != nil {
			return fail(err)
		}
	case "rm":
		if err := os.RemoveAll(a[1]); err != nil {
			return fail(err)
		}
	case "symlink":
		if err := os.Symlink(a[1], a[2]); err != nil {
			return fail(err)
		}
	case "link":
		if err := os.Link(a[1], a[2]); err != nil {
			return fail(err)
		}
	case "mkfifo":
		if err := syscall.Mkfifo(a[1], 0o644); err != nil {
			return fail(err)
		}
	case "pwd":
		d, _ := os.Getwd()
		fmt.Println(d)
	case "env":
		fmt.Println(os.Getenv(a[1]))
	case "exit":
		n, _ := strconv.Atoi(a[1])
		return n
	case "outerr":
		fmt.Fprint(os.Stdout, "to-stdout")
		fmt.Fprint(os.Stderr, "to-stderr")
	case "flood": // write N bytes to stdout
		n, _ := strconv.Atoi(a[1])
		buf := bytes.Repeat([]byte("0123456789abcdef"), 4096)
		for n > 0 {
			k := min(n, len(buf))
			os.Stdout.Write(buf[:k])
			n -= k
		}
	case "sleep": // sleep <seconds> [marker]
		d, _ := strconv.ParseFloat(a[1], 64)
		time.Sleep(time.Duration(d * float64(time.Second)))
	case "spawnsleep": // spawnsleep <marker>: a child that outlives a naive kill of this process
		c := exec.Command("/bin/init", "mf-tool", "sleep", "120", a[1])
		if err := c.Start(); err != nil {
			return fail(err)
		}
		_ = c.Wait()
	case "dial":
		c, err := net.DialTimeout("tcp", a[1], 2*time.Second)
		if err == nil {
			c.Close()
			fmt.Println("dial-ok")
			return 0
		}
		fmt.Println("dial-failed:", err)
	case "ifaces":
		ifs, _ := net.Interfaces()
		for _, i := range ifs {
			fmt.Println(i.Name)
		}
	case "forkbomb":
		var kids []*exec.Cmd
		var ferr error
		for i := 0; i < 600; i++ {
			c := exec.Command("/bin/init", "mf-tool", "sleep", "20")
			c.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
			if err := c.Start(); err != nil {
				ferr = err
				break
			}
			kids = append(kids, c)
		}
		fmt.Printf("forkbomb started=%d err=%v\n", len(kids), ferr)
		for _, c := range kids {
			_ = c.Process.Kill()
		}
		for _, c := range kids {
			_ = c.Wait()
		}
	case "memhog":
		var hold [][]byte
		for i := 0; i < 48; i++ { // up to 768 MiB, 3x the test limit
			b := make([]byte, 16<<20)
			for j := 0; j < len(b); j += 4096 {
				b[j] = 1
			}
			hold = append(hold, b)
		}
		fmt.Println("memhog survived", len(hold))
	default:
		return 2
	}
	return 0
}

// ---- helpers ----

func toolArgs(a ...string) []string { return append([]string{"/bin/init", "mf-tool"}, a...) }

// run executes an in-container tool command and returns its output.
func (te *testEnv) run(t *testing.T, id workspace.ID, a ...string) (stdout, stderr string, res workspace.ExecResult) {
	t.Helper()
	var o, e bytes.Buffer
	ctx, cancel := context.WithTimeout(te.ctx, 90*time.Second)
	defer cancel()
	res, err := te.e.Exec(ctx, id, workspace.ExecRequest{Argv: toolArgs(a...), Stdout: &o, Stderr: &e})
	if err != nil {
		t.Fatalf("Exec %v: %v", a, err)
	}
	return o.String(), e.String(), res
}

func (te *testEnv) newWorkspace(t *testing.T) workspace.ID {
	t.Helper()
	s := testSpec()
	if err := te.e.Create(te.ctx, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	return s.ID
}

// newEnvMode is newEnv with a forced userns mode.
func newEnvMode(t *testing.T, mode string, concurrent int) *testEnv {
	t.Helper()
	c, ctx := testClient(t)
	img := importTestImage(t, c, ctx)
	alias, err := c.ImageService().Create(ctx, images.Image{Name: normalizedTestImage, Target: img.Target()})
	if err != nil {
		t.Fatalf("alias image: %v", err)
	}
	t.Cleanup(func() { _ = c.ImageService().Delete(context.Background(), alias.Name) })
	root := newRoot(t)
	ex, err := New(newConfig(t, root, mode, concurrent), Options{})
	if err != nil {
		t.Fatalf("New (userns %s): %v", mode, err)
	}
	e := ex.(*Executor)
	t.Cleanup(func() {
		infos, _ := e.List(context.Background())
		for _, i := range infos {
			_ = e.Destroy(context.Background(), i.ID)
		}
		_ = e.Close()
	})
	return &testEnv{e: e, root: root, c: c, ctx: ctx, mode: mode}
}

// hostPIDs returns the host-visible pids whose command line contains marker.
func hostPIDs(marker string) []int {
	ents, _ := os.ReadDir("/proc")
	var out []int
	for _, d := range ents {
		pid, err := strconv.Atoi(d.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		b, err := os.ReadFile("/proc/" + d.Name() + "/cmdline")
		if err == nil && strings.Contains(string(b), marker) {
			out = append(out, pid)
		}
	}
	return out
}

func waitNoPIDs(t *testing.T, marker string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		p := hostPIDs(marker)
		if len(p) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("processes with %q still alive: %v", marker, p)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func waitPIDs(t *testing.T, marker string) []int {
	t.Helper()
	for i := 0; i < 100; i++ {
		if p := hostPIDs(marker); len(p) > 0 {
			return p
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("no process with %q appeared", marker)
	return nil
}

func randMarker() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "mfmark" + hex.EncodeToString(b[:])
}

func statusField(t *testing.T, pid int, field string) string {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, field+":"); ok {
			return strings.TrimSpace(v)
		}
	}
	t.Fatalf("no %s in /proc/%d/status", field, pid)
	return ""
}

func readFile(t *testing.T, te *testEnv, id workspace.ID, p string, off, n int64) ([]byte, workspace.FileInfo, error) {
	t.Helper()
	rc, fi, err := te.e.ReadFile(te.ctx, id, p, off, n)
	if err != nil {
		return nil, fi, err
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	return b, fi, err
}

// ---- tests ----

func TestExecBasics(t *testing.T) {
	te := newEnv(t, 1, Options{})
	id := te.newWorkspace(t)

	out, _, res := te.run(t, id, "id")
	want := "uid=1000 gid=1000\n"
	if te.mode == "off" {
		_, _, owner := te.e.identity(0)
		want = fmt.Sprintf("uid=%d gid=%d\n", owner, owner)
	}
	if out != want || res.ExitCode != 0 || res.TimedOut {
		t.Errorf("id = %q %+v, want %q", out, res, want)
	}

	// stdout and stderr stay separate; the exit code is passed through.
	out, errs, res := te.run(t, id, "outerr")
	if out != "to-stdout" || errs != "to-stderr" {
		t.Errorf("outerr = %q / %q", out, errs)
	}
	if _, _, res = te.run(t, id, "exit", "7"); res.ExitCode != 7 {
		t.Errorf("exit code %d, want 7", res.ExitCode)
	}

	// cwd and env.
	if out, _, _ = te.run(t, id, "pwd"); out != "/workspace\n" {
		t.Errorf("default cwd %q", out)
	}
	var o bytes.Buffer
	if _, err := te.e.Exec(te.ctx, id, workspace.ExecRequest{Argv: toolArgs("mkdir", "/workspace/sub/dir"), Stdout: &o}); err != nil {
		t.Fatal(err)
	}
	o.Reset()
	if _, err := te.e.Exec(te.ctx, id, workspace.ExecRequest{Argv: toolArgs("pwd"), Cwd: "sub/dir", Stdout: &o}); err != nil || o.String() != "/workspace/sub/dir\n" {
		t.Errorf("cwd = %q %v", o.String(), err)
	}
	o.Reset()
	if _, err := te.e.Exec(te.ctx, id, workspace.ExecRequest{Argv: toolArgs("env", "FOO"), Env: map[string]string{"FOO": "bar baz"}, Stdout: &o}); err != nil || o.String() != "bar baz\n" {
		t.Errorf("env = %q %v", o.String(), err)
	}
	// Host environment does not leak in.
	t.Setenv("MF_HOST_SECRET", "leak")
	if out, _, _ = te.run(t, id, "env", "MF_HOST_SECRET"); out != "\n" {
		t.Errorf("host env leaked: %q", out)
	}

	// stdin: 1 MiB through, byte for byte; nil stdin reads EOF at once.
	in := make([]byte, 1<<20)
	_, _ = rand.Read(in)
	o.Reset()
	for _, size := range []int{1, 1000, 70 << 10, 1 << 20} {
		o.Reset()
		sctx, scancel := context.WithTimeout(te.ctx, 20*time.Second)
		t0 := time.Now()
		res, err := te.e.Exec(sctx, id, workspace.ExecRequest{Argv: toolArgs("catstdin"), Stdin: bytes.NewReader(in[:size]), Stdout: &o})
		scancel()
		t.Logf("stdin %d bytes: %+v err=%v out=%d took %s", size, res, err, o.Len(), time.Since(t0))
		if err != nil || res.TimedOut || !bytes.Equal(o.Bytes(), in[:size]) {
			t.Errorf("stdin %d bytes: %+v %v got %d bytes", size, res, err, o.Len())
		}
	}
	if out, _, res = te.run(t, id, "catstdin"); out != "" || res.ExitCode != 0 {
		t.Errorf("nil stdin: %q %+v", out, res)
	}

	// Large output arrives complete (8 MiB).
	const flood = 8 << 20
	var sink countingWriter
	if _, err := te.e.Exec(te.ctx, id, workspace.ExecRequest{Argv: toolArgs("flood", strconv.Itoa(flood)), Stdout: &sink}); err != nil {
		t.Fatal(err)
	}
	if sink.n != flood {
		t.Errorf("flood: got %d bytes, want %d", sink.n, flood)
	}

	// A command that does not exist behaves like a shell: 127 and a reason on stderr.
	var e bytes.Buffer
	res, err := te.e.Exec(te.ctx, id, workspace.ExecRequest{Argv: []string{"/nonexistent/prog"}, Stderr: &e})
	if err != nil || res.ExitCode != 127 || !strings.Contains(e.String(), "mirafleet:") {
		t.Errorf("missing program: %+v %v %q", res, err, e.String())
	}

	// Errors: unknown workspace, bad argv, bad cwd, bad env.
	if _, err := te.e.Exec(te.ctx, workspace.NewID(), workspace.ExecRequest{Argv: toolArgs("id")}); !errors.Is(err, workspace.ErrNotFound) {
		t.Errorf("unknown workspace: %v", err)
	}
	for _, r := range []workspace.ExecRequest{
		{}, {Argv: []string{""}}, {Argv: toolArgs("id"), Cwd: "/etc"}, {Argv: toolArgs("id"), Cwd: "../.."},
		{Argv: toolArgs("id"), Env: map[string]string{"A=B": "x"}},
	} {
		if _, err := te.e.Exec(te.ctx, id, r); !errors.Is(err, workspace.ErrRefused) {
			t.Errorf("Exec(%+v) = %v, want ErrRefused", r, err)
		}
	}
}

type countingWriter struct{ n int }

func (c *countingWriter) Write(p []byte) (int, error) { c.n += len(p); return len(p), nil }

func TestExecConcurrent(t *testing.T) {
	te := newEnv(t, 1, Options{})
	id := te.newWorkspace(t)
	var wg sync.WaitGroup
	errs := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var o bytes.Buffer
			m := fmt.Sprintf("job-%d", i)
			_, err := te.e.Exec(te.ctx, id, workspace.ExecRequest{Argv: toolArgs("env", "M"), Env: map[string]string{"M": m}, Stdout: &o})
			if err != nil || o.String() != m+"\n" {
				errs <- fmt.Sprintf("%s: %q %v", m, o.String(), err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// The exec process has the container's hardening, observed from the host side.
func TestExecIdentityAndHardening(t *testing.T) {
	te := newEnv(t, 1, Options{})
	id := te.newWorkspace(t)
	mk := randMarker()
	done := make(chan struct{})
	go func() {
		defer close(done)
		te.run(t, id, "sleep", "4", mk)
	}()
	pids := waitPIDs(t, mk)
	pid := pids[0]
	_, _, owner := te.e.identity(0)
	uid := strings.Fields(statusField(t, pid, "Uid"))[0]
	if uid != strconv.Itoa(int(owner)) || uid == "0" {
		t.Errorf("host uid of exec = %s, want mapped %d (never 0)", uid, owner)
	}
	if v := statusField(t, pid, "NoNewPrivs"); v != "1" {
		t.Errorf("NoNewPrivs = %s", v)
	}
	for _, f := range []string{"CapEff", "CapBnd", "CapPrm", "CapAmb"} {
		if v := statusField(t, pid, f); strings.Trim(v, "0") != "" {
			t.Errorf("%s = %s, want all zero", f, v)
		}
	}
	if v := statusField(t, pid, "Seccomp"); v != "2" {
		t.Errorf("Seccomp = %s", v)
	}
	<-done

	// Files the exec creates in /workspace are owned by the mapped uid on the host.
	te.run(t, id, "touch", "/workspace/owned")
	fi, err := os.Stat(filepath.Join(te.root, string(id), "owned"))
	if err != nil {
		t.Fatal(err)
	}
	if st := fi.Sys().(*syscall.Stat_t); st.Uid != owner {
		t.Errorf("file created in the container has host uid %d, want %d", st.Uid, owner)
	}

	// Read-only rootfs: writes outside /workspace and /tmp fail.
	for _, p := range []string{"/x", "/etc/x", "/bin/x", "/.mirafleet/x", "/sys/x"} {
		out, _, res := te.run(t, id, "touch", p)
		if res.ExitCode == 0 {
			t.Errorf("write to %s succeeded", p)
		}
		if p == "/x" || p == "/etc/x" {
			if !strings.Contains(out, "read-only file system") {
				t.Errorf("write to %s: %q, want EROFS", p, out)
			}
		}
	}
	for _, p := range []string{"/workspace/ok", "/tmp/ok"} {
		if _, _, res := te.run(t, id, "touch", p); res.ExitCode != 0 {
			t.Errorf("write to %s failed", p)
		}
	}
	if out, _, _ := te.run(t, id, "cat", "/etc/hosts"); !strings.Contains(out, "localhost") {
		t.Errorf("/etc/hosts: %q", out)
	}
}

func TestExecNoNetwork(t *testing.T) {
	te := newEnv(t, 1, Options{})
	id := te.newWorkspace(t)
	if out, _, _ := te.run(t, id, "ifaces"); strings.TrimSpace(out) != "lo" {
		t.Errorf("interfaces = %q, want only lo", out)
	}
	for _, addr := range []string{"1.1.1.1:80", "127.0.0.1:9", "169.254.169.254:80", "10.0.0.1:22"} {
		out, _, _ := te.run(t, id, "dial", addr)
		if !strings.HasPrefix(out, "dial-failed") {
			t.Errorf("dial %s: %q", addr, out)
		}
	}
}

func TestExecTimeoutAndCancelKill(t *testing.T) {
	te := newEnv(t, 1, Options{})
	id := te.newWorkspace(t)

	// Deadline: killed, TimedOut, no error; the child it spawned dies too.
	mk := randMarker()
	ctx, cancel := context.WithTimeout(te.ctx, 1500*time.Millisecond)
	start := time.Now()
	res, err := te.e.Exec(ctx, id, workspace.ExecRequest{Argv: toolArgs("spawnsleep", mk)})
	cancel()
	if err != nil || !res.TimedOut {
		t.Fatalf("timeout: %+v %v", res, err)
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Errorf("timeout took %s", d)
	}
	if res.ExitCode != 137 {
		t.Logf("exit code after SIGKILL: %d", res.ExitCode)
	}
	waitNoPIDs(t, mk, 5*time.Second)

	// Cancellation: error is the context's, the process is gone, the workspace still works.
	mk2 := randMarker()
	cctx, ccancel := context.WithCancel(te.ctx)
	go func() { time.Sleep(700 * time.Millisecond); ccancel() }()
	res, err = te.e.Exec(cctx, id, workspace.ExecRequest{Argv: toolArgs("spawnsleep", mk2)})
	if !errors.Is(err, context.Canceled) || res.TimedOut {
		t.Fatalf("cancel: %+v %v", res, err)
	}
	waitNoPIDs(t, mk2, 5*time.Second)
	if out, _, r := te.run(t, id, "id"); r.ExitCode != 0 || out == "" {
		t.Errorf("workspace broken after kill: %q %+v", out, r)
	}
	// No exec or task process leaked from the executor's side.
	task, _ := te.container(t, id).Task(te.ctx, nil)
	ps, err := task.Pids(te.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 {
		t.Errorf("container has %d processes after the kills, want 1 (init): %v", len(ps), ps)
	}
}

func TestForkBombHitsPidsLimit(t *testing.T) {
	te := newEnv(t, 1, Options{})
	id := te.newWorkspace(t)
	out, _, res := te.run(t, id, "forkbomb")
	t.Logf("forkbomb: %q exit %d", strings.TrimSpace(out), res.ExitCode)
	if !strings.Contains(out, "forkbomb started=") && res.ExitCode == 0 {
		t.Errorf("unexpected forkbomb output %q", out)
	}
	if n := strings.TrimSpace(strings.TrimPrefix(strings.SplitN(out, " err=", 2)[0], "forkbomb started=")); n != "" {
		if k, _ := strconv.Atoi(n); k >= 128 {
			t.Errorf("started %d children despite pids limit 128", k)
		}
	}
	// The runner (this process) and the workspace survive.
	time.Sleep(500 * time.Millisecond)
	if out, _, r := te.run(t, id, "id"); r.ExitCode != 0 || out == "" {
		t.Errorf("workspace unusable after fork bomb: %q %+v", out, r)
	}
	if out, _, _ := te.run(t, id, "cat", "/sys/fs/cgroup/pids.events"); out != "" && !strings.Contains(out, "max ") {
		t.Errorf("pids.events: %q", out)
	} else if strings.Contains(out, "max 0") {
		t.Errorf("pids.max never reached: %q", out)
	}
}

func TestMemoryHogIsOOMKilled(t *testing.T) {
	te := newEnv(t, 1, Options{})
	id := te.newWorkspace(t)
	start := time.Now()
	out, _, res := te.run(t, id, "memhog")
	t.Logf("memhog: %q exit %d after %s", strings.TrimSpace(out), res.ExitCode, time.Since(start))
	if strings.Contains(out, "survived") || res.ExitCode == 0 {
		t.Fatalf("memory hog was not killed: %q %+v", out, res)
	}
	if res.ExitCode != 137 {
		t.Errorf("exit code %d, want 137 (SIGKILL)", res.ExitCode)
	}
	if ev, _, _ := te.run(t, id, "cat", "/sys/fs/cgroup/memory.events"); ev != "" && !strings.Contains(ev, "oom_kill 1") {
		t.Errorf("memory.events: %q", ev)
	}
	if out, _, r := te.run(t, id, "id"); r.ExitCode != 0 || out == "" {
		t.Errorf("workspace unusable after OOM: %q %+v", out, r)
	}
}

// Files: everything the container plants in /workspace to trick the host-side access.
func TestFileAttacks(t *testing.T) {
	te := newEnv(t, 1, Options{})
	id := te.newWorkspace(t)
	dir := filepath.Join(te.root, string(id))

	secretDir := t.TempDir()
	secret := filepath.Join(secretDir, "secret")
	if err := os.WriteFile(secret, []byte("HOST SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Symlinks made from inside the container.
	for name, target := range map[string]string{
		"pw":       "/etc/passwd",
		"procroot": "/proc/self/root/etc/passwd",
		"proc1":    "/proc/1/root/etc/passwd",
		"up":       "../../../../../../../../etc/passwd",
		"hostabs":  secret,
		"dirlink":  "/",
	} {
		if out, _, res := te.run(t, id, "symlink", target, "/workspace/"+name); res.ExitCode != 0 {
			t.Fatalf("symlink %s: %s", name, out)
		}
	}
	// And symlinks planted on the host side, straight at host files (a stricter attacker).
	hostRel, _ := filepath.Rel(dir, secret)
	for name, target := range map[string]string{"hs-abs": secret, "hs-rel": hostRel, "hs-dir": secretDir} {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	passwd, _ := os.ReadFile("/etc/passwd") // the harness's own, to detect a host read
	for _, p := range []string{"pw", "procroot", "proc1", "up", "hostabs", "dirlink/etc/passwd", "dirlink",
		"hs-abs", "hs-rel", "hs-dir/secret", "hs-dir"} {
		b, _, err := readFile(t, te, id, p, 0, 0)
		if err == nil {
			t.Errorf("read %s succeeded: %q", p, b)
		}
		if bytes.Contains(b, []byte("HOST SECRET")) || (len(b) > 0 && bytes.Equal(b, passwd)) {
			t.Errorf("read %s returned host data", p)
		}
		if _, err := te.e.WriteFile(te.ctx, id, p, strings.NewReader("pwned"), 0, false, 0o644); err == nil && !strings.HasPrefix(p, "pw") {
			t.Errorf("write through %s succeeded", p)
		}
	}
	if b, _ := os.ReadFile(secret); string(b) != "HOST SECRET" {
		t.Errorf("host secret modified: %q", b)
	}

	// Path tricks in the request itself.
	for _, p := range []string{"/workspace/../etc/passwd", "../../etc/passwd", "/etc/passwd", "/proc/self/root/etc/passwd",
		"/workspace/..", "/workspace", "a\x00b", "/workspace/x/../../etc/passwd"} {
		if _, _, err := readFile(t, te, id, p, 0, 0); !errors.Is(err, workspace.ErrRefused) {
			t.Errorf("read %q = %v, want ErrRefused", p, err)
		}
		if _, err := te.e.WriteFile(te.ctx, id, p, strings.NewReader("x"), 0, true, 0o644); !errors.Is(err, workspace.ErrRefused) {
			t.Errorf("write %q = %v, want ErrRefused", p, err)
		}
	}

	// FIFO planted by the container: an error, quickly (no hang).
	te.run(t, id, "mkfifo", "/workspace/fifo")
	for _, op := range []string{"read", "write"} {
		done := make(chan error, 1)
		go func() {
			if op == "read" {
				_, _, err := readFile(t, te, id, "fifo", 0, 0)
				done <- err
				return
			}
			_, err := te.e.WriteFile(te.ctx, id, "fifo", strings.NewReader("x"), 0, false, 0o644)
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("%s of a FIFO succeeded", op)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s of a FIFO hung", op)
		}
	}

	// Hardlinks: across filesystems impossible; within the workspace harmless.
	if out, _, res := te.run(t, id, "link", "/etc/passwd", "/workspace/hl-passwd"); res.ExitCode == 0 {
		t.Errorf("hardlink to image file succeeded: %q", out)
	}
	if _, err := os.Lstat(filepath.Join(dir, "hl-passwd")); err == nil {
		t.Error("hardlink to /etc/passwd exists in the workspace")
	}
	if out, _, res := te.run(t, id, "link", "/workspace/hs-abs", "/tmp/hl"); res.ExitCode == 0 {
		t.Logf("link from workspace to /tmp: %q", out) // different mounts: expected to fail
	}
	if _, err := te.e.WriteFile(te.ctx, id, "orig", strings.NewReader("one"), 0, true, 0o644); err != nil {
		t.Fatal(err)
	}
	te.run(t, id, "link", "/workspace/orig", "/workspace/twin")
	if b, _, err := readFile(t, te, id, "twin", 0, 0); err != nil || string(b) != "one" {
		t.Errorf("read hardlink twin: %q %v", b, err)
	}
	// Replacing 'orig' (truncate = new inode) must not change the twin.
	if _, err := te.e.WriteFile(te.ctx, id, "orig", strings.NewReader("two"), 0, true, 0o644); err != nil {
		t.Fatal(err)
	}
	if b, _, _ := readFile(t, te, id, "twin", 0, 0); string(b) != "one" {
		t.Errorf("twin changed through truncating write: %q", b)
	}

	// Directory-swap: a directory the container replaced by a symlink to / after a write.
	te.run(t, id, "mkdir", "/workspace/d")
	te.run(t, id, "rm", "/workspace/d")
	te.run(t, id, "symlink", "/", "/workspace/d")
	if _, err := te.e.WriteFile(te.ctx, id, "d/tmp/escape", strings.NewReader("x"), 0, true, 0o644); err == nil {
		t.Error("write through directory symlink to / succeeded")
	}
	if _, err := os.Stat("/tmp/escape"); err == nil {
		t.Error("file created on the host through a directory symlink")
	}
}

func TestFileRoundTripAndOwnership(t *testing.T) {
	te := newEnv(t, 1, Options{})
	id := te.newWorkspace(t)
	_, _, owner := te.e.identity(0)

	data := make([]byte, 1536<<10)
	_, _ = rand.Read(data)
	const chunk = 1 << 20
	for off := 0; off < len(data); off += chunk {
		end := min(off+chunk, len(data))
		fi, err := te.e.WriteFile(te.ctx, id, "dir/big.bin", bytes.NewReader(data[off:end]), int64(off), off == 0, 0o640)
		if err != nil {
			t.Fatalf("write at %d: %v", off, err)
		}
		if fi.Size != int64(end) {
			t.Errorf("size after write = %d, want %d", fi.Size, end)
		}
	}
	var got []byte
	for off := int64(0); ; {
		b, fi, err := readFile(t, te, id, "/workspace/dir/big.bin", off, chunk)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Size != int64(len(data)) || fi.Mode.Perm() != 0o640 {
			t.Errorf("info = %+v", fi)
		}
		if len(b) == 0 {
			break
		}
		got = append(got, b...)
		off += int64(len(b))
	}
	if sha256.Sum256(got) != sha256.Sum256(data) {
		t.Errorf("sha256 mismatch: %d bytes read, %d written", len(got), len(data))
	}
	for _, p := range []string{"dir", "dir/big.bin"} {
		fi, err := os.Stat(filepath.Join(te.root, string(id), p))
		if err != nil {
			t.Fatal(err)
		}
		if st := fi.Sys().(*syscall.Stat_t); st.Uid != owner || st.Gid != owner {
			t.Errorf("%s owned by %d:%d on the host, want %d", p, st.Uid, st.Gid, owner)
		}
	}
	// The container sees (and can modify) what the host side wrote.
	if out, _, _ := te.run(t, id, "cat", "/workspace/dir/big.bin"); len(out) != len(data) {
		t.Errorf("container read %d bytes", len(out))
	}
	// max_file (4 MiB in the test config).
	if _, err := te.e.WriteFile(te.ctx, id, "huge", bytes.NewReader(make([]byte, 5<<20)), 0, true, 0o644); !errors.Is(err, workspace.ErrTooLarge) {
		t.Errorf("5 MiB write: %v", err)
	}
	if _, err := te.e.WriteFile(te.ctx, id, "mode", strings.NewReader("x"), 0, true, 0o4755); !errors.Is(err, workspace.ErrRefused) {
		t.Errorf("setuid mode: %v", err)
	}
}

func TestDiskQuota(t *testing.T) {
	old, oldInt := testDisk, diskInterval
	testDisk, diskInterval = "256KiB", 0
	t.Cleanup(func() { testDisk, diskInterval = old, oldInt })

	te := newEnv(t, 1, Options{})
	id := te.newWorkspace(t)
	blob := make([]byte, 300<<10)
	_, _ = rand.Read(blob)

	if _, err := te.e.WriteFile(te.ctx, id, "a", bytes.NewReader(blob), 0, true, 0o644); err != nil {
		t.Fatalf("first write (under quota): %v", err)
	}
	// 300 KiB >= 256 KiB: the next write is refused straight away (usage includes our own bytes).
	if _, err := te.e.WriteFile(te.ctx, id, "b", strings.NewReader("x"), 0, true, 0o644); !errors.Is(err, workspace.ErrQuota) {
		t.Fatalf("write over quota: %v, want ErrQuota", err)
	}
	u, err := te.e.Usage(te.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !u.Over || u.Kill || u.Quota != 256<<10 || u.Used < 300<<10 {
		t.Errorf("usage = %+v", u)
	}
	// Reads and exec still work over quota.
	if _, _, err := readFile(t, te, id, "a", 0, 10); err != nil {
		t.Errorf("read over quota: %v", err)
	}
	// 2x over: the kill line (data written behind the executor's back, as the container could).
	if err := os.WriteFile(filepath.Join(te.root, string(id), "c"), make([]byte, 400<<10), 0o600); err != nil {
		t.Fatal(err)
	}
	if u, _ = te.e.Usage(te.ctx, id); !u.Kill {
		t.Errorf("usage = %+v, want Kill", u)
	}
	// Deleting makes room again (the cached measurement is at most 2 s old for writes).
	te.run(t, id, "rm", "/workspace/a")
	te.run(t, id, "rm", "/workspace/c")
	time.Sleep(2200 * time.Millisecond)
	if _, err := te.e.WriteFile(te.ctx, id, "d", strings.NewReader("x"), 0, true, 0o644); err != nil {
		t.Errorf("write after freeing space: %v", err)
	}
	if u, _ = te.e.Usage(te.ctx, id); u.Over || u.Used > 64<<10 {
		t.Errorf("usage after cleanup = %+v", u)
	}
}

// The default config of the other tests uses userns "required" when the kernel allows it; this
// forces the "off" path once: create, exec, files, destroy.
func TestUsernsOffPath(t *testing.T) {
	testClient(t)
	te := newEnvMode(t, "off", 1)
	id := te.newWorkspace(t)
	_, _, owner := te.e.identity(0)
	if owner == 0 || owner == 1000 {
		t.Fatalf("userns off owner = %d, want a dedicated non-root uid", owner)
	}
	out, _, res := te.run(t, id, "id")
	if want := fmt.Sprintf("uid=%d gid=%d\n", owner, owner); out != want || res.ExitCode != 0 {
		t.Errorf("id = %q %+v, want %q", out, res, want)
	}
	if _, _, res = te.run(t, id, "touch", "/etc/x"); res.ExitCode == 0 {
		t.Error("write to /etc succeeded")
	}
	if _, err := te.e.WriteFile(te.ctx, id, "f", strings.NewReader("hello"), 0, true, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, _, _ = te.run(t, id, "cat", "/workspace/f"); out != "hello" {
		t.Errorf("container read %q", out)
	}
	if b, _, err := readFile(t, te, id, "f", 0, 0); err != nil || string(b) != "hello" {
		t.Errorf("host read %q %v", b, err)
	}
	te.run(t, id, "touch", "/workspace/g")
	if fi, err := os.Stat(filepath.Join(te.root, string(id), "g")); err != nil || fi.Sys().(*syscall.Stat_t).Uid != owner {
		t.Errorf("owner of file created in container: %v %v", fi, err)
	}
	if err := te.e.Destroy(te.ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := te.e.Exec(te.ctx, id, workspace.ExecRequest{Argv: toolArgs("id")}); !errors.Is(err, workspace.ErrNotFound) {
		t.Errorf("Exec after Destroy: %v", err)
	}
}

// testDisk is the limits.disk of newConfig; TestDiskQuota lowers it.
var testDisk = "1GiB"
