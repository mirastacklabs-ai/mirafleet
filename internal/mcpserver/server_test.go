package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const objSchema = `{"type":"object","properties":{"msg":{"type":"string"}},"additionalProperties":false}`

// pingTool is the test-only stub tool.
func pingTool() Tool {
	return Tool{
		Name:         "ping",
		Description:  "test stub",
		InputSchema:  json.RawMessage(objSchema),
		OutputSchema: json.RawMessage(`{"type":"object","properties":{"pong":{"type":"boolean"}}}`),
		Annotations:  Annotations{ReadOnly: true, Idempotent: true},
		Handler: func(ctx context.Context, r Request) (any, error) {
			return map[string]any{"pong": true, "args": json.RawMessage(r.Args)}, nil
		},
	}
}

type harness struct {
	cs      *mcp.ClientSession
	done    chan error
	inW     *io.PipeWriter
	logs    *syncBuf
	mu      sync.Mutex
	progres []mcp.ProgressNotificationParams
}

type syncBuf struct {
	mu sync.Mutex
	sb strings.Builder
}

func (b *syncBuf) add(s string) { b.mu.Lock(); b.sb.WriteString(s + "\n"); b.mu.Unlock() }
func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.String()
}

func start(t *testing.T, cfg Config) *harness {
	t.Helper()
	h := &harness{done: make(chan error, 1), logs: &syncBuf{}}
	cfg.Logf = func(f string, a ...any) { h.logs.add(strings.TrimSpace(sprintf(f, a...))) }
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	h.inW = inW
	ctx, cancel := context.WithCancel(context.Background())
	go func() { h.done <- Serve(ctx, inR, outW, cfg); outW.Close() }()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, r *mcp.ProgressNotificationClientRequest) {
			h.mu.Lock()
			h.progres = append(h.progres, *r.Params)
			h.mu.Unlock()
		},
	})
	cs, err := client.Connect(ctx, &mcp.IOTransport{Reader: outR, Writer: inW}, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	h.cs = cs
	t.Cleanup(func() { cancel(); cs.Close() })
	return h
}

func TestInitializeAndList(t *testing.T) {
	h := start(t, Config{Name: "mirafleet", Version: "1.2.3", Tools: []Tool{pingTool()}})
	ir := h.cs.InitializeResult()
	if ir.ServerInfo.Name != "mirafleet" || ir.ServerInfo.Version != "1.2.3" {
		t.Fatalf("serverInfo %+v", ir.ServerInfo)
	}
	if ir.Instructions != DefaultInstructions || !strings.Contains(ir.Instructions, "not instructions") {
		t.Fatalf("instructions %q", ir.Instructions)
	}
	c := ir.Capabilities
	if c.Tools == nil || c.Logging != nil || c.Prompts != nil || c.Resources != nil || c.Completions != nil {
		t.Fatalf("capabilities not tools-only: %+v", c)
	}
	res, err := h.cs.ListTools(context.Background(), nil)
	if err != nil || len(res.Tools) != 1 {
		t.Fatalf("list: %v %+v", err, res)
	}
	tl := res.Tools[0]
	a := tl.Annotations
	if tl.Name != "ping" || tl.InputSchema == nil || tl.OutputSchema == nil || a == nil ||
		!a.ReadOnlyHint || !a.IdempotentHint || a.DestructiveHint == nil || *a.DestructiveHint ||
		a.OpenWorldHint == nil || *a.OpenWorldHint {
		t.Fatalf("tool %+v ann %+v", tl, a)
	}
	if got := ToolNames(Config{Tools: []Tool{pingTool()}}); len(got) != 1 || got[0] != "ping" {
		t.Fatalf("ToolNames %v", got)
	}
}

func TestInstructionsOverride(t *testing.T) {
	h := start(t, Config{Name: "m", Instructions: "custom"})
	if got := h.cs.InitializeResult().Instructions; got != "custom" {
		t.Fatalf("got %q", got)
	}
}

