package egress

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeResolver answers from a script: each lookup of a name consumes the next answer
// (the last one repeats). It counts lookups.
type fakeResolver struct {
	mu      sync.Mutex
	answers map[string][][]string
	calls   map[string]int
}

func newResolver(m map[string][][]string) *fakeResolver {
	return &fakeResolver{answers: m, calls: map[string]int{}}
}

func (f *fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	seq, ok := f.answers[host]
	if !ok {
		return nil, errors.New("no such host")
	}
	i := f.calls[host]
	f.calls[host]++
	if i >= len(seq) {
		i = len(seq) - 1
	}
	var out []netip.Addr
	for _, s := range seq[i] {
		out = append(out, netip.MustParseAddr(s))
	}
	return out, nil
}

func (f *fakeResolver) count(h string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[h]
}

// harness runs a proxy on 127.0.0.1:0 whose dialer records addresses and redirects every
// vetted dial to the test upstream.
type harness struct {
	t      *testing.T
	proxy  *Proxy
	addr   string
	mu     sync.Mutex
	dialed []string
	up     string // real upstream address
}

func newHarness(t *testing.T, rules []string, res Resolver, up string, mod func(*Config)) *harness {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, lport, _ := net.SplitHostPort(l.Addr().String())
	rules = append([]string(nil), rules...)
	for i := range rules {
		rules[i] = strings.ReplaceAll(rules[i], "{PORT}", lport)
	}
	al, err := NewAllowlist(rules)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, up: up}
	cfg := Config{
		Allow:    al,
		Resolver: res,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			h.mu.Lock()
			h.dialed = append(h.dialed, addr)
			h.mu.Unlock()
			var d net.Dialer
			return d.DialContext(ctx, network, h.up)
		},
	}
	if mod != nil {
		mod(&cfg)
	}
	h.proxy = New(cfg)
	h.addr = l.Addr().String()
	go h.proxy.Serve(l)
	t.Cleanup(func() { h.proxy.Close() })
	return h
}

func (h *harness) dials() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.dialed...)
}

func upstream(t *testing.T) (*httptest.Server, string, string) {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello from upstream")
	}))
	t.Cleanup(s.Close)
	_, port, _ := net.SplitHostPort(s.Listener.Addr().String())
	return s, s.Listener.Addr().String(), port
}

// connect sends CONNECT target and returns the status code plus the live conn.
func (h *harness) connect(target string) (int, net.Conn, *bufio.Reader) {
	h.t.Helper()
	c, err := net.Dial("tcp", h.addr)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { c.Close() })
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	br := bufio.NewReader(c)
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(br, &http.Request{Method: "CONNECT"})
	if err != nil {
		h.t.Fatalf("CONNECT %s: %v", target, err)
	}
	return resp.StatusCode, c, br
}

