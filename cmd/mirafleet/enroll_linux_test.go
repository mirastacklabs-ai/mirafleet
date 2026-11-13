package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/central"
	"github.com/mirastacklabs-ai/mirafleet/internal/central/registry"
	"github.com/mirastacklabs-ai/mirafleet/internal/keys"
	"github.com/mirastacklabs-ai/mirafleet/internal/opconf"
	"github.com/mirastacklabs-ai/mirafleet/internal/policy"
	"github.com/mirastacklabs-ai/mirafleet/internal/runner"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/store/sqlite"

	"golang.org/x/crypto/ssh"
)

// cenv is a minimal in-process central: temp SQLite store, fleet f1, service, SSH server.
type cenv struct {
	t     *testing.T
	addr  string
	svc   *service.Service
	admin auth.Principal
	logs  *bytes.Buffer
	host  ssh.Signer
}

func newCenv(t *testing.T) *cenv {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	if _, err := keys.Generate(dir, "host", "host"); err != nil {
		t.Fatal(err)
	}
	host, err := keys.LoadSigner(filepath.Join(dir, "host"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := sqlite.Open(ctx, filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	e := &cenv{t: t, admin: auth.LocalAdmin(os.Getuid()), logs: &bytes.Buffer{}, host: host}
	logf := func(f string, a ...any) { e.logs.WriteString(f + "\n") }
	reg, err := registry.New(registry.Options{StaleAfter: time.Minute, Logf: logf})
	if err != nil {
		t.Fatal(err)
	}
	ops := central.NewOperatorConns()
	e.svc, err = service.New(service.Config{Store: db, Policy: policy.TablePolicy{}, Live: reg,
		HostKey: host.PublicKey(), Now: time.Now, OperatorConns: ops, Logf: logf})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CreateFleet(ctx, e.admin, service.SourceCLI, "f1", store.ModeActions, false); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.InitRegistrationSecret(ctx, e.admin, service.SourceCLI); err != nil {
		t.Fatal(err)
	}
	srv, err := central.New(central.Config{Listen: "127.0.0.1:0", HostKey: host, Store: db, Service: e.svc,
		Registry: reg, Operators: ops, Logf: logf})
	if err != nil {
		t.Fatal(err)
	}
	if e.addr, err = srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	return e
}

func (e *cenv) token(approve bool) string {
	e.t.Helper()
	ct, err := e.svc.CreateToken(context.Background(), e.admin, service.SourceCLI, service.TokenParams{Fleet: "f1", Approve: approve})
	if err != nil {
		e.t.Fatal(err)
	}
	return ct.Token
}

func (e *cenv) code() string {
	e.t.Helper()
	r, err := e.svc.OperatorCode(context.Background(), e.admin, service.SourceCLI)
	if err != nil {
		e.t.Fatal(err)
	}
	return r.Code
}

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm()
}

// anyFileContains reports whether any file under dir contains s.
func anyFileContains(t *testing.T, dir, s string) bool {
	t.Helper()
	found := false
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), s) {
				found = true
			}
		}
		return nil
	})
	return found
}

func TestRunnerJoinPending(t *testing.T) {
	e := newCenv(t)
	tok := e.token(false)
	data := filepath.Join(t.TempDir(), "runner")
	var out bytes.Buffer
	err := runnerJoin([]string{e.addr, "-name", "web-1", "-data", data, "-token", "-"}, strings.NewReader(tok+"\n"), &out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "pending") || !strings.Contains(out.String(), "systemd") {
		t.Errorf("output: %s", out.String())
	}
	if mode(t, data) != 0o700 || mode(t, runner.KeyPath(data)) != 0o600 ||
		mode(t, runner.ConfigPath(data)) != 0o600 || mode(t, runner.KnownHostsPath(data)) != 0o600 {
		t.Error("wrong file modes")
	}
	cfg, err := runner.LoadConfig(data)
	if err != nil || cfg.Central != e.addr || cfg.Name != "web-1" || cfg.Fleet != "f1" {
		t.Fatalf("cfg %+v err %v", cfg, err)
	}
	kh, _ := os.ReadFile(runner.KnownHostsPath(data))
	if !strings.Contains(string(kh), e.addr+" "+strings.TrimSpace(string(ssh.MarshalAuthorizedKey(e.host.PublicKey())))) {
		t.Errorf("known_hosts: %s", kh)
	}
	for _, secret := range []string{tok, strings.Split(tok, ".")[2]} {
		if anyFileContains(t, data, secret) || strings.Contains(out.String(), secret) || strings.Contains(e.logs.String(), secret) {
			t.Error("token secret leaked")
		}
	}

	// A second join into the same dir is refused before reading the token.
	err = runnerJoin([]string{e.addr, "-name", "web-1", "-data", data, "-token", "-"}, strings.NewReader(e.token(false)), &out)
	if err == nil || !strings.Contains(err.Error(), "already joined") {
		t.Fatalf("second join: %v", err)
	}
}

