package egress

import (
	"net/netip"
	"testing"
)

func TestParseRuleValid(t *testing.T) {
	for _, s := range []string{
		"example.com", "Example.COM:8443", "*.golang.org", "*.golang.org:443", "proxy.golang.org.",
		"10.0.0.0/8:5432", "[fd12::/16]:443", "192.168.1.5", "192.168.1.5:22", "[2001:db8::1]:443",
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

func TestParseRuleBlockedRanges(t *testing.T) {
	ok := []string{
		"10.1.2.0/24:443", "10.1.2.3:80", "172.16.0.0/12:443", "192.168.0.0/16:22", "100.64.0.0/11:443",
		"[fd12::/16]:443", "8.8.8.0/24:443", "[2606:4700::/32]:443", "[64:ff9b::/96]:443",
	}
	for _, s := range ok {
		if _, err := ParseRule(s); err != nil {
			t.Errorf("%q: unexpected error %v", s, err)
		}
	}
	bad := []string{
		"0.0.0.0/0:443", "[::/0]:443", "0.0.0.0/1:443", "192.0.0.0/8:443", "192.0.0.0/7:443", "128.0.0.0/1:443", "[fc00::/6]:443", "[fe00::/7]:443",
		"127.0.0.0/8:80", "127.0.0.1:80", "[::1]:80", "0.0.0.0:80", "[::]:80", "0.0.0.0/8:80",
		"169.254.0.0/16:80", "169.254.169.254:80", "[fe80::/10]:443", "[fe80::1]:443",
		"[fd00:ec2::254]:80", "[fd00::/8]:443", "100.100.100.200:80", "192.0.0.192:80",
		"100.64.0.0/10:443", "192.0.0.0/24:443", "192.0.0.192/26:443",
		"[::ffff:10.0.0.1]:443", "[::/96]:443",
	}
	for _, s := range bad {
		if _, err := ParseRule(s); err == nil {
			t.Errorf("%q: expected error", s)
		}
	}
}

func TestCheckIPNeverAndEmbedded(t *testing.T) {
	a, err := NewAllowlist([]string{"10.1.2.0/24:443", "[64:ff9b::/96]:443", "8.8.8.8:443"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		ip      string
		literal bool
		deny    bool
	}{
		{"127.0.0.1", true, true}, {"::1", false, true}, {"0.0.0.0", false, true}, {"::", true, true},
		{"169.254.0.1", false, true}, {"169.254.169.254", true, true}, {"fe80::1", false, true},
		{"fd00:ec2::254", false, true}, {"100.100.100.200", false, true}, {"192.0.0.192", false, true},
		{"10.1.2.9", true, false}, {"10.1.3.9", true, true}, {"10.1.3.9", false, true},
		{"8.8.8.8", true, false}, {"1.1.1.1", true, true}, {"1.1.1.1", false, false},
		// NAT64 and IPv4-compatible forms embedding blocked IPv4 are denied, even under a covering rule.
		{"64:ff9b::7f00:1", true, true}, {"64:ff9b::a01:209", true, true}, {"64:ff9b::a9fe:a9fe", false, true},
		{"64:ff9b:1:a00:0:100::", false, true}, // 64:ff9b:1::/48 embedding 10.0.0.1
		{"::7f00:1", false, true}, {"::a00:1", false, true}, {"::a9fe:a9fe", false, true},
		{"::ffff:127.0.0.1", false, true},
	}
	for _, c := range cases {
		got := checkIP(netip.MustParseAddr(c.ip), 443, a, c.literal)
		if (got != "") != c.deny {
			t.Errorf("%s literal=%v: reason %q, want deny=%v", c.ip, c.literal, got, c.deny)
		}
	}
}

func TestBroadRuleDoesNotUnlock(t *testing.T) {
	// Even a (hypothetical) broad prefix can not reach loopback: rules must lie inside the range.
	a := &Allowlist{rules: []Rule{{kind: kindCIDR, prefix: netip.MustParsePrefix("0.0.0.0/0"), port: 443}}}
	for _, ip := range []string{"127.0.0.1", "0.0.0.0", "169.254.1.1", "10.0.0.1", "100.64.0.1"} {
		if checkIP(netip.MustParseAddr(ip), 443, a, true) == "" {
			t.Errorf("%s was unlocked by 0.0.0.0/0", ip)
		}
	}
	if checkIP(netip.MustParseAddr("8.8.4.4"), 443, a, true) != "" {
		t.Error("public literal should pass")
	}
}
