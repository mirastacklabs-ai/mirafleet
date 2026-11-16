package handoff

import (
	"os"
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
