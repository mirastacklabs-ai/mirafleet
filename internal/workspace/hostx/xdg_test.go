//go:build linux

package hostx

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPathsFrom(t *testing.T) {
	env := map[string]string{}
	get := func(k string) string { return env[k] }
	p, err := PathsFrom(get, "/home/dev")
	if err != nil {
		t.Fatal(err)
	}
	if p.Config != "/home/dev/.config/mirafleet" || p.State != "/home/dev/.local/state/mirafleet" || p.Data != "/home/dev/.local/share/mirafleet" {
		t.Fatalf("defaults: %+v", p)
	}
	env["XDG_CONFIG_HOME"] = "/cfg"
	env["XDG_STATE_HOME"] = "relative/ignored"
	env["XDG_DATA_HOME"] = "/data/"
	p, _ = PathsFrom(get, "/home/dev")
	if p.Config != "/cfg/mirafleet" || p.State != "/home/dev/.local/state/mirafleet" || p.Data != "/data/mirafleet" {
		t.Fatalf("env: %+v", p)
	}
	if _, err := PathsFrom(func(string) string { return "" }, ""); err == nil {
		t.Fatal("no home and no XDG must fail")
	}
}

func TestEnsure0700(t *testing.T) {
	base := t.TempDir()
	p := Paths{Config: filepath.Join(base, "c", "mirafleet"), State: filepath.Join(base, "s"), Data: filepath.Join(base, "d")}
	if err := os.MkdirAll(p.State, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{p.Config, p.State, p.Data} {
		fi, err := os.Stat(d)
		if err != nil || fi.Mode().Perm() != 0o700 {
			t.Fatalf("%s: %v %v", d, fi, err)
		}
	}
}
