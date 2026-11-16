package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"
)

const (
	initLine  = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`
	initdLine = `{"jsonrpc":"2.0","method":"notifications/initialized"}`
	listLine  = `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`
	initPrett = "{\n  \"jsonrpc\": \"2.0\",\n  \"id\": 1,\n  \"method\": \"initialize\",\n  \"params\": {\n    \"protocolVersion\": \"2025-06-18\",\n    \"capabilities\": {},\n    \"clientInfo\": {\"name\": \"t\", \"version\": \"0\"}\n  }\n}\n"
)

// pipedServe runs Serve with piped stdin/stdout. Output lines arrive on lines; Serve's result on done.
type pipedServe struct {
	in    *io.PipeWriter
	lines chan string
	done  chan error
}

func startPiped(t *testing.T, cfg Config) *pipedServe {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	p := &pipedServe{in: inW, lines: make(chan string, 64), done: make(chan error, 1)}
	go func() { p.done <- Serve(ctx, inR, outW, cfg); outW.Close() }()
	go func() {
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			p.lines <- sc.Text()
		}
		close(p.lines)
	}()
	t.Cleanup(func() { cancel(); inW.Close() })
	return p
}

// want blocks until a response (result or error) for every id has been seen. The 5 s guard only
// turns a hang into a failure; a correct server never waits on it.
func (p *pipedServe) want(t *testing.T, ids ...float64) {
	t.Helper()
	need := map[float64]bool{}
	for _, id := range ids {
		need[id] = true
	}
	guard := time.After(5 * time.Second)
	for len(need) > 0 {
		select {
		case l, ok := <-p.lines:
			if !ok {
				t.Fatalf("stdout closed; still missing responses for %v", need)
			}
			var msgs []map[string]any // a batch reply is an array
			if err := json.Unmarshal([]byte(l), &msgs); err != nil {
				var m map[string]any
				if err := json.Unmarshal([]byte(l), &m); err != nil {
					t.Fatalf("bad line %q", l)
				}
				msgs = []map[string]any{m}
			}
			for _, m := range msgs {
				if id, ok := m["id"].(float64); ok && (m["result"] != nil || m["error"] != nil) {
					delete(need, id)
				}
			}
		case <-guard:
			t.Fatalf("no response for ids %v", need)
		}
	}
}

func (p *pipedServe) write(t *testing.T, s string) {
	t.Helper()
	if _, err := io.WriteString(p.in, s); err != nil {
		t.Fatal(err)
	}
}

func (p *pipedServe) exited(t *testing.T) {
	t.Helper()
	select {
	case err := <-p.done:
		if err != nil {
			t.Fatalf("Serve = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not exit")
	}
}

// The handshake messages arrive in ONE read, with and without stdin closed right after.
func TestPipelinedHandshake(t *testing.T) {
	cases := []struct {
		name  string
		input string
		eof   bool
		want  []float64
	}{
		{"compact_open", initLine + "\n" + initdLine + "\n" + listLine + "\n", false, []float64{1, 2}},
		{"compact_eof", initLine + "\n" + initdLine + "\n" + listLine + "\n", true, []float64{1, 2}},
		{"crlf_eof", initLine + "\r\n" + initdLine + "\r\n" + listLine + "\r\n", true, []float64{1, 2}},
		{"pretty_eof", initPrett + initdLine + "\n" + listLine + "\n", true, []float64{1, 2}},
		{"no_final_newline_eof", initLine + "\n" + initdLine + "\n" + listLine, true, []float64{1, 2}},
		// A message with no trailing newline must be answered with stdin still open.
		{"no_final_newline_open", initLine + "\n" + initdLine + "\n" + listLine, false, []float64{1, 2}},
		{"batch_eof", `[` + initLine + `,{"jsonrpc":"2.0","id":2,"method":"ping"}]` + "\n", true, []float64{1, 2}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := startPiped(t, Config{Name: "m", Tools: []Tool{pingTool()}})
			p.write(t, c.input) // single write
			if c.eof {
				p.in.Close()
			}
			p.want(t, c.want...)
			if c.eof {
				p.exited(t)
			}
		})
	}
}

// At EOF, quick calls are answered but a running tools/call is cancelled at once (no 10 s wait).
func TestEOFCancelsToolCallButAnswersList(t *testing.T) {
	started, cancelled := make(chan struct{}), make(chan struct{})
	blocker := pingTool()
	blocker.Name = "block"
	blocker.Handler = func(ctx context.Context, r Request) (any, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return nil, ctx.Err()
	}
	p := startPiped(t, Config{Name: "m", Tools: []Tool{blocker}})
	p.write(t, initLine+"\n"+initdLine+"\n")
	p.want(t, 1)
	p.write(t, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"block","arguments":{}}}`+"\n")
	<-started
	p.write(t, listLine+"\n")
	p.in.Close()
	p.want(t, 2)
	select {
	case <-cancelled:
	case <-time.After(drainTimeout):
		t.Fatal("tools/call was not cancelled at EOF")
	}
	p.exited(t)
}