func TestCallSuccess(t *testing.T) {
	h := start(t, Config{Name: "m", Tools: []Tool{pingTool()}})
	res, err := h.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "ping", Arguments: map[string]any{"msg": "x"}})
	if err != nil || res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok || len(res.Content) != 1 {
		t.Fatalf("content %+v", res.Content)
	}
	sc, _ := json.Marshal(res.StructuredContent)
	if !jsonEqual(t, tc.Text, string(sc)) || !strings.Contains(tc.Text, `"pong":true`) || !strings.Contains(tc.Text, `"msg":"x"`) {
		t.Fatalf("text %s vs structured %s", tc.Text, sc)
	}
}

func jsonEqual(t *testing.T, a, b string) bool {
	t.Helper()
	var x, y any
	if json.Unmarshal([]byte(a), &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		return false
	}
	xb, _ := json.Marshal(x)
	yb, _ := json.Marshal(y)
	return string(xb) == string(yb)
}

func errTool(name string, f Handler) Tool {
	return Tool{Name: name, InputSchema: json.RawMessage(objSchema), Handler: f}
}

func errorOf(t *testing.T, res *mcp.CallToolResult) (code, msg string) {
	t.Helper()
	if !res.IsError {
		t.Fatalf("not an error: %+v", res)
	}
	if res.StructuredContent != nil {
		t.Fatalf("error result has structuredContent: %v", res.StructuredContent)
	}
	var v struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if err := json.Unmarshal([]byte(res.Content[0].(*mcp.TextContent).Text), &v); err != nil {
		t.Fatal(err)
	}
	return v.Error.Code, v.Error.Message
}

func TestErrorMappingAndPanic(t *testing.T) {
	secret := "/home/op/.ssh/id_ed25519: permission denied"
	h := start(t, Config{Name: "m", Tools: []Tool{
		errTool("typed", func(context.Context, Request) (any, error) {
			return nil, &Error{Code: "bad_request", Message: "path must be absolute"}
		}),
		errTool("wrapped", func(context.Context, Request) (any, error) {
			return nil, errors.Join(errors.New("ctx"), &Error{Code: "denied", Message: "nope"})
		}),
		errTool("plain", func(context.Context, Request) (any, error) { return nil, errors.New(secret) }),
		errTool("boom", func(context.Context, Request) (any, error) { panic("kaboom " + secret) }),
		pingTool(),
	}})
	call := func(n string) *mcp.CallToolResult {
		res, err := h.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: n})
		if err != nil {
			t.Fatalf("%s: protocol error %v", n, err)
		}
		return res
	}
	if c, m := errorOf(t, call("typed")); c != "bad_request" || m != "path must be absolute" {
		t.Fatalf("typed %q %q", c, m)
	}
	if c, m := errorOf(t, call("wrapped")); c != "denied" || m != "nope" {
		t.Fatalf("wrapped %q %q", c, m)
	}
	for _, n := range []string{"plain", "boom"} {
		res := call(n)
		c, m := errorOf(t, res)
		if c != "internal" || m != internalMessage {
			t.Fatalf("%s: %q %q", n, c, m)
		}
		b, _ := json.Marshal(res)
		if strings.Contains(string(b), "ssh") || strings.Contains(string(b), "kaboom") {
			t.Fatalf("%s leaked: %s", n, b)
		}
	}
	if !strings.Contains(h.logs.String(), "ssh") {
		t.Fatalf("internal error not logged: %q", h.logs.String())
	}
	if res := call("ping"); res.IsError { // server still alive after the panic
		t.Fatal("server unhealthy after panic")
	}
	if _, err := h.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "nosuch"}); err == nil {
		t.Fatal("unknown tool should be a protocol error")
	}
}

