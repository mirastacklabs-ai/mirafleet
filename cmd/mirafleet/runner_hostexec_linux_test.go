package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/hostx"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/wsconfig"
)

// `executor: host` in workspace.yaml builds the hostx executor on the runner user's XDG
// directories, with host.root and the cleanup sweeper wired (S11).
func TestNewExecutorHost(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	old := hostHarden
	var checked []string
	hostHarden = func(d ...string) error { checked = d; return nil } // the test may run as root in a container
	defer func() { hostHarden = old }()

	wc, err := wsconfig.Parse([]byte("executor: host\nconcurrent: 2\nhost: {root: \"~/sessions\"}\n"))
	if err != nil {
		t.Fatal(err)
	}
	ex, err := newExecutor(wc, "fleet/r1")
	if err != nil {
		t.Fatal(err)
	}
	if h, ok := ex.(interface{ HostMode() bool }); !ok || !h.HostMode() || ex.Name() != "host" {
		t.Fatalf("not a host executor: %T", ex)
	}
	id := workspace.NewID()
	if err := ex.Create(context.Background(), workspace.Spec{ID: id, Labels: map[string]string{hostx.LabelWorkdir: "proj"}}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(home, "sessions", "work", "proj")); err != nil || !fi.IsDir() {
		t.Fatalf("workdir not under host.root: %v", err)
	}
	if err := ex.Close(); err != nil { // stops the sweeper
		t.Fatal(err)
	}
	// H-12: both the state dir and $HOME go through the group/world-writable check.
	if len(checked) != 2 || checked[1] != home {
		t.Fatalf("hardening checked %v, want the state dir and %s", checked, home)
	}
	if err := hostx.CheckDir(checked[1], os.Geteuid()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(home, 0o775); err != nil {
		t.Fatal(err)
	}
	if hostx.CheckDir(home, os.Geteuid()) == nil {
		t.Fatal("group-writable $HOME accepted")
	}
}

func TestLoadCorporateCA(t *testing.T) {
	if pem, err := loadCorporateCA(nil); pem != nil || err != nil {
		t.Fatalf("nil config: %v %v", pem, err)
	}
	p := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(p, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCorporateCA(&wsconfig.Config{Host: &wsconfig.Host{CorporateCAFile: p}}); err == nil {
		t.Fatal("garbage CA accepted")
	}
}