func TestRunnerJoinNeedsStdinToken(t *testing.T) {
	e := newCenv(t)
	data := t.TempDir()
	if err := runnerJoin([]string{e.addr, "-name", "a", "-data", data, "-token", e.token(false)}, strings.NewReader(""), &bytes.Buffer{}); err == nil {
		t.Error("token on argv accepted")
	}
	if err := runnerJoin([]string{e.addr, "-name", "a", "-data", data, "-token", "-"}, strings.NewReader("  \n"), &bytes.Buffer{}); err == nil {
		t.Error("empty stdin accepted")
	}
	if _, err := os.Stat(runner.ConfigPath(data)); err == nil {
		t.Error("runner.yaml written")
	}
}

func TestRunnerJoinWrongHostKeyWritesNothing(t *testing.T) {
	e := newCenv(t)
	other := newCenv(t) // a token minted for a different central key
	data := t.TempDir()
	err := runnerJoin([]string{e.addr, "-name", "a", "-data", data, "-token", "-"}, strings.NewReader(other.token(false)), &bytes.Buffer{})
	if err == nil {
		t.Fatal("joined with a token pinned to another host key")
	}
	if _, err := os.Stat(runner.ConfigPath(data)); err == nil {
		t.Error("runner.yaml written after failure")
	}
	if _, err := os.Stat(runner.KnownHostsPath(data)); err == nil {
		t.Error("known_hosts written after failure")
	}
}

