package egress

import (
	"net/netip"
	"testing"
)

func TestParseRuleValid(t *testing.T) {
	for _, s := range []string{
		"example.com", "Example.COM:8443", "*.golang.org", "*.golang.org:443", "proxy.golang.org.",
		"10.0.0.0/8:5432", "[fd00::/8]:443", "192.168.1.5", "192.168.1.5:22", "[2001:db8::1]:443",
		"localhost:3000", "a_b.example.com",
	} {
		if _, err := ParseRule(s); err != nil {
			t.Errorf("%q: unexpected error %v", s, err)
		}
	}
}

func TestParseRuleInvalid(t *testing.T) {
	for _, s := range []string{
		"", " example.com", "example.com ", "exämple.com", "xn--e1afmkfd.com ", "*.com", "*",
		"foo.*.com", "ex*.com", "example.com:0", "example.com:65536", "example.com:http", "example.com:",
		"10.0.0.0/8", "10.0.0.1/8:443", "10.0.0.0/33:443", "::1", "[::1", "[example.com]:443",
		"2130706433", "-a.com", "a-.com", "a..com", "ex ample.com", "a/b.com:443",
		"[::ffff:1.2.3.4]:443", "http://example.com", "example.com:443:80", "fe80::1%eth0",
	} {
		if _, err := ParseRule(s); err == nil {
			t.Errorf("%q: expected error", s)
		}
	}
}

func TestAllowlistMatching(t *testing.T) {
	a, err := NewAllowlist([]string{"Example.com", "*.golang.org:443", "api.x.io:8080", "10.1.0.0/16:5432"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		host string
		port uint16
		want bool
	}{
		{"example.com", 443, true},
		{"example.com", 80, false},
		{"www.example.com", 443, false},
		{"proxy.golang.org", 443, true},
		{"a.b.golang.org", 443, true},
		{"golang.org", 443, false},
		{"evilgolang.org", 443, false},
		{"proxy.golang.org", 80, false},
		{"api.x.io", 8080, true},
		{"api.x.io", 443, false},
	}
	for _, c := range cases {
		if got := a.AllowsName(c.host, c.port); got != c.want {
			t.Errorf("AllowsName(%s,%d)=%v want %v", c.host, c.port, got, c.want)
		}
	}
	if !a.AllowsIP(netip.MustParseAddr("10.1.2.3"), 5432) {
		t.Error("cidr should allow 10.1.2.3:5432")
	}
	if a.AllowsIP(netip.MustParseAddr("10.1.2.3"), 5433) || a.AllowsIP(netip.MustParseAddr("10.2.0.1"), 5432) {
		t.Error("cidr matched wrong port or range")
	}
}

func TestNilAndEmptyAllowlistDenyAll(t *testing.T) {
	var n *Allowlist
	e, _ := NewAllowlist(nil)
	for _, a := range []*Allowlist{n, e, {}} {
		if a.AllowsName("example.com", 443) || a.AllowsIP(netip.MustParseAddr("8.8.8.8"), 443) || a.Len() != 0 {
			t.Fatal("empty allowlist must deny everything")
		}
	}
}

func TestNewAllowlistRejectsBadRule(t *testing.T) {
	if _, err := NewAllowlist([]string{"ok.com", "bad rule"}); err == nil {
		t.Fatal("expected error")
	}
}
