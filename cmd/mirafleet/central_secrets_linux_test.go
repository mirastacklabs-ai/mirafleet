package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/secrets"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/store/sqlite"
)

func TestKeygenFormat(t *testing.T) {
	var out1, err1, out2 bytes.Buffer
	if err := centralKeygen(nil, &out1, &err1); err != nil {
		t.Fatal(err)
	}
	if err := centralKeygen(nil, &out2, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`^MIRAFLEET_SECRETS_KEY=([A-Za-z0-9+/]{43}=)\n$`)
	m := re.FindStringSubmatch(out1.String())
	if m == nil {
		t.Fatalf("stdout = %q", out1.String())
	}
	if _, err := secrets.ParseKey(m[1]); err != nil {
		t.Fatalf("the printed key does not parse: %v", err)
	}
	if out1.String() == out2.String() {
		t.Fatal("two keygen runs printed the same key")
	}
	if !strings.Contains(err1.String(), "lost") || strings.Contains(err1.String(), m[1]) {
		t.Fatalf("stderr must warn about losing the key and never repeat it: %q", err1.String())
	}
	if err := centralKeygen([]string{"x"}, &out1, &err1); err == nil || !errors.Is(err, errAdminUsage) {
		t.Fatalf("args: %v", err)
	}
}

func TestCredentialsConfigFromEnv(t *testing.T) {
	t.Setenv(secrets.EnvKey, "")
	if cc, err := credentialsConfig(); err != nil || cc.Key != nil {
		t.Fatalf("unset: %+v %v", cc, err)
	}
	t.Setenv(secrets.EnvKey, "not-a-key")
	if _, err := credentialsConfig(); err == nil {
		t.Fatal("a malformed key must be an error")
	}
	_, enc, _ := secrets.GenerateKey()
	t.Setenv(secrets.EnvKey, enc)
	if cc, err := credentialsConfig(); err != nil || cc.Key == nil {
		t.Fatalf("set: %+v %v", cc, err)
	}
}

func TestSecretsRekeyRoundTrip(t *testing.T) {
	ctx := context.Background()
	data := filepath.Join(centralTestDir(t), "central")
	if err := doCentralInit(ctx, data, "0.0.0.0:2222", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	_, oldEnc, _ := secrets.GenerateKey()
	_, newEnc, _ := secrets.GenerateKey()
	t.Setenv(secrets.EnvKey, oldEnc)

	const value = "glpat-DECOY-rekey-0123456789"
	d, err := loadCentral(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(value))
	p := api.CredentialSetParams{Length: len(value), SHA256: hex.EncodeToString(sum[:])}
	p.Name, p.Kind, p.Hosts, p.Auth = "gl", "https-token", []string{"gitlab.example.com"}, "bearer"
	if _, err := d.svc.SetCredential(ctx, auth.LocalAdmin(os.Getuid()), service.SourceCLI, p, []byte(value)); err != nil {
		t.Fatal(err)
	}
	d.db.Close()

	// A wrong old key changes nothing.
	wrong, _, _ := secrets.GenerateKey()
	newKey, _ := secrets.ParseKey(newEnc)
	if _, err := rekeyCentral(ctx, data, wrong, newKey, time.Now()); err == nil || !strings.Contains(err.Error(), "nothing changed") {
		t.Fatalf("wrong old key: %v", err)
	}
	if d, err := loadCentral(ctx, data); err != nil {
		t.Fatalf("store was modified by a refused rekey: %v", err)
	} else {
		d.db.Close()
	}

	var out bytes.Buffer
	if err := centralSecrets([]string{"rekey", "-data", data}, strings.NewReader(secrets.EnvKey+"="+newEnc+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "re-encrypted 1 credential") || strings.Contains(out.String(), oldEnc) || strings.Contains(out.String(), newEnc) || strings.Contains(out.String(), "DECOY") {
		t.Fatalf("output = %q", out.String())
	}

	// The old key is refused at start-up, the new one opens the value.
	if d, err := loadCentral(ctx, data); err == nil {
		d.db.Close()
		t.Fatal("central started with the old key after a rekey")
	}
	t.Setenv(secrets.EnvKey, newEnc)
	d, err = loadCentral(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	d.db.Close()
	db, err := sqlite.Open(ctx, filepath.Join(data, "mirafleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sc, err := db.GetSealedCredential(ctx, "gl")
	if err != nil {
		t.Fatal(err)
	}
	pt, err := secrets.Open(newKey, sc.Ciphertext, credAAD(sc.ID, sc.Version))
	if err != nil || !strings.Contains(string(pt), value) {
		t.Fatalf("new key cannot open the value: %v", err)
	}
	oldKey, _ := secrets.ParseKey(oldEnc)
	if _, err := secrets.Open(oldKey, sc.Ciphertext, credAAD(sc.ID, sc.Version)); err == nil {
		t.Fatal("the old key still opens the value")
	}
}

func TestSecretsRekeyUsageAndNoSecrets(t *testing.T) {
	data := filepath.Join(centralTestDir(t), "central")
	if err := doCentralInit(context.Background(), data, "0.0.0.0:2222", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	_, oldEnc, _ := secrets.GenerateKey()
	_, newEnc, _ := secrets.GenerateKey()
	if err := centralSecrets(nil, strings.NewReader(""), &bytes.Buffer{}); !errors.Is(err, errAdminUsage) {
		t.Fatalf("no sub: %v", err)
	}
	t.Setenv(secrets.EnvKey, "")
	if err := centralSecrets([]string{"rekey", "-data", data}, strings.NewReader(newEnc), &bytes.Buffer{}); err == nil {
		t.Fatal("rekey without the old key in the environment must fail")
	}
	t.Setenv(secrets.EnvKey, oldEnc)
	if err := centralSecrets([]string{"rekey", "-data", data}, strings.NewReader("short"), &bytes.Buffer{}); err == nil {
		t.Fatal("a malformed new key must fail")
	}
	if err := centralSecrets([]string{"rekey", "-data", data}, strings.NewReader(newEnc), &bytes.Buffer{}); err == nil ||
		!strings.Contains(err.Error(), "nothing to rekey") {
		t.Fatalf("no stored secrets: %v", err)
	}
}

func TestCentralWithoutKeyOrSecretsStartsAndStoredSecretsNeedTheKey(t *testing.T) {
	ctx := context.Background()
	data := filepath.Join(centralTestDir(t), "central")
	t.Setenv(secrets.EnvKey, "")
	if err := doCentralInit(ctx, data, "0.0.0.0:2222", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	d, err := loadCentral(ctx, data) // feature off: fine
	if err != nil {
		t.Fatal(err)
	}
	d.db.Close()
	_, enc, _ := secrets.GenerateKey()
	t.Setenv(secrets.EnvKey, enc)
	d, err = loadCentral(ctx, data) // first start with a key stores its check
	if err != nil {
		t.Fatal(err)
	}
	d.db.Close()
	t.Setenv(secrets.EnvKey, "")
	if d, err := loadCentral(ctx, data); err == nil {
		d.db.Close()
		t.Fatal("central started without the key although a key check is stored")
	}
}
