//go:build linux

package main

// TestGUIBinary: the real `mirafleet central run` entry as a child
// process (the test binary re-executed, like TestM5NoHooks; the package has no TestMain, so a
// normal test function, TestGUIBinaryChild, is the hook). It drives the GUI like a browser
// without JavaScript: a cookie jar, CSRF tokens parsed from the forms, TLS pinned to the
// fingerprint that central logged.

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	gbChildEnv = "MIRAFLEET_TEST_GUI_CHILD"
	gbStep     = 30 * time.Second
	gbPassword = "correct horse battery staple"
)

// TestGUIBinaryChild is the re-exec entry, not a test: skipped unless the parent set gbChildEnv.
func TestGUIBinaryChild(t *testing.T) {
	if os.Getenv(gbChildEnv) != "1" {
		t.Skip("re-exec helper for TestGUIBinary")
	}
	os.Args = append([]string{"mirafleet"}, flag.Args()...)
	main()
	os.Exit(0)
}

type gbProc struct {
	cmd    *exec.Cmd
	stdout lockedBuf
	stderr lockedBuf
	exited chan struct{}
	err    error
}

func gbStart(t *testing.T, home string, args ...string) *gbProc {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p := &gbProc{exited: make(chan struct{})}
	p.cmd = exec.Command(exe, append([]string{"-test.run=^TestGUIBinaryChild$", "--"}, args...)...)
	p.cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, gbChildEnv + "=1"}
	p.cmd.Dir = home
	p.cmd.Stdout, p.cmd.Stderr = &p.stdout, &p.stderr
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { p.err = p.cmd.Wait(); close(p.exited) }()
	t.Cleanup(func() {
		select {
		case <-p.exited:
		default:
			p.cmd.Process.Kill()
			<-p.exited
		}
	})
	return p
}

// gbRun runs a short-lived command (init, fleet, gui reset-admin) and returns its stdout.
func gbRun(t *testing.T, home string, args ...string) (stdout, stderr string) {
	t.Helper()
	p := gbStart(t, home, args...)
	select {
	case <-p.exited:
	case <-time.After(gbStep):
		t.Fatalf("%v did not exit; stderr: %s", args, p.stderr.String())
	}
	if p.err != nil {
		t.Fatalf("%v: %v; stderr: %s", args, p.err, p.stderr.String())
	}
	// The test binary prints PASS after the child test; keep only mirafleet's own lines.
	return gbStripTest(p.stdout.String()), p.stderr.String()
}

