package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mirastacklabs-ai/mirafleet/internal/central"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/store/sqlite"
)

// centralTestDir returns a temp dir with a short path (unix socket paths are length limited).
func centralTestDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "mf")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func readCentralSecret(t *testing.T, data string) string {
	t.Helper()
	db, err := sqlite.Open(context.Background(), filepath.Join(data, "mirafleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := db.GetSetting(context.Background(), store.SettingRegistrationSecret)
	if err != nil {
		t.Fatal(err)
	}
	return string(s)
}

func TestCentralInitIsIdempotent(t *testing.T) {
	data := filepath.Join(centralTestDir(t), "central")
	ctx := context.Background()
	var out1, out2 bytes.Buffer
	if err := doCentralInit(ctx, data, "0.0.0.0:2222", &out1); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out1.String(), "SHA256:") || !strings.Contains(out1.String(), "created the registration secret") {
		t.Fatalf("first init output: %s", out1.String())
	}
	key1, err := os.ReadFile(filepath.Join(data, "host_ed25519"))
	if err != nil {
		t.Fatal(err)
	}
	pub1, _ := os.ReadFile(filepath.Join(data, "host_ed25519.pub"))
	sec1 := readCentralSecret(t, data)

	if err := doCentralInit(ctx, data, "0.0.0.0:2222", &out2); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out2.String(), "already initialised") {
		t.Fatalf("second init output: %s", out2.String())
	}
	key2, _ := os.ReadFile(filepath.Join(data, "host_ed25519"))
	pub2, _ := os.ReadFile(filepath.Join(data, "host_ed25519.pub"))
	if !bytes.Equal(key1, key2) || !bytes.Equal(pub1, pub2) {
		t.Fatal("second init rewrote the host key")
	}
	if readCentralSecret(t, data) != sec1 {
		t.Fatal("second init rewrote the registration secret")
	}
	// Same fingerprint both times.
	fp := func(s string) string {
		for _, l := range strings.Split(s, "\n") {
			if strings.HasPrefix(l, "fingerprint:") {
				return l
			}
		}
		return ""
	}
	if fp(out1.String()) == "" || fp(out1.String()) != fp(out2.String()) {
		t.Fatalf("fingerprints differ: %q vs %q", fp(out1.String()), fp(out2.String()))
	}
	if fi, _ := os.Stat(data); fi.Mode().Perm() != 0o700 {
		t.Fatalf("data dir mode %v", fi.Mode().Perm())
	}
	if fi, err := os.Stat(filepath.Join(data, "mirafleet.db")); err != nil || fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("db mode: %v %v", fi, err)
	}
}

func TestDataDirLockIsExclusive(t *testing.T) {
	data := centralTestDir(t)
	rel, err := lockDataDir(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockDataDir(data); err == nil || !strings.Contains(err.Error(), "another central") {
		t.Fatalf("second lock: %v", err)
	}
	rel()
	rel2, err := lockDataDir(data)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	rel2()
}

func TestSecondCentralOnSameDirFails(t *testing.T) {
	data := centralTestDir(t)
	ctx := context.Background()
	if err := doCentralInit(ctx, data, "127.0.0.1:0", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	rc, err := startCentral(ctx, data, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if _, err := startCentral(ctx, data, "127.0.0.1:0"); err == nil || !strings.Contains(err.Error(), "another central") {
		t.Fatalf("second central: %v", err)
	}
	// init also refuses while central runs.
	if err := doCentralInit(ctx, data, "127.0.0.1:0", &bytes.Buffer{}); err == nil {
		t.Fatal("init ran against a locked data dir")
	}
}

// Drives the CLI against a real in-process central.
func TestCLIIntegration(t *testing.T) {
	data := centralTestDir(t)
	ctx := context.Background()
	if err := doCentralInit(ctx, data, "127.0.0.1:0", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	rc, err := startCentral(ctx, data, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	run := func(args ...string) (stdout, stderr string, err error) {
		var o, e bytes.Buffer
		c := &adminCLI{call: central.AdminCall, out: &o, err: &e, exit: func(int) { t.Fatal("unexpected exit") }}
		err = c.run(append(args, "-data", data))
		return o.String(), e.String(), err
	}
	mustRun := func(args ...string) string {
		t.Helper()
		o, e, err := run(args...)
		if err != nil {
			t.Fatalf("%v: %v (stderr %q)", args, err, e)
		}
		return o
	}

	mustRun("fleet", "create", "web", "-confirm")
	if out := mustRun("fleet", "list"); !strings.Contains(out, "web") {
		t.Fatalf("fleet list: %s", out)
	}
	if _, _, err := run("fleet", "create", "web"); err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("duplicate fleet: %v", err)
	}

	tokOut := mustRun("token", "create", "-fleet", "web", "-labels", "env=test", "-uses", "2", "-ttl", "1h")
	tok := strings.TrimSpace(tokOut)
	if !strings.HasPrefix(tok, "mfj1.") || strings.Contains(tok, "\n") {
		t.Fatalf("token stdout: %q", tokOut)
	}
	list := mustRun("token", "list", "-fleet", "web")
	if !strings.Contains(list, "env=test") || strings.Contains(list, tok) || !strings.Contains(list, "0/2") {
		t.Fatalf("token list: %s", list)
	}

	if out := mustRun("runner", "list"); !strings.HasPrefix(out, "RUNNER") {
		t.Fatalf("runner list: %s", out)
	}

	code := mustRun("operator", "code")
	if !strings.Contains(code, "mfr1.") || !strings.Contains(code, "SHA256:") || !strings.Contains(code, "valid for:") {
		t.Fatalf("operator code: %s", code)
	}

	aud := mustRun("audit")
	for _, want := range []string{"fleet.create", "token.create", "operator.code"} {
		if !strings.Contains(aud, want) {
			t.Fatalf("audit lacks %s:\n%s", want, aud)
		}
	}
	if strings.Contains(aud, tok) || strings.Contains(aud, strings.Fields(strings.SplitN(code, "code:", 2)[1])[0]) {
		t.Fatal("audit leaks a secret")
	}
}
