package central

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/keys"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// ---- D-03 pre-handshake limit, D-10 Close ----

func TestHSLimiter(t *testing.T) {
	l := newHSLimiter(2, 3)
	r1, _, ok1 := l.acquire("1.1.1.1:1")
	_, _, ok2 := l.acquire("1.1.1.1:2")
	_, reason, ok3 := l.acquire("1.1.1.1:3")
	if !ok1 || !ok2 || ok3 || !strings.Contains(reason, "per-IP") {
		t.Fatalf("per-IP cap: %v %v %v %q", ok1, ok2, ok3, reason)
	}
	if _, _, ok := l.acquire("2.2.2.2:1"); !ok {
		t.Fatal("other IP must have its own allowance")
	}
	if _, reason, ok := l.acquire("3.3.3.3:1"); ok || !strings.Contains(reason, "global") {
		t.Fatalf("global cap: %v %q", ok, reason)
	}
	r1()
	r1() // idempotent
	if _, _, ok := l.acquire("1.1.1.1:4"); !ok {
		t.Fatal("slot not released")
	}
	if l.total != 3 {
		t.Fatalf("total %d", l.total)
	}
}

func TestIPKey(t *testing.T) {
	cases := map[string]string{
		"1.2.3.4:55":                     "1.2.3.4",
		"1.2.3.4":                        "1.2.3.4",
		"[2001:db8:1:2:aaaa::1]:80":      "2001:db8:1:2::/64",
		"[2001:db8:1:2:bbbb:cccc::9]:81": "2001:db8:1:2::/64",
		"[2001:db8:1:3::1]:80":           "2001:db8:1:3::/64",
		"[fe80::1%eth0]:80":              "fe80::/64",
		"[::ffff:1.2.3.4]:80":            "1.2.3.4",
		"not-an-ip":                      "not-an-ip",
	}
	for in, want := range cases {
		if got := ipKey(in); got != want {
			t.Errorf("ipKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPreHandshakeConnectionLimit(t *testing.T) {
	e := newEnvOpts(t, 15*time.Second, func(c *Config) { c.MaxHandshakesPerIP = 2 })
	_, err := keys.Generate(e.dir, "anon", "anon")
	must(t, err)
	var idle []net.Conn
	for i := 0; i < 2; i++ {
		c, err := net.Dial("tcp", e.addr)
		must(t, err)
		defer c.Close()
		idle = append(idle, c)
	}
	waitFor(t, func() bool { e.srv.hs.mu.Lock(); defer e.srv.hs.mu.Unlock(); return e.srv.hs.total == 2 })
	// The third connection is closed by the server without any handshake bytes.
	c3, err := net.Dial("tcp", e.addr)
	must(t, err)
	defer c3.Close()
	c3.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := c3.Read(make([]byte, 1)); n != 0 || err == nil {
		t.Fatalf("refused connection got data: n=%d err=%v", n, err)
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("connection over the limit was not closed")
	}
	waitFor(t, func() bool { return strings.Contains(e.logText(), "refused before handshake") })
	// Freeing a slot lets a real client in again.
	idle[0].Close()
	waitFor(t, func() bool {
		cl, err := e.dial(transport.UserEnroll, "anon")
		if err != nil {
			return false
		}
		cl.Close()
		return true
	})
}

func TestCloseDropsHandshakingConnections(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 3; i++ {
		c, err := net.Dial("tcp", e.addr)
		must(t, err)
		defer c.Close()
	}
	waitFor(t, func() bool { e.srv.hs.mu.Lock(); defer e.srv.hs.mu.Unlock(); return e.srv.hs.total == 3 })
	start := time.Now()
	e.srv.Close()
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("Close took %v with peers stuck in handshake (handshake timeout is %v)", d, e.srv.cfg.HandshakeTimeout)
	}
}

// ---- D-04 add-after-kick ----

func TestOperatorRevokedBetweenAuthAndAddIsDropped(t *testing.T) {
	e := newEnv(t)
	e.addOperator("op")
	var once sync.Once
	e.srv.beforeOperatorAdd = func(auth.Principal) {
		// Authenticated, not yet tracked: the revoke's kick finds no connection to close.
		once.Do(func() {
			if _, err := e.svc.RevokeOperator(context.Background(), e.admin, service.SourceCLI, "op"); err != nil {
				t.Errorf("revoke: %v", err)
			}
		})
	}
	cl, err := e.dial(transport.UserOperator, "op")
	must(t, err)
	defer cl.Close()
	done := make(chan struct{})
	go func() { cl.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("operator revoked in the add-after-kick window stayed connected")
	}
	if !strings.Contains(e.logText(), "no longer active") {
		t.Fatalf("missing log:\n%s", e.logText())
	}
}

// ---- D-05 key algorithms and the enroll key ----

func TestOnlyEd25519KeysAreAccepted(t *testing.T) {
	e := newEnv(t, "r1")
	e.addOperator("op")

	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	must(t, err)
	rsaSigner, err := ssh.NewSignerFromKey(rsaKey)
	must(t, err)
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	ecSigner, err := ssh.NewSignerFromKey(ecKey)
	must(t, err)
	_, caPriv, err := ed25519.GenerateKey(rand.Reader)
	must(t, err)
	ca, err := ssh.NewSignerFromKey(caPriv)
	must(t, err)
	// A certificate over a REGISTERED ed25519 key must not authenticate either.
	certOf := func(name string) ssh.Signer {
		s := e.signer(name)
		cert := &ssh.Certificate{Key: s.PublicKey(), CertType: ssh.UserCert, ValidBefore: ssh.CertTimeInfinity,
			ValidPrincipals: []string{"runner", "operator", "enroll"}}
		must(t, cert.SignCert(rand.Reader, ca))
		cs, err := ssh.NewCertSigner(cert, s)
		must(t, err)
		return cs
	}

	try := func(user string, s ssh.Signer) error {
		cl, err := ssh.Dial("tcp", e.addr, &ssh.ClientConfig{User: user, Auth: []ssh.AuthMethod{ssh.PublicKeys(s)},
			HostKeyCallback: ssh.FixedHostKey(e.hostSigner.PublicKey()), Timeout: 5 * time.Second})
		if err == nil {
			cl.Close()
		}
		return err
	}
	for _, user := range []string{transport.UserRunner, transport.UserOperator, transport.UserEnroll} {
		registered := map[string]string{transport.UserRunner: "r1", transport.UserOperator: "op", transport.UserEnroll: "r1"}[user]
		for name, s := range map[string]ssh.Signer{"rsa": rsaSigner, "ecdsa": ecSigner, "cert": certOf(registered)} {
			if err := try(user, s); err == nil {
				t.Errorf("%s key authenticated as %q", name, user)
			}
		}
		// Positive control: the registered ed25519 key works for its own role.
		if err := try(user, e.signer(registered)); err != nil {
			t.Errorf("ed25519 key refused as %q: %v", user, err)
		}
	}
}

type fakeMeta struct{ user string }

func (f fakeMeta) User() string          { return f.user }
func (f fakeMeta) SessionID() []byte     { return []byte("sid") }
func (f fakeMeta) ClientVersion() []byte { return []byte("SSH-2.0-test") }
func (f fakeMeta) ServerVersion() []byte { return []byte("SSH-2.0-test") }
func (f fakeMeta) RemoteAddr() net.Addr  { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 4242} }
func (f fakeMeta) LocalAddr() net.Addr   { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 22} }