func (h *harness) get(rawURL string) (int, string) {
	h.t.Helper()
	pu, _ := url.Parse("http://" + h.addr)
	cl := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{Proxy: http.ProxyURL(pu), DisableKeepAlives: true},
	}
	resp, err := cl.Get(rawURL)
	if err != nil {
		h.t.Fatalf("GET %s: %v", rawURL, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestConnectAllowed(t *testing.T) {
	_, upAddr, port := upstream(t)
	res := newResolver(map[string][][]string{"app.test": {{"8.8.8.8"}}})
	h := newHarness(t, []string{"app.test:" + port}, res, upAddr, nil)

	code, c, br := h.connect("app.test:" + port)
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	fmt.Fprintf(c, "GET / HTTP/1.1\r\nHost: app.test\r\nConnection: close\r\n\r\n")
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "hello from upstream" {
		t.Fatalf("body %q", b)
	}
	if d := h.dials(); len(d) != 1 || d[0] != "8.8.8.8:"+port {
		t.Fatalf("dialed %v, want the vetted IP only", d)
	}
}

func TestPlainHTTPAllowed(t *testing.T) {
	_, upAddr, port := upstream(t)
	res := newResolver(map[string][][]string{"app.test": {{"8.8.8.8"}}})
	h := newHarness(t, []string{"app.test:" + port}, res, upAddr, nil)
	code, body := h.get("http://app.test:" + port + "/x")
	if code != 200 || body != "hello from upstream" {
		t.Fatalf("got %d %q", code, body)
	}
	if d := h.dials(); len(d) != 1 || d[0] != "8.8.8.8:"+port {
		t.Fatalf("dialed %v", d)
	}
}

func TestNotAllowlistedAndWrongPort(t *testing.T) {
	_, upAddr, port := upstream(t)
	res := newResolver(map[string][][]string{"app.test": {{"8.8.8.8"}}, "other.test": {{"8.8.8.8"}}})
	h := newHarness(t, []string{"app.test:" + port}, res, upAddr, nil)

	for _, target := range []string{"other.test:" + port, "app.test:1", "app.test:443", "sub.app.test:" + port} {
		if code, _, _ := h.connect(target); code != http.StatusForbidden {
			t.Errorf("CONNECT %s: status %d, want 403", target, code)
		}
	}
	if code, _ := h.get2("http://other.test:" + port + "/"); code != http.StatusForbidden {
		t.Errorf("plain: status %d, want 403", code)
	}
	if len(h.dials()) != 0 || res.count("other.test") != 0 {
		t.Fatal("denied requests must not resolve or dial")
	}
}

// get2 is get without failing on non-2xx (get already returns the code; kept for symmetry).
func (h *harness) get2(u string) (int, string) { return h.get(u) }

func TestEmptyAllowlistDeniesAll(t *testing.T) {
	_, upAddr, port := upstream(t)
	res := newResolver(map[string][][]string{"app.test": {{"8.8.8.8"}}})
	h := newHarness(t, nil, res, upAddr, nil)
	if code, _, _ := h.connect("app.test:" + port); code != 403 {
		t.Fatalf("status %d", code)
	}
	if code, _, _ := h.connect("8.8.8.8:" + port); code != 403 {
		t.Fatalf("ip literal status %d", code)
	}
}

func TestIPLiteralNeedsCIDRRule(t *testing.T) {
	_, upAddr, port := upstream(t)
	h := newHarness(t, []string{"8.8.8.8:" + port, "[2606:4700::1111]:" + port}, newResolver(nil), upAddr, nil)
	if code, _, _ := h.connect("8.8.8.8:" + port); code != 200 {
		t.Errorf("allowed literal: %d", code)
	}
	if code, _, _ := h.connect("8.8.4.4:" + port); code != 403 {
		t.Errorf("other literal: %d", code)
	}
	if code, _, _ := h.connect("[2606:4700::1111]:" + port); code != 200 {
		t.Errorf("allowed v6 literal: %d", code)
	}
	// A hostname rule must not unlock literals and vice versa.
	h2 := newHarness(t, []string{"app.test:" + port}, newResolver(nil), upAddr, nil)
	if code, _, _ := h2.connect("8.8.8.8:" + port); code != 403 {
		t.Errorf("literal via host rule: %d", code)
	}
}

func TestObfuscatedIPForms(t *testing.T) {
	_, upAddr, port := upstream(t)
	h := newHarness(t, []string{"app.test:" + port, "8.8.8.8:" + port}, newResolver(nil), upAddr, nil)
	for _, target := range []string{
		"2130706433:" + port, "0x7f.0.0.1:" + port, "127.1:" + port, "[::ffff:7f00:1]:" + port,
		"localhost%00.app.test:" + port, "[fe80::1%25eth0]:" + port,
	} {
		if code, _, _ := h.connect(target); code == 200 {
			t.Errorf("%s: tunnel opened", target)
		}
	}
	if len(h.dials()) != 0 {
		t.Fatalf("dialed %v", h.dials())
	}
}

func TestBlockedResolvedAddresses(t *testing.T) {
	_, upAddr, port := upstream(t)
	bad := map[string]string{
		"loopback4": "127.0.0.1", "loopback6": "::1", "mapped": "::ffff:127.0.0.1",
		"unspec": "0.0.0.0", "unspec6": "::", "ll4": "169.254.1.1", "ll6": "fe80::1",
		"meta": "169.254.169.254", "meta6": "fd00:ec2::254",
		"r10": "10.0.0.5", "r172": "172.16.3.4", "r192": "192.168.0.9", "ula": "fc00::1", "ula2": "fd12::1",
		"cgnat": "100.64.0.1", "cgnat2": "100.127.255.254", "mcast": "224.0.0.1", "mcast6": "ff02::1",
		"bench": "198.18.0.1", "reserved": "240.0.0.1", "nat64": "64:ff9b::7f00:1", "6to4": "2002:7f00:1::1",
	}
	ans := map[string][][]string{}
	var rules []string
	for name, ip := range bad {
		host := name + ".test"
		ans[host] = [][]string{{ip}}
		rules = append(rules, host+":"+port)
	}
	h := newHarness(t, rules, newResolver(ans), upAddr, nil)
	for name := range bad {
		if code, _, _ := h.connect(name + ".test:" + port); code != 403 {
			t.Errorf("%s (%s): status %d, want 403", name, bad[name], code)
		}
	}
	if len(h.dials()) != 0 {
		t.Fatalf("dialed %v", h.dials())
	}
}

func TestPublicIPv6Allowed(t *testing.T) {
	_, upAddr, port := upstream(t)
	res := newResolver(map[string][][]string{"v6.test": {{"2606:4700::1111"}}})
	h := newHarness(t, []string{"v6.test:" + port}, res, upAddr, nil)
	if code, _, _ := h.connect("v6.test:" + port); code != 200 {
		t.Fatalf("status %d", code)
	}
	if d := h.dials(); len(d) != 1 || d[0] != "[2606:4700::1111]:"+port {
		t.Fatalf("dialed %v", d)
	}
}

func TestMixedAnswerDenied(t *testing.T) {
	_, upAddr, port := upstream(t)
	res := newResolver(map[string][][]string{"mix.test": {{"8.8.8.8", "127.0.0.1"}}})
	h := newHarness(t, []string{"mix.test:" + port}, res, upAddr, nil)
	if code, _, _ := h.connect("mix.test:" + port); code != 403 {
		t.Fatalf("status %d", code)
	}
	if len(h.dials()) != 0 {
		t.Fatal("must not dial when any resolved IP is refused")
	}
}

func TestRebindingDenied(t *testing.T) {
	_, upAddr, port := upstream(t)
	// First answer public, second loopback. The proxy resolves once and dials the vetted
	// IP, so the second answer is never consulted for an allowed request.
	res := newResolver(map[string][][]string{"rebind.test": {{"8.8.8.8"}, {"127.0.0.1"}}})
	h := newHarness(t, []string{"rebind.test:" + port}, res, upAddr, nil)
	if code, _, _ := h.connect("rebind.test:" + port); code != 200 {
		t.Fatalf("first: %d", code)
	}
	if d := h.dials(); len(d) != 1 || d[0] != "8.8.8.8:"+port {
		t.Fatalf("dialed %v", d)
	}
	if n := res.count("rebind.test"); n != 1 {
		t.Fatalf("resolver consulted %d times for one request, want 1", n)
	}
	// The next request sees the rebound answer and is refused.
	if code, _, _ := h.connect("rebind.test:" + port); code != 403 {
		t.Fatalf("second: %d", code)
	}
	if len(h.dials()) != 1 {
		t.Fatalf("rebound answer was dialed: %v", h.dials())
	}
}

func TestResolutionFailure(t *testing.T) {
	_, upAddr, port := upstream(t)
	h := newHarness(t, []string{"nx.test:" + port}, newResolver(nil), upAddr, nil)
	if code, _, _ := h.connect("nx.test:" + port); code != http.StatusBadGateway {
		t.Fatalf("status %d", code)
	}
}

func TestCIDRExplicitAllow(t *testing.T) {
	_, upAddr, port := upstream(t)
	res := newResolver(map[string][][]string{
		"db.test":  {{"10.1.2.3"}},
		"db2.test": {{"10.9.9.9"}},
		"lo.test":  {{"127.0.0.1"}},
		"md.test":  {{"169.254.169.254"}},
	})
	h := newHarness(t, []string{
		"db.test:" + port, "db2.test:" + port, "lo.test:" + port, "md.test:" + port,
		"10.1.0.0/16:" + port,
	}, res, upAddr, nil)
	if code, _, _ := h.connect("db.test:" + port); code != 200 {
		t.Errorf("db.test in allowed cidr: %d", code)
	}
	if code, _, _ := h.connect("db2.test:" + port); code != 403 {
		t.Errorf("db2.test outside cidr: %d", code)
	}
	if code, _, _ := h.connect("lo.test:" + port); code != 403 {
		t.Errorf("loopback with no cidr: %d", code)
	}
	if code, _, _ := h.connect("md.test:" + port); code != 403 {
		t.Errorf("metadata must never be allowable: %d", code)
	}
	if code, _, _ := h.connect("169.254.169.254:" + port); code != 403 {
		t.Errorf("metadata literal must never be allowable: %d", code)
	}
	if code, _, _ := h.connect("10.1.9.9:" + port); code != 200 {
		t.Errorf("literal in cidr: %d", code)
	}
}

func TestCIDRPortMismatch(t *testing.T) {
	_, upAddr, port := upstream(t)
	res := newResolver(map[string][][]string{"db.test": {{"10.1.2.3"}}})
	h := newHarness(t, []string{"db.test:" + port, "10.1.0.0/16:1"}, res, upAddr, nil)
	if code, _, _ := h.connect("db.test:" + port); code != 403 {
		t.Fatalf("status %d", code)
	}
}

func TestNotTheProxyItself(t *testing.T) {
	_, upAddr, _ := upstream(t)
	// Allow loopback wholesale on the proxy's own port: still refused.
	h := newHarness(t, []string{"me.test:{PORT}"}, newResolver(map[string][][]string{"me.test": {{"127.0.0.1"}}}), upAddr, nil)
	_, port, _ := net.SplitHostPort(h.addr)
	for _, target := range []string{"127.0.0.1:" + port, "me.test:" + port} {
		if code, _, _ := h.connect(target); code != 403 {
			t.Errorf("%s: %d", target, code)
		}
	}
	if len(h.dials()) != 0 {
		t.Fatal("dialed the proxy itself")
	}
}

func TestBadRequests(t *testing.T) {
	_, upAddr, _ := upstream(t)
	h := newHarness(t, []string{"app.test"}, newResolver(nil), upAddr, nil)
	if code, _, _ := h.connect("app.test"); code != 400 {
		t.Errorf("CONNECT without port: %d", code)
	}
	// origin-form request to the proxy
	c, _ := net.Dial("tcp", h.addr)
	defer c.Close()
	fmt.Fprint(c, "GET /path HTTP/1.1\r\nHost: x\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil || resp.StatusCode != 400 {
		t.Errorf("origin-form: %v %v", resp, err)
	}
	// https absolute URI over plain proxying is refused
	c2, _ := net.Dial("tcp", h.addr)
	defer c2.Close()
	fmt.Fprint(c2, "GET https://app.test/ HTTP/1.1\r\nHost: app.test\r\n\r\n")
	resp, err = http.ReadResponse(bufio.NewReader(c2), nil)
	if err != nil || resp.StatusCode != 403 {
		t.Errorf("https absolute: %v %v", resp, err)
	}
}

// silent returns a listener that accepts and never writes; closes on cleanup.
func silent(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(io.Discard, c); c.Close() }()
		}
	}()
	return l.Addr().String()
}

