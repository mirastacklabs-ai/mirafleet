package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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
// milliseconds; the bound only protects exit time (docs/M5.md: exit within 5 s). Variable for tests.
var drainTimeout = 2 * time.Second

// maxFrame is the largest single JSON-RPC frame the SDK accepts (its decoder fails past it), so it
// is also the most this file ever buffers for one message.
var maxFrame = mcp.DefaultMaxLineLength

// drained reports whether a call with this method is held back at EOF.
func drained(method string) bool { return method != "" && method != "tools/call" }

// tracker is the set of outstanding drained-call ids: added when a request is read from stdin,
// removed when its reply is written to stdout. Ids are keyed by their raw JSON text.
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

// message is the part of a JSON-RPC message the tracker needs.
type message struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
}

func idKey(raw json.RawMessage) (string, bool) {
	s := string(bytes.TrimSpace(raw))
	if s == "" || s == "null" {
		return "", false
	}
	return s, true
}

// onInput records the calls in one complete inbound frame (an object, or an array batch).
func (t *tracker) onInput(frame []byte) {
	if len(frame) > 0 && frame[0] == '[' {
		var batch []json.RawMessage
		if json.Unmarshal(frame, &batch) != nil {
			return
		}
		for _, el := range batch {
			t.onInput(bytes.TrimSpace(el))
		}
		return
	}
	var m message
	if json.Unmarshal(frame, &m) != nil || !drained(m.Method) {
		return
	}
	if id, ok := idKey(m.ID); ok {
		t.add(id)
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

// replyID returns the id of a response object (id plus result or error, no method). It reads
// top-level keys only and returns as soon as it knows, which for the SDK's key order
// ("jsonrpc","id","result") is before the payload.
func replyID(b []byte) (string, bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return "", false
	}
	var (
		id               json.RawMessage
		haveID, hasReply bool
	)
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return "", false
		}
		key, _ := kt.(string)
		switch key {
		case "method":
			return "", false
		case "id":
			if dec.Decode(&id) != nil {
				return "", false
			}
			haveID = true
		case "result", "error":
			hasReply = true
			if haveID {
				k, ok := idKey(id)
				return k, ok
			}
			var skip json.RawMessage
			if dec.Decode(&skip) != nil {
				return "", false
			}
		default:
			var skip json.RawMessage
			if dec.Decode(&skip) != nil {
				return "", false
			}
		}
	}
	if haveID && hasReply {
		return idKey(id)
	}
	return "", false
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
// skipped (the SDK fails on it and closes the session), so memory stays bounded.
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
