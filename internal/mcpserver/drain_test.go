package mcpserver

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests drive pump / tracker / framer directly, without the SDK: the "server" is the test,
// which writes replies through the same replyWriter Serve uses.

type rig struct {
	inW    *io.PipeWriter
	tr     *tracker
	rw     replyWriter
	closed chan struct{} // closed when the downstream pipe reaches EOF
	cancel context.CancelFunc
	done   chan struct{} // the SDK-session-ended signal
	mu     sync.Mutex
	got    bytes.Buffer
}

func newRig(t *testing.T) *rig {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	inR, inW := io.Pipe()
	pr, pw := io.Pipe()
	r := &rig{inW: inW, tr: newTracker(), closed: make(chan struct{}), cancel: cancel, done: make(chan struct{})}
	r.rw = replyWriter{w: io.Discard, t: r.tr}
	go pump(ctx, inR, pw, r.tr, r.done)
	go func() {
		b := make([]byte, 4096)
		for {
			n, err := pr.Read(b)
			r.mu.Lock()
			r.got.Write(b[:n])
			r.mu.Unlock()
			if err != nil {
				close(r.closed)
				return
			}
		}
	}()
	t.Cleanup(func() { cancel(); inW.Close(); pr.Close() })
	return r
}

func (r *rig) send(t *testing.T, s string) {
	t.Helper()
	if _, err := io.WriteString(r.inW, s); err != nil {
		t.Fatal(err)
	}
}

func (r *rig) reply(t *testing.T, s string) {
	t.Helper()
	if _, err := r.rw.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
}

func (r *rig) forwarded() string { r.mu.Lock(); defer r.mu.Unlock(); return r.got.String() }

// stillOpen asserts EOF is being held back. The short sleep only gives a wrong early release time
// to show; a correct pump passes regardless of timing.
func (r *rig) stillOpen(t *testing.T) {
	t.Helper()
	select {
	case <-r.closed:
		t.Fatal("EOF released while a call was outstanding")
	case <-time.After(40 * time.Millisecond):
	}
}

func (r *rig) released(t *testing.T) {
	t.Helper()
	select {
	case <-r.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("EOF never released")
	}
}

