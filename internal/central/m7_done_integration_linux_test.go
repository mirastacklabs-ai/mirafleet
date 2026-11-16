//go:build linux && integration

package central

// The end-to-end host-mode scenario with real host-mode runners, run by
// `scripts/dev.sh integration-host` as root in a throwaway container that has the unprivileged
// users dev and qa, git, openssh-client and (best effort) ripgrep. Everything is the real
// `mirafleet` binary (MIRAFLEET_TEST_BIN): central, two runners (one as dev, one as qa, started
// with setuid to the user) and the MCP server, which the test speaks to in JSON-RPC over stdio.
// The fake GitLab (internal/testfake/gitlab) listens on the container's own address, which
// /etc/hosts maps to gitlab.test and registry.gitlab.test (MIRAFLEET_TEST_FAKE_IP), on ports 443
// and 22 so the clones use plain URLs. Keys and tokens are generated at test time. The long
// build step takes 3 minutes; MIRAFLEET_TEST_M7_BUILD_STEPS shortens it for development.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/opconf"
	"github.com/mirastacklabs-ai/mirafleet/internal/testfake/gitlab"
)

// ---- processes ----

type m7iBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *m7iBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *m7iBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type m7iProc struct {
	cmd  *exec.Cmd
	out  *m7iBuf
	done chan struct{}
}

func (p *m7iProc) running() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

func (p *m7iProc) stop() {
	if !p.running() {
		return
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(20 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.done
	}
}

// m7iUser is an unprivileged account of the harness container.
type m7iUser struct {
	name     string
	uid, gid uint32
	home     string
}

func m7iLookup(t *testing.T, name string) m7iUser {
	t.Helper()
	u, err := user.Lookup(name)
	if err != nil {
		t.Fatalf("user %s: %v (run scripts/dev.sh integration-host)", name, err)
	}
	uid, _ := strconv.ParseUint(u.Uid, 10, 32)
	gid, _ := strconv.ParseUint(u.Gid, 10, 32)
	return m7iUser{name: name, uid: uint32(uid), gid: uint32(gid), home: u.HomeDir}
}

// env is the minimal environment of a process run as this user.
func (u m7iUser) env(path string) []string {
	return []string{"HOME=" + u.home, "USER=" + u.name, "LOGNAME=" + u.name, "PATH=" + path, "LANG=C.UTF-8"}
}

func m7iStart(t *testing.T, as *m7iUser, env []string, stdin string, bin string, args ...string) *m7iProc {
	t.Helper()
	p := &m7iProc{out: &m7iBuf{}, done: make(chan struct{})}
	p.cmd = exec.Command(bin, args...)
	p.cmd.Env = env
	if stdin != "" {
		p.cmd.Stdin = strings.NewReader(stdin)
	}
	p.cmd.Stdout, p.cmd.Stderr = p.out, p.out
	if as != nil {
		p.cmd.Dir = as.home
		p.cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: as.uid, Gid: as.gid}}
	}
	if err := p.cmd.Start(); err != nil {
		t.Fatalf("start %v: %v", args, err)
	}
	go func() { _ = p.cmd.Wait(); close(p.done) }()
	t.Cleanup(p.stop)
	return p
}

