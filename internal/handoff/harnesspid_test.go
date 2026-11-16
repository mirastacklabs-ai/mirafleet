package handoff

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHarnessPID(t *testing.T) {
	for _, c := range []struct {
		env  string
		want int
	}{
		{"4242", 4242},
		{"", os.Getppid()},
		{"0", os.Getppid()},
		{"-5", os.Getppid()},
		{"abc", os.Getppid()},
	} {
		t.Setenv(HarnessPIDEnv, c.env)
		if got := HarnessPID(); got != c.want {
			t.Errorf("%q: got %d, want %d", c.env, got, c.want)
		}
	}
}

func TestKeyResolvesSymlinkedProject(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if (Key{PID: 1, Project: link}).name() != (Key{PID: 1, Project: real}).name() {
		t.Error("logical and physical project paths give different keys")
	}
	missing := filepath.Join(real, "nope")
	if got := (Key{Project: missing}).clean(); got != missing {
		t.Errorf("unresolvable path: got %q, want %q", got, missing)
	}
}