func expectClosed(t *testing.T, c net.Conn, br *bufio.Reader, within time.Duration) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(within))
	start := time.Now()
	_, err := br.ReadByte()
	if err == nil {
		t.Fatal("expected tunnel to close")
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("tunnel still open after %v", time.Since(start))
	}
}

func TestIdleTimeout(t *testing.T) {
	up := silent(t)
	_, port, _ := net.SplitHostPort(up)
	res := newResolver(map[string][][]string{"app.test": {{"8.8.8.8"}}})
	h := newHarness(t, []string{"app.test:" + port}, res, up, func(c *Config) { c.IdleTimeout = 150 * time.Millisecond })
	code, c, br := h.connect("app.test:" + port)
	if code != 200 {
		t.Fatal(code)
	}
	expectClosed(t, c, br, 3*time.Second)
}

func TestTotalTimeout(t *testing.T) {
	up := silent(t)
	_, port, _ := net.SplitHostPort(up)
	res := newResolver(map[string][][]string{"app.test": {{"8.8.8.8"}}})
	h := newHarness(t, []string{"app.test:" + port}, res, up, func(c *Config) {
		c.IdleTimeout = time.Minute
		c.TotalTimeout = 200 * time.Millisecond
	})
	code, c, br := h.connect("app.test:" + port)
	if code != 200 {
		t.Fatal(code)
	}
	expectClosed(t, c, br, 3*time.Second)
}

