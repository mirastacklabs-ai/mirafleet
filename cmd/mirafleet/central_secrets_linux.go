package main

// The secrets key of central (docs/M7.md §4.1, Q14): `central keygen`, `central secrets rekey`
// and the start-up read of MIRAFLEET_SECRETS_KEY. Linux only: darwin and windows binaries never
// link internal/secrets.

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/secrets"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/store/sqlite"
)

// credentialsConfig reads MIRAFLEET_SECRETS_KEY: no variable means the feature is off (the
// service still refuses to start when secrets are already stored); a malformed key is an error.
func credentialsConfig() (service.CredentialsConfig, error) {
	k, err := secrets.KeyFromEnv()
	if errors.Is(err, secrets.ErrNoKey) {
		return service.CredentialsConfig{}, nil
	}
	if err != nil {
		return service.CredentialsConfig{}, err
	}
	return service.CredentialsConfig{Key: keyRing{k}}, nil
}

// keyRing adapts a secrets.Key to service.KeyRing (the service package must not link secrets).
type keyRing struct{ k secrets.Key }

func (r keyRing) Seal(pt, aad []byte) ([]byte, error) { return secrets.Seal(r.k, pt, aad) }
func (r keyRing) Open(ct, aad []byte) ([]byte, error) { return secrets.Open(r.k, ct, aad) }
func (r keyRing) Check() []byte                       { return r.k.Check() }
func (r keyRing) VerifyCheck(stored []byte) bool      { return r.k.VerifyCheck(stored) }

// centralKeygen prints a fresh key as the line for central.env (stdout) with the warning on stderr.
func centralKeygen(args []string, out, errw io.Writer) error {
	if len(args) > 0 {
		return fmt.Errorf("%w: central keygen takes no arguments", errAdminUsage)
	}
	_, enc, err := secrets.GenerateKey()
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s=%s\n", secrets.EnvKey, enc)
	fmt.Fprintf(errw, "Put the line above in central's env file (mode 0600, e.g. /etc/mirafleet/central.env) and load it with\n"+
		"EnvironmentFile= before `central run`. Keep a backup: if this key is lost, every stored credential is lost\n"+
		"(central refuses to start without the right key once credentials exist).\n")
	return nil
}

// envCorporateCA names an optional PEM file of a corporate CA that central trusts, in addition to
// the system roots, when it calls GitLab for self-rotation (docs/M7.md 4.5). Unset = system roots.
const envCorporateCA = "MIRAFLEET_CORPORATE_CA_FILE"

// rotateClient is the HTTP client for GitLab self-rotation: the system pool plus the optional
// corporate CA. A nil client (no CA configured) lets the service use its default.
func rotateClient() (*http.Client, error) {
	p := os.Getenv(envCorporateCA)
	if p == "" {
		return nil, nil
	}
	pem, err := readCorporateCA(p)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", envCorporateCA, err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s: no certificate found in %s", envCorporateCA, p)
	}
	return &http.Client{Timeout: 30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}, nil
}

// maxCorporateCA bounds a corporate CA bundle file.
const maxCorporateCA = 1 << 20

// readCorporateCA reads a PEM bundle of at most 1 MiB that holds at least one certificate.
func readCorporateCA(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	pem, err := io.ReadAll(io.LimitReader(f, maxCorporateCA+1))
	if err != nil {
		return nil, err
	}
	if len(pem) > maxCorporateCA {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, maxCorporateCA)
	}
	if !x509.NewCertPool().AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no certificate found in %s", path)
	}
	return pem, nil
}

func centralSecrets(args []string, stdin io.Reader, out io.Writer) error {
	if len(args) == 0 || args[0] != "rekey" {
		return fmt.Errorf("%w: usage: mirafleet central secrets rekey [-data DIR] (old key in %s, new key on stdin)", errAdminUsage, secrets.EnvKey)
	}
	f := flag.NewFlagSet("central secrets rekey", flag.ContinueOnError)
	data := f.String("data", defaultCentralData, "central data directory")
	if err := f.Parse(args[1:]); err != nil {
		return fmt.Errorf("%w: %v", errAdminUsage, err)
	}
	if f.NArg() > 0 {
		return fmt.Errorf("%w: central secrets rekey takes no arguments", errAdminUsage)
	}
	oldKey, err := secrets.KeyFromEnv()
	if err != nil {
		return fmt.Errorf("old key (%s): %w", secrets.EnvKey, err)
	}
	line, err := bufio.NewReaderSize(io.LimitReader(stdin, 4096), 4096).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read the new key from stdin: %w", err)
	}
	// `central keygen | central secrets rekey` pipes the whole env line.
	newKey, err := secrets.ParseKey(strings.TrimPrefix(strings.TrimSpace(line), secrets.EnvKey+"="))
	if err != nil {
		return fmt.Errorf("new key (stdin): %w", err)
	}
	n, err := rekeyCentral(context.Background(), *data, oldKey, newKey, time.Now())
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "re-encrypted %d credential(s) under the new key in one transaction.\n"+
		"Now set %s to the new key in central's env file and (re)start central; the old key no longer works.\n", n, secrets.EnvKey)
	return nil
}

// credAAD mirrors internal/service's credential AAD; rekey keeps id and version, so it is unchanged.
func credAAD(id, version int64) []byte {
	return []byte(fmt.Sprintf("mirafleet credential v1|%d|%d", id, version))
}

// rekeyCentral re-encrypts every stored credential from oldKey to newKey in one transaction. It is
// offline: it takes the data-dir lock, so a running central makes it fail.
func rekeyCentral(ctx context.Context, data string, oldKey, newKey secrets.Key, now time.Time) (int, error) {
	release, err := prepareDataDir(data)
	if err != nil {
		return 0, err
	}
	defer release()
	db, err := sqlite.Open(ctx, filepath.Join(data, "mirafleet.db"))
	if err != nil {
		return 0, err
	}
	defer db.Close()
	stored, err := db.GetSecretsKeyCheck(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return 0, errors.New("nothing to rekey: no credentials have ever been stored")
	}
	if err != nil {
		return 0, err
	}
	if !oldKey.VerifyCheck(stored) {
		return 0, errors.New(secrets.EnvKey + " does not match the key that sealed the stored credentials; nothing changed")
	}
	rekey := func(id, version int64, _, ct []byte) ([]byte, []byte, error) {
		aad := credAAD(id, version)
		sealed, err := secrets.Rekey(oldKey, newKey, ct, aad, aad)
		if err != nil {
			return nil, nil, errors.New("re-encrypt credential failed")
		}
		const nonceAt, nonceLen = 1 + 4, 12 // envelope: version, key id, nonce, ciphertext
		if len(sealed) < nonceAt+nonceLen {
			return nil, nil, errors.New("re-encrypt credential failed")
		}
		return append([]byte(nil), sealed[nonceAt:nonceAt+nonceLen]...), sealed, nil
	}
	ev := store.AuditEvent{Time: now, Source: "cli", ActorKind: "admin", Actor: fmt.Sprintf("uid:%d", os.Getuid()),
		Action: "secrets.rekey", Outcome: "ok"}
	return db.RekeyCredentials(ctx, rekey, newKey.Check(), now, ev)
}
