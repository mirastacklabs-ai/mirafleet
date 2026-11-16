package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strconv"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Why this file exists: when the SDK's reader hits EOF, jsonrpc2 cancels every call it has queued
// or is running asynchronously (conn.go, readIncoming/handleAsync). A client that writes
// initialize, notifications/initialized and tools/list and then closes stdin would get no
// answers, or a skipped initialize that leaves the queued initialized notification running
// against an uninitialized session. So Serve reads stdin through [pump], which forwards the bytes
// to the SDK untouched and holds the EOF back until the "quick" calls already received have been
// answered. tools/call is not held: it can run for minutes and is cancelled at EOF as before.

// drainTimeout bounds the EOF hold. Quick calls (initialize, tools/list, ping) answer in
// milliseconds; the bound only protects exit time. Worst case after stdin EOF is this 2 s plus
// the cmd's orch.Close grace (mcpShutdownGrace, 5 s): 7 s. Variable for tests.
var drainTimeout = 2 * time.Second

// maxFrame is the largest single JSON-RPC frame the SDK accepts (its decoder fails past it), so it
// is also the most this file ever buffers for one message.
var maxFrame = mcp.DefaultMaxLineLength

// keepBuf is the largest frame buffer the framer keeps between frames.
const keepBuf = 64 << 10

// drained reports whether a call with this method is held back at EOF.
func drained(method string) bool { return method != "" && method != "tools/call" }

// tracker is the set of outstanding drained-call ids: added when a request is read from stdin,
// removed when its reply is written to stdout. Ids are normalised by [idKey].
type tracker struct {
	mu     sync.Mutex
	ids    map[string]struct{}
	notify chan struct{} // 1-buffered; poked whenever the set becomes empty
}

func newTracker() *tracker {
	return &tracker{ids: map[string]struct{}{}, notify: make(chan struct{}, 1)}
}

func (t *tracker) add(id string) {
	t.mu.Lock()
	t.ids[id] = struct{}{}
	t.mu.Unlock()
}

// remove deletes id if present (a reply to an untracked call, or an empty-id error reply, is a no-op).
func (t *tracker) remove(id string) {
	t.mu.Lock()
	if _, ok := t.ids[id]; ok {
		delete(t.ids, id)
		if len(t.ids) == 0 {
			select {
			case t.notify <- struct{}{}:
			default:
			}
		}
	}
	t.mu.Unlock()
}

func (t *tracker) size() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.ids)
}

// wait blocks until the set is empty, d elapses, ctx is done or done is closed.
func (t *tracker) wait(ctx context.Context, done <-chan struct{}, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for t.size() > 0 {
		select {
		case <-t.notify:
		case <-timer.C:
			return
		case <-ctx.Done():
			return
		case <-done:
			return
		}
	}
}

// header is the part of a JSON-RPC message the tracker needs.
type header struct {
	id        json.RawMessage
	hasID     bool
	method    string
	hasMethod bool
	hasReply  bool // a "result" or "error" key
}

// isCall: a request with a non-null id and a method (it will be answered).
func (h header) isCall() bool {
	return h.hasID && h.hasMethod && h.method != "" && string(bytes.TrimSpace(h.id)) != "null"
}

// scanHeader reads the top-level keys of one JSON object without decoding the payload. Keys match
// exactly (case-sensitive) and the last duplicate wins, like the SDK's decoder (segmentio json
// with DontMatchCaseInsensitiveStructFields, which fills struct fields in order). With early set
// it returns as soon as it has an id and sees result/error (SDK-written replies put "id" first), so
// large tool results are never walked. Inbound frames are scanned to the end, because a later
// duplicate key must win; values are skipped token by token, never copied.
func scanHeader(b []byte, early bool) (h header, ok bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return h, false
	}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return h, false
		}
		switch key, _ := kt.(string); key {
		case "id":
			var raw json.RawMessage
			if dec.Decode(&raw) != nil {
				return h, false
			}
			h.id, h.hasID = raw, true
		case "method":
			var v any
			if dec.Decode(&v) != nil {
				return h, false
			}
			s, isStr := v.(string)
			h.method, h.hasMethod = s, isStr
		case "result", "error":
			h.hasReply = true
			if early && h.hasID {
				return h, true
			}
			if skipValue(dec) != nil {
				return h, false
			}
		default:
			if skipValue(dec) != nil {
				return h, false
			}
		}
	}
	return h, true
}

// skipValue consumes one JSON value token by token, without copying it.
func skipValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d == '}' || d == ']' {
		return nil
	}
	for depth := 1; depth > 0; {
		tok, err = dec.Token()
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); ok {
			if d == '{' || d == '[' {
				depth++
			} else {
				depth--
			}
		}
	}
	return nil
}

// idKey normalises an id the way the SDK does (jsonrpc2.MakeID): a number goes through float64 to
// int64, a string is its decoded value. 1, 1.0 and 1e0 are one id; "A" and "A" are one id.
func idKey(raw json.RawMessage) (string, bool) {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return "", false
	}
	switch v := v.(type) {
	case float64:
		return "n:" + strconv.FormatInt(int64(v), 10), true
	case string:
		return "s:" + v, true
	}
	return "", false
}

