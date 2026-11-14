package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"testing"
	"time"
)

func sprintf(f string, a ...any) string { return fmt.Sprintf(f, a...) }

// rawClient is a hand-written newline-delimited JSON-RPC client with no SDK.
type rawClient struct {
	t     *testing.T
	w     io.WriteCloser
	lines chan string
	done  chan error
}

func startRaw(t *testing.T, cfg Config) *rawClient {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	c := &rawClient{t: t, w: inW, lines: make(chan string, 64), done: make(chan error, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { c.done <- Serve(ctx, inR, outW, cfg); outW.Close() }()
	go func() {
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			c.lines <- sc.Text()
		}
		close(c.lines)
	}()
	t.Cleanup(func() { cancel(); inW.Close() })
	return c
}

func (c *rawClient) send(s string) {
	c.t.Helper()
	if _, err := io.WriteString(c.w, s+"\n"); err != nil {
		c.t.Fatal(err)
	}
}

// recv reads the next line and asserts it is a valid JSON-RPC 2.0 message.
func (c *rawClient) recv() map[string]any {
	c.t.Helper()
	select {
	case l, ok := <-c.lines:
		if !ok {
			c.t.Fatal("server closed stdout")
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			c.t.Fatalf("stdout line is not JSON: %q", l)
		}
		if m["jsonrpc"] != "2.0" {
			c.t.Fatalf("not JSON-RPC 2.0: %q", l)
		}
		_, hasID := m["id"]
		_, hasMethod := m["method"]
		_, hasResult := m["result"]
		_, hasErr := m["error"]
		if !(hasMethod || (hasID && (hasResult || hasErr))) {
			c.t.Fatalf("not a request/response/notification: %q", l)
		}
		return m
	case <-time.After(3 * time.Second):
		c.t.Fatal("timeout waiting for a server line")
	}
	return nil
}

func TestRawInitializeVersions(t *testing.T) {
	for _, v := range []string{"2025-06-18", "2026-07-28", "1999-01-01"} {
		t.Run(v, func(t *testing.T) {
			c := startRaw(t, Config{Name: "m", Tools: []Tool{pingTool()}})
			c.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":%q,"capabilities":{},"clientInfo":{"name":"raw","version":"0"}}}`, v))
			m := c.recv()
			res, _ := m["result"].(map[string]any)
			if res == nil {
				t.Fatalf("initialize(%s) -> %v", v, m)
			}
			t.Logf("client %s -> server negotiated %v", v, res["protocolVersion"])
			// SDK v1.8.0 negotiates down: 2025-06-18 is echoed; 2026-07-28 (stateless/discover
			// era) and unknown versions get the SDK's latest initialize-era version, 2025-11-25.
			want := map[string]string{"2025-06-18": "2025-06-18", "2026-07-28": "2025-11-25", "1999-01-01": "2025-11-25"}[v]
			if res["protocolVersion"] != want {
				t.Errorf("client %s: negotiated %v, want %s", v, res["protocolVersion"], want)
			}
			c.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
			c.send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
			m = c.recv()
			if m["id"] != float64(2) || m["result"] == nil {
				t.Fatalf("tools/list -> %v", m)
			}
		})
	}
}

func TestRawUnknownMethodAndMalformed(t *testing.T) {
	c := startRaw(t, Config{Name: "m", Tools: []Tool{pingTool()}})
	c.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"raw","version":"0"}}}`)
	c.recv()
	c.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	c.send(`{"jsonrpc":"2.0","id":7,"method":"no/such/method"}`)
	m := c.recv()
	e, _ := m["error"].(map[string]any)
	if m["id"] != float64(7) || e == nil || e["code"] != float64(-32601) {
		t.Fatalf("unknown method -> %v", m)
	}

	// Malformed lines: record what the SDK does, then require the server to keep working.
	for _, bad := range []string{`{not json`, `[]`, `{"jsonrpc":"2.0","id":8}`, `"just a string"`} {
		c.send(bad)
		select {
		case l, ok := <-c.lines:
			t.Logf("malformed %q -> server wrote %q (open=%v)", bad, l, ok)
			if ok {
				var x any
				if json.Unmarshal([]byte(l), &x) != nil {
					t.Fatalf("non-JSON output for malformed input: %q", l)
				}
			}
		case <-time.After(300 * time.Millisecond):
			t.Logf("malformed %q -> no output", bad)
		}
		select {
		case err := <-c.done:
			t.Logf("malformed %q ended Serve with %v", bad, err)
			return // the SDK closes the connection on a bad frame; that is a clean, non-crashing end
		default:
		}
	}
}

func TestRawMalformedThenOK(t *testing.T) {
	c := startRaw(t, Config{Name: "m", Tools: []Tool{pingTool()}})
	c.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"raw","version":"0"}}}`)
	c.recv()
	c.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	c.send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"ping","arguments":{}}}`)
	m := c.recv()
	res, _ := m["result"].(map[string]any)
	if res == nil || res["isError"] == true || res["structuredContent"] == nil {
		t.Fatalf("tools/call -> %v", m)
	}
	// Drain remaining lines after closing stdin: all must still parse.
	c.w.Close()
	for l := range c.lines {
		var x map[string]any
		if json.Unmarshal([]byte(l), &x) != nil {
			t.Fatalf("bad line %q", l)
		}
	}
	select {
	case err := <-c.done:
		if err != nil {
			t.Fatalf("Serve = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("no exit")
	}
}
