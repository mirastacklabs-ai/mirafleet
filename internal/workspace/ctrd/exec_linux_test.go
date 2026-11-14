//go:build linux

package ctrd

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/opencontainers/runtime-spec/specs-go"

	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
)

func TestExecEnv(t *testing.T) {
	env, err := execEnv(map[string]string{"B": "2", "A": "1", "PATH": "/x"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(env, ",") != "A=1,B=2,HOME=/workspace,PATH=/x" {
		t.Errorf("env = %v", env)
	}
	for _, bad := range []map[string]string{{"": "x"}, {"A=B": "x"}, {"A\x00": "x"}, {"A": "x\x00"}} {
		if _, err := execEnv(bad); !errors.Is(err, workspace.ErrRefused) {
			t.Errorf("execEnv(%q) = %v", bad, err)
		}
	}
}

func TestExecProcessSpecPinsSecurity(t *testing.T) {
	base := &specs.Spec{Process: &specs.Process{
		User: specs.User{UID: 1000, GID: 1000}, NoNewPrivileges: false, Terminal: true,
		Capabilities:    &specs.LinuxCapabilities{Effective: []string{"CAP_SYS_ADMIN"}, Bounding: []string{"CAP_SYS_ADMIN"}},
		Rlimits:         []specs.POSIXRlimit{{Type: "RLIMIT_NOFILE", Hard: 4096, Soft: 4096}},
		ApparmorProfile: "prof",
	}}
	argv := []string{"ls"}
	p, err := execProcessSpec(base, argv, "/workspace/x", []string{"A=1"})
	if err != nil {
		t.Fatal(err)
	}
	if !p.NoNewPrivileges || p.Terminal || p.User.UID != 1000 || p.Cwd != "/workspace/x" || p.ApparmorProfile != "prof" {
		t.Errorf("process = %+v", p)
	}
	c := p.Capabilities
	if len(c.Effective)+len(c.Bounding)+len(c.Permitted)+len(c.Inheritable)+len(c.Ambient) != 0 || c.Bounding == nil {
		t.Errorf("caps = %+v", c)
	}
	if len(p.Rlimits) != 2 || len(base.Process.Rlimits) != 1 || !base.Process.Terminal {
		t.Errorf("rlimits %v / base mutated", p.Rlimits)
	}
	argv[0] = "rm"
	if p.Args[0] != "ls" {
		t.Error("args alias the request")
	}
	// An init that somehow runs as root is refused.
	base.Process.User.UID = 0
	if _, err := execProcessSpec(base, argv, "/workspace", nil); err == nil {
		t.Error("root exec accepted")
	}
	if _, err := execProcessSpec(&specs.Spec{}, argv, "/workspace", nil); err == nil {
		t.Error("spec without process accepted")
	}
}

func TestStartFailure(t *testing.T) {
	for msg, code := range map[string]int{
		`OCI runtime exec failed: exec failed: unable to start container process: exec: "nope": executable file not found in $PATH`: 127,
		`unable to start container process: chdir to cwd ("/workspace/x") set in config.json failed: no such file or directory`:     127,
		`unable to start container process: exec: "/workspace/f": permission denied`:                                                126,
	} {
		c, m, ok := startFailure(errors.New(msg))
		if !ok || c != code || strings.Contains(m, "OCI runtime") {
			t.Errorf("startFailure(%q) = %d %q %v", msg, c, m, ok)
		}
	}
	if _, _, ok := startFailure(errors.New("containerd exploded")); ok {
		t.Error("unrelated error mapped to an exit code")
	}
}

func TestGateDropsWritesAfterClose(t *testing.T) {
	var buf bytes.Buffer
	g := &gate{}
	w1, w2 := g.wrap(&buf), g.wrap(&buf)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = w1.Write([]byte("a")); _, _ = w2.Write([]byte("b")) }()
	}
	wg.Wait()
	g.close()
	n := buf.Len()
	if n != 16 {
		t.Errorf("wrote %d", n)
	}
	if k, err := w1.Write([]byte("late")); k != 4 || err != nil || buf.Len() != n {
		t.Errorf("late write: %d %v len %d", k, err, buf.Len())
	}
}

func TestParseStartTime(t *testing.T) {
	line := "123 (a b) c) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 98765 23 24"
	if v, ok := parseStartTime(line); !ok || v != 98765 {
		t.Errorf("parseStartTime = %d, %v", v, ok)
	}
	for _, bad := range []string{"", "123 no parens", "1 (x) S 1 2"} {
		if _, ok := parseStartTime(bad); ok {
			t.Errorf("parseStartTime(%q) accepted", bad)
		}
	}
}