// onInput records the calls in one complete inbound frame (an object, or an array batch).
func (t *tracker) onInput(frame []byte) {
	if len(frame) > 0 && frame[0] == '[' {
		var els []json.RawMessage
		if json.Unmarshal(frame, &els) != nil {
			return
		}
		var calls []header
		for _, el := range els {
			h, ok := scanHeader(bytes.TrimSpace(el), false)
			if !ok || !h.isCall() {
				continue
			}
			if !drained(h.method) {
				// The SDK writes a batch reply only when every call in it has finished, so a
				// batch holding a tools/call cannot be drained: track none of it.
				return
			}
			calls = append(calls, h)
		}
		for _, h := range calls {
			if id, ok := idKey(h.id); ok {
				t.add(id)
			}
		}
		return
	}
	if h, ok := scanHeader(frame, false); ok && h.isCall() && drained(h.method) {
		if id, ok := idKey(h.id); ok {
			t.add(id)
		}
	}
}

// onOutput clears the replies in one outbound write. It is a no-op while nothing is outstanding,
// so large tool results are never decoded on the hot path.
func (t *tracker) onOutput(b []byte) {
	if t.size() == 0 {
		return
	}
	for _, line := range bytes.Split(b, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if line[0] == '[' {
			var batch []json.RawMessage
			if json.Unmarshal(line, &batch) == nil {
				for _, el := range batch {
					if id, ok := replyID(el); ok {
						t.remove(id)
					}
				}
			}
			continue
		}
		if id, ok := replyID(line); ok {
			t.remove(id)
		}
	}
}

// replyID returns the normalised id of a response object (an id plus result or error, no method).
func replyID(b []byte) (string, bool) {
	h, ok := scanHeader(b, true)
	if !ok || h.hasMethod || !h.hasID || !h.hasReply {
		return "", false
	}
	return idKey(h.id)
}

// replyWriter forwards to w and then clears the replies it carried.
type replyWriter struct {
	w io.Writer
	t *tracker
}

func (r replyWriter) Write(b []byte) (int, error) {
	n, err := r.w.Write(b)
	if err == nil {
		r.t.onOutput(b[:n])
	}
	return n, err
}

// framer splits a byte stream into top-level JSON values the way the SDK's json.Decoder does
// (whitespace and newlines between values are free; one value may span lines) and hands each
// complete value to fn. It keeps at most max bytes of the current value: a longer one is
// skipped (the SDK fails on it and closes the session), so memory stays bounded. A buffer grown
// past keepBuf is released when its frame ends.
type framer struct {
	max     int
	fn      func([]byte)
	buf     []byte
	started bool // inside a value
	scalar  bool // value is not an object or array: ends at whitespace
	over    bool // current value exceeded max; bytes are dropped
	depth   int
	inStr   bool
	esc     bool
}

func isSpace(c byte) bool { return c == ' ' || c == '\n' || c == '\r' || c == '\t' }

func (f *framer) feed(p []byte) {
	for _, c := range p {
		if !f.started {
			if isSpace(c) {
				continue
			}
			f.started, f.over, f.depth, f.inStr, f.esc = true, false, 0, false, false
			f.scalar = c != '{' && c != '['
			f.buf = f.buf[:0]
		}
		if !f.over {
			if len(f.buf) >= f.max {
				f.over, f.buf = true, nil
			} else {
				f.buf = append(f.buf, c)
			}
		}
		if f.scalar {
			if isSpace(c) {
				f.started = false
			}
			continue
		}
		switch {
		case f.inStr:
			switch {
			case f.esc:
				f.esc = false
			case c == '\\':
				f.esc = true
			case c == '"':
				f.inStr = false
			}
		case c == '"':
			f.inStr = true
		case c == '{' || c == '[':
			f.depth++
		case c == '}' || c == ']':
			f.depth--
			if f.depth == 0 {
				f.started = false
				if !f.over {
					f.fn(f.buf)
				}
				if cap(f.buf) > keepBuf {
					f.buf = nil
				}
			}
		}
	}
}

// pump copies in to pw unchanged, as the bytes arrive (nothing is held back, and nothing needs a
// trailing newline), registering each call before the SDK can see it. At EOF (or a read error) it
// waits for the outstanding drained calls to be answered, then closes pw. done is closed by Serve
// when the SDK session has ended on its own.
func pump(ctx context.Context, in io.Reader, pw *io.PipeWriter, t *tracker, done <-chan struct{}) {
	fr := &framer{max: maxFrame, fn: t.onInput}
	buf := make([]byte, 32<<10)
	var rerr error
	for {
		n, err := in.Read(buf)
		if n > 0 {
			fr.feed(buf[:n])
			if _, werr := pw.Write(buf[:n]); werr != nil {
				return // reader side closed: Serve is finished
			}
		}
		if err != nil {
			if err != io.EOF {
				rerr = err
			}
			break
		}
	}
	t.wait(ctx, done, drainTimeout)
	_ = pw.CloseWithError(rerr) // nil = EOF
}
