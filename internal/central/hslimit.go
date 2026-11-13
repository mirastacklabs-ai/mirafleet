package central

import (
	"net"
	"strings"
	"sync"
)

// hsLimiter is the cheap pre-handshake connection limit (docs/M2.md D-03): it counts SSH
// handshakes currently in progress, per source IP and globally, and refuses new connections
// beyond the caps before any cryptography is done. It complements the post-handshake enroll
// limiter, which runs once the role is known.
type hsLimiter struct {
	perIP, global int

	mu    sync.Mutex
	total int
	byIP  map[string]int
}

func newHSLimiter(perIP, global int) *hsLimiter {
	return &hsLimiter{perIP: perIP, global: global, byIP: map[string]int{}}
}

// acquire reserves a handshake slot for remote (host:port). On success it returns the release
// func; on refusal ok is false and reason says which cap was hit.
func (l *hsLimiter) acquire(remote string) (release func(), reason string, ok bool) {
	ip := ipKey(remote)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.total >= l.global {
		return nil, "global handshake limit", false
	}
	if l.byIP[ip] >= l.perIP {
		return nil, "per-IP handshake limit", false
	}
	l.total++
	l.byIP[ip]++
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.total--
			if l.byIP[ip]--; l.byIP[ip] <= 0 {
				delete(l.byIP, ip)
			}
		})
	}, "", true
}

// ipKey is the rate-limit key of a remote address (host:port or bare host): the IP itself for
// IPv4, the /64 prefix for IPv6 (a single host typically owns a whole /64, so per-address
// buckets would be free to multiply). Unparseable input is used verbatim.
func ipKey(remote string) string {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	if i := strings.IndexByte(host, '%'); i >= 0 { // zone
		host = host[:i]
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return host
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}
