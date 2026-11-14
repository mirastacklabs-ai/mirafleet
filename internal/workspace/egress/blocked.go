package egress

import "net/netip"

// blockedRange is an address range the proxy refuses. Ranges marked never can not be unlocked
// by any rule (the proxy dials from the runner host's network namespace). The others can be
// unlocked only by a CIDR/IP rule whose prefix lies inside the range.
type blockedRange struct {
	pfx   netip.Prefix
	name  string
	never bool
}

func br(p, name string, never bool) blockedRange {
	return blockedRange{netip.MustParsePrefix(p), name, never}
}

// blockedRanges is ordered: never ranges first, so they win over enclosing unlockable ranges.
var blockedRanges = []blockedRange{
	br("127.0.0.0/8", "loopback", true),
	br("::1/128", "loopback", true),
	br("0.0.0.0/8", "unspecified", true),
	br("::/128", "unspecified", true),
	br("169.254.0.0/16", "link-local/cloud metadata", true),
	br("fe80::/10", "link-local", true),
	br("fd00:ec2::/32", "cloud metadata", true),
	br("100.100.100.200/32", "cloud metadata", true),
	br("192.0.0.192/32", "cloud metadata", true),
	br("::/96", "IPv4-compatible", true), // deprecated; also covers ::/128 and ::1/128 above

	br("10.0.0.0/8", "private", false),
	br("172.16.0.0/12", "private", false),
	br("192.168.0.0/16", "private", false),
	br("fc00::/7", "private", false),
	br("100.64.0.0/10", "reserved/CGNAT", false),
	br("192.0.0.0/24", "reserved/CGNAT", false),
	br("192.0.2.0/24", "reserved/CGNAT", false),
	br("198.18.0.0/15", "reserved/CGNAT", false),
	br("198.51.100.0/24", "reserved/CGNAT", false),
	br("203.0.113.0/24", "reserved/CGNAT", false),
	br("240.0.0.0/4", "reserved/CGNAT", false),
	br("64:ff9b::/96", "reserved/CGNAT", false),
	br("64:ff9b:1::/48", "reserved/CGNAT", false),
	br("100::/64", "reserved/CGNAT", false),
	br("2001::/32", "reserved/CGNAT", false),
	br("2001:db8::/32", "reserved/CGNAT", false),
	br("2002::/16", "reserved/CGNAT", false),
	br("192.88.99.0/24", "reserved/CGNAT", false), // 6to4 relay anycast (deprecated)
	br("fec0::/10", "reserved/CGNAT", false),      // site-local (deprecated)
	br("2001:10::/28", "reserved/CGNAT", false),   // ORCHID (deprecated)
	br("2001:20::/28", "reserved/CGNAT", false),   // ORCHIDv2
	br("2001:2::/48", "reserved/CGNAT", false),    // benchmarking
	br("5f00::/16", "reserved/CGNAT", false),      // SRv6 SIDs
	br("3fff::/20", "reserved/CGNAT", false),      // documentation
	br("224.0.0.0/4", "multicast", false),
	br("ff00::/8", "multicast", false),
}

var (
	nat64a = netip.MustParsePrefix("64:ff9b::/96")
	nat64b = netip.MustParsePrefix("64:ff9b:1::/48")
	v4comp = netip.MustParsePrefix("::/96")
)

// embeddedV4 extracts the IPv4 address embedded in a NAT64 (64:ff9b::/96, 64:ff9b:1::/48) or
// IPv4-compatible (::/96) IPv6 address.
func embeddedV4(ip netip.Addr) (netip.Addr, bool) {
	if !ip.Is6() {
		return netip.Addr{}, false
	}
	b := ip.As16()
	switch {
	case nat64a.Contains(ip), v4comp.Contains(ip):
		return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), true
	case nat64b.Contains(ip):
		// RFC 8215 local-use prefix is /48: the IPv4 sits at bits 48..79 of the address
		// when the prefix is /48 in the RFC 6052 layout (bytes 6,7,9,10 skip byte 8).
		return netip.AddrFrom4([4]byte{b[6], b[7], b[9], b[10]}), true
	}
	return netip.Addr{}, false
}

// rangeOf returns the blocked range containing ip (never ranges first).
func rangeOf(ip netip.Addr) (blockedRange, bool) {
	for _, r := range blockedRanges {
		if r.pfx.Contains(ip) {
			return r, true
		}
	}
	return blockedRange{}, false
}

// checkIP decides whether the proxy may dial ip:port. needRule says the address must be matched
// by a CIDR/IP rule (an IP literal target); resolved addresses in public space need none.
// It returns "" when allowed, otherwise the refusal reason.
func checkIP(ip netip.Addr, port uint16, allow *Allowlist, needRule bool) string {
	ip = ip.Unmap()
	if e, ok := embeddedV4(ip); ok {
		if r, blocked := rangeOf(e); blocked {
			return "embedded IPv4 is " + r.name
		}
	}
	if r, blocked := rangeOf(ip); blocked {
		if r.never {
			return r.name + " address"
		}
		if !allow.unlocks(ip, port, r) {
			return "resolved to " + r.name + " address"
		}
		return ""
	}
	if needRule && !allow.AllowsIP(ip, port) {
		return "IP literal not allowlisted"
	}
	return ""
}

// unlocks reports whether a CIDR/IP rule for port, lying inside blocked range r, contains ip.
func (a *Allowlist) unlocks(ip netip.Addr, port uint16, r blockedRange) bool {
	if a == nil {
		return false
	}
	for _, rule := range a.rules {
		if rule.kind == kindCIDR && rule.port == port && rule.prefix.Contains(ip) &&
			rule.prefix.Bits() >= r.pfx.Bits() && r.pfx.Contains(rule.prefix.Addr()) {
			return true
		}
	}
	return false
}

// checkRulePrefix refuses CIDR/IP rules that sit inside an always-blocked range, overlap one,
// or span more than one blocked class.
func checkRulePrefix(p netip.Prefix) string {
	n := 0
	for _, r := range blockedRanges {
		if !r.pfx.Overlaps(p) {
			continue
		}
		if r.never {
			return "range overlaps " + r.name + " addresses, which can never be allowed"
		}
		n++
	}
	if n > 1 {
		return "range spans more than one blocked address class"
	}
	return ""
}
