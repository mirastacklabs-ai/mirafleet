package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mirastacklabs-ai/mirafleet/internal/central"
)

func TestParseCentralRun(t *testing.T) {
	cfg, err := parseCentralRun(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GUI.Listen != "0.0.0.0:8443" || !cfg.GUI.enabled() || cfg.Listen != "0.0.0.0:2222" || cfg.Data != defaultCentralData {
		t.Fatalf("defaults: %+v", cfg)
	}
	cfg, err = parseCentralRun([]string{"-gui-listen", "off"}, nil)
	if err != nil || cfg.GUI.enabled() {
		t.Fatalf("off: %+v %v", cfg, err)
	}
	cfg, err = parseCentralRun([]string{"-data", "/d", "-gui-listen", "127.0.0.1:9", "-gui-cert", "c", "-gui-key", "k",
		"-gui-host", "a.example", "-gui-host", "b.example, c.example", "-advertise", "central.example:2222"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cfg.GUI.Hosts, "|") != "a.example|b.example|c.example" || cfg.GUI.CertFile != "c" ||
		cfg.GUI.KeyFile != "k" || cfg.GUI.Advertise != "central.example:2222" || cfg.Data != "/d" {
		t.Fatalf("parsed: %+v", cfg)
	}
	for _, args := range [][]string{
		{"-gui-cert", "c"}, {"-gui-key", "k"}, {"-gui-listen", ""}, {"-gui-listen", "nonsense"},
		{"-advertise", "nohost"}, {"-advertise", ":2222"}, {"-advertise", "h:99999"}, {"extra"}, {"-nope"},
	} {
		if _, err := parseCentralRun(args, &bytes.Buffer{}); !errors.Is(err, errAdminUsage) {
			t.Errorf("%v: err %v", args, err)
		}
	}
}

type logBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuf) logf(f string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(&l.b, f+"\n", a...)
}
func (l *logBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// The GUI starts with central, writes the setup token file (0600) and logs only its path;
// reset-admin prints a fresh token on stdout only; fleet set toggles the guard; Close stops the
// GUI before the store.
func TestCentralGUIWiring(t *testing.T) {
	data := centralTestDir(t)
	ctx := context.Background()
	if err := doCentralInit(ctx, data, "127.0.0.1:0", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var lg logBuf
	rc, err := startCentral(ctx, data, "127.0.0.1:0", guiOptions{Listen: "127.0.0.1:0", Logf: lg.logf,
		Hosts: []string{"central.example.com"}, Advertise: "central.example.com:2222"})
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			rc.Close()
		}
	}()
	if rc.GUIAddr == "" {
		t.Fatal("GUI did not start")
	}
	path := filepath.Join(data, "gui", "setup-token")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode %v", fi.Mode())
	}
	if d, _ := os.Stat(filepath.Join(data, "gui")); d.Mode().Perm() != 0o700 {
		t.Fatalf("gui dir mode %v", d.Mode())
	}
	raw, _ := os.ReadFile(path)
	tok := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(tok, "mfs1.") {
		t.Fatalf("token file: %q", raw)
	}
	log := lg.String()
	for _, want := range []string{path, "SHA-256 fingerprint", "GUI listening on", "central.example.com:2222"} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, tok) {
		t.Fatal("setup token in the log")
	}

	// reset-admin: fresh token on stdout only.
	var o, e bytes.Buffer
	c := &adminCLI{call: central.AdminCall, out: &o, err: &e, exit: func(int) { t.Fatal("exit") }}
	if err := c.run([]string{"gui", "reset-admin", "-data", data}); err != nil {
		t.Fatal(err)
	}
	fresh := strings.TrimSpace(o.String())
	if !strings.HasPrefix(fresh, "mfs1.") || strings.Contains(fresh, "\n") || fresh == tok || strings.Contains(e.String(), fresh) {
		t.Fatalf("reset-admin stdout %q stderr %q", o.String(), e.String())
	}
	if strings.Contains(lg.String(), fresh) {
		t.Fatal("fresh token in the log")
	}

	// fleet set.
	run := func(args ...string) string {
		t.Helper()
		o.Reset()
		if err := c.run(append(args, "-data", data)); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return o.String()
	}
	run("fleet", "create", "web")
	if out := run("fleet", "set", "web", "-confirm=true"); !strings.Contains(out, "confirm true") {
		t.Fatalf("fleet set: %s", out)
	}
	if out := run("fleet", "list"); !strings.Contains(out, "true") {
		t.Fatalf("fleet list: %s", out)
	}
	if out := run("audit", "-action", "fleet.", "-action-prefix", "-source", "cli"); !strings.Contains(out, "fleet.create") {
		t.Fatalf("filtered audit: %s", out)
	}
	if out := run("audit", "-action", "gui.reset_admin"); !strings.Contains(out, "gui.reset_admin") {
		t.Fatalf("audit lacks reset_admin: %s", out)
	}

	addr := rc.GUIAddr
	rc.Close()
	closed = true
	if conn, err := net.Dial("tcp", addr); err == nil {
		conn.Close()
		t.Fatal("GUI still listening after Close")
	}
}

func TestCentralGUIOff(t *testing.T) {
	data := centralTestDir(t)
	ctx := context.Background()
	if err := doCentralInit(ctx, data, "127.0.0.1:0", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	rc, err := startCentral(ctx, data, "127.0.0.1:0", guiOptions{Listen: "off"})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if rc.GUIAddr != "" {
		t.Fatal("GUI started with off")
	}
	if _, err := os.Stat(filepath.Join(data, "gui")); err == nil {
		t.Fatal("gui dir created with the GUI off")
	}
}

func TestWriteSetupTokenReplacesStale(t *testing.T) {
	data := centralTestDir(t)
	if _, err := writeSetupToken(data, "mfs1.old"); err != nil {
		t.Fatal(err)
	}
	p, err := writeSetupToken(data, "mfs1.new")
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); strings.TrimSpace(string(b)) != "mfs1.new" {
		t.Fatalf("token file: %q", b)
	}
}
