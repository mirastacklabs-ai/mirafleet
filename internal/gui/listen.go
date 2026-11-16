package gui

import (
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultListen is the GUI bind address.
const DefaultListen = "0.0.0.0:8443"

// DefaultHostRefresh is how often the interface addresses behind the Host allowlist are re-read.
const DefaultHostRefresh = 60 * time.Second

// InterfaceLister returns the addresses of this machine's up interfaces. It is injected in tests.
type InterfaceLister func() ([]netip.Addr, error)

// SystemInterfaces lists the addresses of every up interface via net.Interfaces.
func SystemInterfaces() ([]netip.Addr, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []netip.Addr
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if addr, ok := netip.AddrFromSlice(ip); ok {
				out = append(out, addr.Unmap())
			}
		}
	}
	return out, nil
}

// usableIP reports whether a is an address the allowlist and the certificate may carry: not
// unspecified, link-local or multicast, and without a zone.
func usableIP(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsValid() && a.Zone() == "" && !a.IsUnspecified() && !a.IsLinkLocalUnicast() &&
		!a.IsLinkLocalMulticast() && !a.IsMulticast()
}

// ownIPs returns the sorted, de-duplicated usable addresses of the machine plus the loopbacks.
func ownIPs(list InterfaceLister) ([]netip.Addr, error) {
	set := map[netip.Addr]bool{
		netip.MustParseAddr("127.0.0.1"): true,
		netip.MustParseAddr("::1"):       true,
	}
	addrs, err := list()
	for _, a := range addrs {
		if a = a.Unmap(); usableIP(a) {
			set[a] = true
		}
	}
	out := make([]netip.Addr, 0, len(set))
	for a := range set {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Less(out[j]) })
	return out, err
}

// hostGuard is the Host header allowlist: IP literals only when they are this
// machine's own addresses, names only when listed exactly. Anything else, and a missing Host,
// is refused.
type hostGuard struct {
	names map[string]bool
	list  InterfaceLister
	logf  func(string, ...any)

	mu  sync.RWMutex
	ips map[netip.Addr]bool
}

func newHostGuard(names []string, list InterfaceLister, logf func(string, ...any)) *hostGuard {
	g := &hostGuard{names: map[string]bool{}, list: list, logf: logf}
	for _, n := range names {
		if n = normHost(n); n != "" {
			g.names[n] = true
		}
	}
	g.refresh()
	return g
}

// normHost lower-cases a name and trims one trailing dot.
func normHost(n string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(n)), ".")
}

// refresh re-reads the interface addresses. A failed read keeps the previous set; the set only
// ever holds addresses the machine reports (plus the loopbacks), never anything a client sent.
func (g *hostGuard) refresh() {
	ips, err := ownIPs(g.list)
	if err != nil {
		g.logf("gui: reading interface addresses: %v", err)
		g.mu.RLock()
		have := g.ips != nil
		g.mu.RUnlock()
		if have {
			return
		}
	}
	set := make(map[netip.Addr]bool, len(ips))
	for _, a := range ips {
		set[a] = true
	}
	g.mu.Lock()
	g.ips = set
	g.mu.Unlock()
}

func (g *hostGuard) ownIP(a netip.Addr) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.ips[a.Unmap()]
}

func validPort(p string) bool {
	if p == "" || len(p) > 5 {
		return false
	}
	for _, c := range p {
		if c < '0' || c > '9' {
			return false
		}
	}
	n, _ := strconv.Atoi(p)
	return n <= 65535
}

// allowed reports whether a Host header value is accepted. Any port is accepted.
func (g *hostGuard) allowed(host string) bool {
	if host == "" {
		return false
	}
	if strings.HasPrefix(host, "[") {
		end := strings.IndexByte(host, ']')
		if end < 0 {
			return false
		}
		if rest := host[end+1:]; rest != "" && (rest[0] != ':' || !validPort(rest[1:])) {
			return false
		}
		a, err := netip.ParseAddr(host[1:end])
		if err != nil || !a.Is6() || a.Is4In6() || a.Zone() != "" {
			return false
		}
		return g.ownIP(a)
	}
	name := host
	if strings.Count(host, ":") > 1 {
		return false // an IPv6 literal must be bracketed
	}
	if i := strings.IndexByte(host, ':'); i >= 0 {
		if !validPort(host[i+1:]) {
			return false
		}
		name = host[:i]
	}
	if name == "" {
		return false
	}
	if a, err := netip.ParseAddr(name); err == nil {
		return a.Is4() && g.ownIP(a)
	}
	return g.names[normHost(name)]
}

// hostNames returns the allowlisted names, sorted (for tests and the startup log).
func (g *hostGuard) hostNames() []string {
	out := make([]string, 0, len(g.names))
	for n := range g.names {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// isWildcard reports whether host in a listen address means every interface.
func isWildcard(host string) bool {
	if host == "" {
		return true
	}
	a, err := netip.ParseAddr(host)
	return err == nil && a.IsUnspecified()
}

// isLoopbackHost reports whether host is a loopback literal or "localhost".
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	a, err := netip.ParseAddr(host)
	return err == nil && a.IsLoopback()
}

// bindNotice is the "reachable beyond this machine" warning, or "" for a
// loopback bind. addr is the bound ip:port.
func bindNotice(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || isLoopbackHost(host) {
		return ""
	}
	where := "the network address " + addr
	if isWildcard(host) {
		where = "all interfaces (" + addr + ")"
	}
	return fmt.Sprintf("GUI is reachable on %s. To restrict it: -gui-listen 127.0.0.1:%s (then use ssh -L), -gui-listen off, or a firewall rule for port %s.",
		where, port, port)
}

// fingerprintNotice is the self-signed certificate warning.
func fingerprintNotice(fp string) string {
	return fmt.Sprintf("Check that your browser's certificate fingerprint matches %s before you type the setup token or your password.", fp)
}

// guiURLs lists the URLs the GUI answers on for a bound address, bounded for the log.
func guiURLs(addr string, own []netip.Addr, hostnames []string) []string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil
	}
	mk := func(h string) string { return "https://" + net.JoinHostPort(h, port) + "/" }
	if !isWildcard(host) {
		return []string{mk(host)}
	}
	out := []string{mk("localhost")}
	// An IPv4 wildcard bind does not answer on IPv6 addresses.
	v4only := false
	if w, err := netip.ParseAddr(host); err == nil && w.Unmap().Is4() {
		v4only = true
	}
	for _, a := range own {
		if a.IsLoopback() || len(out) >= 8 || (v4only && !a.Unmap().Is4()) {
			continue
		}
		out = append(out, mk(a.String()))
	}
	for _, h := range hostnames {
		if len(out) >= 10 {
			break
		}
		out = append(out, mk(h))
	}
	return out
}
