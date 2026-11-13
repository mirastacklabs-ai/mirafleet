package egress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Resolver is the DNS dependency; *net.Resolver satisfies it.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// DialFunc dials an already vetted IP literal address ("1.2.3.4:443" / "[::1]:443").
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Config configures a Proxy. The zero value of every field except Allow is usable.
type Config struct {
	Allow     *Allowlist // nil or empty denies everything
	Workspace string     // label used for per-workspace caps and logs
	Limits    *Limits    // shared across proxies; nil creates a private default
	Resolver  Resolver   // nil = net.DefaultResolver
	Dial      DialFunc   // nil = net.Dialer (tests inject)
	Logger    *slog.Logger

	IdleTimeout    time.Duration // no bytes in a direction for this long closes a tunnel (default 60s)
	TotalTimeout   time.Duration // hard cap on one connection or request (default 1h)
	DialTimeout    time.Duration // per dial attempt (default 10s)
	ResolveTimeout time.Duration // DNS lookup (default 5s)
	HeaderTimeout  time.Duration // reading the client's request head (default 10s)
}

// Proxy is a deny-by-default HTTP/CONNECT forward proxy.
type Proxy struct {
	cfg Config
	tr  *http.Transport

	mu     sync.Mutex
	srv    *http.Server
	addr   net.Addr
	tunnel map[net.Conn]struct{}
	closed bool
}

type dialKey struct{}

// New builds a Proxy. Call Serve with a listener.
func New(cfg Config) *Proxy {
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 60 * time.Second
	}
	if cfg.TotalTimeout <= 0 {
		cfg.TotalTimeout = time.Hour
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 10 * time.Second
	}
	if cfg.ResolveTimeout <= 0 {
		cfg.ResolveTimeout = 5 * time.Second
	}
	if cfg.HeaderTimeout <= 0 {
		cfg.HeaderTimeout = 10 * time.Second
	}
	if cfg.Resolver == nil {
		cfg.Resolver = net.DefaultResolver
	}
	if cfg.Limits == nil {
		cfg.Limits = NewLimits(0, 0)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	p := &Proxy{cfg: cfg, tunnel: map[net.Conn]struct{}{}}
	p.tr = &http.Transport{
		Proxy:                 nil, // never chain
		DisableKeepAlives:     true,
		DisableCompression:    true,
		ResponseHeaderTimeout: cfg.IdleTimeout,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			// The address http.Transport asks for is ignored on purpose: only the IP vetted
			// by this proxy, carried in the context, is ever dialed (no re-resolution).
			t, ok := ctx.Value(dialKey{}).(vetted)
			if !ok {
				return nil, errors.New("egress: no vetted target")
			}
			return p.dialVetted(ctx, t)
		},
	}
	return p
}

// Serve accepts connections on l until Close. It returns http.ErrServerClosed after Close.
func (p *Proxy) Serve(l net.Listener) error {
	srv := &http.Server{
		Handler:           p,
		ReadHeaderTimeout: p.cfg.HeaderTimeout,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          nil,
	}
	srv.SetKeepAlivesEnabled(false)
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = l.Close()
		return http.ErrServerClosed
	}
	p.srv, p.addr = srv, l.Addr()
	p.mu.Unlock()
	return srv.Serve(l)
}

// Close stops the listener and tears down every open tunnel.
func (p *Proxy) Close() error {
	p.mu.Lock()
	p.closed = true
	srv := p.srv
	conns := make([]net.Conn, 0, len(p.tunnel))
	for c := range p.tunnel {
		conns = append(conns, c)
	}
	p.mu.Unlock()
	var err error
	if srv != nil {
		err = srv.Close()
	}
	for _, c := range conns {
		_ = c.Close()
	}
	return err
}

func (p *Proxy) track(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.tunnel[c] = struct{}{}
	return true
}

func (p *Proxy) untrack(c net.Conn) {
	p.mu.Lock()
	delete(p.tunnel, c)
	p.mu.Unlock()
}

// vetted is a target that passed the allowlist and address checks.
type vetted struct {
	addrs []netip.Addr
	port  uint16
}

type denial struct {
	status int
	reason string
}

func (d *denial) Error() string { return d.reason }

