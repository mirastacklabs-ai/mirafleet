package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/hostx"
)

func xdgPaths(t *testing.T) (hostx.Paths, func() (hostx.Paths, error)) {
	t.Helper()
	home := t.TempDir()
	env := map[string]string{}
	p, err := hostx.PathsFrom(func(k string) string { return env[k] }, home)
	if err != nil {
		t.Fatal(err)
	}
	return p, func() (hostx.Paths, error) { return p, nil }
}

func TestResolveRunnerDataXDGDefaults(t *testing.T) {
	p, paths := xdgPaths(t)
	if !strings.HasSuffix(p.Config, filepath.Join(".config", "mirafleet")) {
		t.Fatalf("config dir = %s", p.Config)
	}
	// Explicit -data and root keep the given value and touch nothing.
	for _, c := range []struct {
		given bool
		uid   int
	}{{true, 1000}, {false, 0}} {
		got, err := resolveRunnerData(c.given, defaultRunnerData, c.uid, paths)
		if err != nil || got != defaultRunnerData {
			t.Fatalf("%+v: %q %v", c, got, err)
		}
	}
	if _, err := os.Stat(p.Config); err == nil {
		t.Fatal("the XDG dirs were created although they were not used")
	}
	got, err := resolveRunnerData(false, defaultRunnerData, 1000, paths)
	if err != nil || got != p.Config {
		t.Fatalf("ordinary user: %q %v", got, err)
	}
	for _, d := range []string{p.Config, p.State, p.Data} {
		st, err := os.Stat(d)
		if err != nil || st.Mode().Perm() != 0o700 {
			t.Fatalf("%s: %v %v", d, st, err)
		}
	}
}

func TestXDGEnvOverridesHome(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", cfg)
	got, err := resolveRunnerData(false, defaultRunnerData, 1000, hostx.DefaultPaths)
	if err != nil || got != filepath.Join(cfg, "mirafleet") {
		t.Fatalf("%q %v", got, err)
	}
}

// pruneFixture creates an old (48 h) and a fresh session directory under the XDG data dir.
func pruneFixture(t *testing.T) (oldDir, newDir string, paths func() (hostx.Paths, error)) {
	t.Helper()
	p, paths := xdgPaths(t)
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	mk := func(id string, now func() time.Time) string {
		m, err := hostx.NewManager(hostx.SessionsConfig{Root: p.SessionsRoot(), StateDir: p.State, Now: now})
		if err != nil {
			t.Fatal(err)
		}
		s, err := m.Open(id, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(s.Dir, "f.txt"), []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
		s.Close()
		return s.Dir
	}
	oldDir = mk("sess-old", func() time.Time { return time.Now().Add(-48 * time.Hour) })
	newDir = mk("sess-new", time.Now)
	return oldDir, newDir, paths
}

func TestRunnerHostPruneDryRunThenYes(t *testing.T) {
	oldDir, newDir, paths := pruneFixture(t)

	var dry bytes.Buffer
	if err := runnerHostPrune([]string{"-older-than", "24h"}, &dry, paths); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dry.String(), "sess-old") || strings.Contains(dry.String(), "sess-new") ||
		!strings.Contains(dry.String(), "re-run with -yes") {
		t.Fatalf("dry run output:\n%s", dry.String())
	}
	if _, err := os.Stat(oldDir); err != nil {
		t.Fatalf("a dry run deleted the directory: %v", err)
	}

	var real bytes.Buffer
	if err := runnerHostPrune([]string{"-older-than", "24h", "-yes"}, &real, paths); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(real.String(), "deleted 1 session") {
		t.Fatalf("output:\n%s", real.String())
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Fatalf("old directory still there: %v", err)
	}
	if _, err := os.Stat(newDir); err != nil {
		t.Fatalf("the fresh directory was deleted: %v", err)
	}

	var again bytes.Buffer
	if err := runnerHostPrune([]string{"-older-than", "24h", "-yes"}, &again, paths); err != nil || !strings.Contains(again.String(), "nothing to prune") {
		t.Fatalf("second run: %v %q", err, again.String())
	}
}

func TestRunnerHostPruneDefaultIs30Days(t *testing.T) {
	oldDir, _, paths := pruneFixture(t) // 48 h old: younger than the 30 d default
	var out bytes.Buffer
	if err := runnerHostPrune([]string{"-yes"}, &out, paths); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldDir); err != nil || !strings.Contains(out.String(), "nothing to prune") {
		t.Fatalf("%v %q", err, out.String())
	}
}

func TestRunnerHostPruneArgs(t *testing.T) {
	_, _, paths := pruneFixture(t)
	for _, args := range [][]string{{"-older-than", "-1h"}, {"extra"}, {"-nope"}, {"-older-than", "soon"}} {
		if err := runnerHostPrune(args, &bytes.Buffer{}, paths); err == nil {
			t.Errorf("%v: want an error", args)
		}
	}
	if err := runnerHost(nil, &bytes.Buffer{}); err == nil {
		t.Error("runner host without a subcommand must be a usage error")
	}
	if err := runnerHost([]string{"clean"}, &bytes.Buffer{}); err == nil {
		t.Error("unknown runner host subcommand must be a usage error")
	}
}
