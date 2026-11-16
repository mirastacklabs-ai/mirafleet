package runner

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/actions"
	"github.com/mirastacklabs-ai/mirafleet/internal/keys"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// fakeCentral accepts any key, then either drops the connection right after the handshake
// ("drop") or keeps it open and never answers requests ("mute").
type fakeCentral struct {
	addr  string
	mu    sync.Mutex
	times []time.Time

	replied atomic.Int64 // "delay:<d>" mode: requests answered so far
}

func (f *fakeCentral) connTimes() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Time(nil), f.times...)
}

func startFake(t *testing.T, mode string) (*fakeCentral, runnerKit) {
	t.Helper()
	dir := t.TempDir()
	if _, err := keys.Generate(dir, "host", "host"); err != nil {
		t.Fatal(err)
	}
	host, err := keys.LoadSigner(filepath.Join(dir, "host"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Generate(dir, "r", "r"); err != nil {
		t.Fatal(err)
	}
	signer, err := keys.LoadSigner(filepath.Join(dir, "r"))
	if err != nil {
		t.Fatal(err)
	}
	kh := filepath.Join(dir, "known_hosts")
	if err := writeFile(kh, "* "+keys.PubLine(host.PublicKey(), "")+"\n"); err != nil {
		t.Fatal(err)
	}
	sc := &ssh.ServerConfig{PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) { return nil, nil }}
	sc.AddHostKey(host)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeCentral{addr: ln.Addr().String()}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				conn, chans, reqs, err := ssh.NewServerConn(nc, sc)
				if err != nil {
					nc.Close()
					return
				}
				f.mu.Lock()
				f.times = append(f.times, time.Now())
				f.mu.Unlock()
				go func() {
					for c := range chans {
						c.Reject(ssh.Prohibited, "")
					}
				}()
				if mode == "drop" {
					conn.Close()
					return
				}
				if d, ok := strings.CutPrefix(mode, "delay:"); ok {
					delay, _ := time.ParseDuration(d)
					for req := range reqs { // reply to every request, but only after delay
						go func() {
							time.Sleep(delay)
							req.Reply(true, nil)
							f.replied.Add(1)
						}()
					}
					return
				}
				for range reqs { // mute: never reply
				}
			}()
		}
	}()
	return f, runnerKit{signer, kh}
}

type runnerKit struct {
	signer ssh.Signer
	kh     string
}

