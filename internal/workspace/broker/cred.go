// Package broker is the per-session HTTPS credential broker: a local HTTP CONNECT proxy that
// terminates TLS (with a per-session CA) only for hosts that have a credential, injects the
// credential, and blind-tunnels every other host. Secrets live in memory only and are never
// logged or returned to the client.
package broker

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"regexp"
	"strings"
	"sync"
)

// Injection kinds.
const (
	Basic    = "basic"    // Authorization: Basic base64(User:Secret)
	Bearer   = "bearer"   // Authorization: Bearer Secret
	Header   = "header"   // HeaderName: Secret (e.g. PRIVATE-TOKEN)
	Registry = "registry" // OCI token exchange with User:Secret; Authorization: Bearer <token>
)

var headerNameRE = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]{1,128}$")

// Credential is one secret and where it applies. Hosts are "host" or "host:port" (default 443).
type Credential struct {
	Name       string
	Kind       string
	Hosts      []string
	User       string // basic, registry
	HeaderName string // header
	Secret     string
}

// Validate checks the credential's shape.
func (c Credential) Validate() error {
	if c.Name == "" || len(c.Hosts) == 0 || c.Secret == "" {
		return errors.New("broker: credential needs name, hosts and secret")
	}
	for _, h := range c.Hosts {
		if _, _, err := normHost(h); err != nil {
			return err
		}
	}
	switch c.Kind {
	case Basic, Registry:
		if c.User == "" {
			return fmt.Errorf("broker: credential %q kind %s needs a user", c.Name, c.Kind)
		}
	case Bearer:
	case Header:
		if !headerNameRE.MatchString(c.HeaderName) {
			return fmt.Errorf("broker: credential %q has an invalid header name", c.Name)
		}
	default:
		return fmt.Errorf("broker: credential %q has unknown kind", c.Name)
	}
	return nil
}

// Redacting formatters: a Credential printed or logged never shows the secret.
func (c Credential) String() string   { return "credential(" + c.Name + ")" }
func (c Credential) GoString() string { return c.String() }
func (c Credential) LogValue() slog.Value {
	return slog.StringValue(c.String())
}

// Provider supplies credentials from memory. Hosts is read once, when the broker (and its
// NameConstraints-limited CA) is created; Lookup is called per request so removed or replaced
// credentials take effect at once.
type Provider interface {
	Hosts() []string
	Lookup(host, port string) (Credential, bool)
}

// MapProvider is an atomically replaceable in-memory Provider.
type MapProvider struct {
	mu    sync.RWMutex
	creds []Credential
}

// NewMapProvider returns a provider holding creds.
func NewMapProvider(creds ...Credential) *MapProvider {
	p := &MapProvider{}
	p.Replace(creds)
	return p
}

// Replace swaps the whole set.
func (p *MapProvider) Replace(creds []Credential) {
	p.mu.Lock()
	p.creds = append([]Credential(nil), creds...)
	p.mu.Unlock()
}

// Hosts lists every credential host (normalised host names without ports).
func (p *MapProvider) Hosts() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var out []string
	for _, c := range p.creds {
		for _, h := range c.Hosts {
			if hh, _, err := normHost(h); err == nil {
				out = append(out, hh)
			}
		}
	}
	return out
}

// Lookup finds the credential for host:port.
func (p *MapProvider) Lookup(host, port string) (Credential, bool) {
	host = strings.ToLower(host)
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, c := range p.creds {
		for _, h := range c.Hosts {
			if hh, pp, err := normHost(h); err == nil && hh == host && pp == port {
				return c, true
			}
		}
	}
	return Credential{}, false
}

// normHost splits "host[:port]" into a lower-case host and a port (default "443").
func normHost(s string) (string, string, error) {
	host, port := s, "443"
	if h, p, err := net.SplitHostPort(s); err == nil {
		host, port = h, p
	} else if strings.Contains(s, ":") && !strings.HasPrefix(s, "[") && net.ParseIP(s) == nil {
		return "", "", fmt.Errorf("broker: bad host %q", s)
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "" || port == "" || strings.ContainsAny(host, " /@?#*") {
		return "", "", fmt.Errorf("broker: bad host %q", s)
	}
	return host, port, nil
}
