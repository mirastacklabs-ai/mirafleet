package osb

// Per-node PKI for the TLS front (`mirafleet runner tls-init`): a private CA, the front's server
// certificate and the runner's client certificate, all ECDSA P-256. The CA private key is used to
// sign the two leaves and then discarded (never written): nothing on the node can mint more client
// certificates. To rotate, run tls-init into a fresh directory and update workspace.yaml.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// File names written by InitPKI.
const (
	PKICAFile         = "ca.pem"
	PKIFrontCertFile  = "front.pem"
	PKIFrontKeyFile   = "front-key.pem"
	PKIClientCertFile = "client.pem"
	PKIClientKeyFile  = "client-key.pem"
)

// PKI describes what InitPKI wrote.
type PKI struct {
	Dir         string
	CAFile      string
	FrontCert   string
	FrontKey    string
	ClientCert  string
	ClientKey   string
	FrontPinSHA string // hex SHA-256 of the front certificate's SPKI (workspace.yaml pin_sha256)
	NotAfter    time.Time
}

// InitPKI mints the node PKI into dir, which must exist with mode 0700 (no group/other access).
// No existing file is overwritten. days is the validity of all three certificates (1..3650).
func InitPKI(dir string, days int, now time.Time) (*PKI, error) {
	if days < 1 || days > 3650 {
		return nil, fmt.Errorf("days %d out of range (1..3650)", days)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s: mode %04o is too open (want 0700)", dir, fi.Mode().Perm())
	}
	notBefore := now.Add(-5 * time.Minute)
	notAfter := now.Add(time.Duration(days) * 24 * time.Hour)

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	caTmpl := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: "mirafleet node CA", Organization: []string{"mirafleet"}},
		NotBefore: notBefore, NotAfter: notAfter,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign, BasicConstraintsValid: true,
		IsCA: true, MaxPathLen: 0, MaxPathLenZero: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, err
	}

	leaf := func(cn string, eku x509.ExtKeyUsage, dns []string, ips []net.IP) (certDER []byte, keyDER []byte, err error) {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, nil, err
		}
		t := &x509.Certificate{
			SerialNumber: serial(), Subject: pkix.Name{CommonName: cn, Organization: []string{"mirafleet"}},
			NotBefore: notBefore, NotAfter: notAfter,
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{eku},
			BasicConstraintsValid: true, DNSNames: dns, IPAddresses: ips,
		}
		certDER, err = x509.CreateCertificate(rand.Reader, t, caCert, &k.PublicKey, caKey)
		if err != nil {
			return nil, nil, err
		}
		keyDER, err = x509.MarshalPKCS8PrivateKey(k)
		return certDER, keyDER, err
	}
	srvDER, srvKey, err := leaf("mirafleet-tls-front", x509.ExtKeyUsageServerAuth,
		[]string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")})
	if err != nil {
		return nil, err
	}
	cliDER, cliKey, err := leaf("mirafleet-runner", x509.ExtKeyUsageClientAuth, nil, nil)
	if err != nil {
		return nil, err
	}
	srvCert, err := x509.ParseCertificate(srvDER)
	if err != nil {
		return nil, err
	}

	p := &PKI{
		Dir: dir, CAFile: filepath.Join(dir, PKICAFile),
		FrontCert: filepath.Join(dir, PKIFrontCertFile), FrontKey: filepath.Join(dir, PKIFrontKeyFile),
		ClientCert: filepath.Join(dir, PKIClientCertFile), ClientKey: filepath.Join(dir, PKIClientKeyFile),
		FrontPinSHA: SPKIPin(srvCert), NotAfter: notAfter,
	}
	files := []struct {
		path, typ string
		der       []byte
	}{
		{p.CAFile, "CERTIFICATE", caDER}, {p.FrontCert, "CERTIFICATE", srvDER}, {p.FrontKey, "PRIVATE KEY", srvKey},
		{p.ClientCert, "CERTIFICATE", cliDER}, {p.ClientKey, "PRIVATE KEY", cliKey},
	}
	for _, f := range files { // refuse before writing anything
		if _, err := os.Lstat(f.path); err == nil {
			return nil, fmt.Errorf("%s already exists: use an empty directory (nothing was written)", f.path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	var written []string
	for _, f := range files {
		if err := writeNew0600(f.path, pem.EncodeToMemory(&pem.Block{Type: f.typ, Bytes: f.der})); err != nil {
			for _, w := range written {
				os.Remove(w)
			}
			return nil, err
		}
		written = append(written, f.path)
	}
	return p, nil
}

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		panic(err) // crypto/rand failing is unrecoverable
	}
	return n.Add(n, big.NewInt(1))
}

// writeNew0600 creates path exclusively with mode 0600.
func writeNew0600(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	return f.Close()
}