func runFake(t *testing.T, f *fakeCentral, kit runnerKit, mod func(*Config)) {
	t.Helper()
	acts, err := actions.Parse([]byte(`{actions: {}}`))
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Central: f.addr, Name: "r", Signer: kit.signer, KnownHosts: kit.kh, Actions: acts}
	mod(&cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { New(cfg).Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

// A handshake-then-drop central must not reset the backoff; waits grow.
func TestBackoffGrowsWhenConnectionNeverHealthy(t *testing.T) {
	f, kit := startFake(t, "drop")
	runFake(t, f, kit, func(c *Config) {
		c.HeartbeatEvery = time.Second
		c.BackoffMin, c.BackoffMax = 50*time.Millisecond, 2*time.Second
	})
	deadline := time.Now().Add(10 * time.Second)
	for len(f.connTimes()) < 5 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	ts := f.connTimes()
	if len(ts) < 5 {
		t.Fatalf("only %d connections", len(ts))
	}
	// Only lower bounds are asserted: host load can delay a reconnect but never shorten a wait,
	// so these cannot flake. Nominal gaps are 50, 100, 200, 400ms (jitter -20% at worst); a
	// constant 50ms backoff would give a ~200ms total.
	last, total := ts[4].Sub(ts[3]), ts[4].Sub(ts[0])
	if last < 320*time.Millisecond || total < 600*time.Millisecond {
		t.Fatalf("backoff did not grow: fourth gap %v (want >= 320ms), total %v (want >= 600ms)", last, total)
	}
}

// A central that never replies to the heartbeat gets dropped and redialled.
func TestHeartbeatReplyTimeoutReconnects(t *testing.T) {
	f, kit := startFake(t, "mute")
	runFake(t, f, kit, func(c *Config) {
		c.HeartbeatEvery = 50 * time.Millisecond
		c.BackoffMin, c.BackoffMax = 20*time.Millisecond, 100*time.Millisecond
	})
	deadline := time.Now().Add(10 * time.Second)
	for len(f.connTimes()) < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := len(f.connTimes()); n < 3 {
		t.Fatalf("runner did not reconnect after heartbeat timeout (%d connections)", n)
	}
}

// A reply later than HeartbeatTimeout drops the connection (runner redials).
func TestHeartbeatTimeoutDropsSlowReply(t *testing.T) {
	f, kit := startFake(t, "delay:3s")
	runFake(t, f, kit, func(c *Config) {
		c.HeartbeatEvery = 50 * time.Millisecond
		c.HeartbeatTimeout = 200 * time.Millisecond
		c.BackoffMin, c.BackoffMax = 20*time.Millisecond, 100*time.Millisecond
	})
	deadline := time.Now().Add(10 * time.Second)
	for len(f.connTimes()) < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := len(f.connTimes()); n < 3 {
		t.Fatalf("runner did not drop a reply later than HeartbeatTimeout (%d connections)", n)
	}
}

// A reply later than 2*HeartbeatEvery but within HeartbeatTimeout keeps the connection. Only an
// absence is asserted, so host load cannot flake it (the 10s budget is never near).
func TestHeartbeatSlowReplyWithinTimeoutKeepsConnection(t *testing.T) {
	f, kit := startFake(t, "delay:300ms") // > 2*50ms
	runFake(t, f, kit, func(c *Config) {
		c.HeartbeatEvery = 50 * time.Millisecond
		c.HeartbeatTimeout = 10 * time.Second
		c.BackoffMin, c.BackoffMax = 20*time.Millisecond, 100*time.Millisecond
	})
	deadline := time.Now().Add(20 * time.Second)
	for f.replied.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := f.replied.Load(); n < 3 {
		t.Fatalf("only %d slow replies seen", n)
	}
	if n := len(f.connTimes()); n != 1 {
		t.Fatalf("runner dropped the connection despite replies within HeartbeatTimeout (%d connections)", n)
	}
}

func TestJitterBounds(t *testing.T) {
	d := time.Second
	lo, hi := d, d
	for i := 0; i < 2000; i++ {
		j := jitter(d)
		lo, hi = min(lo, j), max(hi, j)
		if j < 800*time.Millisecond || j > 1200*time.Millisecond {
			t.Fatalf("jitter %v out of +-20%%", j)
		}
	}
	if lo == hi {
		t.Fatal("no jitter applied")
	}
}

func TestBackoffMaxClamped(t *testing.T) {
	r := New(Config{BackoffMin: 5 * time.Minute})
	if r.cfg.BackoffMax < r.cfg.BackoffMin {
		t.Fatalf("max %v < min %v", r.cfg.BackoffMax, r.cfg.BackoffMin)
	}
	r = New(Config{BackoffMin: 10 * time.Second, BackoffMax: time.Second})
	if r.cfg.BackoffMax != 10*time.Second {
		t.Fatalf("max %v", r.cfg.BackoffMax)
	}
	if r = New(Config{}); r.cfg.MaxExec != 4 || r.cfg.BackoffMax != 60*time.Second {
		t.Fatalf("defaults: %+v", r.cfg)
	}
}

// Worst-case escaping of a capped result stays under central's read limit, unescaped HTML.
func TestEncodeResultWorstCase(t *testing.T) {
	ctrl := strings.Repeat("\x01", actions.MaxOutput) // 6 bytes each once escaped
	b := encodeResult(&transport.ExecResult{Stdout: ctrl[:actions.MaxOutput/2], Stderr: ctrl[:actions.MaxOutput/2], Message: "m"})
	if len(b) >= transport.MaxResultBytes || len(b) < 5<<20 {
		t.Fatalf("len=%d", len(b))
	}
	b = encodeResult(&transport.ExecResult{Stdout: "<a&b>"})
	if !strings.Contains(string(b), "<a&b>") {
		t.Fatalf("HTML escaped: %s", b)
	}
	big := &transport.ExecResult{Stdout: strings.Repeat("\x01", 2<<20)} // beyond contract
	if b = encodeResult(big); len(b) > 1024 || !strings.Contains(string(b), "too large") {
		t.Fatalf("oversized not replaced: %d", len(b))
	}
}

func writeFile(p, s string) error { return os.WriteFile(p, []byte(s), 0o600) }

// An actions-mode runner advertises its actions (bounded, valid); a workspace-mode or
// unconfigured runner sends none.
func TestFillActions(t *testing.T) {
	set, err := actions.Parse([]byte(`{actions: {b: {run: ["/bin/echo", "{{x}}"], params: {x: {pattern: "[a-z]+", default: "d"}}}, a: {run: ["/usr/bin/true"]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	var hb transport.Heartbeat
	(&Runner{cfg: Config{Actions: set}}).fillActions(&hb)
	if len(hb.Actions) != 2 || hb.Actions[0].Name != "a" || hb.Actions[1].Name != "b" ||
		len(hb.Actions[1].Params) != 1 || hb.Actions[1].Params[0].Default == nil || *hb.Actions[1].Params[0].Default != "d" {
		t.Fatalf("actions: %+v", hb.Actions)
	}
	if err := hb.Validate(); err != nil {
		t.Fatal(err)
	}
	var none transport.Heartbeat
	(&Runner{}).fillActions(&none)
	if none.Actions != nil {
		t.Fatalf("no actions config sent actions: %+v", none.Actions)
	}
	// Oversize entries are dropped rather than breaking the heartbeat.
	big, err := actions.Parse([]byte(`{actions: {ok: {run: ["/usr/bin/true"]}, "` + strings.Repeat("n", 65) + `": {run: ["/usr/bin/true"]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	var h2 transport.Heartbeat
	(&Runner{cfg: Config{Actions: big}}).fillActions(&h2)
	if len(h2.Actions) != 1 || h2.Actions[0].Name != "ok" || h2.Validate() != nil {
		t.Fatalf("oversize not dropped: %+v", h2.Actions)
	}
}

// A huge actions.yaml must not push the heartbeat over the byte cap or stop registration.
func TestFillActionsByteBudget(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("{actions: {")
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&sb, "a%03d: {run: [\"/usr/bin/true\"], params: {", i)
		for j := 0; j < 8; j++ {
			fmt.Fprintf(&sb, "p%d: {pattern: \"%s\", default: \"x\"}, ", j, "x"+strings.Repeat("|x", 120))
		}
		sb.WriteString("}}, ")
	}
	sb.WriteString("}}")
	set, err := actions.Parse([]byte(sb.String()))
	if err != nil {
		t.Fatal(err)
	}
	var logged string
	r := &Runner{cfg: Config{Actions: set, Logf: func(f string, a ...any) { logged = fmt.Sprintf(f, a...) }}}
	var hb transport.Heartbeat
	r.fillActions(&hb)
	if len(hb.Actions) == 0 || len(hb.Actions) >= 100 {
		t.Fatalf("actions kept: %d", len(hb.Actions))
	}
	if n := transport.ActionsEncodedSize(hb.Actions); n > transport.MaxActionsBytes {
		t.Fatalf("encoded %d > cap", n)
	}
	if err := hb.Validate(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logged, "left out") {
		t.Fatalf("nothing logged: %q", logged)
	}
}