func (p *Proxy) deny(w http.ResponseWriter, host, port string, d *denial) {
	p.cfg.Logger.Warn("egress denied", "workspace", p.cfg.Workspace, "host", host, "port", port, "reason", d.reason)
	http.Error(w, "egress denied: "+d.reason, d.status)
}

// ServeHTTP implements the proxy: CONNECT host:port and absolute-URI http requests.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	release, ok := p.cfg.Limits.Acquire(p.cfg.Workspace)
	if !ok {
		p.cfg.Logger.Warn("egress denied", "workspace", p.cfg.Workspace, "host", hostOnly(r), "reason", "connection cap reached")
		http.Error(w, "egress denied: too many connections", http.StatusServiceUnavailable)
		return
	}
	defer release()

	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	p.handlePlain(w, r)
}

func hostOnly(r *http.Request) string {
	h := r.Host
	if r.URL != nil && r.URL.Host != "" {
		h = r.URL.Host
	}
	if len(h) > 255 {
		h = h[:255]
	}
	return h
}

// splitTarget splits host[:port]; defPort 0 means a port is mandatory.
func splitTarget(hostport string, defPort uint16) (host string, port uint16, err error) {
	if hostport == "" {
		return "", 0, errors.New("empty target")
	}
	h, ps, serr := net.SplitHostPort(hostport)
	if serr != nil {
		if defPort == 0 {
			return "", 0, errors.New("target needs host:port")
		}
		h = strings.TrimSuffix(strings.TrimPrefix(hostport, "["), "]")
		if strings.Contains(h, ":") && !strings.HasPrefix(hostport, "[") {
			return "", 0, errors.New("bad target")
		}
		return h, defPort, nil
	}
	p, perr := parsePort(ps)
	if perr != nil {
		return "", 0, perr
	}
	return h, p, nil
}

// vet applies the allowlist, resolves the name itself and checks every resulting IP.
func (p *Proxy) vet(ctx context.Context, host string, port uint16) (vetted, *denial) {
	if strings.ContainsAny(host, "%\x00") || host == "" {
		return vetted{}, &denial{http.StatusBadRequest, "invalid host"}
	}
	allow := p.cfg.Allow

	if ip, err := netip.ParseAddr(host); err == nil {
		ip = ip.Unmap()
		// IP literals are only reachable through an explicit cidr/ip rule.
		if !allow.AllowsIP(ip, port) {
			return vetted{}, &denial{http.StatusForbidden, "IP literal not allowlisted"}
		}
		if reason := neverAllowed(ip); reason != "" {
			return vetted{}, &denial{http.StatusForbidden, reason}
		}
		if p.isSelf(ip, port) {
			return vetted{}, &denial{http.StatusForbidden, "target is the proxy itself"}
		}
		return vetted{addrs: []netip.Addr{ip}, port: port}, nil
	}

	name, err := normalizeHost(host)
	if err != nil {
		return vetted{}, &denial{http.StatusBadRequest, "invalid host: " + err.Error()}
	}
	if !allow.AllowsName(name, port) {
		return vetted{}, &denial{http.StatusForbidden, "host:port not allowlisted"}
	}

	rctx, cancel := context.WithTimeout(ctx, p.cfg.ResolveTimeout)
	defer cancel()
	ips, err := p.cfg.Resolver.LookupNetIP(rctx, "ip", name)
	if err != nil || len(ips) == 0 {
		return vetted{}, &denial{http.StatusBadGateway, "name resolution failed"}
	}
	if len(ips) > 16 {
		ips = ips[:16]
	}
	out := make([]netip.Addr, 0, len(ips))
	for _, ip := range ips {
		ip = ip.Unmap()
		if reason := neverAllowed(ip); reason != "" {
			return vetted{}, &denial{http.StatusForbidden, reason}
		}
		if reason := blockedReason(ip); reason != "" && !allow.AllowsIP(ip, port) {
			return vetted{}, &denial{http.StatusForbidden, fmt.Sprintf("resolved to %s address", reason)}
		}
		if p.isSelf(ip, port) {
			return vetted{}, &denial{http.StatusForbidden, "target is the proxy itself"}
		}
		out = append(out, ip)
	}
	return vetted{addrs: out, port: port}, nil
}