func m7iWait(t *testing.T, what string, timeout time.Duration, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// ---- the MCP client (JSON-RPC over the stdio of a real `mirafleet mcp`) ----

type m7iMCP struct {
	t      *testing.T
	cmd    *exec.Cmd
	in     *os.File
	lines  chan string
	stderr *m7iBuf
	done   chan struct{}
	nextID int
	all    []string // every stdout line, for the final hygiene scan
	home   map[string]string
}

func m7iStartMCP(t *testing.T, bin, project string, env []string) *m7iMCP {
	t.Helper()
	inR, inW, err := os.Pipe()
	must(t, err)
	outR, outW, err := os.Pipe()
	must(t, err)
	c := &m7iMCP{t: t, in: inW, lines: make(chan string, 256), stderr: &m7iBuf{}, done: make(chan struct{}), home: map[string]string{}}
	c.cmd = exec.Command(bin, "mcp", "-project-dir", project)
	c.cmd.Env, c.cmd.Dir = env, project
	c.cmd.Stdin, c.cmd.Stdout, c.cmd.Stderr = inR, outW, c.stderr
	must(t, c.cmd.Start())
	inR.Close()
	outW.Close()
	go func() {
		defer close(c.lines)
		defer outR.Close()
		br := bufio.NewReaderSize(outR, 1<<20)
		for {
			l, err := br.ReadString('\n')
			if l != "" {
				c.lines <- l
			}
			if err != nil {
				return
			}
		}
	}()
	go func() { _ = c.cmd.Wait(); close(c.done) }()
	t.Cleanup(func() {
		c.in.Close()
		_ = c.cmd.Process.Kill()
		<-c.done
	})
	return c
}

func (c *m7iMCP) rpc(method string, params any) map[string]any {
	c.t.Helper()
	c.nextID++
	id := c.nextID
	b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	must(c.t, err)
	if _, err := c.in.Write(append(b, '\n')); err != nil {
		c.t.Fatal(err)
	}
	timeout := time.After(10 * time.Minute)
	for {
		select {
		case l, ok := <-c.lines:
			if !ok {
				c.t.Fatalf("mcp stdout closed; stderr: %q", c.stderr.String())
			}
			c.all = append(c.all, l)
			var m map[string]any
			if err := json.Unmarshal([]byte(l), &m); err != nil || m["jsonrpc"] != "2.0" {
				c.t.Fatalf("mcp stdout line is not JSON-RPC: %q", l)
			}
			if m["method"] != nil {
				continue // notification
			}
			if m["id"] != float64(id) {
				c.t.Fatalf("response id %v, want %d", m["id"], id)
			}
			return m
		case <-timeout:
			c.t.Fatalf("timeout waiting for %s; stderr: %q", method, c.stderr.String())
		}
	}
}

func (c *m7iMCP) handshake() {
	c.t.Helper()
	c.rpc("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "m7i", "version": "0"}})
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	_, err := c.in.Write(append(b, '\n'))
	must(c.t, err)
}

// tryCall runs a tool and returns its structured result, the text of an error result, and
// whether it was an error.
func (c *m7iMCP) tryCall(fleet, name string, args map[string]any) (map[string]any, string, bool) {
	c.t.Helper()
	a := map[string]any{"fleet": fleet}
	for k, v := range args {
		a[k] = v
	}
	r := c.rpc("tools/call", map[string]any{"name": name, "arguments": a})
	if e, ok := r["error"]; ok {
		return nil, fmt.Sprint(e), true
	}
	res, ok := r["result"].(map[string]any)
	if !ok {
		c.t.Fatalf("%s: %v", name, r)
	}
	text := ""
	if cs, ok := res["content"].([]any); ok {
		for _, x := range cs {
			if m, ok := x.(map[string]any); ok {
				text += fmt.Sprint(m["text"]) + "\n"
			}
		}
	}
	if res["isError"] == true {
		return nil, text, true
	}
	if sc, ok := res["structuredContent"].(map[string]any); ok {
		return sc, text, false
	}
	var out map[string]any
	if json.Unmarshal([]byte(strings.TrimSpace(text)), &out) == nil {
		return out, text, false
	}
	c.t.Fatalf("%s: no structured result: %v", name, res)
	return nil, "", false
}

func (c *m7iMCP) call(fleet, name string, args map[string]any) map[string]any {
	c.t.Helper()
	out, text, isErr := c.tryCall(fleet, name, args)
	if isErr {
		c.t.Fatalf("%s %v: tool error: %s", name, args, text)
	}
	return out
}

// sh runs a command through the bash tool; it returns exit code, stdout and stderr.
func (c *m7iMCP) sh(fleet, cmd string) (int, string, string) {
	c.t.Helper()
	if d := c.home[fleet]; d != "" { // the shell's directory persists between calls: start each from the session dir
		cmd = "cd " + d + " && " + cmd
	}
	r := c.call(fleet, "bash", map[string]any{"command": cmd, "timeout_s": 300})
	code, _ := r["exit_code"].(float64)
	return int(code), fmt.Sprint(r["stdout"]), fmt.Sprint(r["stderr"])
}

func (c *m7iMCP) shOK(fleet, cmd string) string {
	c.t.Helper()
	code, so, se := c.sh(fleet, cmd)
	if code != 0 {
		c.t.Fatalf("%s: exit %d\nstdout: %s\nstderr: %s", cmd, code, so, se)
	}
	return so
}

// ---- helpers ----

// m7iDecoy hides the first character of s in a bracket expression, so that a grep -E pattern
// built from it matches s but not the text of its own command line.
func m7iDecoy(s string) string {
	return "[" + s[:1] + "]" + regexp.QuoteMeta(s[1:])
}

func m7iFreePort(t *testing.T, ip string) string {
	t.Helper()
	l, err := net.Listen("tcp", ip+":0")
	must(t, err)
	defer l.Close()
	return l.Addr().String()
}

// ---- the test ----

func TestM7DoneIntegration(t *testing.T) {
	bin := os.Getenv("MIRAFLEET_TEST_BIN")
	ip := os.Getenv("MIRAFLEET_TEST_FAKE_IP")
	users := strings.Fields(os.Getenv("MIRAFLEET_TEST_RUNNER_USERS"))
	if bin == "" || ip == "" || len(users) < 2 {
		t.Skip("MIRAFLEET_TEST_BIN / MIRAFLEET_TEST_FAKE_IP / MIRAFLEET_TEST_RUNNER_USERS not set; run scripts/dev.sh integration-host")
	}
	if os.Geteuid() != 0 {
		t.Skip("needs root to run the runners as other users")
	}
	steps := 18 // 18 x 10 s = the 3-minute build of point 2
	if s := os.Getenv("MIRAFLEET_TEST_M7_BUILD_STEPS"); s != "" {
		steps, _ = strconv.Atoi(s)
	}
	dev, qa := m7iLookup(t, users[0]), m7iLookup(t, users[1])
	t.Cleanup(func() { // whatever a failed run left behind
		for _, u := range []string{dev.name, qa.name} {
			_ = exec.Command("pkill", "-KILL", "-u", u).Run()
		}
	})

	base, err := os.MkdirTemp("", "m7i")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(base) })
	must(t, os.Chmod(base, 0o755))
	dataDir := filepath.Join(base, "central")
	opCfg := filepath.Join(base, "operator")
	project := filepath.Join(base, "project")
	for _, d := range []string{opCfg, project, filepath.Join(base, "state")} {
		must(t, os.Mkdir(d, 0o700))
	}

	// ---- fake GitLab on the container's address, plain ports ----
	fake := gitlab.Start(t, gitlab.Options{
		HTTPSAddr: ip + ":443", SSHAddr: ip + ":22",
		Hosts: []string{"gitlab.test", "registry.gitlab.test", ip},
	})
	fake.NewRepo(t, "acme/app", map[string]string{
		"README.md": "# app\n", "main.txt": "needle in the haystack\nsecond line\n", "sub/deep.txt": "needle deep\n",
	})
	manifestDigest := fake.AddImage(t, "acme/app", "1.0")
	// A second fake: an SSH server whose host key is not pinned and an HTTPS host that is not a
	// credential host (certificate from the same corporate CA).
	other := gitlab.Start(t, gitlab.Options{
		HTTPSAddr: m7iFreePort(t, ip), SSHAddr: m7iFreePort(t, ip), CA: fake.CA,
		Hosts: []string{"other.test", ip},
	})
	hosts, err := os.OpenFile("/etc/hosts", os.O_APPEND|os.O_WRONLY, 0)
	must(t, err)
	_, err = fmt.Fprintf(hosts, "%s other.test\n", ip)
	must(t, err)
	hosts.Close()
	caFile := filepath.Join(base, "ca.pem")
	must(t, os.WriteFile(caFile, fake.CA.PEM(), 0o644))
	khFile := filepath.Join(base, "known_hosts")
	must(t, os.WriteFile(khFile, []byte(fake.KnownHostsLine("gitlab.test")+"\n"), 0o644))

	privBody := strings.Split(strings.TrimSpace(string(fake.DeployKey.PrivatePEM)), "\n")
	decoys := []string{fake.ProjectToken, fake.RegistryPassword, privBody[2]}
	pubBody := strings.Fields(fake.DeployKey.Authorized)[1]
	var pat []string
	for _, d := range decoys {
		pat = append(pat, "-e", "'"+m7iDecoy(d)+"'")
	}
	scanArgs := strings.Join(pat, " ")

	// ---- the CLI ----
	cliEnv := []string{opconf.EnvDir + "=" + opCfg, opconf.EnvStateDir + "=" + filepath.Join(base, "state"),
		"HOME=" + base, "PATH=" + os.Getenv("PATH")}
	cli := func(stdin string, args ...string) (string, error) {
		cmd := exec.Command(bin, args...)
		cmd.Env = cliEnv
		cmd.Stdin = strings.NewReader(stdin)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		return out.String(), err
	}
	mustCLI := func(stdin string, args ...string) string {
		t.Helper()
		out, err := cli(stdin, args...)
		if err != nil {
			t.Fatalf("mirafleet %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}

	// ---- central ----
	keyLine := ""
	for _, l := range strings.Split(mustCLI("", "central", "keygen"), "\n") { // stderr carries a warning
		if strings.HasPrefix(l, "MIRAFLEET_SECRETS_KEY=") {
			keyLine = strings.TrimSpace(l)
		}
	}
	if keyLine == "" {
		t.Fatal("keygen printed no key line")
	}
	mustCLI("", "central", "init", "-data", dataDir)
	must(t, os.Chmod(dataDir, 0o700))
	addr := m7iFreePort(t, "127.0.0.1")
	centralProc := m7iStart(t, nil, append(append([]string{}, cliEnv...), keyLine), "", bin, "central", "run", "-data", dataDir, "-listen", addr)
	m7iWait(t, "central admin socket", 30*time.Second, func() bool {
		if !centralProc.running() {
			t.Fatalf("central exited:\n%s", centralProc.out)
		}
		_, err := os.Stat(filepath.Join(dataDir, "admin.sock"))
		return err == nil
	})
	const fleetDev, fleetQA = "devf", "qaf"
	for _, f := range []string{fleetDev, fleetQA} {
		mustCLI("", "central", "fleet", "create", f, "-data", dataDir, "-mode", "workspace", "-executor", "host",
			"-idle-ttl", "1h", "-max-ttl", "2h", "-exec-timeout", "2m", "-exec-max", "10m")
	}
	cred := func(stdin string, args ...string) {
		t.Helper()
		mustCLI(stdin, append([]string{"central", "credential", "set"}, append(args, "-data", dataDir)...)...)
	}
	cred(string(fake.DeployKey.PrivatePEM), "deploykey", "-kind", "ssh-key", "-hosts", "gitlab.test", "-user", "git", "-known-hosts", khFile)
	cred(fake.ProjectToken+"\n", "gltoken", "-kind", "https-token", "-hosts", "gitlab.test", "-auth", "basic", "-user", "oauth2")
	cred(fake.RegistryPassword+"\n", "glregistry", "-kind", "registry", "-hosts", "registry.gitlab.test", "-user", fake.RegistryUser)
	for _, c := range []string{"deploykey", "gltoken", "glregistry"} {
		for _, f := range []string{fleetDev, fleetQA} {
			mustCLI("", "central", "credential", "assign", c, "-data", dataDir, "-fleet", f)
		}
	}

	// ---- two host runners, one per OS user; qa's PATH has no rg ----
	norg := filepath.Join(base, "norg")
	must(t, os.Mkdir(norg, 0o755))
	for _, d := range []string{"/usr/local/bin", "/usr/bin", "/bin"} {
		ents, _ := os.ReadDir(d)
		for _, e := range ents {
			if e.Name() != "rg" {
				_ = os.Symlink(filepath.Join(d, e.Name()), filepath.Join(norg, e.Name()))
			}
		}
	}
	devPath := os.Getenv("PATH")
	_, noRG := exec.LookPath("rg")
	startRunner := func(u m7iUser, fleet, name, path string, join bool) *m7iProc {
		t.Helper()
		env := u.env(path)
		cfgDir := filepath.Join(u.home, ".config", "mirafleet")
		if join {
			tok := ""
			for _, l := range strings.Split(mustCLI("", "central", "token", "create", "-data", dataDir, "-fleet", fleet, "-approve"), "\n") {
				if l = strings.TrimSpace(l); l != "" && !strings.Contains(l, " ") { // stderr has a note
					tok = l
				}
			}
			must(t, os.MkdirAll(cfgDir, 0o700))
			y := "executor: host\nconcurrent: 4\nhost:\n  root: ~/work\n  corporate_ca_file: " + caFile + "\nenv_allow: [LANG]\n"
			must(t, os.WriteFile(filepath.Join(cfgDir, "workspace.yaml"), []byte(y), 0o600))
			for p := cfgDir; p != u.home; p = filepath.Dir(p) {
				must(t, os.Chown(p, int(u.uid), int(u.gid)))
			}
			must(t, os.Chown(filepath.Join(cfgDir, "workspace.yaml"), int(u.uid), int(u.gid)))
			jp := m7iStart(t, &u, env, tok+"\n", bin, "runner", "join", addr, "-name", name, "-token", "-")
			<-jp.done
			if !jp.cmd.ProcessState.Success() {
				t.Fatalf("runner join as %s:\n%s", u.name, jp.out)
			}
		}
		return m7iStart(t, &u, env, "", bin, "runner", "run", "-workspace", filepath.Join(cfgDir, "workspace.yaml"))
	}
	devRunner := startRunner(dev, fleetDev, "vm1-dev", devPath, true)
	qaRunner := startRunner(qa, fleetQA, "vm1-qa", norg, true)
	online := func(fleet string, p *m7iProc) {
		t.Helper()
		m7iWait(t, "runner online in "+fleet, 60*time.Second, func() bool {
			if !p.running() {
				t.Fatalf("runner exited:\n%s", p.out)
			}
			out, _ := cli("", "central", "runner", "list", "-data", dataDir, "-fleet", fleet, "-json")
			return strings.Contains(out, `"online": true`)
		})
	}
	online(fleetDev, devRunner)
	online(fleetQA, qaRunner)

	// ---- the operator and its MCP server ----
	waitCodeWindow()
	var otp string
	for _, l := range strings.Split(mustCLI("", "central", "operator", "code", "-data", dataDir), "\n") {
		if strings.HasPrefix(l, "code:") {
			otp = strings.TrimSpace(strings.TrimPrefix(l, "code:"))
		}
	}
	if otp == "" {
		t.Fatal("no operator code")
	}
	mustCLI(otp+"\n", "mcp", "login", addr, "-name", "alice", "-code", "-")
	mcp := m7iStartMCP(t, bin, project, cliEnv)
	mcp.handshake()
	for _, f := range []string{fleetDev, fleetQA} {
		op := mcp.call(f, "workspace_open", nil)
		if op["reused"] != false {
			t.Fatalf("workspace_open %s: %v", f, op)
		}
	}
	for _, f := range []string{fleetDev, fleetQA} {
		mcp.home[f] = strings.TrimSpace(mcp.shOK(f, "pwd"))
	}
	devRun := func(cmd string) string { t.Helper(); return mcp.shOK(fleetDev, cmd) }
	qaRun := func(cmd string) string { t.Helper(); return mcp.shOK(fleetQA, cmd) }
	if got := strings.TrimSpace(devRun("id -un")); got != dev.name {
		t.Fatalf("dev session runs as %q", got)
	}
	if got := strings.TrimSpace(qaRun("id -un")); got != qa.name {
		t.Fatalf("qa session runs as %q", got)
	}

	// ---- 1. clone over SSH and HTTPS with plain URLs; registry pull via the token exchange ----
	devRun("git clone -q git@gitlab.test:acme/app.git app-ssh")
	devRun("git clone -q https://gitlab.test/acme/app.git app-https")
	if got := devRun("cat app-ssh/README.md app-https/README.md"); got != "# app\n# app\n" {
		t.Fatalf("clones differ: %q", got)
	}
	reg := devRun("curl -sS -o /dev/null -D - -H 'Accept: application/vnd.oci.image.manifest.v1+json' https://registry.gitlab.test/v2/acme/app/manifests/1.0")
	if !strings.HasPrefix(reg, "HTTP/1.1 200") || !strings.Contains(reg, manifestDigest) {
		t.Fatalf("registry pull through the broker:\n%s", reg)
	}

	// ---- 2. edit, a long background build, runner restart mid-build, tail, completion ----
	mcp.call(fleetDev, "edit", map[string]any{"path": "app-ssh/main.txt", "old_string": "needle in the haystack", "new_string": "edited needle"})
	if got := devRun("cat app-ssh/main.txt"); !strings.Contains(got, "edited needle") {
		t.Fatalf("edit did not land: %q", got)
	}
	job := mcp.call(fleetDev, "bash", map[string]any{"run_in_background": true, "description": "long build",
		"command": fmt.Sprintf("cd app-ssh && for i in $(seq 1 %d); do echo build-step-$i; sleep 10; done; echo BUILD-OK", steps)})
	shellID, _ := job["shell_id"].(string)
	if shellID == "" {
		t.Fatalf("background bash: %v", job)
	}
	time.Sleep(25 * time.Second)
	devRunner.stop() // the runner restarts mid-build; the job must survive it
	devRunner = startRunner(dev, fleetDev, "vm1-dev", devPath, false)
	online(fleetDev, devRunner)
	var tail strings.Builder
	finished := false
	deadline := time.Now().Add(time.Duration(steps)*10*time.Second + 3*time.Minute)
	for time.Now().Before(deadline) && !finished {
		o := mcp.call(fleetDev, "bash_output", map[string]any{"shell_id": shellID})
		tail.WriteString(fmt.Sprint(o["output"]))
		finished = o["running"] == false
		if !finished {
			time.Sleep(15 * time.Second)
		}
	}
	if !finished || !strings.Contains(tail.String(), "BUILD-OK") || !strings.Contains(tail.String(), "build-step-1\n") ||
		!strings.Contains(tail.String(), fmt.Sprintf("build-step-%d\n", steps)) {
		t.Fatalf("background build after the runner restart (finished=%v):\n%s\nrunner log:\n%s", finished, tail.String(), devRunner.out)
	}
	if !devRunner.running() {
		t.Fatalf("restarted runner died:\n%s", devRunner.out)
	}

	// ---- 3. commit and push over both remotes; the fake records the credentials ----
	// The restarted runner must wire the sessions it adopted (agent socket, broker, env) again.
	if so := devRun("echo SOCK=$SSH_AUTH_SOCK PROXY=${HTTPS_PROXY:+set}"); !strings.Contains(so, "SOCK=/") || !strings.Contains(so, "PROXY=set") {
		t.Fatalf("after a runner restart the adopted session has no credential wiring: %q", so)
	}
	devRun("cd app-ssh && git -c user.name=dev -c user.email=dev@x.test commit -qam 'edit over ssh' && git push -q origin HEAD:refs/heads/from-ssh")
	devRun("cd app-https && echo https >> README.md && git -c user.name=dev -c user.email=dev@x.test commit -qam 'edit over https' && git push -q origin HEAD:refs/heads/from-https")
	refs := fake.Refs(t, "acme/app")
	if refs["refs/heads/from-ssh"] == "" || refs["refs/heads/from-https"] == "" {
		t.Fatalf("pushes missing: %v", refs)
	}
	var sawSSH, sawHTTPS, sawReg bool
	for _, e := range fake.Events() {
		switch {
		case e.Transport == "ssh" && e.OK && e.Credential == fake.DeployKey.Fingerprint && e.Identity == "git":
			sawSSH = true
		case e.Transport == "https" && e.OK && e.Credential == fake.ProjectToken:
			sawHTTPS = true
		case e.Transport == "registry-basic" && e.OK && e.Credential == fake.RegistryPassword:
			sawReg = true
		}
	}
	if !sawSSH || !sawHTTPS || !sawReg {
		t.Fatalf("the fake did not see the credentials (ssh=%v https=%v registry=%v): %+v", sawSSH, sawHTTPS, sawReg, fake.Events())
	}

	// ---- 4. leak scan as dev, through the MCP bash tool ----
	scan := func(cmd string) string {
		t.Helper()
		return strings.TrimSpace(devRun(cmd + "; true"))
	}
	if out := scan("grep -rlaE " + scanArgs + " /home /tmp /var /etc /run /opt /srv /itest /mnt 2>/dev/null"); out != "" {
		t.Errorf("secret bytes in readable files:\n%s", out)
	}
	if out := scan("grep -laE " + scanArgs + " /proc/[0-9]*/environ /proc/[0-9]*/cmdline 2>/dev/null"); out != "" {
		t.Errorf("secret bytes in /proc environ or cmdline:\n%s", out)
	}
	runnerPID := strings.TrimSpace(devRun("pgrep -u dev -f '[r]unner run -workspace' | head -n1"))
	if runnerPID == "" {
		t.Fatal("cannot find the runner process as dev")
	}
	for _, f := range []string{"mem", "environ"} {
		if code, _, _ := mcp.sh(fleetDev, "head -c 16 /proc/"+runnerPID+"/"+f+" >/dev/null"); code == 0 {
			t.Errorf("/proc/<runner>/%s readable", f)
		}
	}
	sysPtrace := map[string]string{"amd64": "101", "arm64": "117"}[runtime.GOARCH]
	if sysPtrace != "" {
		out := devRun("perl -e '$r = syscall(" + sysPtrace + ", 16, " + runnerPID + ", 0, 0); print $r == -1 ? \"refused\" : \"ATTACHED\"'")
		if strings.TrimSpace(out) != "refused" {
			t.Errorf("ptrace attach to the runner: %q", out)
		}
	}
	if out := scan("printf 'protocol=https\\nhost=gitlab.test\\n\\n' | GIT_TERMINAL_PROMPT=0 git credential fill 2>&1"); strings.Contains(out, "password=") {
		t.Errorf("git credential fill shows a password: %q", out)
	}
	if out := scan("GIT_TRACE=1 GIT_TRACE_CURL=1 git ls-remote https://gitlab.test/acme/app.git 2>&1"); !strings.Contains(out, "refs/heads/main") {
		t.Errorf("ls-remote through the broker failed:\n%s", out)
	}
	pub := devRun("ssh-add -L")
	if !strings.Contains(pub, pubBody) || strings.Contains(pub, "PRIVATE") || strings.Count(strings.TrimSpace(pub), "\n") != 0 {
		t.Errorf("ssh-add -L: %q", pub)
	}
	if code, so, _ := mcp.sh(fleetDev, "ssh-add -L > pub.txt; echo hi | ssh-keygen -Y sign -n file -f pub.txt -U 2>&1"); code == 0 || strings.Contains(so, "BEGIN SSH SIGNATURE") {
		t.Errorf("agent signed a non-auth message: exit %d %q", code, so)
	}
	_, osshPort, _ := net.SplitHostPort(other.SSHAddr)
	if code, _, _ := mcp.sh(fleetDev, "git ls-remote ssh://git@gitlab.test:"+osshPort+"/acme/app.git"); code == 0 {
		t.Error("git over SSH to an unpinned host key succeeded")
	}
	for _, e := range other.Events() {
		if e.OK {
			t.Errorf("the unpinned server authenticated a client: %+v", e)
		}
	}
	_, ohttpsPort, _ := net.SplitHostPort(other.HTTPSAddr)
	cert := devRun("curl -sk -v -o /dev/null https://other.test:" + ohttpsPort + "/ 2>&1; true")
	if !strings.Contains(cert, "Corp Test Root CA") {
		t.Errorf("non-credential HTTPS host did not show its real certificate:\n%s", cert)
	}
	// grep gives the same result with rg (dev) and without it (qa, whose PATH has none).
	if noRG != nil {
		t.Log("ripgrep is not installed in the harness: grep compared Go-vs-Go only")
	} else if out := strings.TrimSpace(devRun("command -v rg")); out == "" {
		t.Error("rg is installed but not on dev's PATH")
	}
	// (bash -lc resets PATH via /etc/profile, so a shell check on qa would find /usr/bin/rg; the
	// grep tool looks rg up on the runner's own PATH, which the qa runner was started without.)
	for _, f := range []string{fleetDev, fleetQA} {
		for p, body := range map[string]string{"g/a.txt": "needle one\nplain\nNeedle two\n", "g/b.txt": "nothing\n", "g/.hidden": "needle hidden\n",
			"g/ig.txt": "needle ignored\n", "g/.gitignore": "ig.txt\n", "g/c/d.go": "func needle() {}\n"} {
			mcp.call(f, "write", map[string]any{"path": p, "content": body})
		}
	}
	norm := func(r map[string]any) string {
		b, _ := json.Marshal(map[string]any{"c": r["content"], "m": r["matches"], "f": r["files"], "n": r["count"], "counts": r["counts"], "text": r["text"], "result": r["result"], "output": r["output"]})
		return regexp.MustCompile(`/[^"\s]*/g/`).ReplaceAllString(string(b), "g/")
	}
	for _, q := range []map[string]any{
		{"pattern": "needle", "path": "g", "output_mode": "files_with_matches"},
		{"pattern": "needle", "path": "g", "output_mode": "content", "-n": true, "-i": true},
		{"pattern": "needle", "path": "g", "output_mode": "count"},
	} {
		rd, rq := mcp.call(fleetDev, "grep", q), mcp.call(fleetQA, "grep", q)
		if norm(rd) != norm(rq) {
			t.Errorf("grep %v differs with and without rg:\nwith:    %s\nwithout: %s", q, norm(rd), norm(rq))
		}
		if strings.Contains(norm(rd), "ignored") {
			t.Errorf("grep ignored .gitignore: %s", norm(rd))
		}
	}

	// ---- 5. isolation between the runner users ----
	for _, c := range []struct {
		fleet, cmd string
		what       string
	}{
		{fleetQA, "ls /home/" + dev.name, "qa lists dev's home"},
		{fleetQA, "cat /home/" + dev.name + "/work/*/app-ssh/README.md", "qa reads dev's session dir"},
		{fleetDev, "ls /home/" + qa.name, "dev lists qa's home"},
	} {
		if code, so, _ := mcp.sh(c.fleet, c.cmd); code == 0 || strings.Contains(so, "README") {
			t.Errorf("%s: exit %d %q", c.what, code, so)
		}
	}
	devSock := strings.TrimSpace(devRun("echo $SSH_AUTH_SOCK"))
	if devSock == "" {
		t.Fatal("dev session has no SSH_AUTH_SOCK")
	}
	if code, so, _ := mcp.sh(fleetQA, "SSH_AUTH_SOCK="+devSock+" ssh-add -L"); code == 0 {
		t.Errorf("qa reached dev's agent socket: %q", so)
	}
	devProxy := strings.TrimSpace(devRun("echo $HTTPS_PROXY"))
	qaProxy := strings.TrimSpace(qaRun("echo $HTTPS_PROXY"))
	if devProxy == "" || qaProxy == "" || devProxy == qaProxy {
		t.Fatalf("proxy env: dev %q qa %q", devProxy, qaProxy)
	}
	devAddr := devProxy[strings.LastIndex(devProxy, "@")+1:]
	devAddr = strings.TrimSuffix(devAddr, "/")
	devPort := devAddr[strings.LastIndex(devAddr, ":")+1:]
	qaPort := strings.TrimSuffix(qaProxy, "/")
	qaPort = qaPort[strings.LastIndex(qaPort, ":")+1:]
	// qa's own proxy password against dev's broker port: refused, nothing is served.
	cross := qaRun("curl -s -o body.txt -w '%{http_code}' -x \"$(echo \"$HTTPS_PROXY\" | sed 's/:" + qaPort + "/:" + devPort + "/')\" https://gitlab.test/acme/app.git/info/refs?service=git-upload-pack; echo; cat body.txt; true")
	if strings.Contains(cross, "refs/heads") || strings.HasPrefix(strings.TrimSpace(cross), "200") {
		t.Errorf("qa used dev's broker: %q", cross)
	}
	if code, _, _ := mcp.sh(fleetQA, "cat /proc/"+runnerPID+"/environ"); code == 0 {
		t.Error("qa read the dev runner's environ")
	}
	if code, so, _ := mcp.sh(fleetDev, "cat /proc/$(pgrep -u qa -f '[r]unner run -workspace' | head -n1)/environ"); code == 0 {
		t.Errorf("dev read the qa runner's environ: %q", so)
	}
	// workspace_close kills the session's processes and keeps the directory.
	mcp.call(fleetDev, "bash", map[string]any{"run_in_background": true, "command": "exec sleep 987"})
	sessDir := mcp.home[fleetDev]
	if out, _ := exec.Command("pgrep", "-u", dev.name, "-f", "[s]leep 987").Output(); strings.TrimSpace(string(out)) == "" {
		t.Fatal("the background sleep is not running")
	}
	mcp.call(fleetDev, "workspace_close", nil)
	m7iWait(t, "the session's processes to die on close", 30*time.Second, func() bool {
		out, _ := exec.Command("pgrep", "-u", dev.name, "-f", "[s]leep 987").Output()
		return strings.TrimSpace(string(out)) == ""
	})
	if fi, err := os.Stat(filepath.Join(sessDir, "app-ssh")); err != nil || !fi.IsDir() {
		t.Errorf("the session directory %s did not survive close: %v", sessDir, err)
	}
	mcp.call(fleetQA, "workspace_close", nil)

	// ---- 6. central audit: aggregated credential.use rows, no secret in the DB ----
	var audit string
	for end := time.Now().Add(120 * time.Second); time.Now().Before(end); time.Sleep(3 * time.Second) {
		audit = mustCLI("", "central", "audit", "-data", dataDir, "-action", "credential.use", "-limit", "200", "-json")
		if strings.Contains(audit, `"deploykey"`) && strings.Contains(audit, `"gltoken"`) && strings.Contains(audit, `"glregistry"`) {
			break
		}
	}
	for _, name := range []string{"deploykey", "gltoken", "glregistry"} {
		if !strings.Contains(audit, name) {
			t.Errorf("no credential.use row for %s:\n%s", name, audit)
		}
	}
	centralProc.stop() // checkpoints the WAL
	allOut := mcp.stderr.String() + centralProc.out.String() + devRunner.out.String() + qaRunner.out.String() +
		strings.Join(mcp.all, "") + audit
	var files []string
	_ = filepath.Walk(dataDir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			files = append(files, p)
		}
		return nil
	})
	if len(files) == 0 {
		t.Fatal("central data dir is empty")
	}
	for _, p := range files {
		b, err := os.ReadFile(p)
		must(t, err)
		allOut += "\x00" + string(b)
	}
	for i, d := range decoys {
		if strings.Contains(allOut, d) {
			t.Errorf("decoy %d found in central files, logs or MCP output", i)
		}
	}
}