func TestRunnerRunNotJoined(t *testing.T) {
	err := runnerRun([]string{"-data", t.TempDir(), "-actions", filepath.Join(t.TempDir(), "none.yaml")})
	if err == nil || !strings.Contains(err.Error(), "not joined") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunnerMainUsage(t *testing.T) {
	if err := runnerMain(nil); err == nil {
		t.Error("bare runner accepted")
	}
	if err := runnerMain([]string{"-init"}); err == nil {
		t.Error("-init still accepted")
	}
}

func TestMCPLoginAndAPI(t *testing.T) {
	e := newCenv(t)
	cfgDir := filepath.Join(t.TempDir(), "cfg")
	t.Setenv(opconf.EnvDir, cfgDir)
	code := e.code()

	var out bytes.Buffer
	if err := mcpMain([]string{"login", e.addr, "-name", "alice", "-code", "-"}, strings.NewReader(code+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	if mode(t, cfgDir) != 0o700 || mode(t, opconf.KeyPath(cfgDir)) != 0o600 ||
		mode(t, opconf.KnownHostsPath(cfgDir)) != 0o600 || mode(t, opconf.ContextsPath(cfgDir)) != 0o600 {
		t.Error("wrong file modes")
	}
	cs, err := opconf.LoadContexts(cfgDir)
	if err != nil || cs.Current != "alice" || len(cs.Contexts) != 1 || cs.Contexts[0].Addr != e.addr {
		t.Fatalf("contexts %+v err %v", cs, err)
	}
	if anyFileContains(t, cfgDir, code) || anyFileContains(t, cfgDir, strings.Split(code, ".")[1]) || strings.Contains(out.String(), code) {
		t.Error("registration code leaked")
	}

	// Re-login updates rather than duplicates.
	if err := mcpMain([]string{"login", e.addr, "-name", "alice", "-code", "-"}, strings.NewReader(e.code()), &out); err != nil {
		t.Fatal(err)
	}
	if cs, _ = opconf.LoadContexts(cfgDir); len(cs.Contexts) != 1 {
		t.Errorf("contexts after relogin: %+v", cs.Contexts)
	}
	if kh, _ := os.ReadFile(opconf.KnownHostsPath(cfgDir)); strings.Count(string(kh), "\n") != 1 {
		t.Errorf("known_hosts: %q", kh)
	}

	// A runner joins and an admin approves it; the operator sees it through mcp api.
	data := t.TempDir()
	if err := runnerJoin([]string{e.addr, "-name", "web-1", "-data", data, "-token", "-"}, strings.NewReader(e.token(false)), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Approve(context.Background(), e.admin, service.SourceCLI, "f1/web-1"); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := mcpMain([]string{"api", "runners.list", `{"fleet":"f1"}`}, nil, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"web-1"`) || !strings.Contains(out.String(), `"active"`) {
		t.Errorf("runners.list: %s", out.String())
	}

	// Errors surface code and message.
	err = mcpMain([]string{"api", "runners.list", `{"fleet":"nope"}`}, nil, &out)
	if err == nil || !strings.Contains(err.Error(), ":") {
		t.Errorf("err = %v", err)
	}
	if err := mcpMain([]string{"api", "fleet.create", `{"name":"x"}`}, nil, &out); err == nil {
		t.Error("admin method reachable through mcp api")
	}
	if err := mcpMain([]string{"api", "hello", `{bad`}, nil, &out); err == nil {
		t.Error("invalid JSON accepted")
	}
}

func TestMCPLoginWrongHostKeyRefused(t *testing.T) {
	e := newCenv(t)
	other := newCenv(t)
	cfgDir := filepath.Join(t.TempDir(), "cfg")
	t.Setenv(opconf.EnvDir, cfgDir)
	err := mcpMain([]string{"login", e.addr, "-name", "alice", "-code", "-"}, strings.NewReader(other.code()), &bytes.Buffer{})
	if err == nil {
		t.Fatal("login succeeded against a different host key")
	}
	if _, err := os.Stat(opconf.KnownHostsPath(cfgDir)); err == nil {
		t.Error("known_hosts written after a refused login")
	}
	if _, err := os.Stat(opconf.ContextsPath(cfgDir)); err == nil {
		t.Error("context saved after a refused login")
	}
}

func TestMCPAPIWrongPinnedKeyRefused(t *testing.T) {
	e := newCenv(t)
	cfgDir := filepath.Join(t.TempDir(), "cfg")
	t.Setenv(opconf.EnvDir, cfgDir)
	if err := mcpMain([]string{"login", e.addr, "-name", "alice", "-code", "-"}, strings.NewReader(e.code()), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	// Replace the pinned key with another host's: the API call must be refused.
	other := newCenv(t)
	line := e.addr + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(other.host.PublicKey()))) + "\n"
	if err := os.WriteFile(opconf.KnownHostsPath(cfgDir), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mcpMain([]string{"api", "hello"}, nil, &bytes.Buffer{}); err == nil {
		t.Fatal("api call accepted a host key that is not pinned")
	}
}

func TestMCPUsage(t *testing.T) {
	var out bytes.Buffer
	if err := mcpMain(nil, nil, &out); err != nil || !strings.Contains(out.String(), "M5") {
		t.Errorf("bare mcp: %v %q", err, out.String())
	}
	t.Setenv(opconf.EnvDir, t.TempDir())
	if err := mcpMain([]string{"login", "127.0.0.1:1", "-name", "a", "-code", "x"}, strings.NewReader(""), &out); err == nil {
		t.Error("-code other than - accepted")
	}
	if err := mcpMain([]string{"api", "hello"}, nil, &out); err == nil || !strings.Contains(err.Error(), "no current context") {
		t.Errorf("api without login: %v", err)
	}
}