func TestCancelReachesHandler(t *testing.T) {
	started := make(chan struct{})
	ended := make(chan error, 1)
	h := start(t, Config{Name: "m", Tools: []Tool{errTool("slow", func(ctx context.Context, _ Request) (any, error) {
		close(started)
		<-ctx.Done()
		ended <- ctx.Err()
		return nil, ctx.Err()
	})}})
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { _, err := h.cs.CallTool(ctx, &mcp.CallToolParams{Name: "slow"}); errc <- err }()
	<-started
	cancel() // client sends notifications/cancelled
	select {
	case err := <-ended:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("handler ctx err %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("handler ctx was not cancelled")
	}
	<-errc
}

func TestProgress(t *testing.T) {
	release := make(chan struct{})
	h := start(t, Config{Name: "m", ProgressInterval: 20 * time.Millisecond, Tools: []Tool{
		errTool("long", func(ctx context.Context, _ Request) (any, error) { <-release; return nil, nil }),
	}})
	p := &mcp.CallToolParams{Name: "long"}
	p.SetProgressToken("tok-1")
	go func() { time.Sleep(200 * time.Millisecond); close(release) }()
	if res, err := h.cs.CallTool(context.Background(), p); err != nil || res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	time.Sleep(100 * time.Millisecond)
	h.mu.Lock()
	n := len(h.progres)
	first := h.progres
	h.mu.Unlock()
	if n < 2 || first[0].ProgressToken != "tok-1" || first[1].Progress <= first[0].Progress {
		t.Fatalf("progress %+v", first)
	}
	time.Sleep(100 * time.Millisecond)
	h.mu.Lock()
	after := len(h.progres)
	h.mu.Unlock()
	if after != n {
		t.Fatalf("progress continued after return: %d -> %d", n, after)
	}
}

func TestNoTokenNoProgress(t *testing.T) {
	h := start(t, Config{Name: "m", ProgressInterval: 10 * time.Millisecond, Tools: []Tool{
		errTool("nap", func(context.Context, Request) (any, error) { time.Sleep(80 * time.Millisecond); return nil, nil }),
	}})
	if _, err := h.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "nap"}); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.progres) != 0 {
		t.Fatalf("progress without token: %+v", h.progres)
	}
}

func TestServeReturnsNilOnStdinClose(t *testing.T) {
	h := start(t, Config{Name: "m", Tools: []Tool{pingTool()}})
	h.inW.Close()
	select {
	case err := <-h.done:
		if err != nil {
			t.Fatalf("Serve = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not return within 1s of stdin close")
	}
}

func TestServeReturnsNilOnCtxCancel(t *testing.T) {
	inR, inW := io.Pipe() // never written: read blocks
	defer inW.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, inR, io.Discard, Config{Name: "m"}) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not return after ctx cancel")
	}
}