func TestEnrollKeyIsTheAuthenticatedKey(t *testing.T) {
	e := newEnv(t)
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	must(t, err)
	key, err := ssh.NewPublicKey(pub)
	must(t, err)

	perms, err := e.srv.ssh.PublicKeyCallback(fakeMeta{user: transport.UserEnroll}, key)
	must(t, err)
	raw, err := base64.StdEncoding.DecodeString(perms.Extensions[extPubKey])
	must(t, err)
	if string(raw) != string(key.Marshal()) {
		t.Fatal("mirafleet-pubkey is not the presented key")
	}
	p, err := auth.Decode(perms)
	must(t, err)
	if p.Kind != auth.KindEnroll || p.Fingerprint != ssh.FingerprintSHA256(key) {
		t.Fatalf("principal: %+v", p)
	}
	sc := &ssh.ServerConn{Permissions: perms}
	got, err := enrollKey(sc, p)
	if err != nil || string(got.Marshal()) != string(key.Marshal()) {
		t.Fatalf("enrollKey: %v", err)
	}

	// D-07: fail closed on any disagreement between the stored key and the principal.
	other := p
	other.Fingerprint = "SHA256:other"
	if _, err := enrollKey(sc, other); err == nil {
		t.Fatal("fingerprint mismatch accepted")
	}
	other.Fingerprint = ""
	if _, err := enrollKey(sc, other); err == nil {
		t.Fatal("empty fingerprint accepted")
	}
	pub2, _, _ := ed25519.GenerateKey(rand.Reader)
	key2, _ := ssh.NewPublicKey(pub2)
	swapped := &ssh.ServerConn{Permissions: &ssh.Permissions{Extensions: map[string]string{
		extPubKey: base64.StdEncoding.EncodeToString(key2.Marshal())}}}
	if _, err := enrollKey(swapped, p); err == nil {
		t.Fatal("swapped key accepted")
	}

	// Only enroll connections carry the key.
	e.addOperator("op")
	operPerms, err := e.srv.ssh.PublicKeyCallback(fakeMeta{user: transport.UserOperator}, e.signer("op").PublicKey())
	must(t, err)
	if _, ok := operPerms.Extensions[extPubKey]; ok {
		t.Fatal("operator permissions carry the enroll key")
	}
}

// ---- D-09 write deadline ----

type blockingWriter struct{ unblock chan struct{} }

func (b *blockingWriter) Write(p []byte) (int, error) {
	<-b.unblock
	return 0, io.ErrClosedPipe
}
func (b *blockingWriter) Close() error { close(b.unblock); return nil }

