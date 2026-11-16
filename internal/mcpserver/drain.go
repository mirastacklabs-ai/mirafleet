package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"sync"
	"time"
)

// drainTimeout bounds how long Serve waits, after stdin EOF, for requests already received to be
// answered before it lets the SDK tear the connection down.
const drainTimeout = 10 * time.Second

// pending counts JSON-RPC calls read from stdin and replies written to stdout. The SDK cancels
// in-flight (asynchronous) calls the moment the reader hits EOF, so a client that writes its
// requests and closes stdin would otherwise get no answers. Serve holds the EOF back until
// replies catch up with calls.
type pending struct {
	mu             sync.Mutex
	calls, replies int
}

func (p *pending) addCall()  { p.mu.Lock(); p.calls++; p.mu.Unlock() }
func (p *pending) addReply() { p.mu.Lock(); p.replies++; p.mu.Unlock() }
func (p *pending) outstanding() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls - p.replies
}

// wait blocks until every received call has been answered, d elapses or ctx is done.
func (p *pending) wait(ctx context.Context, d time.Duration) {
	deadline := time.NewTimer(d)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for p.outstanding() > 0 {
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			return
		case <-tick.C:
		}
	}
}

// envelope is the subset of a JSON-RPC message needed to tell calls from replies.
type envelope struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
}

func (e envelope) hasID() bool { return len(e.ID) > 0 && !bytes.Equal(e.ID, []byte("null")) }

// countedWriter counts reply messages (id, no method) written to w, after the write completes.
type countedWriter struct {
	w io.Writer
	p *pending
}

func (c countedWriter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	if err == nil {
		for _, line := range bytes.Split(b, []byte("\n")) {
			var e envelope
			if json.Unmarshal(line, &e) == nil && e.hasID() && e.Method == "" {
				c.p.addReply()
			}
		}
	}
	return n, err
}

// pump copies in to pw unchanged, line by line, counting calls. At EOF (or a read error) it waits
// for the outstanding calls to be answered, then closes pw.
func pump(ctx context.Context, in io.Reader, pw *io.PipeWriter, p *pending) {
	br := bufio.NewReader(in)
	var rerr error
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			var e envelope
			if json.Unmarshal(line, &e) == nil && e.hasID() && e.Method != "" {
				p.addCall() // count before the SDK can see the line
			}
			if _, werr := pw.Write(line); werr != nil {
				return // reader side closed: Serve is done
			}
		}
		if err != nil {
			if err != io.EOF {
				rerr = err
			}
			break
		}
	}
	p.wait(ctx, drainTimeout)
	_ = pw.CloseWithError(rerr) // nil = EOF
}
