package central

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/apiclient"
	"github.com/mirastacklabs-ai/mirafleet/internal/keys"
	"github.com/mirastacklabs-ai/mirafleet/internal/opconf"
	"github.com/mirastacklabs-ai/mirafleet/internal/orch"
	"github.com/mirastacklabs-ai/mirafleet/internal/protover"
	"github.com/mirastacklabs-ai/mirafleet/internal/runner"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// Version skew in process: a real central, a real runner and a real orch that
// fake their protocol levels. Central is at level 6 unless a test says otherwise.

const (
	skewLow    = "this runner is protocol 4, central needs 5 or 6: upgrade mirafleet on that runner"
	skewHigh   = "central is protocol 6, this runner is 7: upgrade central first"
	skewMissng = "this runner reports no protocol version, central needs 5 or 6: upgrade mirafleet on that runner"
)

// skewName rewrites the "this runner" subject of the spec texts to the named form central uses.
func skewName(msg, name string) string {
	return strings.Replace(msg, "this runner", "runner `"+name+"`", 1)
}

// fakeClock is the runner's injected wait: After records each requested wait and blocks until the
// test releases it.
type fakeClock struct {
	mu    sync.Mutex
	waits []time.Duration
	gates []chan time.Time
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.mu.Lock()
	c.waits = append(c.waits, d)
	c.gates = append(c.gates, ch)
	c.mu.Unlock()
	return ch
}

func (c *fakeClock) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waits)
}

// release lets the i-th wait (0-based) elapse.
func (c *fakeClock) release(i int) { c.gates[i] <- time.Time{} }

type safeLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *safeLog) logf(f string, a ...any) {
	l.mu.Lock()
	fmt.Fprintf(&l.b, f+"\n", a...)
	l.mu.Unlock()
}

func (l *safeLog) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestSkewRunnerRefusedAudited(t *testing.T) {
	for _, c := range []struct {
		name  string
		proto int
		want  string
	}{
		{"n-2", 4, skewLow},
		{"n+1", 7, skewHigh},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, "r1")
			clk, rlog := &fakeClock{}, &safeLog{}
			e.startRunner("r1", func(cfg *runner.Config) {
				cfg.Proto, cfg.After, cfg.Logf = c.proto, clk.After, rlog.logf
				cfg.BackoffMin, cfg.BackoffMax = time.Second, 60*time.Second
			})
			want := skewName(c.want, "r1")
			waitFor(t, func() bool { return clk.count() == 1 })
			// The runner got the exact message and backs off to its maximum interval.
			if !strings.Contains(rlog.text(), want) {
				t.Fatalf("runner log lacks %q:\n%s", want, rlog.text())
			}
			if d := clk.waits[0]; d < 48*time.Second || d > 72*time.Second {
				t.Fatalf("first wait %s, want ~60s (BackoffMax +-20%%)", d)
			}
			// Refused means never registered, and exactly one audit row.
			if e.session("r1") != nil {
				t.Fatal("refused runner is online")
			}
			rows := e.auditRowsFor("runner.refused_version")
			if len(rows) != 1 || rows[0].Message != want || rows[0].Runner != "r1" || rows[0].Outcome != "denied" {
				t.Fatalf("audit rows: %+v", rows)
			}
			// No reconnect storm: nothing else dials while the clock is held.
			time.Sleep(300 * time.Millisecond)
			if n := len(e.auditRowsFor("runner.refused_version")); n != 1 || clk.count() != 1 {
				t.Fatalf("reconnected without the wait elapsing: %d rows, %d waits", n, clk.count())
			}
			// After the wait it tries once more and waits the maximum again (no ramp-up).
			clk.release(0)
			waitFor(t, func() bool { return clk.count() == 2 })
			if d := clk.waits[1]; d < 48*time.Second || d > 72*time.Second {
				t.Fatalf("second wait %s, want ~60s", d)
			}
			// The retry is refused again but audited once per runner per 10 minutes.
			if n := len(e.auditRowsFor("runner.refused_version")); n != 1 {
				t.Fatalf("audit rows after retry = %d, want 1 (throttled)", n)
			}
		})
	}
}

func TestSkewRunnerAccepted(t *testing.T) {
	for _, p := range []int{protover.Current, protover.Current - 1} {
		e := newEnv(t, "r1")
		e.startRunner("r1", func(cfg *runner.Config) { cfg.Proto = p })
		e.waitOnline("r1")
		if n := len(e.auditRowsFor("runner.refused_version")); n != 0 {
			t.Fatalf("proto %d: %d refusal rows", p, n)
		}
	}
}