func TestWriteResponseTimeoutClosesStalledChannel(t *testing.T) {
	old := api.WriteTimeout
	api.WriteTimeout = 100 * time.Millisecond
	t.Cleanup(func() { api.WriteTimeout = old })
	w := &blockingWriter{unblock: make(chan struct{})}
	resp, err := api.NewResult("x", map[string]string{"a": "b"})
	must(t, err)
	done := make(chan error, 1)
	go func() { done <- writeResponseTimeout(w, w, resp) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("stalled write reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stalled response write was never abandoned")
	}
}

// ---- D-11 limiter ----

func TestIPLimiterBoundedAndIPv6Prefix(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newIPLimiter(1, func() time.Time { return now })
	l.max = 8
	for i := 0; i < 100; i++ {
		now = now.Add(time.Millisecond)
		l.allow(net.JoinHostPort("10.0.0."+strconv.Itoa(i), "1"))
		if len(l.m) > l.max {
			t.Fatalf("bucket map grew to %d (cap %d)", len(l.m), l.max)
		}
	}
	// One /64 is one source: the second address shares the first one's single token.
	l = newIPLimiter(1, func() time.Time { return now })
	if !l.allow("[2001:db8::1]:1") || l.allow("[2001:db8::2]:1") {
		t.Fatal("addresses of one /64 must share a bucket")
	}
	if !l.allow("[2001:db8:0:1::1]:1") {
		t.Fatal("a different /64 must have its own bucket")
	}
}

// ---- D-08 runner touches ----

func TestClipInfoValue(t *testing.T) {
	if clip("host") != "host" {
		t.Fatal("short value changed")
	}
	long := strings.Repeat("a", 5000)
	if len(clip(long)) != maxInfoValue {
		t.Fatalf("len %d", len(clip(long)))
	}
	multi := strings.Repeat("é", 500) // 2 bytes per rune
	if c := clip(multi); len(c) > maxInfoValue || strings.ToValidUTF8(c, "") != c {
		t.Fatalf("clip split a rune: len %d", len(c))
	}
	if clip("a\xffb") != "ab" {
		t.Fatal("invalid UTF-8 kept")
	}
}

func TestClaimEdgeThrottles(t *testing.T) {
	e := newEnvOpts(t, 15*time.Second, func(c *Config) { c.TouchInterval = 10 * time.Second })
	now := time.Unix(5000, 0)
	if !e.srv.claimEdge(1, now) || e.srv.claimEdge(1, now.Add(500*time.Millisecond)) {
		t.Fatal("second edge within the gap must be throttled")
	}
	if !e.srv.claimEdge(2, now) {
		t.Fatal("other runner throttled")
	}
	if !e.srv.claimEdge(1, now.Add(e.srv.edgeGap()+time.Millisecond)) {
		t.Fatal("edge after the gap must pass")
	}
}

type blockTouchStore struct {
	store.Store
	release chan struct{}
}

func (b *blockTouchStore) TouchRunner(ctx context.Context, id int64, seen time.Time, info map[string]string) error {
	select {
	case <-b.release:
		return b.Store.TouchRunner(ctx, id, seen, info)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// A store that never answers a touch must not slow heartbeat replies.
func TestHeartbeatReplyDoesNotWaitOnStore(t *testing.T) {
	bs := &blockTouchStore{release: make(chan struct{})}
	e := newEnvOpts(t, 15*time.Second, func(c *Config) {
		bs.Store = c.Store
		c.Store = bs
		c.TouchInterval = 20 * time.Millisecond
	}, "r1")
	t.Cleanup(func() { close(bs.release) }) // runs before the server's Close
	start := time.Now()
	cl := e.rawRunner("r1", nil) // first heartbeat: registers, enqueues a touch
	for i := 0; i < 5; i++ {
		time.Sleep(30 * time.Millisecond)
		hb, _ := json.Marshal(transport.Heartbeat{Hostname: "raw", Proto: testProto})
		if ok, _, err := cl.SendRequest(transport.ReqHeartbeat, true, hb); err != nil || !ok {
			t.Fatalf("heartbeat %d: %v %v", i, ok, err)
		}
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("heartbeats took %v with a stuck store", d)
	}
}

// ---- D-12 ----

func TestForbiddenMethodAttemptIsLogged(t *testing.T) {
	e := newEnv(t)
	e.addOperator("op")
	cl, err := e.dial(transport.UserOperator, "op")
	must(t, err)
	defer cl.Close()
	resp, err := rawCall(cl, transport.ChanAPI, api.MethodTokenCreate, map[string]string{"secret": "do-not-log"})
	must(t, err)
	if resp.Error == nil || resp.Error.Code != api.CodeForbidden {
		t.Fatalf("resp: %+v", resp)
	}
	waitFor(t, func() bool {
		return strings.Contains(e.logText(), `"op" attempted forbidden method "`+api.MethodTokenCreate+`"`)
	})
	if strings.Contains(e.logText(), "do-not-log") {
		t.Fatal("params leaked to the log")
	}
}
