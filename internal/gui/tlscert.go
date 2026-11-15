package gui

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"net/netip"
	"os"
	"sort"
	"strings"
	"time"
)

const (
	selfSignedValidity = 2 * 365 * 24 * time.Hour
	selfSignedRenewAt  = 30 * 24 * time.Hour
	guiCertDir         = "gui"
	guiCertFile        = "tls.crt"
	guiKeyFile         = "tls.key"
)

// certInfo is the certificate the GUI serves.
type certInfo struct {
	cert        tls.Certificate
	Fingerprint string // SHA-256 of the leaf DER, upper-case hex with colons
	NotAfter    time.Time
	Operator    bool     // supplied by the operator (-gui-cert/-gui-key), not generated
	DNSNames    []string // DNS SANs of the leaf
}

// Fingerprint returns the SHA-256 fingerprint of a DER certificate as "AB:CD:...".
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}

func newCertInfo(cert tls.Certificate, operator bool) (*certInfo, error) {
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, err
	}
	cert.Leaf = leaf
	return &certInfo{cert: cert, Fingerprint: Fingerprint(leaf.Raw), NotAfter: leaf.NotAfter,
		Operator: operator, DNSNames: append([]string(nil), leaf.DNSNames...)}, nil
}

// loadOperatorCert loads an operator-supplied PEM certificate and key. The key file must not be
// accessible to group or others and must be owned by this process's user (checked on the open
// file, so the check and the read cannot be separated by a swap).
func loadOperatorCert(certFile, keyFile string) (*certInfo, error) {
	kf, err := os.Open(keyFile)
	if err != nil {
		return nil, fmt.Errorf("gui: -gui-key: %w", err)
	}
	defer kf.Close()
	st, err := kf.Stat()
	if err != nil {
		return nil, fmt.Errorf("gui: -gui-key: %w", err)
	}
	if err := checkKeyFile(keyFile, st); err != nil {
		return nil, err
	}
	keyPEM, err := io.ReadAll(io.LimitReader(kf, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("gui: -gui-key: %w", err)
	}
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		return nil, fmt.Errorf("gui: -gui-cert: %w", err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("gui: -gui-cert/-gui-key: %w", err)
	}
	return newCertInfo(pair, true)
}

// selfSignedNames returns the sorted, de-duplicated lower-case DNS names for the generated
// certificate: localhost, the short and full host name and every -gui-host name.
func selfSignedNames(hostname string, guiHosts []string) []string {
	set := map[string]bool{"localhost": true}
	if h := normHost(hostname); h != "" {
		set[h] = true
		if i := strings.IndexByte(h, '.'); i > 0 {
			set[h[:i]] = true
		}
	}
	for _, n := range guiHosts {
		if n = normHost(n); n != "" {
			set[n] = true
		}
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ensureSelfSigned loads <dataDir>/gui/tls.crt|tls.key or, when absent, unreadable, mismatched,
// loose in permissions, within 30 days of expiry, or carrying a different DNS name set than
// dns, generates a new ECDSA P-256 certificate valid for two years. ips are added as IP SANs
// (with the loopbacks). All file access goes through os.Root on the data directory.
// regenerated reports whether a new certificate was written.
func ensureSelfSigned(dataDir string, dns []string, ips []netip.Addr, now time.Time) (ci *certInfo, regenerated bool, err error) {
	root, err := os.OpenRoot(dataDir)
	if err != nil {
		return nil, false, fmt.Errorf("gui: data directory: %w", err)
	}
	defer root.Close()
	if err := root.Mkdir(guiCertDir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, false, fmt.Errorf("gui: %s directory: %w", guiCertDir, err)
	}
	// An existing directory must be ours and closed to group and others: the key lives in it.
	dst, err := root.Lstat(guiCertDir)
	if err != nil {
		return nil, false, fmt.Errorf("gui: %s directory: %w", guiCertDir, err)
	}
	if !dst.IsDir() || dst.Mode().Perm()&0o077 != 0 || !ownedByUs(dst) {
		return nil, false, fmt.Errorf("gui: %s/%s must be a directory owned by this user with mode 0700 (chmod 700)", dataDir, guiCertDir)
	}
	dir, err := root.OpenRoot(guiCertDir)
	if err != nil {
		return nil, false, fmt.Errorf("gui: %s directory: %w", guiCertDir, err)
	}
	defer dir.Close()

	if existing := loadExistingSelfSigned(dir, dns, now); existing != nil {
		return existing, false, nil
	}
	certPEM, keyPEM, err := generateSelfSigned(dns, ips, now)
	if err != nil {
		return nil, false, err
	}
	// Key first: a crash between the two writes leaves a mismatched pair, which the next start
	// detects and replaces.
	if err := writeRootFile(dir, guiKeyFile, keyPEM); err != nil {
		return nil, false, err
	}
	if err := writeRootFile(dir, guiCertFile, certPEM); err != nil {
		return nil, false, err
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, false, err
	}
	ci, err = newCertInfo(pair, false)
	return ci, true, err
}

func readRootFile(dir *os.Root, name string) ([]byte, os.FileInfo, error) {
	f, err := dir.Open(name)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	b, err := io.ReadAll(io.LimitReader(f, 1<<20))
	return b, st, err
}

func loadExistingSelfSigned(dir *os.Root, dns []string, now time.Time) *certInfo {
	keyPEM, kst, err := readRootFile(dir, guiKeyFile)
	if err != nil || kst.Mode().Perm()&0o077 != 0 || !ownedByUs(kst) {
		return nil
	}
	certPEM, _, err := readRootFile(dir, guiCertFile)
	if err != nil {
		return nil
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil
	}
	ci, err := newCertInfo(pair, false)
	if err != nil || !ci.NotAfter.After(now.Add(selfSignedRenewAt)) {
		return nil
	}
	have := append([]string(nil), ci.DNSNames...)
	sort.Strings(have)
	want := append([]string(nil), dns...)
	sort.Strings(want)
	if strings.Join(have, ",") != strings.Join(want, ",") {
		return nil
	}
	return ci
}

func generateSelfSigned(dns []string, ips []netip.Addr, now time.Time) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "mirafleet central", Organization: []string{"mirafleet"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(selfSignedValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dns,
	}
	seen := map[netip.Addr]bool{}
	for _, a := range append([]netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("::1")}, ips...) {
		if a = a.Unmap(); !seen[a] && (a.IsLoopback() || usableIP(a)) {
			seen[a] = true
			tmpl.IPAddresses = append(tmpl.IPAddresses, a.AsSlice())
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
}

// writeRootFile writes name atomically (0600, temp file + rename) inside dir.
func writeRootFile(dir *os.Root, name string, data []byte) error {
	tmp := name + ".tmp"
	_ = dir.Remove(tmp)
	f, err := dir.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("gui: write %s: %w", name, err)
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = dir.Rename(tmp, name)
	}
	if err != nil {
		_ = dir.Remove(tmp)
		return fmt.Errorf("gui: write %s: %w", name, err)
	}
	return nil
}