// isSelf reports whether ip:port is this proxy's own listener.
func (p *Proxy) isSelf(ip netip.Addr, port uint16) bool {
	p.mu.Lock()
	addr := p.addr
	p.mu.Unlock()
	ta, ok := addr.(*net.TCPAddr)
	if !ok || ta == nil || uint16(ta.Port) != port {
		return false
	}
	la, ok := netip.AddrFromSlice(ta.IP)
	if !ok {
		return true // unknown listener address with the same port: refuse
	}
	la = la.Unmap()
	if la.IsUnspecified() {
		return ip.IsLoopback() || ip.IsUnspecified() || isLocalIP(ip)
	}
	return la == ip || (la.IsLoopback() && ip.IsLoopback())
}

func isLocalIP(ip netip.Addr) bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return true
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok {
			if x, ok := netip.AddrFromSlice(ipn.IP); ok && x.Unmap() == ip {
				return true
			}
		}
	}
	return false
}

// dialVetted dials the vetted IPs in order; names are never re-resolved.
func (p *Proxy) dialVetted(ctx context.Context, t vetted) (net.Conn, error) {
	dial := p.cfg.Dial
	if dial == nil {
		d := &net.Dialer{}
		dial = d.DialContext
	}
	var last error
	for _, ip := range t.addrs {
		actx, cancel := context.WithTimeout(ctx, p.cfg.DialTimeout)
		c, err := dial(actx, "tcp", netip.AddrPortFrom(ip, t.port).String())
		cancel()
		if err == nil {
			return c, nil
		}
		last = err
		if ctx.Err() != nil {
			break
		}
	}
	return nil, last
}

func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	host, port, err := splitTarget(r.Host, 0)
	if err != nil {
		p.deny(w, hostOnly(r), "", &denial{http.StatusBadRequest, err.Error()})
		return
	}
	t, d := p.vet(r.Context(), host, port)
	if d != nil {
		p.deny(w, host, strconv.Itoa(int(port)), d)
		return
	}
	up, err := p.dialVetted(r.Context(), t)
	if err != nil {
		p.cfg.Logger.Warn("egress dial failed", "workspace", p.cfg.Workspace, "host", host, "port", port)
		http.Error(w, "egress: upstream unreachable", http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = up.Close()
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	cc, brw, err := hj.Hijack()
	if err != nil {
		_ = up.Close()
		return
	}
	_ = cc.SetDeadline(time.Time{})
	if !p.track(cc) {
		_ = up.Close()
		_ = cc.Close()
		return
	}
	defer p.untrack(cc)
	p.track(up)
	defer p.untrack(up)

	if _, err := io.WriteString(cc, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		_ = up.Close()
		_ = cc.Close()
		return
	}
	var pre []byte
	if n := brw.Reader.Buffered(); n > 0 {
		pre, _ = brw.Reader.Peek(n)
		pre = append([]byte(nil), pre...)
	}
	p.pipe(cc, up, pre)
}

// pipe copies both ways with a per-direction idle deadline and a total deadline.
func (p *Proxy) pipe(client, up net.Conn, pre []byte) {
	total := time.AfterFunc(p.cfg.TotalTimeout, func() {
		_ = client.Close()
		_ = up.Close()
	})
	defer total.Stop()
	defer client.Close()
	defer up.Close()

	c := &idleConn{Conn: client, idle: p.cfg.IdleTimeout}
	u := &idleConn{Conn: up, idle: p.cfg.IdleTimeout}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		var err error
		if len(pre) > 0 {
			_, err = u.Write(pre)
		}
		if err == nil {
			_, err = io.Copy(u, c)
		}
		finish(up, client, err)
	}()
	go func() {
		defer wg.Done()
		_, err := io.Copy(c, u)
		finish(client, up, err)
	}()
	wg.Wait()
}

// finish half-closes dst on clean EOF, otherwise tears both ends down.
func finish(dst, src net.Conn, err error) {
	if err == nil {
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
			return
		}
	}
	_ = dst.Close()
	_ = src.Close()
}

type idleConn struct {
	net.Conn
	idle time.Duration
}

func (c *idleConn) Read(b []byte) (int, error) {
	_ = c.Conn.SetReadDeadline(time.Now().Add(c.idle))
	return c.Conn.Read(b)
}

func (c *idleConn) Write(b []byte) (int, error) {
	_ = c.Conn.SetWriteDeadline(time.Now().Add(c.idle))
	return c.Conn.Write(b)
}