func TestGlobalAndPerWorkspaceCaps(t *testing.T) {
	up := silent(t)
	_, port, _ := net.SplitHostPort(up)
	res := newResolver(map[string][][]string{"app.test": {{"8.8.8.8"}}})
	lim := NewLimits(3, 2)
	mk := func(ws string) *harness {
		return newHarness(t, []string{"app.test:" + port}, res, up, func(c *Config) { c.Limits = lim; c.Workspace = ws })
	}
	a, b := mk("a"), mk("b")

	var conns []net.Conn
	for i := 0; i < 2; i++ {
		code, c, _ := a.connect("app.test:" + port)
		if code != 200 {
			t.Fatalf("a#%d: %d", i, code)
		}
		conns = append(conns, c)
	}
	if code, _, _ := a.connect("app.test:" + port); code != 503 {
		t.Fatalf("per-workspace cap: %d", code)
	}
	code, cb, _ := b.connect("app.test:" + port)
	if code != 200 {
		t.Fatalf("b#0: %d", code)
	}
	conns = append(conns, cb)
	if code, _, _ := b.connect("app.test:" + port); code != 503 {
		t.Fatalf("global cap: %d", code)
	}
	// Releasing a slot frees capacity.
	conns[0].Close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		code, c, _ := a.connect("app.test:" + port)
		if code == 200 {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("slot never released")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestLimitsReleaseOnce(t *testing.T) {
	l := NewLimits(1, 1)
	rel, ok := l.Acquire("w")
	if !ok {
		t.Fatal("acquire")
	}
	rel()
	rel()
	if _, ok := l.Acquire("w"); !ok {
		t.Fatal("slot lost")
	}
	if _, ok := l.Acquire("w"); ok {
		t.Fatal("double release leaked capacity")
	}
}

func TestDenialLogHasNoBody(t *testing.T) {
	var sb syncBuf
	_, upAddr, port := upstream(t)
	h := newHarness(t, nil, newResolver(nil), upAddr, func(c *Config) {
		c.Logger = slog.New(slog.NewTextHandler(&sb, nil))
		c.Workspace = "ws1"
	})
	c, _ := net.Dial("tcp", h.addr)
	defer c.Close()
	fmt.Fprintf(c, "POST http://evil.test:%s/ HTTP/1.1\r\nHost: evil.test:%s\r\nContent-Length: 6\r\n\r\nSECRET", port, port)
	_, _ = http.ReadResponse(bufio.NewReader(c), nil)
	out := sb.String()
	if !strings.Contains(out, "evil.test") || !strings.Contains(out, "not allowlisted") || !strings.Contains(out, "ws1") {
		t.Fatalf("log missing fields: %s", out)
	}
	if strings.Contains(out, "SECRET") {
		t.Fatal("body leaked into log")
	}
}

func TestCloseTearsDownTunnels(t *testing.T) {
	up := silent(t)
	_, port, _ := net.SplitHostPort(up)
	res := newResolver(map[string][][]string{"app.test": {{"8.8.8.8"}}})
	h := newHarness(t, []string{"app.test:" + port}, res, up, nil)
	code, c, br := h.connect("app.test:" + port)
	if code != 200 {
		t.Fatal(code)
	}
	h.proxy.Close()
	expectClosed(t, c, br, 3*time.Second)
}

type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
