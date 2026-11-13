package runner

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := &FileConfig{Central: "127.0.0.1:2222", Name: "web-1", Fleet: "prod"}
	if err := SaveConfig(dir, want); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(ConfigPath(dir))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("stat %v mode %v", err, st)
	}
	got, err := LoadConfig(dir)
	if err != nil || *got != *want {
		t.Fatalf("got %+v err %v", got, err)
	}
}

func TestConfigStrict(t *testing.T) {
	for name, body := range map[string]string{
		"unknown field": "central: 1.2.3.4:2222\nname: a\nfleet: f\nlabels: x\n",
		"bad addr":      "central: nohostport\nname: a\nfleet: f\n",
		"bad name":      "central: 1.2.3.4:2222\nname: A_B\nfleet: f\n",
		"no fleet":      "central: 1.2.3.4:2222\nname: a\n",
	} {
		dir := t.TempDir()
		if err := os.WriteFile(ConfigPath(dir), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(dir); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := SaveConfig(t.TempDir(), &FileConfig{Central: "x", Name: "a", Fleet: "f"}); err == nil {
		t.Error("SaveConfig accepted an invalid config")
	}
}

func TestLoadConfigNotJoined(t *testing.T) {
	if _, err := LoadConfig(t.TempDir()); !errors.Is(err, ErrNotJoined) {
		t.Fatalf("err = %v", err)
	}
}

func TestJoinRefusesJoinedDir(t *testing.T) {
	dir := t.TempDir()
	if err := SaveConfig(dir, &FileConfig{Central: "127.0.0.1:2222", Name: "a", Fleet: "f"}); err != nil {
		t.Fatal(err)
	}
	_, err := Join(context.Background(), dir, "127.0.0.1:1", "a", "mfj1.x")
	if !errors.Is(err, ErrAlreadyJoined) {
		t.Fatalf("err = %v", err)
	}
	if _, e := os.Stat(KeyPath(dir)); e == nil {
		t.Error("a key was generated in an already-joined dir")
	}
}

func TestJoinValidatesInputs(t *testing.T) {
	dir := t.TempDir()
	if _, err := Join(context.Background(), dir, "noport", "a", "t"); err == nil {
		t.Error("bad address accepted")
	}
	if _, err := Join(context.Background(), dir, "127.0.0.1:1", "Bad Name", "t"); err == nil {
		t.Error("bad name accepted")
	}
	_, err := Join(context.Background(), dir, "127.0.0.1:1", "a", "SECRETISH")
	if err == nil || strings.Contains(err.Error(), "SECRETISH") {
		t.Errorf("err = %v", err)
	}
}