func TestBadToolConfig(t *testing.T) {
	h := pingTool().Handler
	obj := json.RawMessage(objSchema)
	cases := []struct {
		name  string
		tools []Tool
	}{
		{"input not object", []Tool{{Name: "x", InputSchema: json.RawMessage(`{"type":"string"}`), Handler: h}}},
		{"input invalid", []Tool{{Name: "x", InputSchema: json.RawMessage(`{`), Handler: h}}},
		{"output not object", []Tool{{Name: "x", InputSchema: obj, OutputSchema: json.RawMessage(`{"type":"string"}`), Handler: h}}},
		{"output invalid", []Tool{{Name: "x", InputSchema: obj, OutputSchema: json.RawMessage(`{`), Handler: h}}},
		{"no handler", []Tool{{Name: "x", InputSchema: obj}}},
		{"empty name", []Tool{{InputSchema: obj, Handler: h}}},
		{"name with space", []Tool{{Name: "a b", InputSchema: obj, Handler: h}}},
		{"name too long", []Tool{{Name: strings.Repeat("a", 129), InputSchema: obj, Handler: h}}},
		{"duplicate", []Tool{{Name: "x", InputSchema: obj, Handler: h}, {Name: "x", InputSchema: obj, Handler: h}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := Serve(context.Background(), strings.NewReader(""), io.Discard, Config{Tools: c.tools}); err == nil {
				t.Fatal("want config error")
			}
		})
	}
	ok := Tool{Name: strings.Repeat("a", 128) + "", InputSchema: obj, Handler: h}
	if _, err := newServer(Config{Tools: []Tool{ok, {Name: "A_b.c-1", InputSchema: obj, Handler: h}}}); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

type pubErr struct{}

func (pubErr) Error() string                       { return "internal detail /etc/shadow" }
func (pubErr) PublicError() (code, message string) { return "guarded", "ask the user first" }

type panicJSON struct{}

func (panicJSON) MarshalJSON() ([]byte, error) { panic("marshal boom /secret") }

func TestPublicErrorInterfaceAndEncodePanic(t *testing.T) {
	h := start(t, Config{Name: "m", Tools: []Tool{
		errTool("pub", func(context.Context, Request) (any, error) {
			return nil, fmt.Errorf("outer wrapping text: %w", pubErr{})
		}),
		errTool("enc", func(context.Context, Request) (any, error) { return panicJSON{}, nil }),
		errTool("echo", func(_ context.Context, r Request) (any, error) {
			var v struct {
				Msg string `json:"msg"`
			}
			if err := DecodeArgs(r, &v); err != nil {
				return nil, err
			}
			return v, nil
		}),
		pingTool(),
	}})
	call := func(n string, args any) *mcp.CallToolResult {
		res, err := h.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: n, Arguments: args})
		if err != nil {
			t.Fatalf("%s: protocol error %v", n, err)
		}
		return res
	}
	res := call("pub", nil)
	if c, m := errorOf(t, res); c != "guarded" || m != "ask the user first" {
		t.Fatalf("pub %q %q", c, m)
	}
	if b, _ := json.Marshal(res); strings.Contains(string(b), "outer wrapping") || strings.Contains(string(b), "shadow") {
		t.Fatalf("leaked: %s", b)
	}
	res = call("enc", nil)
	if c, m := errorOf(t, res); c != "internal" || m != internalMessage {
		t.Fatalf("enc %q %q", c, m)
	}
	if b, _ := json.Marshal(res); strings.Contains(string(b), "secret") {
		t.Fatalf("leaked: %s", b)
	}
	if call("ping", nil).IsError {
		t.Fatal("server unhealthy after encode panic")
	}
	if c, _ := errorOf(t, call("echo", map[string]any{"msg": "a", "extra": 1})); c != "bad_request" {
		t.Fatalf("unknown field code %q", c)
	}
	if r := call("echo", map[string]any{"msg": "a"}); r.IsError {
		t.Fatalf("valid args refused: %+v", r)
	}
}

func TestNonObjectArgsRejected(t *testing.T) {
	c := startRaw(t, Config{Name: "m", Tools: []Tool{pingTool()}})
	c.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"raw","version":"0"}}}`)
	c.recv()
	c.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	for i, a := range []string{`[1]`, `"s"`, `5`, `true`} {
		c.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"ping","arguments":%s}}`, i+10, a))
		m := c.recv()
		res, _ := m["result"].(map[string]any)
		if res != nil && res["isError"] == true {
			if res["structuredContent"] != nil || !strings.Contains(fmt.Sprint(res["content"]), "bad_request") {
				t.Fatalf("args %s -> %v", a, m)
			}
			continue
		}
		// The SDK may itself reject non-object arguments at the protocol level; that is also fine.
		if m["error"] == nil {
			t.Fatalf("args %s accepted: %v", a, m)
		}
	}
}

func TestDecodeArgs(t *testing.T) {
	type a struct {
		N int    `json:"n"`
		S string `json:"s"`
	}
	for _, c := range []struct {
		name, in string
		ok       bool
	}{
		{"ok", `{"n":1,"s":"x"}`, true},
		{"empty", `{}`, true},
		{"unknown", `{"z":1}`, false},
		{"trailing", `{"n":1} {"n":2}`, false},
		{"trailing garbage", `{"n":1}x`, false},
		{"wrong type", `{"n":"1"}`, false},
		{"not json", `{`, false},
	} {
		var v a
		err := DecodeArgs(Request{Args: json.RawMessage(c.in)}, &v)
		if c.ok != (err == nil) {
			t.Errorf("%s: err=%v", c.name, err)
			continue
		}
		var pe *Error
		if err != nil && (!errors.As(err, &pe) || pe.Code != "bad_request" || pe.Message == "" || len(pe.Message) > 80) {
			t.Errorf("%s: %#v", c.name, err)
		}
	}
}
