package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

const pipelinedHandshake = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":2,"method":"tools/list"}
`

// pipeBuf is a goroutine-safe output buffer.
type pipeBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *pipeBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *pipeBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// responseIDs returns the ids of the response lines (those with a result) in out.
func responseIDs(t *testing.T, out string) map[float64]bool {
	t.Helper()
	ids := map[float64]bool{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("bad line %q", l)
		}
		if id, ok := m["id"].(float64); ok && m["result"] != nil {
			ids[id] = true
		}
	}
	return ids
}

// The three handshake messages arrive in ONE read, with and without stdin closed right after.
func TestPipelinedHandshake(t *testing.T) {
	for _, closeStdin := range []bool{false, true} {
		name := "open"
		if closeStdin {
			name = "eof"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			inR, inW := io.Pipe()
			out := &pipeBuf{}
			done := make(chan error, 1)
			go func() { done <- Serve(ctx, inR, out, Config{Name: "m", Tools: []Tool{pingTool()}}) }()
			if _, err := io.WriteString(inW, pipelinedHandshake); err != nil { // single write
				t.Fatal(err)
			}
			if closeStdin {
				inW.Close()
			} else {
				defer inW.Close()
			}
			deadline := time.Now().Add(3 * time.Second)
			var ids map[float64]bool
			for time.Now().Before(deadline) {
				if ids = responseIDs(t, out.String()); ids[1] && ids[2] {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !ids[1] || !ids[2] {
				t.Fatalf("want responses for ids 1 and 2, got output:\n%s", out.String())
			}
			if closeStdin {
				select {
				case err := <-done:
					if err != nil {
						t.Fatalf("Serve = %v", err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("Serve did not exit after EOF")
				}
			}
		})
	}
}
