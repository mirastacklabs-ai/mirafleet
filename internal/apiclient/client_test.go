package apiclient

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/tokens"
)

func dialFake(t *testing.T, f *fakeServer) *Client {
	t.Helper()
	c, err := Dial(context.Background(), f.addr, UserOperator, newSigner(t), ssh.FixedHostKey(f.hostKey))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestCallAndHelpers(t *testing.T) {
	f := startFake(t, func(typ string, ch ssh.Channel, req *api.Request) {
		if typ != ChanAPI || req == nil {
			t.Errorf("chan %q req %v", typ, req)
			return
		}
		switch req.Method {
		case api.MethodHello:
			reply(t, ch, req, api.HelloResult{Operator: "alice", Versions: api.SupportedVersions})
		case api.MethodFleetsList:
			reply(t, ch, req, api.FleetsListResult{Fleets: []api.FleetInfo{{Name: "prod", Mode: "actions", Runners: 2}}})
		case api.MethodRunnersList:
			var p api.RunnersListParams
			if e := api.DecodeParams(req.Params, &p); e != nil || p.Fleet != "prod" {
				t.Errorf("params %v %v", e, p)
			}
			reply(t, ch, req, api.RunnersListResult{Runners: []api.RunnerInfo{{Name: "r1", Status: "active", Online: true}}})
		case api.MethodActionRun:
			var p api.ActionRunParams
			api.DecodeParams(req.Params, &p)
			reply(t, ch, req, api.ActionRunResult{Stdout: p.Action + ":" + p.Confirm, DurationMS: 3})
		default:
			resp := api.NewError(req.ID, api.Errorf(api.CodeNotFound, "no such method"))
			api.WriteResponse(ch, resp)
		}
	})
	c := dialFake(t, f)
	ctx := context.Background()
	h, err := c.Hello(ctx, "test")
	if err != nil || h.Operator != "alice" {
		t.Fatal(h, err)
	}
	fl, err := c.FleetsList(ctx)
	if err != nil || len(fl.Fleets) != 1 || fl.Fleets[0].Name != "prod" {
		t.Fatal(fl, err)
	}
	rl, err := c.RunnersList(ctx, "prod")
	if err != nil || rl.Runners[0].Name != "r1" {
		t.Fatal(rl, err)
	}
	ar, err := c.ActionRun(ctx, api.ActionRunParams{Fleet: "prod", Runner: "r1", Action: "uptime", Confirm: "prod"})
	if err != nil || ar.Stdout != "uptime:prod" {
		t.Fatal(ar, err)
	}
	// Wire error surfaces as *api.Error.
	err = c.Call(ctx, "bogus", nil, nil)
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Code != api.CodeNotFound {
		t.Fatalf("got %v", err)
	}
	if f.lastUser() != UserOperator {
		t.Fatal(f.lastUser())
	}
	// Concurrent calls each get their own channel.
	errs := make(chan error, 6)
	for i := 0; i < 6; i++ {
		go func() { _, err := c.Hello(ctx, "x"); errs <- err }()
	}
	for i := 0; i < 6; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

func TestCallContextCancel(t *testing.T) {
	started := make(chan struct{}, 1)
	released := make(chan struct{})
	f := startFake(t, func(_ string, ch ssh.Channel, req *api.Request) {
		started <- struct{}{}
		// Hold the request until the client goes away; a closed channel unblocks the read.
		ch.Read(make([]byte, 1))
		close(released)
	})
	c := dialFake(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Call(ctx, api.MethodHello, nil, nil) }()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Call did not return after cancel")
	}
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("server never saw the channel close")
	}
	// Already-cancelled context: no channel opened.
	n := f.channels.Load()
	if err := c.Call(ctx, api.MethodHello, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if f.channels.Load() != n {
		t.Fatal("channel opened for cancelled ctx")
	}
}

func TestCallOversizedResponse(t *testing.T) {
	f := startFake(t, func(_ string, ch ssh.Channel, req *api.Request) {
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], uint32(api.MaxResponseBytes+1))
		ch.Write(hdr[:])
		ch.Write(bytes.Repeat([]byte("a"), 1024))
	})
	c := dialFake(t, f)
	err := c.Call(context.Background(), api.MethodHello, nil, nil)
	if !errors.Is(err, api.ErrFrameTooLarge) {
		t.Fatalf("got %v", err)
	}
}