var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Proxy-Authenticate", "Proxy-Authorization",
	"Keep-Alive", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func stripHop(h http.Header) {
	for _, v := range h.Values("Connection") {
		for _, tok := range strings.Split(v, ",") {
			if tok = strings.TrimSpace(tok); tok != "" {
				h.Del(tok)
			}
		}
	}
	for _, k := range hopHeaders {
		h.Del(k)
	}
}

func (p *Proxy) handlePlain(w http.ResponseWriter, r *http.Request) {
	u := r.URL
	if u == nil || u.Host == "" || !strings.EqualFold(u.Scheme, "http") {
		code := http.StatusBadRequest
		reason := "absolute http:// URI or CONNECT required (this is a proxy, not a web server)"
		if u != nil && u.Host != "" {
			code, reason = http.StatusForbidden, "only http:// and CONNECT are proxied"
		}
		p.deny(w, hostOnly(r), "", &denial{code, reason})
		return
	}
	host, port, err := splitTarget(u.Host, 80)
	if err != nil {
		p.deny(w, hostOnly(r), "", &denial{http.StatusBadRequest, err.Error()})
		return
	}
	t, d := p.vet(r.Context(), host, port)
	if d != nil {
		p.deny(w, host, strconv.Itoa(int(port)), d)
		return
	}

	ctx, cancel := context.WithTimeout(context.WithValue(r.Context(), dialKey{}, t), p.cfg.TotalTimeout)
	defer cancel()
	out := r.Clone(ctx)
	out.RequestURI = ""
	stripHop(out.Header)
	out.Close = true

	resp, err := p.tr.RoundTrip(out)
	if err != nil {
		p.cfg.Logger.Warn("egress upstream failed", "workspace", p.cfg.Workspace, "host", host, "port", port)
		http.Error(w, "egress: upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	stripHop(resp.Header)
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("Connection", "close")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// Limits caps concurrent proxied connections globally and per workspace. Share one
// *Limits between all proxies of a runner.
type Limits struct {
	mu     sync.Mutex
	global int
	perWS  int
	total  int
	byWS   map[string]int
}

// NewLimits returns caps; values <= 0 select defaults (256 global, 64 per workspace).
func NewLimits(global, perWorkspace int) *Limits {
	if global <= 0 {
		global = 256
	}
	if perWorkspace <= 0 {
		perWorkspace = 64
	}
	return &Limits{global: global, perWS: perWorkspace, byWS: map[string]int{}}
}

// Acquire reserves a slot; release must be called exactly once when ok.
func (l *Limits) Acquire(ws string) (release func(), ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.total >= l.global || l.byWS[ws] >= l.perWS {
		return nil, false
	}
	l.total++
	l.byWS[ws]++
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.total--
			if l.byWS[ws]--; l.byWS[ws] <= 0 {
				delete(l.byWS, ws)
			}
		})
	}, true
}

var (
	metadataV4 = netip.MustParseAddr("169.254.169.254")
	metadataV6 = netip.MustParseAddr("fd00:ec2::254")

	reservedPrefixes = mustPrefixes(
		"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15",
		"198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4",
		"64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/32", "2001:db8::/32", "2002::/16",
	)
)

func mustPrefixes(s ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(s))
	for i, v := range s {
		out[i] = netip.MustParsePrefix(v)
	}
	return out
}

// neverAllowed returns a reason for addresses no rule can unlock (cloud metadata).
func neverAllowed(ip netip.Addr) string {
	ip = ip.Unmap()
	if ip == metadataV4 || ip == metadataV6 {
		return "cloud metadata address"
	}
	return ""
}

// blockedReason names the refused class of ip, or "" when it is ordinary public space.
func blockedReason(ip netip.Addr) string {
	ip = ip.Unmap()
	switch {
	case ip.IsUnspecified():
		return "unspecified"
	case ip.IsLoopback():
		return "loopback"
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(), ip.IsInterfaceLocalMulticast():
		return "link-local"
	case ip.IsMulticast():
		return "multicast"
	case ip.IsPrivate():
		return "private"
	}
	for _, pf := range reservedPrefixes {
		if pf.Contains(ip) {
			return "reserved/CGNAT"
		}
	}
	return ""
}
