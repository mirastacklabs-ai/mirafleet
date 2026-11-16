package central

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/apiclient"
	"github.com/mirastacklabs-ai/mirafleet/internal/central/registry"
	"github.com/mirastacklabs-ai/mirafleet/internal/mcpserver"
	"github.com/mirastacklabs-ai/mirafleet/internal/orch"
	"github.com/mirastacklabs-ai/mirafleet/internal/policy"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// Helpers of TestM5Done (m5_done_test.go).

// newM5Env is newEnvOpts with a service on the injected clock and a registry with the workspace stale window.
func newM5Env(t *testing.T, clock func() time.Time) *env {
	t.Helper()
	var reg *registry.Registry
	var svc *service.Service
	e := newEnvOpts(t, 15*time.Second, func(c *Config) {
		r, err := registry.New(registry.Options{StaleAfter: 45 * time.Second, Logf: c.Logf})
		must(t, err)
		s, err := service.New(service.Config{Store: c.Store, Policy: policy.TablePolicy{}, Live: r,
			HostKey: c.HostKey.PublicKey(), Now: clock, OperatorConns: c.Operators, Logf: c.Logf, ReaperInterval: time.Hour})
		must(t, err)
		c.Registry, c.Service = r, s
		reg, svc = r, s
	})
	e.reg, e.svc = reg, svc
	return e
}

// m5Req is one request that left an orchestrator for a central.
type m5Req struct{ method, params, body string }

// m5Rec records every operator call all orchestrators of a test make (the requests central receives).
type m5Rec struct {
	mu   sync.Mutex
	reqs []m5Req
}

func (r *m5Rec) add(method string, params any, body []byte) {
	b, _ := json.Marshal(params)
	r.mu.Lock()
	r.reqs = append(r.reqs, m5Req{method, string(b), string(body)})
	r.mu.Unlock()
}

// m5ctx is a context that ends after 30 s (or with the test), so nothing can hang the package.
func m5ctx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// m5Code is the api error code of an error from an orchestrator call ("" if it has none).
func m5Code(err error) string {
	var oe *orch.Error
	if errors.As(err, &oe) {
		return oe.Code
	}
	var ae *api.Error
	if errors.As(err, &ae) {
		return ae.Code
	}
	return ""
}

func (r *m5Rec) snapshot() []m5Req {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]m5Req(nil), r.reqs...)
}

func (r *m5Rec) count(method string) int {
	n := 0
	for _, q := range r.snapshot() {
		if q.method == method {
			n++
		}
	}
	return n
}

// m5Conn is an orch.Conn over a real apiclient connection that records what it sends.
type m5Conn struct {
	*apiclient.Client
	rec *m5Rec
}

func (c *m5Conn) Call(ctx context.Context, method string, params, result any) error {
	c.rec.add(method, params, nil)
	return c.Client.Call(ctx, method, params, result)
}

func (c *m5Conn) CallBody(ctx context.Context, method string, params any, body []byte, result any) error {
	c.rec.add(method, params, body)
	return c.Client.CallBody(ctx, method, params, body, result)
}

func (c *m5Conn) CallReadBody(ctx context.Context, method string, params, result any) ([]byte, error) {
	c.rec.add(method, params, nil)
	return c.Client.CallReadBody(ctx, method, params, result)
}

func (c *m5Conn) Ping(ctx context.Context) error {
	_, err := c.Client.Hello(ctx, "mcp")
	return err
}

func m5Dial(rec *m5Rec) orch.DialFunc {
	return func(ctx context.Context, addr string, signer ssh.Signer, hk ssh.HostKeyCallback) (orch.Conn, error) {
		cl, err := apiclient.Dial(ctx, addr, transport.UserOperator, signer, hk)
		if err != nil {
			return nil, err
		}
		if _, err := cl.Hello(ctx, "mcp"); err != nil {
			cl.Close()
			return nil, err
		}
		return &m5Conn{Client: cl, rec: rec}, nil
	}
}

// m5Sink collects the raw text of every tool result of a test.
type m5Sink struct {
	mu    sync.Mutex
	texts []string
}

func (s *m5Sink) add(x string) {
	s.mu.Lock()
	s.texts = append(s.texts, x)
	s.mu.Unlock()
}

func (s *m5Sink) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.texts...)
}

// m5Server is one `mirafleet mcp` process: mcpserver.Serve over pipes with its own orch,
// driven by the go-sdk client.
type m5Server struct {
	t      *testing.T
	o      *orch.Orch
	cs     *mcp.ClientSession
	inW    *io.PipeWriter
	cancel context.CancelFunc
	done   chan struct{} // closed when Serve returned
	serr   *error        // Serve's result; shared by the copies at() makes; read only after done is closed
	sink   *m5Sink
}

