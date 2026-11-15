//go:build linux

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestHostShimMainRunsAJob(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "j1")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "command"), []byte("echo out; echo err >&2; exit 3"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := hostShimMain([]string{"job", "-dir", dir, "-cwd", t.TempDir(), "-timeout", "30"}); err != nil {
		t.Fatal(err)
	}
	var st struct {
		State    string `json:"state"`
		ExitCode *int   `json:"exit_code"`
	}
	b, err := os.ReadFile(filepath.Join(dir, "status.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	if st.State != "exited" || st.ExitCode == nil || *st.ExitCode != 3 {
		t.Fatalf("status = %s", b)
	}
	if o, _ := os.ReadFile(filepath.Join(dir, "stdout")); string(o) != "out\n" {
		t.Fatalf("stdout = %q", o)
	}
	if o, _ := os.ReadFile(filepath.Join(dir, "stderr")); string(o) != "err\n" {
		t.Fatalf("stderr = %q", o)
	}
}

func TestHostShimMainRefusesBadArgs(t *testing.T) {
	for _, a := range [][]string{nil, {"run"}, {"job", "-dir", "rel", "-cwd", "/"}} {
		if err := hostShimMain(a); err == nil {
			t.Errorf("args %v accepted", a)
		}
	}
}
