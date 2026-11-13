// Package osb is the OpenSandbox client used by the osb workspace executor (docs/M3.md §0, §1.14).
// This file builds the hardened HTTP client: TLS 1.3 only, trust anchored in one configured CA
// (never the system roots), an optional SPKI pin, mandatory client certificate (mTLS), no proxy
// from the environment and no redirects.
package osb

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"
)

// Config configures the transport and the API client.
type Config struct {
	// BaseURL is the https URL of the node-local TLS front, e.g. https://127.0.0.1:8443.
	// It must have no path, query or userinfo.
	BaseURL string
	// CAFile is a PEM file with the node CA. It is the only trust anchor (system roots are never
	// used). Required.
	CAFile string
	// PinSHA256 optionally pins the server leaf: the base64 (std) SHA-256 of its
	// SubjectPublicKeyInfo. It is checked in addition to chain verification. A pin alone is not
	// accepted: Go cannot verify a chain without a trust anchor except via InsecureSkipVerify,
	// which this package never uses. Point CAFile at the self-signed certificate if need be.
	PinSHA256 string
	// CertFile and KeyFile are the client certificate and key (PEM). Required (mTLS).
	CertFile string
	KeyFile  string
	// APIKeyFile holds the OPEN-SANDBOX-API-KEY value. Read once by NewClient.
	APIKeyFile string
	// CallTimeout bounds every non-streaming call. Default 60 s.
	CallTimeout time.Duration
}

const (
	defaultCallTimeout = 60 * time.Second
	dialTimeout        = 10 * time.Second
	tlsHandshakeTO     = 10 * time.Second
	responseHeaderTO   = 60 * time.Second
)

// ErrRedirect is returned (wrapped in a *url.Error) when the server answers with a redirect.
var ErrRedirect = errors.New("osb: redirects are refused")

// parseBase validates the configured base URL and returns it.
func parseBase(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("osb: invalid base URL")
	}
	if u.Scheme != "https" {
		return nil, errors.New("osb: base URL must use https")
	}
	if u.Host == "" || u.Hostname() == "" {
		return nil, errors.New("osb: base URL has no host")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("osb: base URL must be scheme://host[:port] only")
	}
	return &url.URL{Scheme: "https", Host: u.Host}, nil
}

// decodePin decodes a base64 SHA-256 pin.
func decodePin(p string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(p)
	if err != nil || len(b) != sha256.Size {
		return nil, errors.New("osb: pin_sha256 must be the base64 of a SHA-256 digest")
	}
	return b, nil
}

// SPKIPin returns the pin string (base64 SHA-256 of the SubjectPublicKeyInfo) for a certificate.
func SPKIPin(c *x509.Certificate) string {
	sum := sha256.Sum256(c.RawSubjectPublicKeyInfo)
	return base64.StdEncoding.EncodeToString(sum[:])
}

func tlsConfig(cfg Config, host string) (*tls.Config, error) {
	if cfg.CAFile == "" {
		return nil, errors.New("osb: ca_file is required (system roots are never used)")
	}
	if cfg.CertFile == "" || cfg.KeyFile == "" {
		return nil, errors.New("osb: client certificate and key are required (mTLS)")
	}
	pem, err := os.ReadFile(cfg.CAFile)
	if err != nil {
		return nil, fmt.Errorf("osb: read ca_file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("osb: ca_file contains no PEM certificate")
	}
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("osb: load client certificate: %w", err)
	}
	var pin []byte
	if cfg.PinSHA256 != "" {
		if pin, err = decodePin(cfg.PinSHA256); err != nil {
			return nil, err
		}
	}
	tc := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
		RootCAs:      pool,
		Certificates: []tls.Certificate{cert},
		ServerName:   host,
	}
	if pin != nil {
		tc.VerifyConnection = func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("osb: server presented no certificate")
			}
			sum := sha256.Sum256(cs.PeerCertificates[0].RawSubjectPublicKeyInfo)
			if subtle.ConstantTimeCompare(sum[:], pin) != 1 {
				return errors.New("osb: server certificate does not match the configured pin")
			}
			return nil
		}
	}
	return tc, nil
}

// NewHTTPClient returns the hardened client for cfg. It has no overall Timeout (SSE streams are
// long-lived); callers bound non-streaming calls with contexts.
func NewHTTPClient(cfg Config) (*http.Client, error) {
	base, err := parseBase(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	tc, err := tlsConfig(cfg, base.Hostname())
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{
		Proxy:                 nil, // never honour HTTP(S)_PROXY from the environment
		DialContext:           (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       tc,
		TLSHandshakeTimeout:   tlsHandshakeTO,
		ResponseHeaderTimeout: responseHeaderTO,
		IdleConnTimeout:       60 * time.Second,
		MaxIdleConnsPerHost:   4,
		ForceAttemptHTTP2:     false,
		DisableCompression:    true,
	}
	return &http.Client{
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return ErrRedirect
		},
	}, nil
}