func TestCallBadResponses(t *testing.T) {
	for name, payload := range map[string]string{
		"wrong id":    `{"v":1,"id":"nope","result":{}}`,
		"wrong ver":   `{"v":2,"id":"x","result":{}}`,
		"garbage":     `not json`,
		"no result":   `{"v":1,"id":"x"}`,
		"unknown err": `{"v":1,"id":"x","error":{"code":"forbidden","message":"no"}}`,
	} {
		f := startFake(t, func(_ string, ch ssh.Channel, req *api.Request) {
			p := strings.Replace(payload, `"id":"x"`, `"id":"`+req.ID+`"`, 1)
			api.WriteFrame(ch, []byte(p))
		})
		c := dialFake(t, f)
		if err := c.Call(context.Background(), api.MethodHello, nil, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCallRejectsOversizedRequest(t *testing.T) {
	f := startFake(t, func(string, ssh.Channel, *api.Request) {})
	c := dialFake(t, f)
	big := map[string]string{"k": strings.Repeat("a", api.MaxRequestBytes)}
	if err := c.Call(context.Background(), api.MethodActionRun, big, nil); err == nil {
		t.Fatal("accepted")
	}
	if f.channels.Load() != 0 {
		t.Fatal("channel opened")
	}
}

func TestDialRequiresHostKeyCallback(t *testing.T) {
	if _, err := Dial(context.Background(), "127.0.0.1:1", UserOperator, newSigner(t), nil); err == nil {
		t.Fatal("nil callback accepted")
	}
}

func TestJoinPinned(t *testing.T) {
	var got api.EnrollJoinParams
	f := startFake(t, func(typ string, ch ssh.Channel, req *api.Request) {
		if typ != ChanJoin || req.Method != api.MethodEnrollJoin {
			t.Errorf("%s %s", typ, req.Method)
		}
		api.DecodeParams(req.Params, &got)
		reply(t, ch, req, api.EnrollJoinResult{Fleet: "prod", Runner: got.Name, Status: "pending"})
	})
	tok, _, _, err := tokens.NewJoinToken(f.hostKey)
	if err != nil {
		t.Fatal(err)
	}
	res, hk, err := Join(context.Background(), f.addr, tok, "web-1", map[string]string{"role": "web"}, newSigner(t))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "pending" || got.Token != tok || got.Name != "web-1" || got.Labels["role"] != "web" {
		t.Fatal(res, got)
	}
	if !bytes.Equal(hk.Marshal(), f.hostKey.Marshal()) {
		t.Fatal("host key not returned")
	}
	if f.lastUser() != UserEnroll {
		t.Fatal(f.lastUser())
	}
}

func TestRegisterPinned(t *testing.T) {
	var got api.EnrollRegisterParams
	f := startFake(t, func(typ string, ch ssh.Channel, req *api.Request) {
		if typ != ChanRegister || req.Method != api.MethodEnrollRegister {
			t.Errorf("%s %s", typ, req.Method)
		}
		api.DecodeParams(req.Params, &got)
		reply(t, ch, req, api.EnrollRegisterResult{Operator: got.Name, Status: "active"})
	})
	code, err := tokens.Code(bytes.Repeat([]byte{5}, 32), tokens.Step(time.Now()), f.hostKey)
	if err != nil {
		t.Fatal(err)
	}
	res, _, err := Register(context.Background(), f.addr, code, "alice", newSigner(t))
	if err != nil || res.Status != "active" || got.Code != code || got.Name != "alice" {
		t.Fatal(res, got, err)
	}
}

// A wrong host key must be refused during the handshake, before authentication or any channel:
// nothing (token, code, key signature) reaches the impostor.
func TestEnrollWrongHostKeyRefusedBeforeAnyData(t *testing.T) {
	f := startFake(t, func(string, ssh.Channel, *api.Request) { t.Error("handler reached") })
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	other, _ := ssh.NewPublicKey(pub)
	tok, _, _, _ := tokens.NewJoinToken(other)
	if _, _, err := Join(context.Background(), f.addr, tok, "web-1", nil, newSigner(t)); err == nil || !strings.Contains(err.Error(), "host key") {
		t.Fatalf("join: %v", err)
	}
	code, err := tokens.Code(bytes.Repeat([]byte{5}, 32), 1, other)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Register(context.Background(), f.addr, code, "alice", newSigner(t)); err == nil {
		t.Fatal("register accepted wrong host key")
	}
	time.Sleep(100 * time.Millisecond)
	if f.authed.Load() != 0 || f.channels.Load() != 0 {
		t.Fatalf("data sent: authed=%d channels=%d", f.authed.Load(), f.channels.Load())
	}
}

func TestEnrollMalformedInputsNeverDial(t *testing.T) {
	f := startFake(t, func(string, ssh.Channel, *api.Request) {})
	if _, _, err := Join(context.Background(), f.addr, "junk", "n", nil, newSigner(t)); err == nil {
		t.Fatal("join junk")
	}
	if _, _, err := Register(context.Background(), f.addr, "junk", "n", newSigner(t)); err == nil {
		t.Fatal("register junk")
	}
	if f.authed.Load() != 0 {
		t.Fatal("dialled")
	}
}

func TestHostKeyCallbacks(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	k, _ := ssh.NewPublicKey(pub)
	pub2, _, _ := ed25519.GenerateKey(rand.Reader)
	k2, _ := ssh.NewPublicKey(pub2)
	if err := PinnedHash(tokens.HostKeyHash(k))("h", nil, k); err != nil {
		t.Fatal(err)
	}
	if err := PinnedHash(tokens.HostKeyHash(k))("h", nil, k2); !errors.Is(err, ErrHostKeyMismatch) {
		t.Fatal(err)
	}
	if err := PinnedHash("short")("h", nil, k); err == nil {
		t.Fatal("malformed hash accepted")
	}
	for _, fp := range []string{ssh.FingerprintSHA256(k), tokens.HostKeyHash(k)} {
		if err := Fingerprint(fp)("h", nil, k); err != nil {
			t.Fatal(fp, err)
		}
		if err := Fingerprint(fp)("h", nil, k2); err == nil {
			t.Fatal("wrong fp accepted")
		}
	}
	// KnownHosts: pinned key accepted, other refused, missing file refused.
	p := t.TempDir() + "/kh"
	if err := os.WriteFile(p, []byte(KnownHostsLine("127.0.0.1:2222", k)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := KnownHosts(p)("127.0.0.1:2222", nil, k); err != nil {
		t.Fatal(err)
	}
	if KnownHosts(p)("127.0.0.1:2222", nil, k2) == nil || KnownHosts(p+"x")("127.0.0.1:2222", nil, k) == nil {
		t.Fatal("unpinned accepted")
	}
}

func TestPromptRefusesWithoutTTY(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	k, _ := ssh.NewPublicKey(pub)
	var out bytes.Buffer
	// Even if stdin contains "yes", a non-TTY must refuse and must not print a prompt.
	if err := prompt(false, strings.NewReader("yes\n"), &out, "h", k); !errors.Is(err, ErrNoTTY) || out.Len() != 0 {
		t.Fatal(err, out.String())
	}
	// Public entry point with a regular file, a pipe and the null device: all refused.
	f, _ := os.Create(t.TempDir() + "/in")
	defer f.Close()
	r, w, _ := os.Pipe()
	defer r.Close()
	defer w.Close()
	null, _ := os.Open(os.DevNull)
	defer null.Close()
	for _, in := range []*os.File{f, r, null, nil} {
		if IsTTY(in) {
			t.Fatalf("%v reported as TTY", in)
		}
		if err := Prompt(in, &out)("h", nil, k); !errors.Is(err, ErrNoTTY) {
			t.Fatal(err)
		}
	}
	// With a TTY: only exact "yes" accepts.
	for ans, ok := range map[string]bool{"yes\n": true, "y\n": false, "YES\n": false, "\n": false, "": false, "no\n": false} {
		err := prompt(true, strings.NewReader(ans), &out, "h", k)
		if (err == nil) != ok {
			t.Errorf("answer %q: %v", ans, err)
		}
	}
	// An endless reader must terminate with a refusal after at most maxAnswerBytes.
	er := &endless{}
	if err := prompt(true, er, &out, "h", k); err == nil || er.n > maxAnswerBytes {
		t.Fatalf("endless stdin: err=%v read=%d", err, er.n)
	}
	if !strings.Contains(out.String(), "SHA256:") {
		t.Fatal("fingerprint not shown")
	}
}

// endless yields 'a' forever, counting bytes handed out.
type endless struct{ n int }

func (e *endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	e.n += len(p)
	return len(p), nil
}
