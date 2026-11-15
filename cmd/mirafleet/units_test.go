package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// parseUnit returns section -> key -> values of a systemd unit file (comments and blanks skipped).
func parseUnit(t *testing.T, rel string) map[string]map[string][]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "packaging", "systemd", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string][]string{}
	sec := ""
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		switch {
		case l == "" || strings.HasPrefix(l, "#") || strings.HasPrefix(l, ";"):
		case strings.HasPrefix(l, "["):
			sec = strings.Trim(l, "[]")
			out[sec] = map[string][]string{}
		default:
			k, v, ok := strings.Cut(l, "=")
			if !ok || sec == "" {
				t.Fatalf("%s: malformed line %q", rel, l)
			}
			out[sec][k] = append(out[sec][k], v)
		}
	}
	return out
}

func TestRunnerUnitFiles(t *testing.T) {
	cases := []struct {
		file, execStart, install string
		user                     bool
	}{
		{"user/mirafleet-runner.service", "/usr/bin/mirafleet runner run -workspace %h/.config/mirafleet/workspace.yaml", "default.target", false},
		{"mirafleet-runner@.service", "/usr/bin/mirafleet runner run -workspace /home/%i/.config/mirafleet/workspace.yaml", "multi-user.target", true},
	}
	for _, c := range cases {
		u := parseUnit(t, c.file)
		svc := u["Service"]
		if got := svc["ExecStart"]; len(got) != 1 || got[0] != c.execStart {
			t.Errorf("%s: ExecStart = %q", c.file, got)
		}
		// Background shells must survive a runner restart (docs/M7.md §3.5).
		if svc["KillMode"][0] != "process" || svc["Restart"][0] != "on-failure" {
			t.Errorf("%s: KillMode/Restart = %v/%v", c.file, svc["KillMode"], svc["Restart"])
		}
		if got := u["Install"]["WantedBy"]; len(got) != 1 || got[0] != c.install {
			t.Errorf("%s: WantedBy = %q", c.file, got)
		}
		_, hasUser := svc["User"]
		if c.user && (!hasUser || svc["User"][0] != "%i") {
			t.Errorf("%s: User = %q, want %%i", c.file, svc["User"])
		}
		if !c.user && hasUser {
			t.Errorf("%s: a user unit must not set User=", c.file)
		}
		if strings.Contains(strings.Join(svc["ExecStart"], ""), "-data") {
			t.Errorf("%s: -data must stay at the XDG default", c.file)
		}
	}
	// The user unit documents linger (an admin step, once).
	b, err := os.ReadFile(filepath.Join("..", "..", "packaging", "systemd", "user", "mirafleet-runner.service"))
	if err != nil || !strings.Contains(string(b), "enable-linger") {
		t.Errorf("the user unit must mention loginctl enable-linger: %v", err)
	}
}
