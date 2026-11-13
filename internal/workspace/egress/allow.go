// Package egress implements the workspace egress proxy: a stdlib HTTP/CONNECT forward proxy
// that enforces a host:port allowlist, does its own DNS and refuses internal address ranges.
package egress

import (
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// DefaultPort is applied to host and wildcard rules written without a port.
const DefaultPort = 443

type ruleKind int

const (
	kindHost ruleKind = iota
	kindWildcard
	kindCIDR
)

// Rule is one parsed allowlist entry.
//
// Accepted forms (ASCII only, case-insensitive hostnames, no IDNA/punycode conversion):
//
//	host[:port]          exact hostname, port defaults to 443
//	*.domain[:port]      any subdomain (not the apex), at least two labels after "*.", port defaults to 443
//	cidr:port            e.g. 10.1.0.0/16:5432 or [fd00::/8]:443; the port is REQUIRED
//	ip[:port]            IP literal, shorthand for ip/32 (ip/128); port defaults to 443
//
// IPv6 literals and CIDRs must be bracketed: [2001:db8::1]:443, [2001:db8::/32]:443.
// A cidr or IP rule is the only way to reach an otherwise refused range (private, loopback ...)
// and the only way to dial an IP-literal target. Cloud metadata addresses can never be allowed.
type Rule struct {
	Raw    string
	kind   ruleKind
	host   string // exact name (kindHost) or suffix without the "*." (kindWildcard)
	prefix netip.Prefix
	port   uint16
}

// ParseRule parses and strictly validates one allowlist entry.
func ParseRule(s string) (Rule, error) {
	r := Rule{Raw: s}
	if s == "" {
		return r, errors.New("empty rule")
	}
	if s != strings.TrimSpace(s) {
		return r, errors.New("surrounding whitespace")
	}
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return r, errors.New("non-ASCII characters are not supported (no IDNA)")
		}
		if s[i] < 0x21 || s[i] == 0x7f {
			return r, errors.New("control or space character")
		}
	}

	var hostPart, portPart string
	var hasPort, bracketed bool
	if s[0] == '[' {
		end := strings.IndexByte(s, ']')
		if end < 0 {
			return r, errors.New("missing ]")
		}
		bracketed = true
		hostPart = s[1:end]
		if rest := s[end+1:]; rest != "" {
			if rest[0] != ':' {
				return r, errors.New("garbage after ]")
			}
			portPart, hasPort = rest[1:], true
		}
		if !strings.Contains(hostPart, ":") {
			return r, errors.New("brackets are only for IPv6 literals and CIDRs")
		}
	} else {
		if strings.Count(s, ":") > 1 {
			return r, errors.New("IPv6 needs brackets")
		}
		if i := strings.IndexByte(s, ':'); i >= 0 {
			hostPart, portPart, hasPort = s[:i], s[i+1:], true
		} else {
			hostPart = s
		}
	}
	if hostPart == "" {
		return r, errors.New("empty host")
	}
	if hasPort {
		p, err := parsePort(portPart)
		if err != nil {
			return r, err
		}
		r.port = p
	} else {
		r.port = DefaultPort
	}

	switch {
	case strings.Contains(hostPart, "/"):
		pfx, err := netip.ParsePrefix(hostPart)
		if err != nil {
			return r, fmt.Errorf("bad CIDR: %w", err)
		}
		if pfx.Addr().Zone() != "" || pfx.Addr().Is4In6() {
			return r, errors.New("zoned or IPv4-mapped CIDR not allowed")
		}
		if pfx != pfx.Masked() {
			return r, fmt.Errorf("CIDR has host bits set (want %s)", pfx.Masked())
		}
		if !hasPort {
			return r, errors.New("a CIDR rule requires an explicit port")
		}
		r.kind, r.prefix = kindCIDR, pfx
	default:
		if ip, err := netip.ParseAddr(hostPart); err == nil {
			if ip.Zone() != "" || ip.Is4In6() {
				return r, errors.New("zoned or IPv4-mapped address not allowed")
			}
			if ip.Is6() && !bracketed {
				return r, errors.New("IPv6 needs brackets")
			}
			r.kind, r.prefix = kindCIDR, netip.PrefixFrom(ip, ip.BitLen())
			break
		}
		if bracketed {
			return r, errors.New("bad IPv6 literal")
		}
		if rest, ok := strings.CutPrefix(strings.ToLower(hostPart), "*."); ok {
			name, err := normalizeHost(rest)
			if err != nil {
				return r, err
			}
			if !strings.Contains(name, ".") {
				return r, errors.New("wildcard needs at least two labels after \"*.\"")
			}
			r.kind, r.host = kindWildcard, name
			break
		}
		if strings.Contains(hostPart, "*") {
			return r, errors.New("wildcard only allowed as a leading \"*.\"")
		}
		name, err := normalizeHost(hostPart)
		if err != nil {
			return r, err
		}
		r.kind, r.host = kindHost, name
	}
	return r, nil
}