// A heartbeat without proto is level 0: refused, with the message delivered on refused@mirafleet.
func TestSkewRunnerMissingProto(t *testing.T) {
	e := newEnv(t, "r1")
	nc, err := e.dialCapture(transport.UserRunner, "r1")
	must(t, err)
	defer nc.close()
	hb, _ := json.Marshal(transport.Heartbeat{Hostname: "old"}) // no Proto
	ok, _, err := nc.cl.SendRequest(transport.ReqHeartbeat, true, hb)
	if err == nil && ok {
		t.Fatal("heartbeat without proto accepted")
	}
	select {
	case msg := <-nc.refused:
		if want := skewName(skewMissng, "r1"); msg != want {
			t.Fatalf("refusal = %q, want %q", msg, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no refused@mirafleet request")
	}
	if rows := e.auditRowsFor("runner.refused_version"); len(rows) != 1 {
		t.Fatalf("audit rows: %+v", rows)
	}
	if e.session("r1") != nil {
		t.Fatal("runner without proto is online")
	}
}

// Central at another level shifts the accepted window.
func TestSkewCentralLevel(t *testing.T) {
	e := newEnvOpts(t, 15*time.Second, func(c *Config) { c.Proto = 7 }, "r1")
	clk, rlog := &fakeClock{}, &safeLog{}
	e.startRunner("r1", func(cfg *runner.Config) { cfg.Proto, cfg.After, cfg.Logf = 5, clk.After, rlog.logf })
	waitFor(t, func() bool { return clk.count() == 1 })
	want := "runner `r1` is protocol 5, central needs 6 or 7: upgrade mirafleet on that runner"
	if !strings.Contains(rlog.text(), want) {
		t.Fatalf("runner log lacks %q:\n%s", want, rlog.text())
	}
}

// rawConn is a raw runner connection that captures refused@mirafleet.
type rawConn struct {
	cl      *ssh.Client
	refused chan string
}

func (c *rawConn) close() { c.cl.Close() }

func (e *env) dialCapture(user, keyName string) (*rawConn, error) {
	nc, err := net.DialTimeout("tcp", e.addr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	cc, chans, reqs, err := ssh.NewClientConn(nc, e.addr, &ssh.ClientConfig{
		User: user, Auth: []ssh.AuthMethod{ssh.PublicKeys(e.signer(keyName))},
		HostKeyCallback: ssh.FixedHostKey(e.hostSigner.PublicKey()), Timeout: 5 * time.Second,
	})
	if err != nil {
		nc.Close()
		return nil, err
	}
	rc := &rawConn{refused: make(chan string, 1)}
	fwd := make(chan *ssh.Request)
	go func() {
		defer close(fwd)
		for r := range reqs {
			if r.Type == transport.ReqRefused {
				rc.refused <- string(r.Payload)
				if r.WantReply {
					r.Reply(true, nil)
				}
				continue
			}
			fwd <- r
		}
	}()
	rc.cl = ssh.NewClient(cc, chans, fwd)
	return rc, nil
}

func TestSkewOperator(t *testing.T) {
	e := newEnv(t)
	e.addOperator("alice")
	cl := e.opClient("alice")
	ctx := tctx(t)
	for _, p := range []int{6, 5} {
		res, err := cl.HelloProto(ctx, "test", p)
		if err != nil || res.Proto != 6 {
			t.Fatalf("proto %d: %v %+v", p, err, res)
		}
	}
	for _, c := range []struct {
		proto int
		want  string
	}{
		{4, "orchestrator `alice` is protocol 4, central needs 5 or 6: upgrade mirafleet on that orchestrator"},
		{7, "central is protocol 6, orchestrator `alice` is 7: upgrade central first"},
		{0, "orchestrator `alice` reports no protocol version, central needs 5 or 6: upgrade mirafleet on that orchestrator"},
	} {
		_, err := cl.HelloProto(ctx, "test", c.proto)
		var ae *api.Error
		if !errors.As(err, &ae) || ae.Code != api.CodeUnsupportedVersion || ae.Message != c.want {
			t.Fatalf("proto %d: %v, want %q", c.proto, err, c.want)
		}
	}
}

// An orchestrator behind central by a release gets the refusal as a tool error with the same text.
func TestSkewOrchestratorToolError(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir() + "/cfg"
	pub, err := keys.Generate(dir, opconf.KeyName, "bob")
	must(t, err)
	pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pub))
	must(t, err)
	_, _, err = e.db.RegisterOperator(context.Background(), "bob", pk.Marshal(), ssh.FingerprintSHA256(pk), time.Now(), auditEv("operator.register"))
	must(t, err)
	must(t, os.WriteFile(opconf.KnownHostsPath(dir), []byte(apiclient.KnownHostsLine(e.addr, e.hostSigner.PublicKey())+"\n"), 0o600))
	must(t, opconf.SaveContexts(dir, &opconf.Contexts{Current: "work", Contexts: []opconf.Context{{Name: "work", Addr: e.addr}}}))

	for _, c := range []struct {
		proto int
		want  string // "" = works
	}{
		{6, ""}, {5, ""},
		{4, "orchestrator `bob` is protocol 4, central needs 5 or 6: upgrade mirafleet on that orchestrator"},
		{7, "central is protocol 6, orchestrator `bob` is 7: upgrade central first"},
	} {
		proto := c.proto
		o, err := orch.New(orch.Config{ConfigDir: dir, StateDir: t.TempDir(), ProjectDir: t.TempDir(), KeepaliveInterval: time.Hour,
			Dial: func(ctx context.Context, addr string, signer ssh.Signer, hk ssh.HostKeyCallback) (orch.Conn, error) {
				cl, err := apiclient.Dial(ctx, addr, transport.UserOperator, signer, hk)
				if err != nil {
					return nil, err
				}
				if _, err := cl.HelloProto(ctx, "mcp", proto); err != nil {
					cl.Close()
					return nil, err
				}
				return &m5Conn{Client: cl, rec: &m5Rec{}}, nil
			}})
		must(t, err)
		_, err = o.FleetsList(m5ctx(t), "work")
		o.Close()
		if c.want == "" {
			if err != nil {
				t.Fatalf("proto %d: %v", c.proto, err)
			}
			continue
		}
		var oe *orch.Error
		if !errors.As(err, &oe) || oe.Code != api.CodeUnsupportedVersion || oe.Message != c.want {
			t.Fatalf("proto %d: %v, want unsupported_version %q", c.proto, err, c.want)
		}
	}
}