func gbStripTest(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l == "PASS" || strings.HasPrefix(l, "ok ") {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// waitLog polls the child's stderr until re matches, the child dies or the step times out.
func (p *gbProc) waitLog(t *testing.T, re *regexp.Regexp) []string {
	t.Helper()
	deadline := time.Now().Add(gbStep)
	for time.Now().Before(deadline) {
		if m := re.FindStringSubmatch(p.stderr.String()); m != nil {
			return m
		}
		select {
		case <-p.exited:
			t.Fatalf("central exited early (%v); log:\n%s", p.err, p.stderr.String())
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatalf("log never matched %v:\n%s", re, p.stderr.String())
	return nil
}

// stop sends SIGTERM and requires a clean exit (status 0) within 5 s.
func (p *gbProc) stop(t *testing.T) {
	t.Helper()
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.exited:
	case <-time.After(5 * time.Second):
		t.Fatalf("central did not exit within 5 s of SIGTERM; log:\n%s", p.stderr.String())
	}
	if p.err != nil {
		t.Fatalf("central exit: %v; log:\n%s", p.err, p.stderr.String())
	}
}

var (
	gbReURL  = regexp.MustCompile(`GUI listening on (https://\S+)`)
	gbReFP   = regexp.MustCompile(`GUI certificate \(self-signed\) SHA-256 fingerprint: ([0-9A-F:]+),`)
	gbReCSRF = regexp.MustCompile(`name="_csrf" value="([^"]*)"`)
)

type gbGUI struct {
	port string
	fp   string
}

// gbStartCentral starts `central run` with the default-shaped GUI bind and waits for the GUI.
func gbStartCentral(t *testing.T, home, data string) (*gbProc, gbGUI) {
	t.Helper()
	p := gbStart(t, home, "central", "run", "-data", data, "-listen", "127.0.0.1:0", "-gui-listen", "0.0.0.0:0")
	u := p.waitLog(t, gbReURL)[1]
	fp := p.waitLog(t, gbReFP)[1]
	pu, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	p.waitLog(t, regexp.MustCompile(`central listening on`))
	return p, gbGUI{port: pu.Port(), fp: fp}
}

type gbClient struct {
	t    *testing.T
	c    *http.Client
	base string
}

type gbResp struct {
	code int
	body string
	loc  string
	hdr  http.Header
}

// gbNewClient pins the TLS certificate to fp; it never follows redirects.
func gbNewClient(t *testing.T, port, fp string) *gbClient {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := &http.Transport{DisableKeepAlives: true, TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS13, InsecureSkipVerify: true, //nolint:gosec // pinned below
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("no certificate")
			}
			if got := gbFingerprint(raw[0]); got != fp {
				return fmt.Errorf("certificate fingerprint %s, pinned %s", got, fp)
			}
			return nil
		}}}
	t.Cleanup(tr.CloseIdleConnections)
	// Argon2id under -race on a loaded host can take many seconds per hash.
	return &gbClient{t: t, base: "https://127.0.0.1:" + port, c: &http.Client{Jar: jar, Transport: tr, Timeout: 3 * gbStep,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func gbFingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}

// do sends one request; host overrides the Host header when non-empty.
func (g *gbClient) do(method, path, host string, form url.Values) gbResp {
	g.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, g.base+path, body)
	if err != nil {
		g.t.Fatal(err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", g.base)
	}
	if host != "" {
		req.Host = host
	}
	resp, err := g.c.Do(req)
	if err != nil {
		g.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return gbResp{code: resp.StatusCode, body: string(b), loc: resp.Header.Get("Location"), hdr: resp.Header}
}

// csrf fetches path and returns the form's _csrf token.
func (g *gbClient) csrf(path string) string {
	g.t.Helper()
	r := g.do("GET", path, "", nil)
	if r.code != 200 {
		g.t.Fatalf("GET %s: %d %s", path, r.code, r.body)
	}
	m := gbReCSRF.FindStringSubmatch(r.body)
	if m == nil {
		g.t.Fatalf("GET %s: no _csrf field", path)
	}
	return html.UnescapeString(m[1])
}

func (g *gbClient) hasSession() bool {
	u, _ := url.Parse(g.base)
	for _, c := range g.c.Jar.Cookies(u) {
		if c.Name == "__Host-mf_session" {
			return true
		}
	}
	return false
}

func gbRedirect(r gbResp) bool { return r.code >= 300 && r.code < 400 }

func TestGUIBinary(t *testing.T) {
	home := t.TempDir()
	data := filepath.Join(home, "data")

	gbRun(t, home, "central", "init", "-data", data, "-listen", "127.0.0.1:0")

	// First start: default-shaped bind (all interfaces), self-signed certificate.
	p1, g1 := gbStartCentral(t, home, data)
	log := p1.stderr.String()
	tokPath := filepath.Join(data, "gui", "setup-token")
	raw, err := os.ReadFile(tokPath)
	if err != nil {
		t.Fatal(err)
	}
	tok := strings.TrimSpace(string(raw))
	for _, want := range []string{"GUI listening on https://", "SHA-256 fingerprint: " + g1.fp, "all interfaces", tokPath} {
		if !strings.Contains(log, want) {
			t.Errorf("startup log lacks %q:\n%s", want, log)
		}
	}
	if !strings.HasPrefix(tok, "mfs1.") || strings.Contains(log, tok) {
		t.Fatalf("setup token file %q, or the token is in the log", tok)
	}

	cl := gbNewClient(t, g1.port, g1.fp)

	// A wrong pin must fail the handshake.
	bad := gbNewClient(t, g1.port, strings.Repeat("00:", 31)+"00")
	if _, err := bad.c.Get(bad.base + "/login"); err == nil {
		t.Fatal("a wrong pinned fingerprint was accepted")
	}

	// Before setup every page redirects to /setup (all screens sit behind requireAdmin).
	if r := cl.do("GET", "/fleets", "", nil); !gbRedirect(r) || !strings.HasPrefix(r.loc, "/setup") {
		t.Fatalf("GET /fleets before setup: %d %q, want a redirect to /setup", r.code, r.loc)
	}
	if r := cl.do("GET", "/setup", "rebind.evil.example:"+g1.port, nil); r.code != 421 {
		t.Fatalf("foreign Host on /setup: %d, want 421", r.code)
	}
	if r := cl.do("GET", "/static/app.css", "rebind.evil.example", nil); r.code != 421 {
		t.Fatalf("foreign Host on static: %d, want 421", r.code)
	}
	if r := cl.do("GET", "/setup", "127.0.0.1:"+g1.port, nil); r.code != 200 {
		t.Fatalf("Host by 127.0.0.1: %d", r.code)
	}
	if hn, err := os.Hostname(); err == nil && hn != "" {
		if r := cl.do("GET", "/setup", hn+":"+g1.port, nil); r.code != 200 {
			t.Fatalf("Host by hostname %q: %d", hn, r.code)
		}
	}

	// Setup from the token file: a wrong token is refused, the right one logs in.
	form := func(token, pw string) url.Values {
		return url.Values{"_csrf": {cl.csrf("/setup")}, "token": {token}, "username": {"admin"}, "password": {pw}, "password2": {pw}}
	}
	if r := cl.do("POST", "/setup", "", form("mfs1.wrong", gbPassword)); gbRedirect(r) || cl.hasSession() {
		t.Fatalf("a wrong setup token was accepted: %d", r.code)
	}
	if r := cl.do("POST", "/setup", "", form(tok, gbPassword)); !gbRedirect(r) {
		t.Fatalf("setup: %d %s", r.code, r.body)
	}
	if !cl.hasSession() {
		t.Fatal("setup did not log in (no __Host-mf_session cookie)")
	}
	if _, err := os.Stat(tokPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("setup token file still there after setup: %v", err)
	}
	if r := cl.do("GET", "/fleets", "", nil); r.code != 200 {
		t.Fatalf("GET /fleets after setup: %d", r.code)
	}
	if r := cl.do("GET", "/setup", "", nil); r.code != 404 {
		t.Fatalf("GET /setup after setup: %d, want 404", r.code)
	}

	// Login with a fresh jar (a second browser).
	b2 := gbNewClient(t, g1.port, g1.fp)
	if r := b2.do("GET", "/fleets", "", nil); !gbRedirect(r) || !strings.Contains(r.loc, "/login") {
		t.Fatalf("anonymous GET /fleets: %d %q", r.code, r.loc)
	}
	if r := b2.do("POST", "/login", "", url.Values{"_csrf": {b2.csrf("/login")}, "username": {"admin"}, "password": {"nope nope nope nope"}}); b2.hasSession() || gbRedirect(r) {
		t.Fatalf("wrong password logged in: %d", r.code)
	}
	if r := b2.do("POST", "/login", "", url.Values{"_csrf": {b2.csrf("/login")}, "username": {"admin"}, "password": {gbPassword}}); !gbRedirect(r) || !b2.hasSession() {
		t.Fatalf("login: %d %s", r.code, r.body)
	}
	if r := b2.do("GET", "/fleets", "", nil); r.code != 200 {
		t.Fatalf("GET /fleets after login: %d", r.code)
	}

	// A fleet created in the GUI shows in the CLI; one created by the CLI shows in the GUI.
	r := cl.do("POST", "/fleets", "", url.Values{"_csrf": {cl.csrf("/fleets/new")}, "name": {"guifleet"}, "mode": {"actions"}})
	if !gbRedirect(r) {
		t.Fatalf("create fleet in the GUI: %d %s", r.code, r.body)
	}
	if out, _ := gbRun(t, home, "central", "fleet", "list", "-data", data); !strings.Contains(out, "guifleet") {
		t.Fatalf("fleet list lacks guifleet:\n%s", out)
	}
	gbRun(t, home, "central", "fleet", "create", "clifleet", "-data", data)
	if r := cl.do("GET", "/fleets", "", nil); r.code != 200 || !strings.Contains(r.body, "clifleet") || !strings.Contains(r.body, "guifleet") {
		t.Fatalf("GUI fleets page: %d, lacks clifleet/guifleet", r.code)
	}

	// Restart on the same data dir: the old cookie still works, the certificate is unchanged.
	p1.stop(t)
	p2, g2 := gbStartCentral(t, home, data)
	if g2.fp != g1.fp {
		t.Fatalf("certificate fingerprint changed across restart: %s -> %s", g1.fp, g2.fp)
	}
	if strings.Contains(p2.stderr.String(), "GUI setup:") {
		t.Errorf("restart with an admin logged a setup-token notice:\n%s", p2.stderr.String())
	}
	cl.base = "https://127.0.0.1:" + g2.port
	if r := cl.do("GET", "/fleets", "", nil); r.code != 200 {
		t.Fatalf("old session after restart: %d %q", r.code, r.loc)
	}

	// reset-admin prints a token on stdout and ends every session at once.
	out, errOut := gbRun(t, home, "central", "gui", "reset-admin", "-data", data)
	fresh := strings.TrimSpace(out)
	if !strings.HasPrefix(fresh, "mfs1.") || strings.Contains(fresh, "\n") || fresh == tok || strings.Contains(errOut, fresh) {
		t.Fatalf("reset-admin stdout %q stderr %q", out, errOut)
	}
	if strings.Contains(p2.stderr.String(), fresh) {
		t.Fatal("the fresh setup token is in central's log")
	}
	for name, c := range map[string]*gbClient{"first": cl, "second": func() *gbClient { b2.base = cl.base; return b2 }()} {
		if r := c.do("GET", "/fleets", "", nil); r.code == 200 {
			t.Fatalf("%s session still works after reset-admin", name)
		}
	}
	p2.stop(t)

	// -gui-listen off: no GUI at all.
	p3 := gbStart(t, home, "central", "run", "-data", data, "-listen", "127.0.0.1:0", "-gui-listen", "off")
	p3.waitLog(t, regexp.MustCompile(`GUI is off`))
	if l := p3.stderr.String(); strings.Contains(l, "GUI listening") || strings.Contains(l, "fingerprint") {
		t.Fatalf("-gui-listen off still started a GUI:\n%s", l)
	}
	p3.stop(t)
}
