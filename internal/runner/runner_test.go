package runner

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs/mirafleet/internal/actions"
	"github.com/mirastacklabs/mirafleet/internal/keys"
	"github.com/mirastacklabs/mirafleet/internal/transport"
)

// fakeCentral accepts any key, then either drops the connection right after the handshake
// ("drop") or keeps it open and never answers requests ("mute").
type fakeCentral struct {
	addr  string
	mu    sync.Mutex
	times []time.Time
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

// Q17: a handshake-then-drop central must not reset the backoff; waits grow.
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
	first, last := ts[1].Sub(ts[0]), ts[4].Sub(ts[3]) // nominal 50ms vs 400ms (+-20%)
	if last < 2*first {
		t.Fatalf("backoff did not grow: first gap %v, fourth gap %v", first, last)
	}
}

// Q06: a central that never replies to the heartbeat gets dropped and redialled.
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

// Q08: worst-case escaping of a capped result stays under central's read limit, unescaped HTML.
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