func m5Start(t *testing.T, cfg orch.Config, rec *m5Rec, sink *m5Sink) *m5Server {
	t.Helper()
	cfg.Dial = m5Dial(rec)
	cfg.KeepaliveInterval = time.Hour
	o, err := orch.New(cfg)
	must(t, err)
	// The server lives for the whole test (many steps); the cap only guards against a hang.
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	s := &m5Server{t: t, o: o, cancel: cancel, done: make(chan struct{}), sink: sink, serr: new(error)}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	s.inW = inW
	tools := append(mcpserver.ListTools(o), mcpserver.WorkspaceTools(o)...)
	go func() {
		*s.serr = mcpserver.Serve(ctx, inR, outW, mcpserver.Config{Name: "mirafleet", Version: "test", Tools: tools})
		outW.Close()
		close(s.done)
	}()
	client := mcp.NewClient(&mcp.Implementation{Name: "m5-test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, &mcp.IOTransport{Reader: outR, Writer: inW}, nil)
	if err != nil {
		cancel()
		t.Fatalf("mcp connect: %v", err)
	}
	s.cs = cs
	t.Cleanup(func() {
		cancel()
		inW.Close()
		cs.Close()
		<-s.done
		o.Close()
	})
	return s
}

// m5Res is a decoded tool result.
type m5Res struct {
	IsErr bool
	Text  string
	M     map[string]any
}

// code is the error code of an isError result.
func (r m5Res) code() string {
	e, _ := r.M["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

// get walks nested objects.
func (r m5Res) get(path ...string) any {
	var cur any = r.M
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[p]
	}
	return cur
}

func (r m5Res) str(path ...string) string { s, _ := r.get(path...).(string); return s }

func (r m5Res) num(path ...string) float64 { n, _ := r.get(path...).(float64); return n }

func (r m5Res) boolean(path ...string) bool { b, _ := r.get(path...).(bool); return b }

func (s *m5Server) decode(res *mcp.CallToolResult) m5Res {
	s.t.Helper()
	if len(res.Content) != 1 {
		s.t.Fatalf("result has %d content items", len(res.Content))
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		s.t.Fatalf("result content is %T", res.Content[0])
	}
	s.sink.add(tc.Text)
	out := m5Res{IsErr: res.IsError, Text: tc.Text}
	if err := json.Unmarshal([]byte(tc.Text), &out.M); err != nil {
		s.t.Fatalf("result is not JSON: %v: %.200s", err, tc.Text)
	}
	return out
}

// callCtx calls a tool under ctx; a protocol-level failure is returned, not fatal.
func (s *m5Server) callCtx(ctx context.Context, name string, args map[string]any) (m5Res, error) {
	res, err := s.cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return m5Res{}, err
	}
	return s.decode(res), nil
}

// call calls a tool with a 30 s limit; a protocol-level failure is fatal.
func (s *m5Server) call(name string, args map[string]any) m5Res {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := s.callCtx(ctx, name, args)
	if err != nil {
		s.t.Fatalf("%s: %v", name, err)
	}
	return r
}

// closeStdin closes the server's stdin, as a harness exiting does, waits for Serve to return
// (failing the test after within) and returns Serve's error. The wait on done orders the read.
func (s *m5Server) closeStdin(within time.Duration) error {
	s.t.Helper()
	s.inW.Close()
	select {
	case <-s.done:
	case <-time.After(within):
		s.t.Fatalf("Serve did not return within %v after stdin closed", within)
	}
	return *s.serr
}

// at returns the server with its failures reported on t (a subtest).
func (s *m5Server) at(t *testing.T) *m5Server {
	c := *s
	c.t = t
	return &c
}

// m5RawInitialize serves a throwaway server for cfg, sends one hand-written initialize asking for
// version and returns the protocol version in the answer.
func m5RawInitialize(t *testing.T, cfg orch.Config, rec *m5Rec, version string) string {
	t.Helper()
	cfg.Dial = m5Dial(rec)
	o, err := orch.New(cfg)
	must(t, err)
	defer o.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	// Closing the pipes on every path unblocks the writer goroutine, the read and Serve.
	defer inW.Close()
	defer outR.Close()
	done := make(chan struct{})
	go func() {
		mcpserver.Serve(ctx, inR, outW, mcpserver.Config{Name: "mirafleet", Tools: mcpserver.ListTools(o)})
		outW.Close()
		close(done)
	}()
	req := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"` + version +
		`","capabilities":{},"clientInfo":{"name":"raw","version":"0"}}}` + "\n"
	go inW.Write([]byte(req))
	type read struct {
		line []byte
		err  error
	}
	got := make(chan read, 1)
	go func() {
		line, err := bufio.NewReader(outR).ReadBytes('\n')
		got <- read{line, err}
	}()
	var rd read
	select {
	case rd = <-got:
	case <-time.After(30 * time.Second):
		t.Fatal("no answer to the raw initialize within 30 s")
	}
	must(t, rd.err)
	var resp struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	must(t, json.Unmarshal(rd.line, &resp))
	inW.Close()
	outR.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("raw server did not exit on stdin EOF")
	}
	return resp.Result.ProtocolVersion
}