func setVar[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

func call(id, method string) string {
	return `{"jsonrpc":"2.0","id":` + id + `,"method":"` + method + `"}`
}
func result(id string) string { return `{"jsonrpc":"2.0","id":` + id + `,"result":{}}` + "\n" }

func TestPumpHoldsEOFUntilReply(t *testing.T) {
	r := newRig(t)
	in := call("1", "initialize") + "\n" + `{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" + call("2", "tools/list") + "\n"
	r.send(t, in)
	r.inW.Close()
	r.stillOpen(t)
	r.reply(t, result("1"))
	r.stillOpen(t)
	r.reply(t, result("2"))
	r.released(t)
	if got := r.forwarded(); got != in {
		t.Fatalf("bytes were altered: %q", got)
	}
}

func TestPumpToolsCallNotHeld(t *testing.T) {
	r := newRig(t)
	r.send(t, call("1", "tools/call")+"\n")
	r.inW.Close()
	r.released(t) // cancelled at EOF as before, never waited on
}

func TestPumpFramings(t *testing.T) {
	multi := "{\n \"jsonrpc\": \"2.0\",\n \"id\": 7,\n \"method\": \"tools/list\"\n}\n"
	cases := map[string]string{
		"crlf":             call("7", "tools/list") + "\r\n",
		"no_final_newline": call("7", "tools/list"),
		"multi_line":       multi,
		"concatenated":     call("6", "ping") + call("7", "tools/list"), // no separator at all
		"string_with_brace": `{"jsonrpc":"2.0","id":7,"method":"tools/list","params":{"s":"}]\\\"{"}}` +
			"\n",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.send(t, in)
			r.inW.Close()
			r.stillOpen(t)
			if name == "concatenated" {
				r.reply(t, result("6"))
				r.stillOpen(t)
			}
			r.reply(t, result("7"))
			r.released(t)
		})
	}
}

// Calls the tracker could not see must not offset ones it did (the old counters compared totals).
func TestPumpSetNotCount(t *testing.T) {
	r := newRig(t)
	r.send(t, call("5", "tools/list")+"\n")
	r.inW.Close()
	r.reply(t, result("99")) // reply to something never tracked
	r.reply(t, `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"dup"}}`+"\n")
	r.stillOpen(t)
	r.reply(t, result("5"))
	r.released(t)
}

func TestPumpDuplicateID(t *testing.T) {
	r := newRig(t)
	r.send(t, call("1", "tools/list")+"\n"+call("1", "tools/list")+"\n")
	r.inW.Close()
	r.stillOpen(t)
	// The SDK answers the first call and sends an empty-id error for the duplicate.
	r.reply(t, result("1"))
	r.released(t)
}

func TestPumpBatch(t *testing.T) {
	r := newRig(t)
	r.send(t, "["+call("1", "initialize")+","+`{"jsonrpc":"2.0","method":"notifications/initialized"},`+call("2", "ping")+"]\n")
	r.inW.Close()
	r.stillOpen(t)
	r.reply(t, result("1"))
	r.stillOpen(t)
	r.reply(t, "["+strings.TrimSpace(result("2"))+"]\n")
	r.released(t)
}

func TestPumpBatchReplyClearsAll(t *testing.T) {
	r := newRig(t)
	r.send(t, "["+call("1", "ping")+","+call("2", "ping")+"]\n")
	r.inW.Close()
	r.stillOpen(t)
	r.reply(t, "["+strings.TrimSpace(result("1"))+","+strings.TrimSpace(result("2"))+"]\n")
	r.released(t)
}

func TestPumpStringIDAndKeyOrder(t *testing.T) {
	r := newRig(t)
	r.send(t, `{"jsonrpc":"2.0","id":"abc","method":"ping"}`+"\n")
	r.inW.Close()
	r.stillOpen(t)
	// payload before id: replyID must skip it and still find the id
	r.reply(t, `{"jsonrpc":"2.0","result":{"a":[1,2,{"id":"zzz"}]},"id":"abc"}`+"\n")
	r.released(t)
}

func TestPumpTimeout(t *testing.T) {
	setVar(t, &drainTimeout, 60*time.Millisecond)
	r := newRig(t)
	start := time.Now()
	r.send(t, call("1", "tools/list")+"\n")
	r.inW.Close()
	r.released(t) // never answered
	if d := time.Since(start); d < 60*time.Millisecond {
		t.Fatalf("released after %v, before the cap", d)
	}
}

func TestPumpCtxCancelMidWait(t *testing.T) {
	setVar(t, &drainTimeout, time.Hour)
	r := newRig(t)
	r.send(t, call("1", "tools/list")+"\n")
	r.inW.Close()
	r.stillOpen(t)
	r.cancel()
	r.released(t)
}

func TestPumpDoneChannelExit(t *testing.T) {
	setVar(t, &drainTimeout, time.Hour)
	r := newRig(t)
	r.send(t, call("1", "tools/list")+"\n")
	r.inW.Close()
	r.stillOpen(t)
	close(r.done) // the SDK session ended on its own
	r.released(t)
}

// Bytes reach the SDK as they arrive: no newline, no complete value needed.
func TestPumpForwardsImmediately(t *testing.T) {
	r := newRig(t)
	part := `{"jsonrpc":"2.0","id":1,"meth`
	r.send(t, part)
	deadline := time.After(5 * time.Second)
	for r.forwarded() != part {
		select {
		case <-deadline:
			t.Fatalf("forwarded %q", r.forwarded())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestFramerCapBoundsMemory(t *testing.T) {
	var seen []string
	f := &framer{max: 64, fn: func(b []byte) { seen = append(seen, string(b)) }}
	big := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"s":"` + strings.Repeat("x", 100000) + `"}}`
	maxBuf := 0
	for i := 0; i < len(big); i += 1000 {
		end := min(i+1000, len(big))
		f.feed([]byte(big[i:end]))
		maxBuf = max(maxBuf, cap(f.buf))
	}
	small := `{"id":2,"method":"ping"}`
	f.feed([]byte("\n" + small + "\n"))
	if len(seen) != 1 || seen[0] != small {
		t.Fatalf("seen = %q (oversized value must be skipped, the next one parsed)", seen)
	}
	if maxBuf > 256 {
		t.Fatalf("buffered %d bytes for a 64-byte cap", maxBuf)
	}
}

func TestPumpOversizedNotTracked(t *testing.T) {
	setVar(t, &maxFrame, 64)
	r := newRig(t)
	r.send(t, `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"s":"`+strings.Repeat("x", 500)+`"}}`+"\n")
	r.inW.Close()
	r.released(t) // the SDK errors on it; there is nothing to wait for
}

// A frame past the SDK's real 16 MiB cap: skipped, not buffered, and the stream recovers.
func TestFramerOver16MiB(t *testing.T) {
	if testing.Short() {
		t.Skip("17 MiB")
	}
	var seen int
	f := &framer{max: maxFrame, fn: func([]byte) { seen++ }}
	f.feed([]byte(`{"id":1,"method":"ping","params":{"s":"`))
	chunk := bytes.Repeat([]byte("x"), 1<<20)
	maxBuf := 0
	for i := 0; i < 17; i++ {
		f.feed(chunk)
		maxBuf = max(maxBuf, cap(f.buf))
	}
	f.feed([]byte(`"}}` + "\n" + `{"id":2,"method":"ping"}` + "\n"))
	if seen != 1 {
		t.Fatalf("callbacks = %d, want 1 (only the small frame)", seen)
	}
	if maxBuf > 2*maxFrame {
		t.Fatalf("buffered %d bytes", maxBuf)
	}
}

func TestReplyID(t *testing.T) {
	cases := []struct {
		in   string
		id   string
		want bool
	}{
		{`{"jsonrpc":"2.0","id":1,"result":{}}`, "1", true},
		{`{"jsonrpc":"2.0","id":"a","error":{"code":1}}`, `"a"`, true},
		{`{"result":{"x":1},"id":3,"jsonrpc":"2.0"}`, "3", true},
		{`{"jsonrpc":"2.0","id":null,"error":{}}`, "", false},
		{`{"jsonrpc":"2.0","method":"notifications/progress","params":{}}`, "", false},
		{`{"jsonrpc":"2.0","id":1,"method":"ping"}`, "", false}, // a server-to-client request
		{`not json`, "", false},
	}
	for _, c := range cases {
		id, ok := replyID([]byte(c.in))
		if id != c.id || ok != c.want {
			t.Errorf("replyID(%s) = %q,%v want %q,%v", c.in, id, ok, c.id, c.want)
		}
	}
}