func parsePort(s string) (uint16, error) {
	if s == "" {
		return 0, errors.New("empty port")
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, errors.New("port must be decimal digits")
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 {
		return 0, errors.New("port out of range 1-65535")
	}
	return uint16(n), nil
}

// normalizeHost lower-cases a DNS name, drops one trailing dot and validates it as an
// LDH hostname (underscore tolerated). The last label may not be all digits, which also
// rejects shorthand IPv4 spellings such as 2130706433 or 127.1.
func normalizeHost(h string) (string, error) {
	h = strings.ToLower(h)
	h = strings.TrimSuffix(h, ".")
	if h == "" || len(h) > 253 {
		return "", errors.New("bad hostname length")
	}
	labels := strings.Split(h, ".")
	for _, l := range labels {
		if l == "" || len(l) > 63 {
			return "", errors.New("bad hostname label length")
		}
		if l[0] == '-' || l[len(l)-1] == '-' {
			return "", errors.New("hostname label starts or ends with '-'")
		}
		for i := 0; i < len(l); i++ {
			c := l[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return "", errors.New("invalid character in hostname")
			}
		}
	}
	last := labels[len(labels)-1]
	allDigits := true
	for i := 0; i < len(last); i++ {
		if last[i] < '0' || last[i] > '9' {
			allDigits = false
			break
		}
	}
	if allDigits {
		return "", errors.New("hostname looks like a numeric address")
	}
	return h, nil
}

// Allowlist is an immutable set of rules. The zero value and nil deny everything.
type Allowlist struct{ rules []Rule }

// NewAllowlist parses all rules; the first invalid rule fails the whole list.
func NewAllowlist(rules []string) (*Allowlist, error) {
	a := &Allowlist{}
	for _, s := range rules {
		r, err := ParseRule(s)
		if err != nil {
			return nil, fmt.Errorf("egress rule %q: %w", s, err)
		}
		a.rules = append(a.rules, r)
	}
	return a, nil
}

// Len returns the number of rules.
func (a *Allowlist) Len() int {
	if a == nil {
		return 0
	}
	return len(a.rules)
}

// AllowsName reports whether a hostname rule permits host:port. host must already be
// normalized (see normalizeHost); IP literals never match name rules.
func (a *Allowlist) AllowsName(host string, port uint16) bool {
	if a == nil {
		return false
	}
	for _, r := range a.rules {
		if r.port != port {
			continue
		}
		switch r.kind {
		case kindHost:
			if host == r.host {
				return true
			}
		case kindWildcard:
			if len(host) > len(r.host)+1 && strings.HasSuffix(host, "."+r.host) {
				return true
			}
		}
	}
	return false
}

// AllowsIP reports whether a CIDR/IP rule explicitly permits ip:port.
func (a *Allowlist) AllowsIP(ip netip.Addr, port uint16) bool {
	if a == nil {
		return false
	}
	ip = ip.Unmap()
	for _, r := range a.rules {
		if r.kind == kindCIDR && r.port == port && r.prefix.Contains(ip) {
			return true
		}
	}
	return false
}
