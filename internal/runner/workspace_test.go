package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/actions"
	"github.com/mirastacklabs-ai/mirafleet/internal/audit"
	"github.com/mirastacklabs-ai/mirafleet/internal/keys"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/fake"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/wsconfig"
)

const wsYAML = `
executor: containerd
concurrent: 2
max_age: 24h
images: ["alpine:3.20", "ghcr.io/acme/dev:*"]
limits: {cpus: 2, memory: 1GiB, pids: 256, tmpfs: 64MiB, disk: 1GiB, max_file: 4KiB, exec_max: 5s}
network: [none]
env_allow: [LANG, CI]
containerd:
  namespace: mirafleet
  root: /var/lib/mirafleet/ws
`

func parseWS(t *testing.T, y string) *wsconfig.Config {
	t.Helper()
	c, err := wsconfig.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// chanHook (host-mode tests) lets a test accept channels the runner opens toward the fake central.
var chanHook func(ssh.NewChannel) bool

// wsHarness is a fake central (a real SSH server) driving a real Runner in workspace mode.
type wsHarness struct {
	t     *testing.T
	conn  ssh.Conn
	hbs   chan transport.Heartbeat
	ex    *fake.Executor
	log   *syncBuf
	cfg   *wsconfig.Config
	runnr *Runner
}

func newHarness(t *testing.T, y string, ex *fake.Executor, mod func(*Config)) *wsHarness {
	t.Helper()
	var w workspace.Executor
	if ex != nil {
		w = ex
	}
	h := newHarnessFor(t, y, w, mod)
	h.ex = ex
	return h
}

// newHarnessFor drives any executor (h.ex stays nil unless the caller sets it).
func newHarnessFor(t *testing.T, y string, ex workspace.Executor, mod func(*Config)) *wsHarness {
	t.Helper()
	dir := t.TempDir()
	if _, err := keys.Generate(dir, "host", "host"); err != nil {
		t.Fatal(err)
	}
	host, err := keys.LoadSigner(filepath.Join(dir, "host"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Generate(dir, "r", "r"); err != nil {
		t.Fatal(err)
	}
	signer, err := keys.LoadSigner(filepath.Join(dir, "r"))
	if err != nil {
		t.Fatal(err)
	}
	kh := filepath.Join(dir, "known_hosts")
	if err := writeFile(kh, "* "+keys.PubLine(host.PublicKey(), "")+"\n"); err != nil {
		t.Fatal(err)
	}
	sc := &ssh.ServerConfig{PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) { return nil, nil }}
	sc.AddHostKey(host)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	h := &wsHarness{t: t, hbs: make(chan transport.Heartbeat, 64), log: &syncBuf{}}
	conns := make(chan ssh.Conn, 4)
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				conn, chans, reqs, err := ssh.NewServerConn(nc, sc)
				if err != nil {
					nc.Close()
					return
				}
				go func() {
					for c := range chans {
						if chanHook != nil && chanHook(c) {
							continue
						}
						c.Reject(ssh.Prohibited, "")
					}
				}()
				conns <- conn
				for rq := range reqs {
					if rq.Type == transport.ReqHeartbeat {
						var hb transport.Heartbeat
						if json.Unmarshal(rq.Payload, &hb) == nil {
							select {
							case h.hbs <- hb:
							default:
							}
						}
					}
					if rq.WantReply {
						rq.Reply(true, nil)
					}
				}
			}()
		}
	}()
	if y != "" {
		h.cfg = parseWS(t, y)
	}
	cfg := Config{Central: ln.Addr().String(), Name: "r", Signer: signer, KnownHosts: kh,
		HeartbeatEvery: 100 * time.Millisecond, HeartbeatTimeout: 10 * time.Second, Audit: audit.New(h.log)}
	if h.cfg != nil {
		cfg.Workspace, cfg.WorkspaceConfig = ex, h.cfg
	}
	if mod != nil {
		mod(&cfg)
	}
	h.runnr = New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.runnr.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case h.conn = <-conns:
	case <-time.After(10 * time.Second):
		t.Fatal("runner did not connect")
	}
	t.Cleanup(func() { h.conn.Close() })
	return h
}

func (h *wsHarness) openRaw(name string, payload []byte) (ssh.Channel, error) {
	ch, reqs, err := h.conn.OpenChannel(name, payload)
	if err != nil {
		return nil, err
	}
	go ssh.DiscardRequests(reqs)
	return ch, nil
}

func (h *wsHarness) open(req *transport.WorkspaceRequest) (ssh.Channel, error) {
	p, err := transport.EncodeWorkspaceRequest(req)
	if err != nil {
		h.t.Fatalf("encode: %v", err)
	}
	return h.openRaw(transport.ChanWorkspace, p)
}

// finish sends body, CloseWrite and reads the result (and the read body).
func (h *wsHarness) finish(ch ssh.Channel, op string, body []byte) (*transport.WorkspaceResult, []byte, error) {
	timer := time.AfterFunc(15*time.Second, func() { ch.Close() })
	defer timer.Stop()
	if len(body) > 0 {
		if _, err := ch.Write(body); err != nil {
			return nil, nil, err
		}
	}
	ch.CloseWrite()
	res, err := transport.ReadResult(ch, wsResultMax)
	if err != nil {
		return nil, nil, err
	}
	var data []byte
	if op == transport.OpRead && res.Code == transport.CodeOK {
		var buf bytes.Buffer
		if err := transport.CopyBody(&buf, ch, res.File.Length); err != nil {
			return res, nil, err
		}
		data = buf.Bytes()
	}
	return res, data, nil
}

func (h *wsHarness) try(req transport.WorkspaceRequest, body []byte) (*transport.WorkspaceResult, []byte, error) {
	ch, err := h.open(&req)
	if err != nil {
		return nil, nil, err
	}
	defer ch.Close()
	return h.finish(ch, req.Op, body)
}

func (h *wsHarness) do(req transport.WorkspaceRequest, body []byte) (*transport.WorkspaceResult, []byte) {
	h.t.Helper()
	res, data, err := h.try(req, body)
	if err != nil {
		h.t.Fatalf("%s: %v", req.Op, err)
	}
	return res, data
}

func (h *wsHarness) want(req transport.WorkspaceRequest, body []byte, code string) *transport.WorkspaceResult {
	h.t.Helper()
	res, _ := h.do(req, body)
	if res.Code != code {
		h.t.Fatalf("%s: code %q (%s), want %q", req.Op, res.Code, res.Message, code)
	}
	return res
}

func createReq(id workspace.ID) transport.WorkspaceRequest {
	return transport.WorkspaceRequest{Op: transport.OpCreate, ID: string(id), Spec: &transport.WorkspaceSpec{Image: "alpine:3.20"}}
}

func (h *wsHarness) create() workspace.ID {
	h.t.Helper()
	id := workspace.NewID()
	h.want(createReq(id), nil, transport.CodeOK)
	return id
}

func busy(err error) bool {
	var oe *ssh.OpenChannelError
	return errors.As(err, &oe) && oe.Reason == ssh.ResourceShortage
}

func TestWorkspaceHappyPaths(t *testing.T) {
	ex := fake.New()
	ex.ExecFunc = func(ctx context.Context, id workspace.ID, r workspace.ExecRequest) (workspace.ExecResult, error) {
		in, _ := io.ReadAll(r.Stdin)
		io.WriteString(r.Stdout, "out:"+string(in)+":"+r.Cwd+":"+r.Env["CI"])
		io.WriteString(r.Stderr, "err")
		return workspace.ExecResult{ExitCode: 3}, nil
	}
	h := newHarness(t, wsYAML, ex, nil)
	id := h.create()
	h.want(createReq(id), nil, transport.CodeOK) // idempotent, no second slot

	w := h.want(transport.WorkspaceRequest{Op: transport.OpWrite, ID: string(id), Path: "a/b.txt", Mode: 0o600, Length: 10, Truncate: true}, []byte("0123456789"), transport.CodeOK)
	if w.File == nil || w.File.Size != 10 || w.File.Mode != 0o600 {
		t.Fatalf("write: %+v", w.File)
	}
	res, data := h.do(transport.WorkspaceRequest{Op: transport.OpRead, ID: string(id), Path: "/workspace/a/b.txt", Offset: 4, Limit: 3}, nil)
	if res.Code != transport.CodeOK || string(data) != "456" || res.File.Length != 3 || res.File.EOF || res.File.Size != 10 {
		t.Fatalf("read: %+v %q", res, data)
	}
	res, data = h.do(transport.WorkspaceRequest{Op: transport.OpRead, ID: string(id), Path: "a/b.txt", Offset: 7}, nil)
	if string(data) != "789" || !res.File.EOF {
		t.Fatalf("read tail: %+v %q", res.File, data)
	}
	res = h.want(transport.WorkspaceRequest{Op: transport.OpRead, ID: string(id), Path: "nope"}, nil, transport.CodeNotFound)

	res, _ = h.do(transport.WorkspaceRequest{Op: transport.OpExec, ID: string(id), Argv: []string{"echo", "x"}, Cwd: "sub",
		Env: map[string]string{"CI": "1"}, StdinLen: 3, TimeoutMs: 2000}, []byte("abc"))
	if res.Code != transport.CodeOK || res.Exec == nil || res.Exec.ExitCode != 3 ||
		res.Exec.Stdout != "out:abc:/workspace/sub:1" || res.Exec.Stderr != "err" {
		t.Fatalf("exec: %+v %+v", res, res.Exec)
	}
	res = h.want(transport.WorkspaceRequest{Op: transport.OpList}, nil, transport.CodeOK)
	if len(res.List) != 1 || res.List[0].ID != string(id) || res.List[0].Image != "alpine:3.20" {
		t.Fatalf("list: %+v", res.List)
	}
	h.want(transport.WorkspaceRequest{Op: transport.OpDestroy, ID: string(id)}, nil, transport.CodeOK)
	h.want(transport.WorkspaceRequest{Op: transport.OpDestroy, ID: string(id)}, nil, transport.CodeOK) // idempotent
	h.want(transport.WorkspaceRequest{Op: transport.OpExec, ID: string(id), Argv: []string{"x"}}, nil, transport.CodeNotFound)
	if res = h.want(transport.WorkspaceRequest{Op: transport.OpList}, nil, transport.CodeOK); len(res.List) != 0 {
		t.Fatalf("list after destroy: %+v", res.List)
	}
}

func TestWorkspaceRevalidationRefusals(t *testing.T) {
	ex := fake.New()
	h := newHarness(t, wsYAML, ex, nil)
	id := h.create()
	sid := string(id)
	spec := func(mod func(*transport.WorkspaceSpec)) transport.WorkspaceRequest {
		s := &transport.WorkspaceSpec{Image: "alpine:3.20"}
		mod(s)
		return transport.WorkspaceRequest{Op: transport.OpCreate, ID: string(workspace.NewID()), Spec: s}
	}
	cases := []struct {
		name string
		req  transport.WorkspaceRequest
		body []byte
		code string
	}{
		{"image not allowed", spec(func(s *transport.WorkspaceSpec) { s.Image = "evil/img:1" }), nil, transport.CodeRefused},
		{"image wildcard", spec(func(s *transport.WorkspaceSpec) { s.Image = "ghcr.io/acme/dev:*" }), nil, transport.CodeRefused},
		{"cpu above cap", spec(func(s *transport.WorkspaceSpec) { s.CPUMilli = 2001 }), nil, transport.CodeRefused},
		{"memory above cap", spec(func(s *transport.WorkspaceSpec) { s.MemoryBytes = 2 << 30 }), nil, transport.CodeRefused},
		{"pids above cap", spec(func(s *transport.WorkspaceSpec) { s.Pids = 257 }), nil, transport.CodeRefused},
		{"tmpfs above cap", spec(func(s *transport.WorkspaceSpec) { s.TmpfsBytes = 65 << 20 }), nil, transport.CodeRefused},
		{"disk above cap", spec(func(s *transport.WorkspaceSpec) { s.DiskBytes = 2 << 30 }), nil, transport.CodeRefused},
		{"network egress not allowed", spec(func(s *transport.WorkspaceSpec) { s.NetworkMode = "egress" }), nil, transport.CodeRefused},
		{"secure runtime absent", spec(func(s *transport.WorkspaceSpec) { s.RequireSecureRuntime = true }), nil, transport.CodeRefused},
		{"max age above cap", spec(func(s *transport.WorkspaceSpec) { s.MaxAgeMs = 25 * 3600 * 1000 }), nil, transport.CodeRefused},
		{"reserved label", spec(func(s *transport.WorkspaceSpec) { s.Labels = map[string]string{"mirafleet.workspace": "x"} }), nil, transport.CodeRefused},
		{"bad pull", spec(func(s *transport.WorkspaceSpec) { s.Pull = "sometimes" }), nil, transport.CodeRefused},
		{"env denied even if central sent it", transport.WorkspaceRequest{Op: transport.OpExec, ID: sid, Argv: []string{"x"},
			Env: map[string]string{"ANTHROPIC_API_KEY": "k"}}, nil, transport.CodeRefused},
		{"env not on allowlist", transport.WorkspaceRequest{Op: transport.OpExec, ID: sid, Argv: []string{"x"},
			Env: map[string]string{"HOME": "/x"}}, nil, transport.CodeRefused},
		{"cwd escape", transport.WorkspaceRequest{Op: transport.OpExec, ID: sid, Argv: []string{"x"}, Cwd: "../../etc"}, nil, transport.CodeRefused},
		{"empty argv0", transport.WorkspaceRequest{Op: transport.OpExec, ID: sid, Argv: []string{""}}, nil, transport.CodeRefused},
		{"read path escape", transport.WorkspaceRequest{Op: transport.OpRead, ID: sid, Path: "../etc/passwd"}, nil, transport.CodeRefused},
		{"read outside root", transport.WorkspaceRequest{Op: transport.OpRead, ID: sid, Path: "/etc/passwd"}, nil, transport.CodeRefused},
		{"write path escape", transport.WorkspaceRequest{Op: transport.OpWrite, ID: sid, Path: "a/../../x", Length: 1}, []byte("x"), transport.CodeRefused},
		{"write to root", transport.WorkspaceRequest{Op: transport.OpWrite, ID: sid, Path: ".", Length: 1}, []byte("x"), transport.CodeRefused},
		{"write above max_file", transport.WorkspaceRequest{Op: transport.OpWrite, ID: sid, Path: "big", Length: 4097}, bytes.Repeat([]byte("x"), 4097), transport.CodeTooLarge},
		{"write offset above max_file", transport.WorkspaceRequest{Op: transport.OpWrite, ID: sid, Path: "big", Offset: 4090, Length: 10}, bytes.Repeat([]byte("x"), 10), transport.CodeTooLarge},
		{"body shorter than announced", transport.WorkspaceRequest{Op: transport.OpWrite, ID: sid, Path: "short", Length: 100}, bytes.Repeat([]byte("x"), 50), transport.CodeBadRequest},
		{"body longer than announced", transport.WorkspaceRequest{Op: transport.OpWrite, ID: sid, Path: "long", Length: 100}, bytes.Repeat([]byte("x"), 200), transport.CodeBadRequest},
		{"stdin longer than announced", transport.WorkspaceRequest{Op: transport.OpExec, ID: sid, Argv: []string{"x"}, StdinLen: 2}, []byte("abcd"), transport.CodeBadRequest},
		{"stdin shorter than announced", transport.WorkspaceRequest{Op: transport.OpExec, ID: sid, Argv: []string{"x"}, StdinLen: 5}, []byte("ab"), transport.CodeBadRequest},
	}
	for _, c := range cases {
		res, _, err := h.try(c.req, c.body)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if res.Code != c.code {
			t.Errorf("%s: code %q (%s), want %q", c.name, res.Code, res.Message, c.code)
		}
	}
	// None of the refused writes landed.
	for _, p := range []string{"big", "short", "long"} {
		h.want(transport.WorkspaceRequest{Op: transport.OpRead, ID: sid, Path: p}, nil, transport.CodeNotFound)
	}
	// Nothing refused created a workspace or took a slot.
	if l := h.want(transport.WorkspaceRequest{Op: transport.OpList}, nil, transport.CodeOK); len(l.List) != 1 {
		t.Fatalf("list: %+v", l.List)
	}
	h.create() // the second slot is still free
}

// Requests the strict decoder or Validate refuse never get a channel (mode 0o4755, oversize bodies).
func TestWorkspaceMalformedRejected(t *testing.T) {
	h := newHarness(t, wsYAML, fake.New(), nil)
	id := h.create()
	for name, req := range map[string]map[string]any{
		"setuid mode":       {"op": "write", "id": string(id), "path": "x", "mode": 0o4755, "length": 1},
		"length too big":    {"op": "write", "id": string(id), "path": "x", "length": transport.MaxBodyBytes + 1},
		"stdin too big":     {"op": "exec", "id": string(id), "argv": []string{"x"}, "stdin_len": transport.MaxBodyBytes + 1},
		"bad id":            {"op": "exec", "id": "../../etc", "argv": []string{"x"}},
		"unknown field":     {"op": "list", "evil": true},
		"unknown op":        {"op": "shell", "id": string(id)},
		"exec fields on rd": {"op": "read", "id": string(id), "path": "x", "argv": []string{"x"}},
	} {
		p, _ := json.Marshal(req)
		_, err := h.openRaw(transport.ChanWorkspace, p)
		var oe *ssh.OpenChannelError
		if !errors.As(err, &oe) || oe.Reason != ssh.ConnectionFailed {
			t.Errorf("%s: err=%v, want ConnectionFailed reject", name, err)
		}
	}
	if _, err := h.openRaw(transport.ChanWorkspace, bytes.Repeat([]byte("x"), transport.MaxWorkspaceRequestBytes+1)); err == nil {
		t.Error("oversize request accepted")
	}
}

// Direct checks of the local policy where the wire format already stops the request.
func TestPrepareDirect(t *testing.T) {
	s := newWSState(fake.New(), parseWS(t, wsYAML))
	id := string(workspace.NewID())
	for name, req := range map[string]*transport.WorkspaceRequest{
		"mode 04755":    {Op: transport.OpWrite, ID: id, Path: "x", Mode: 0o4755, Length: 1},
		"mode negative": {Op: transport.OpWrite, ID: id, Path: "x", Mode: -1, Length: 1},
		"bad id":        {Op: transport.OpExec, ID: "ABC", Argv: []string{"x"}},
		"nul argv":      {Op: transport.OpExec, ID: id, Argv: []string{"x\x00y"}},
		"stdin huge":    {Op: transport.OpExec, ID: id, Argv: []string{"x"}, StdinLen: transport.MaxBodyBytes + 1},
		"unknown op":    {Op: "shell", ID: id},
		"env bad name":  {Op: transport.OpExec, ID: id, Argv: []string{"x"}, Env: map[string]string{"lang": "x"}},
	} {
		if _, err := s.prepare(req); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A timeout above exec_max is cut to it; a smaller one is kept; zero means exec_max.
	for ms, want := range map[int64]time.Duration{0: 5 * time.Second, 100: 100 * time.Millisecond, 3600000: 5 * time.Second} {
		op, err := s.prepare(&transport.WorkspaceRequest{Op: transport.OpExec, ID: id, Argv: []string{"x"}, TimeoutMs: ms})
		if err != nil || op.timeout != want {
			t.Errorf("timeout_ms=%d: %v %v, want %v", ms, op, err, want)
		}
	}
}

func TestWorkspaceSlotsFull(t *testing.T) {
	ex := fake.New()
	h := newHarness(t, wsYAML, ex, nil) // concurrent: 2
	a, b := h.create(), h.create()
	_, _, err := h.try(createReq(workspace.NewID()), nil)
	if !busy(err) {
		t.Fatalf("third create: %v, want ResourceShortage reject", err)
	}
	// A refused create must not have left anything behind.
	if l := h.want(transport.WorkspaceRequest{Op: transport.OpList}, nil, transport.CodeOK); len(l.List) != 2 {
		t.Fatalf("list: %+v", l.List)
	}
	h.want(createReq(a), nil, transport.CodeOK) // existing id: idempotent, not a new slot
	h.want(transport.WorkspaceRequest{Op: transport.OpDestroy, ID: string(b)}, nil, transport.CodeOK)
	h.create() // slot freed
	// A failing create frees its slot.
	h.want(transport.WorkspaceRequest{Op: transport.OpDestroy, ID: string(a)}, nil, transport.CodeOK)
	ex.Images = []string{"nothing:1"} // the executor now refuses alpine
	h.want(createReq(workspace.NewID()), nil, transport.CodeRefused)
	ex.Images = nil
	h.create()
}

// A runner restart keeps existing workspaces: slots are seeded from the executor.
func TestWorkspaceSlotsSeededFromExecutor(t *testing.T) {
	ex := fake.New()
	for i := 0; i < 2; i++ {
		if err := ex.Create(context.Background(), workspace.Spec{ID: workspace.NewID(), Image: "alpine:3.20"}); err != nil {
			t.Fatal(err)
		}
	}
	h := newHarness(t, wsYAML, ex, nil)
	if _, _, err := h.try(createReq(workspace.NewID()), nil); !busy(err) {
		t.Fatalf("create with 2 pre-existing: %v", err)
	}
}

func TestWorkspaceExecConcurrencyCap(t *testing.T) {
	ex := fake.New()
	gate := make(chan struct{})
	var running atomic.Int32
	ex.ExecFunc = func(ctx context.Context, id workspace.ID, r workspace.ExecRequest) (workspace.ExecResult, error) {
		running.Add(1)
		defer running.Add(-1)
		select {
		case <-gate:
		case <-ctx.Done():
		}
		return workspace.ExecResult{}, nil
	}
	h := newHarness(t, wsYAML, ex, nil)
	id, other := h.create(), h.create()
	req := transport.WorkspaceRequest{Op: transport.OpExec, ID: string(id), Argv: []string{"sleep"}}
	var wg sync.WaitGroup
	for i := 0; i < maxExecPerWorkspace; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if res, _, err := h.try(req, nil); err != nil || res.Code != transport.CodeOK {
				t.Errorf("exec: %v %+v", err, res)
			}
		}()
	}
	deadline := time.Now().Add(10 * time.Second)
	for running.Load() < maxExecPerWorkspace && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if _, _, err := h.try(req, nil); !busy(err) {
		t.Errorf("fifth exec on the same workspace: %v, want ResourceShortage", err)
	}
	// The cap is per workspace.
	o := req
	o.ID = string(other)
	ch, err := h.open(&o)
	if err != nil {
		t.Errorf("exec on another workspace: %v", err)
	} else {
		ch.Close()
	}
	close(gate)
	wg.Wait()
	if res, _, err := h.try(req, nil); err != nil || res.Code != transport.CodeOK {
		t.Errorf("exec after drain: %v %+v", err, res)
	}
}

func TestWorkspaceChannelCloseCancelsExec(t *testing.T) {
	ex := fake.New()
	started, cancelled := make(chan struct{}), make(chan struct{})
	ex.ExecFunc = func(ctx context.Context, id workspace.ID, r workspace.ExecRequest) (workspace.ExecResult, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return workspace.ExecResult{ExitCode: -1}, ctx.Err()
	}
	h := newHarness(t, wsYAML, ex, nil)
	id := h.create()
	ch, err := h.open(&transport.WorkspaceRequest{Op: transport.OpExec, ID: string(id), Argv: []string{"sleep", "999"}, TimeoutMs: 60000})
	if err != nil {
		t.Fatal(err)
	}
	ch.CloseWrite() // central always half-closes; that alone must not cancel
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("exec did not start")
	}
	select {
	case <-cancelled:
		t.Fatal("CloseWrite cancelled the exec")
	case <-time.After(3 * wsCancelPoll):
	}
	ch.Close()
	select {
	case <-cancelled:
	case <-time.After(10 * time.Second):
		t.Fatal("channel close did not cancel the exec")
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(h.log.String(), `"outcome":"cancelled"`) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(h.log.String(), `"outcome":"cancelled"`) {
		t.Fatalf("no cancelled audit row:\n%s", h.log.String())
	}
}

func TestWorkspaceExecTimeouts(t *testing.T) {
	ex := fake.New()
	ex.ExecFunc = func(ctx context.Context, id workspace.ID, r workspace.ExecRequest) (workspace.ExecResult, error) {
		<-ctx.Done()
		return workspace.ExecResult{ExitCode: -1}, ctx.Err()
	}
	h := newHarness(t, strings.Replace(wsYAML, "exec_max: 5s", "exec_max: 200ms", 1), ex, nil)
	id := h.create()
	for _, ms := range []int64{50, 0, 600000} { // request's own, default, far above exec_max
		start := time.Now()
		res := h.want(transport.WorkspaceRequest{Op: transport.OpExec, ID: string(id), Argv: []string{"sleep"}, TimeoutMs: ms}, nil, transport.CodeOK)
		if !res.Exec.TimedOut {
			t.Errorf("timeout_ms=%d: not timed out: %+v", ms, res.Exec)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("timeout_ms=%d took %v", ms, d)
		}
	}
}

func TestWorkspaceExecOutputFitsResultCap(t *testing.T) {
	ex := fake.New()
	ex.ExecFunc = func(ctx context.Context, id workspace.ID, r workspace.ExecRequest) (workspace.ExecResult, error) {
		junk := bytes.Repeat([]byte{0x01, '<', 0xff, '"'}, 256<<10) // 1 MiB each, worst case for JSON
		r.Stdout.Write(junk)
		r.Stderr.Write(junk)
		return workspace.ExecResult{}, nil
	}
	h := newHarness(t, wsYAML, ex, nil)
	id := h.create()
	res := h.want(transport.WorkspaceRequest{Op: transport.OpExec, ID: string(id), Argv: []string{"x"}}, nil, transport.CodeOK)
	if !res.Exec.Truncated {
		t.Error("not marked truncated")
	}
	if n := len(res.Exec.Stdout) + len(res.Exec.Stderr); n == 0 {
		t.Error("no output kept")
	}
}

func TestWorkspaceAuditNeverHasSecrets(t *testing.T) {
	ex := fake.New()
	h := newHarness(t, wsYAML, ex, nil)
	id := h.create()
	const envVal, content = "ENVVALUE-s3cr3t-1", "FILECONTENT-t0ps3cret"
	h.want(transport.WorkspaceRequest{Op: transport.OpWrite, ID: string(id), Path: "f.txt", Length: int64(len(content))}, []byte(content), transport.CodeOK)
	h.want(transport.WorkspaceRequest{Op: transport.OpRead, ID: string(id), Path: "f.txt"}, nil, transport.CodeOK)
	long := strings.Repeat("a", 10<<10)
	h.want(transport.WorkspaceRequest{Op: transport.OpExec, ID: string(id), Argv: []string{"echo", long}, Cwd: "d",
		Env: map[string]string{"CI": envVal, "LANG": envVal}, StdinLen: 5, TimeoutMs: 1000}, []byte("STDIN"), transport.CodeOK)
	// A refused request must not echo its env value either.
	h.want(transport.WorkspaceRequest{Op: transport.OpExec, ID: string(id), Argv: []string{"x"}, Env: map[string]string{"ANTHROPIC_API_KEY": envVal}}, nil, transport.CodeRefused)
	h.want(transport.WorkspaceRequest{Op: transport.OpList}, nil, transport.CodeOK)
	h.want(transport.WorkspaceRequest{Op: transport.OpDestroy, ID: string(id)}, nil, transport.CodeOK)
	// Outcome rows are written before the reply, so the log is complete by now.
	log := h.log.String()
	for _, secret := range []string{envVal, content, "STDIN"} {
		if strings.Contains(log, secret) {
			t.Fatalf("audit leaks %q:\n%s", secret, log)
		}
	}
	sum := sha256.Sum256([]byte(content))
	for _, want := range []string{`"outcome":"start"`, `"outcome":"ok"`, `"outcome":"refused"`, `"action":"workspace.write"`,
		`"action":"workspace.exec"`, `"action":"workspace.list"`, `"action":"workspace.destroy"`,
		hex.EncodeToString(sum[:]), `"env_names":"CI,LANG"`, `"path":"f.txt"`, `"cwd":"d"`, `"bytes":"21"`} {
		if !strings.Contains(log, want) {
			t.Errorf("audit lacks %s:\n%s", want, log)
		}
	}
	// Every op has a start row and an outcome row with the same request id; argv is clipped to 4 KiB.
	starts, ends := map[string]bool{}, map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(log), "\n") {
		var ev audit.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("bad audit line %q: %v", line, err)
		}
		if ev.RequestID == "" {
			t.Errorf("row without request id: %s", line)
		}
		if ev.Outcome == "start" {
			starts[ev.RequestID] = true
		} else {
			ends[ev.RequestID] = true
		}
		if a := ev.Params["argv"]; len(a) > auditArgvMax {
			t.Errorf("argv %d bytes", len(a))
		}
	}
	for rid := range starts {
		if !ends[rid] {
			t.Errorf("request %s has no outcome row", rid)
		}
	}
	if len(starts) < 7 {
		t.Errorf("only %d start rows", len(starts))
	}
}

func TestRunnerModeExclusive(t *testing.T) {
	acts, err := actions.Parse([]byte(`{actions: {}}`))
	if err != nil {
		t.Fatal(err)
	}
	cfg := parseWS(t, wsYAML)
	ex := fake.New()
	for name, c := range map[string]Config{
		"both":              {Actions: acts, Workspace: ex, WorkspaceConfig: cfg},
		"executor only":     {Workspace: ex},
		"config only":       {WorkspaceConfig: cfg},
		"both but exec set": {Actions: acts, WorkspaceConfig: cfg},
	} {
		if c.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
		if err := New(c).Run(context.Background()); err == nil {
			t.Errorf("%s: Run did not refuse", name)
		}
	}
	for name, c := range map[string]Config{"actions": {Actions: acts}, "workspace": {Workspace: ex, WorkspaceConfig: cfg}, "neither": {}} {
		if err := c.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestWorkspaceChannelProhibitedWithoutConfig(t *testing.T) {
	acts, err := actions.Parse([]byte(`{actions: {}}`))
	if err != nil {
		t.Fatal(err)
	}
	// An actions runner refuses workspace@ with Prohibited.
	h := newHarness(t, "", nil, func(c *Config) { c.Actions = acts })
	_, err = h.open(&transport.WorkspaceRequest{Op: transport.OpList})
	var oe *ssh.OpenChannelError
	if !errors.As(err, &oe) || oe.Reason != ssh.Prohibited {
		t.Fatalf("workspace@ on an actions runner: %v", err)
	}
	// A workspace runner refuses exec@ with Prohibited.
	h = newHarness(t, wsYAML, fake.New(), nil)
	p, _ := json.Marshal(transport.ExecRequest{Action: "anything"})
	_, err = h.openRaw(transport.ChanExec, p)
	if !errors.As(err, &oe) || oe.Reason != ssh.Prohibited {
		t.Fatalf("exec@ on a workspace runner: %v", err)
	}
	// A runner with neither refuses both.
	h = newHarness(t, "", nil, nil)
	if _, err = h.open(&transport.WorkspaceRequest{Op: transport.OpList}); !errors.As(err, &oe) || oe.Reason != ssh.Prohibited {
		t.Fatalf("workspace@ on a bare runner: %v", err)
	}
	if _, err = h.openRaw(transport.ChanExec, p); !errors.As(err, &oe) || oe.Reason != ssh.Prohibited {
		t.Fatalf("exec@ on a bare runner: %v", err)
	}
}

func TestWorkspaceHeartbeatFields(t *testing.T) {
	y := strings.Replace(wsYAML, "root: /var/lib/mirafleet/ws", "root: /var/lib/mirafleet/ws\n  secure_runtime: io.containerd.runsc.v1", 1)
	h := newHarness(t, y, fake.New(), nil)
	select {
	case hb := <-h.hbs:
		if err := hb.Validate(); err != nil {
			t.Fatal(err)
		}
		if hb.Slots != 2 || hb.Executor != "fake" || hb.SecureRuntime != "io.containerd.runsc.v1" ||
			len(hb.Images) != 2 || hb.Images[0] != "alpine:3.20" || hb.Images[1] != "ghcr.io/acme/dev:*" {
			t.Fatalf("heartbeat: %+v", hb)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no heartbeat")
	}
	// An actions runner sends none of the workspace fields.
	acts, _ := actions.Parse([]byte(`{actions: {}}`))
	h = newHarness(t, "", nil, func(c *Config) { c.Actions = acts })
	hb := <-h.hbs
	if hb.Slots != 0 || hb.Executor != "" || hb.SecureRuntime != "" || len(hb.Images) != 0 {
		t.Fatalf("actions heartbeat: %+v", hb)
	}
}

// The secure-runtime requirement is honoured when the runner has one.
func TestWorkspaceSecureRuntimeAccepted(t *testing.T) {
	y := strings.Replace(wsYAML, "root: /var/lib/mirafleet/ws", "root: /var/lib/mirafleet/ws\n  secure_runtime: io.containerd.runsc.v1", 1)
	ex := fake.New()
	ex.SecureRuntime = true
	h := newHarness(t, y, ex, nil)
	req := createReq(workspace.NewID())
	req.Spec.RequireSecureRuntime = true
	h.want(req, nil, transport.CodeOK)
}

const wsEgressYAML = `
executor: containerd
concurrent: 2
images: ["alpine:3.20"]
limits: {cpus: 2, memory: 1GiB, pids: 256, tmpfs: 64MiB, disk: 1GiB, max_file: 4KiB, exec_max: 5s}
network: [none, egress]
containerd:
  namespace: mirafleet
  root: /var/lib/mirafleet/ws
`

func egressReq(rules ...transport.EgressRule) transport.WorkspaceRequest {
	r := createReq(workspace.NewID())
	r.Spec.NetworkMode = "egress"
	r.Spec.Allow = rules
	return r
}

// A compromised central must not be able to open private ranges or send rules the egress proxy
// would reject; CIDR/IP-literal rules need egress_cidr_rules in workspace.yaml.
func TestWorkspaceEgressRulesRevalidated(t *testing.T) {
	h := newHarness(t, wsEgressYAML, fake.New(), nil)
	for name, rule := range map[string]transport.EgressRule{
		"cidr":         {Host: "10.0.0.0/8", Port: 443},
		"ip literal":   {Host: "10.1.2.3", Port: 443},
		"ipv6 literal": {Host: "fd00::1", Port: 443},
		"metadata":     {Host: "169.254.169.254", Port: 80},
		"bad host":     {Host: "not a host", Port: 443},
		"bad wildcard": {Host: "*.com*", Port: 443},
	} {
		res, _ := h.do(egressReq(rule), nil)
		if res.Code != transport.CodeRefused {
			t.Errorf("%s: code %q, want refused", name, res.Code)
		}
	}
	h.want(egressReq(transport.EgressRule{Host: "api.example.com", Port: 443}, transport.EgressRule{Host: "*.example.org", Port: 8443}), nil, transport.CodeOK)

	on := newHarness(t, strings.Replace(wsEgressYAML, "network: [none, egress]", "network: [none, egress]\negress_cidr_rules: true", 1), fake.New(), nil)
	on.want(egressReq(transport.EgressRule{Host: "10.0.0.0/8", Port: 443}), nil, transport.CodeOK)
	on.want(egressReq(transport.EgressRule{Host: "10.1.2.3", Port: 443}), nil, transport.CodeOK)
	// Even with the flag, rules the proxy refuses (loopback, metadata) stay refused.
	on.want(egressReq(transport.EgressRule{Host: "169.254.169.254", Port: 80}), nil, transport.CodeRefused)
	on.want(egressReq(transport.EgressRule{Host: "127.0.0.1", Port: 80}), nil, transport.CodeRefused)
}

// destroyCounter wraps the fake executor to fail Create and count Destroy calls.
type destroyCounter struct {
	*fake.Executor
	failCreate atomic.Bool
	destroys   atomic.Int32
}

func (d *destroyCounter) Create(ctx context.Context, s workspace.Spec) error {
	if d.failCreate.Load() {
		return errors.New("boom: partial state left")
	}
	return d.Executor.Create(ctx, s)
}

func (d *destroyCounter) Destroy(ctx context.Context, id workspace.ID) error {
	d.destroys.Add(1)
	return d.Executor.Destroy(ctx, id)
}

func TestWorkspaceFailedCreateDestroys(t *testing.T) {
	dc := &destroyCounter{Executor: fake.New()}
	h := newHarness(t, wsYAML, dc.Executor, func(c *Config) { c.Workspace = dc })
	dc.failCreate.Store(true)
	h.want(createReq(workspace.NewID()), nil, transport.CodeInternal)
	if n := dc.destroys.Load(); n != 1 {
		t.Fatalf("failed create: %d destroys, want 1", n)
	}
	// A failed re-create of an existing, confirmed workspace must not destroy it.
	dc.failCreate.Store(false)
	id := h.create()
	dc.failCreate.Store(true)
	before := dc.destroys.Load()
	h.want(createReq(id), nil, transport.CodeInternal)
	if dc.destroys.Load() != before {
		t.Fatal("a failed duplicate create destroyed a live workspace")
	}
	// The slot of the failed create is free again.
	dc.failCreate.Store(false)
	h.create()
}

type blockCloser struct {
	closed chan struct{}
	once   sync.Once
}

func (b *blockCloser) Close() error { b.once.Do(func() { close(b.closed) }); return nil }

// A central that never reads must not pin a slot: the result/body write is bounded and closes
// the channel. (The SSH flow-control window is larger than any single result, so a real peer
// cannot block a write; this exercises the bound directly.)
func TestWriteBoundedClosesStuckWrite(t *testing.T) {
	old := wsWriteTimeout
	wsWriteTimeout = 50 * time.Millisecond
	defer func() { wsWriteTimeout = old }()
	bc := &blockCloser{closed: make(chan struct{})}
	start := time.Now()
	err := writeBounded(bc, func() error { // a write that only returns once the channel is closed
		<-bc.closed
		return io.ErrClosedPipe
	})
	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("stuck write not bounded: %v after %v", err, time.Since(start))
	}
	// A fast write is untouched.
	if err := writeBounded(&blockCloser{closed: make(chan struct{})}, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
}

// Refused requests announcing large bodies are bounded before the body is read: at most
// wsRefuseMax drain at once, the rest are rejected, and the bodies are discarded not buffered.
func TestWorkspaceRefusedChannelsBounded(t *testing.T) {
	h := newHarness(t, wsYAML, fake.New(), nil)
	mk := func() transport.WorkspaceRequest { // unknown workspace -> refused after the body
		return transport.WorkspaceRequest{Op: transport.OpExec, ID: string(workspace.NewID()),
			Argv: []string{"x"}, StdinLen: transport.MaxBodyBytes}
	}
	const n = 4 * wsRefuseMax
	var chans []ssh.Channel
	rejected := 0
	for i := 0; i < n; i++ {
		req := mk()
		ch, err := h.open(&req)
		if err != nil {
			if !busy(err) {
				t.Fatalf("open %d: %v", i, err)
			}
			rejected++
			continue
		}
		chans = append(chans, ch) // accepted; the body is never sent
	}
	if len(chans) > wsRefuseMax || rejected < n-wsRefuseMax {
		t.Fatalf("accepted %d, rejected %d of %d (cap %d)", len(chans), rejected, n, wsRefuseMax)
	}
	// Closing them frees the tokens.
	for _, c := range chans {
		c.Close()
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		req := mk()
		res, _, err := h.try(req, bytes.Repeat([]byte("a"), transport.MaxBodyBytes))
		if err == nil && res.Code == transport.CodeNotFound {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("tokens not released: %v %v", res, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// usageEx is a fake executor with the optional disk-usage capability.
type usageEx struct {
	*fake.Executor
	mu   sync.Mutex
	used map[workspace.ID]int64
}

func (u *usageEx) Usage(_ context.Context, id workspace.ID) (workspace.DiskUsage, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	n := u.used[id]
	const quota = 100
	return workspace.DiskUsage{Used: n, Quota: quota, Over: n >= quota, Kill: n >= 2*quota}, nil
}

func newSamplerRunner(t *testing.T, ex *usageEx, log *syncBuf) *Runner {
	t.Helper()
	r := New(Config{Central: "127.0.0.1:1", Workspace: ex, WorkspaceConfig: parseWS(t, wsYAML), Audit: audit.New(log),
		BackoffMin: time.Hour})
	if r.cfgErr != nil {
		t.Fatal(r.cfgErr)
	}
	return r
}

func TestDiskSamplerDestroysAtTwiceQuota(t *testing.T) {
	ex := &usageEx{Executor: fake.New(), used: map[workspace.ID]int64{}}
	log := &syncBuf{}
	r := newSamplerRunner(t, ex, log)
	over, kill, ok := workspace.NewID(), workspace.NewID(), workspace.NewID()
	for _, id := range []workspace.ID{over, kill, ok} {
		if err := ex.Create(context.Background(), workspace.Spec{ID: id, Image: "img", Pull: workspace.PullNever}); err != nil {
			t.Fatal(err)
		}
	}
	ex.used[over], ex.used[kill], ex.used[ok] = 150, 200, 10 // soft over, 2x, fine
	r.sampleDisks(context.Background())
	infos, err := ex.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[workspace.ID]bool{}
	for _, in := range infos {
		got[in.ID] = true
	}
	if !got[over] || !got[ok] || got[kill] {
		t.Fatalf("survivors %v", got)
	}
	if l := log.String(); !strings.Contains(l, "workspace.disk_kill") || !strings.Contains(l, string(kill)) || strings.Contains(l, string(over)) {
		t.Fatalf("audit:\n%s", l)
	}
	r.ws.mu.Lock()
	_, stillLive := r.ws.live[kill]
	r.ws.mu.Unlock()
	if stillLive {
		t.Fatal("slot not released")
	}
}

func TestDiskSamplerRunsFromRunAndStops(t *testing.T) {
	old := diskSampleEvery
	diskSampleEvery = 10 * time.Millisecond
	defer func() { diskSampleEvery = old }()
	ex := &usageEx{Executor: fake.New(), used: map[workspace.ID]int64{}}
	log := &syncBuf{}
	r := newSamplerRunner(t, ex, log)
	id := workspace.NewID()
	if err := ex.Create(context.Background(), workspace.Spec{ID: id, Image: "img", Pull: workspace.PullNever}); err != nil {
		t.Fatal(err)
	}
	ex.mu.Lock()
	ex.used[id] = 1000
	ex.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(log.String(), "workspace.disk_kill") {
		if time.Now().After(deadline) {
			t.Fatal("workspace not destroyed by the sampler")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop")
	}
}

// gateEx blocks Create until released and records Destroy calls.
type gateEx struct {
	*fake.Executor
	entered  chan struct{}
	release  chan struct{}
	mu       sync.Mutex
	destroyd []workspace.ID
}

func (g *gateEx) Create(ctx context.Context, s workspace.Spec) error {
	g.entered <- struct{}{}
	<-g.release
	return g.Executor.Create(ctx, s)
}

func (g *gateEx) Destroy(ctx context.Context, id workspace.ID) error {
	g.mu.Lock()
	g.destroyd = append(g.destroyd, id)
	g.mu.Unlock()
	return g.Executor.Destroy(ctx, id)
}

func (g *gateEx) destroyed(id workspace.ID) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, d := range g.destroyd {
		if d == id {
			return true
		}
	}
	return false
}

// A destroy that lands while the create is still in flight must not lose the container: the
// runner answers ok, runs the destroy once the create settles, and only then frees the slot.
func TestWorkspaceDestroyDuringCreate(t *testing.T) {
	g := &gateEx{Executor: fake.New(), entered: make(chan struct{}, 4), release: make(chan struct{})}
	h := newHarness(t, wsYAML, g.Executor, func(c *Config) { c.Workspace = g })
	// E-17a: never leave Create blocked on an early Fatal (registered after the harness so it
	// runs before the harness shuts the runner down).
	var gateOnce sync.Once
	closeGate := func() { gateOnce.Do(func() { close(g.release) }) }
	t.Cleanup(closeGate)
	id := workspace.NewID()
	created := make(chan error, 1)
	go func() {
		res, _, err := h.try(createReq(id), nil)
		if err == nil && res.Code != transport.CodeOK {
			err = errors.New("create: " + res.Code)
		}
		created <- err
	}()
	<-g.entered
	h.want(transport.WorkspaceRequest{Op: transport.OpDestroy, ID: string(id)}, nil, transport.CodeOK)
	h.want(transport.WorkspaceRequest{Op: transport.OpExec, ID: string(id), Argv: []string{"x"}}, nil, transport.CodeNotFound)
	if _, _, err := h.try(createReq(id), nil); !busy(err) {
		t.Fatalf("create of a doomed id: %v, want busy", err)
	}
	closeGate()
	if err := <-created; err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !g.destroyed(id) {
		if time.Now().After(deadline) {
			t.Fatal("deferred destroy never ran")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for {
		infos, _ := g.Executor.List(context.Background())
		h.runnr.ws.mu.Lock()
		n := len(h.runnr.ws.live)
		h.runnr.ws.mu.Unlock()
		if len(infos) == 0 && n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not cleaned up: %d containers, %d entries", len(infos), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.create() // slots are free again (concurrent: 2)
	h.create()
}

// ctlEx gates Create and Destroy and scripts Destroy errors, counting executor calls.
type ctlEx struct {
	*fake.Executor
	entered   chan struct{} // Create entered (buffered)
	release   chan struct{} // closes to let Create through; nil means no gate
	dEntered  chan struct{} // Destroy entered (buffered)
	dRelease  chan struct{} // closes to let Destroy through; nil means no gate
	once      sync.Once
	mu        sync.Mutex
	destroyed int
	destroyEr error
}

func newCtlEx() *ctlEx {
	return &ctlEx{Executor: fake.New(), entered: make(chan struct{}, 8), release: make(chan struct{}),
		dEntered: make(chan struct{}, 8)}
}

func (c *ctlEx) Create(ctx context.Context, s workspace.Spec) error {
	c.entered <- struct{}{}
	<-c.release
	return c.Executor.Create(ctx, s)
}

func (c *ctlEx) Destroy(ctx context.Context, id workspace.ID) error {
	c.mu.Lock()
	c.destroyed++
	err, gate := c.destroyEr, c.dRelease
	c.mu.Unlock()
	c.dEntered <- struct{}{}
	if gate != nil {
		<-gate
	}
	if err != nil {
		return err
	}
	return c.Executor.Destroy(ctx, id)
}

func (c *ctlEx) set(err error, gate chan struct{}) {
	c.mu.Lock()
	c.destroyEr, c.dRelease = err, gate
	c.mu.Unlock()
}

func (c *ctlEx) destroys() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.destroyed
}

func (c *ctlEx) openGate() func() {
	return func() { c.once.Do(func() { close(c.release) }) }
}

func (h *wsHarness) slots() int {
	h.runnr.ws.mu.Lock()
	defer h.runnr.ws.mu.Unlock()
	return len(h.runnr.ws.live)
}

func (h *wsHarness) entry(id workspace.ID) (doomed, confirmed, ok bool) {
	h.runnr.ws.mu.Lock()
	defer h.runnr.ws.mu.Unlock()
	e := h.runnr.ws.live[id]
	if e == nil {
		return false, false, false
	}
	return e.doomed, e.confirmed, true
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out: " + what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func recv(t *testing.T, ch chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out: " + what)
	}
}

func startGatedCreate(h *wsHarness, c *ctlEx, id workspace.ID) chan error {
	created := make(chan error, 1)
	go func() {
		res, _, err := h.try(createReq(id), nil)
		if err == nil && res.Code != transport.CodeOK {
			err = errors.New("create: " + res.Code)
		}
		created <- err
	}()
	return created
}

// E-13: a failed deferred destroy keeps the entry (slot held, confirmed); a later destroy takes
// the normal path and frees the slot exactly once.
func TestWorkspaceDoomedDestroyFailureKeepsEntry(t *testing.T) {
	c := newCtlEx()
	h := newHarness(t, wsYAML, c.Executor, func(cfg *Config) { cfg.Workspace = c })
	t.Cleanup(c.openGate())
	id := workspace.NewID()
	c.set(errors.New("boom"), nil)
	created := startGatedCreate(h, c, id)
	recv(t, c.entered, "create entered")
	h.want(transport.WorkspaceRequest{Op: transport.OpDestroy, ID: string(id)}, nil, transport.CodeOK)
	if d, _, ok := h.entry(id); !ok || !d {
		t.Fatalf("entry doomed=%v ok=%v, want doomed", d, ok)
	}
	c.openGate()()
	if err := <-created; err != nil {
		t.Fatal(err)
	}
	recv(t, c.dEntered, "deferred destroy")
	waitFor(t, "entry kept as confirmed", func() bool {
		d, cf, ok := h.entry(id)
		return ok && !d && cf
	})
	if n := h.slots(); n != 1 {
		t.Fatalf("slots = %d after failed deferred destroy, want 1", n)
	}
	// A later destroy that fails is still retryable and still holds the slot.
	h.want(transport.WorkspaceRequest{Op: transport.OpDestroy, ID: string(id)}, nil, transport.CodeInternal)
	if n := h.slots(); n != 1 {
		t.Fatalf("slots = %d after failed retry, want 1", n)
	}
	c.set(nil, nil)
	h.want(transport.WorkspaceRequest{Op: transport.OpDestroy, ID: string(id)}, nil, transport.CodeOK)
	if n := h.slots(); n != 0 {
		t.Fatalf("slots = %d after successful destroy, want 0", n)
	}
	h.want(transport.WorkspaceRequest{Op: transport.OpDestroy, ID: string(id)}, nil, transport.CodeOK)
	if n := h.slots(); n != 0 {
		t.Fatalf("slots = %d after repeat destroy, want 0", n)
	}
}

// E-14: a second destroy arriving while the deferred destroy runs answers ok without freeing
// the slot or calling the executor; the slot count never exceeds concurrent (2).
func TestWorkspaceDoomedDoubleDestroyHoldsSlot(t *testing.T) {
	c := newCtlEx()
	h := newHarness(t, wsYAML, c.Executor, func(cfg *Config) { cfg.Workspace = c })
	t.Cleanup(c.openGate())
	dGate := make(chan struct{})
	var dOnce sync.Once
	openD := func() { dOnce.Do(func() { close(dGate) }) }
	t.Cleanup(openD)
	c.set(nil, dGate)
	atMost := func(step string, want int) {
		t.Helper()
		if n := h.slots(); n != want || n > 2 {
			t.Fatalf("%s: slots = %d, want %d (concurrent 2)", step, n, want)
		}
	}
	id := workspace.NewID()
	created := startGatedCreate(h, c, id)
	recv(t, c.entered, "create entered")
	atMost("create in flight", 1)
	h.want(transport.WorkspaceRequest{Op: transport.OpDestroy, ID: string(id)}, nil, transport.CodeOK)
	atMost("first destroy", 1)
	c.openGate()()
	// The create's own answer waits for createDone, which runs the deferred destroy.
	recv(t, c.dEntered, "deferred destroy running")
	atMost("deferred destroy running", 1)
	h.want(transport.WorkspaceRequest{Op: transport.OpDestroy, ID: string(id)}, nil, transport.CodeOK)
	atMost("second destroy", 1)
	if n := c.destroys(); n != 1 {
		t.Fatalf("executor destroys = %d, want 1", n)
	}
	// The other slot is usable, a third create is busy.
	other := workspace.NewID()
	h.want(createReq(other), nil, transport.CodeOK)
	atMost("other create", 2)
	if _, _, err := h.try(createReq(workspace.NewID()), nil); !busy(err) {
		t.Fatalf("third create: %v, want busy", err)
	}
	atMost("third create refused", 2)
	openD()
	if err := <-created; err != nil {
		t.Fatal(err)
	}
	waitFor(t, "slot freed", func() bool { return h.slots() == 1 })
	atMost("deferred destroy done", 1)
	h.want(createReq(workspace.NewID()), nil, transport.CodeOK)
	atMost("slot reused", 2)
}

// notFoundEx answers every Destroy with ErrNotFound (nothing there).
type notFoundEx struct{ *fake.Executor }

func (notFoundEx) Destroy(context.Context, workspace.ID) error { return workspace.ErrNotFound }

// E-15: a destroy of an unknown id leaves a tombstone, so a create arriving later is refused
// (not_found); the tombstone expires.
func TestWorkspaceTombstoneRefusesLateCreate(t *testing.T) {
	ex := fake.New()
	h := newHarness(t, wsYAML, ex, func(cfg *Config) { cfg.Workspace = notFoundEx{ex} })
	now := time.Unix(1000, 0)
	var cm sync.Mutex
	h.runnr.ws.mu.Lock()
	h.runnr.ws.now = func() time.Time { cm.Lock(); defer cm.Unlock(); return now }
	h.runnr.ws.mu.Unlock()
	id := workspace.NewID()
	h.want(transport.WorkspaceRequest{Op: transport.OpDestroy, ID: string(id)}, nil, transport.CodeOK)
	h.want(createReq(id), nil, transport.CodeNotFound)
	if n := h.slots(); n != 0 {
		t.Fatalf("slots = %d after refused create, want 0", n)
	}
	// Other ids are unaffected.
	h.create()
	cm.Lock()
	now = now.Add(tombTTL + time.Second)
	cm.Unlock()
	h.want(createReq(id), nil, transport.CodeOK)
}

func TestTombstoneBoundedAndExpiring(t *testing.T) {
	var tb tombstones
	t0 := time.Unix(5000, 0)
	ids := make([]workspace.ID, tombMax+50)
	for i := range ids {
		ids[i] = workspace.NewID()
		tb.add(ids[i], t0.Add(time.Duration(i)*time.Second))
	}
	now := t0.Add(time.Duration(len(ids)) * time.Second)
	if len(tb.order) != tombMax || len(tb.at) != tombMax {
		t.Fatalf("size = %d/%d, want %d", len(tb.order), len(tb.at), tombMax)
	}
	if tb.has(ids[0], now) || tb.has(ids[49], now) {
		t.Error("oldest ids were not evicted")
	}
	if !tb.has(ids[50], now) || !tb.has(ids[len(ids)-1], now) {
		t.Error("recent ids missing")
	}
	// Re-adding an id refreshes it without growing the set.
	tb.add(ids[50], now)
	if len(tb.order) != tombMax {
		t.Fatalf("size after refresh = %d", len(tb.order))
	}
	// Expiry: id 51 is old enough, the refreshed id 50 is not.
	later := t0.Add(51*time.Second + tombTTL)
	if tb.has(ids[51], later) {
		t.Error("expired id still present")
	}
	if !tb.has(ids[50], later) {
		t.Error("refreshed id expired early")
	}
	if tb.has(ids[50], now.Add(tombTTL)) {
		t.Error("refreshed id never expires")
	}
	if len(tb.order) != len(tb.at) {
		t.Fatalf("order/map out of sync: %d vs %d", len(tb.order), len(tb.at))
	}
}

// E-01: max_age_ms is compared in milliseconds before converting to a Duration.
func TestPrepareMaxAgeNoOverflow(t *testing.T) {
	s := newWSState(fake.New(), parseWS(t, wsYAML)) // cap 24h
	prep := func(ms int64) (*wsOp, *opErr) {
		return s.prepare(&transport.WorkspaceRequest{Op: transport.OpCreate, ID: string(workspace.NewID()),
			Spec: &transport.WorkspaceSpec{Image: "alpine:3.20", MaxAgeMs: ms}})
	}
	day := (24 * time.Hour).Milliseconds()
	for name, ms := range map[string]int64{
		"MaxInt64":        math.MaxInt64,
		"wraps negative":  math.MaxInt64/int64(time.Millisecond) + 1,
		"just over bound": transport.MaxWorkspaceAgeMs + 1,
		"cap plus 1ms":    day + 1,
		"negative":        -1,
		"min int64":       math.MinInt64,
	} {
		if _, e := prep(ms); e == nil || e.code != transport.CodeRefused {
			t.Errorf("%s: %v, want refused", name, e)
		}
	}
	for ms, want := range map[int64]time.Duration{0: 24 * time.Hour, 1: time.Millisecond, day: 24 * time.Hour} {
		op, e := prep(ms)
		if e != nil || op.spec.MaxAge != want {
			t.Errorf("max_age_ms=%d: %v %v, want %v", ms, op, e, want)
		}
	}
	// No runner cap: the transport bound still applies and the result never wraps.
	s2 := newWSState(fake.New(), parseWS(t, strings.Replace(wsYAML, "max_age: 24h\n", "", 1)))
	if s2.cfg.MaxAge != 0 {
		t.Fatalf("cap = %v, want none", s2.cfg.MaxAge)
	}
	op, e := s2.prepare(&transport.WorkspaceRequest{Op: transport.OpCreate, ID: string(workspace.NewID()),
		Spec: &transport.WorkspaceSpec{Image: "alpine:3.20", MaxAgeMs: transport.MaxWorkspaceAgeMs}})
	if e != nil || op.spec.MaxAge != 365*24*time.Hour {
		t.Errorf("at bound: %v %v", op, e)
	}
	if _, e := s2.prepare(&transport.WorkspaceRequest{Op: transport.OpCreate, ID: string(workspace.NewID()),
		Spec: &transport.WorkspaceSpec{Image: "alpine:3.20", MaxAgeMs: math.MaxInt64}}); e == nil {
		t.Error("MaxInt64 accepted without a cap")
	}
}

// failEx has no DiskUsager. Create waits for cRelease then fails with createErr; Destroy waits
// for dRelease (when set) and then answers destroyErr.
type failEx struct {
	*fake.Executor
	entered    chan struct{}
	cRelease   chan struct{}
	dEntered   chan struct{}
	dRelease   chan struct{}
	createErr  error
	destroyErr error
	mu         sync.Mutex
	destroys   int
}

func newFailEx() *failEx {
	return &failEx{Executor: fake.New(), entered: make(chan struct{}, 8), cRelease: make(chan struct{}),
		dEntered: make(chan struct{}, 8)}
}

func (f *failEx) Create(ctx context.Context, s workspace.Spec) error {
	f.entered <- struct{}{}
	<-f.cRelease
	if f.createErr != nil {
		return f.createErr
	}
	return f.Executor.Create(ctx, s)
}

func (f *failEx) Destroy(ctx context.Context, id workspace.ID) error {
	f.mu.Lock()
	f.destroys++
	g := f.dRelease
	f.mu.Unlock()
	f.dEntered <- struct{}{}
	if g != nil {
		<-g
	}
	if f.destroyErr != nil {
		return f.destroyErr
	}
	return f.Executor.Destroy(ctx, id)
}

// A create fails while a destroy is deferred behind it, and the deferred destroy fails too (the
// executor is unavailable): no container exists, yet the entry is confirmed and holds a slot.
// The next OpList (reconcile) must prune it, on an executor without DiskUsager.
func TestWorkspaceListPrunesPhantomEntry(t *testing.T) {
	f := newFailEx()
	f.createErr, f.destroyErr = workspace.ErrUnavailable, workspace.ErrUnavailable
	h := newHarness(t, wsYAML, f.Executor, func(cfg *Config) { cfg.Workspace = f })
	var once sync.Once
	open := func() { once.Do(func() { close(f.cRelease) }) }
	t.Cleanup(open)
	if _, ok := workspace.Executor(f).(workspace.DiskUsager); ok {
		t.Fatal("failEx must not implement DiskUsager")
	}
	id := workspace.NewID()
	created := make(chan struct{})
	go func() { h.try(createReq(id), nil); close(created) }()
	recv(t, f.entered, "create entered")
	h.want(transport.WorkspaceRequest{Op: transport.OpDestroy, ID: string(id)}, nil, transport.CodeOK)
	open()
	<-created
	waitFor(t, "phantom confirmed entry", func() bool {
		d, cf, ok := h.entry(id)
		return ok && !d && cf
	})
	// A live container listed by the executor is never pruned.
	f.createErr, f.destroyErr = nil, nil
	keep := h.create()
	h.want(transport.WorkspaceRequest{Op: transport.OpList}, nil, transport.CodeOK)
	if _, _, ok := h.entry(id); ok {
		t.Fatal("phantom entry survived OpList")
	}
	if _, cf, ok := h.entry(keep); !ok || !cf {
		t.Fatalf("listed workspace pruned: ok=%v confirmed=%v", ok, cf)
	}
	if n := h.slots(); n != 1 {
		t.Fatalf("slots = %d, want 1", n)
	}
}

// prune keeps entries confirmed after the List began, in-flight creates and doomed entries.
func TestWorkspacePruneSkipsNewerAndBusyEntries(t *testing.T) {
	s := newWSState(fake.New(), &wsconfig.Config{Concurrent: 8})
	mk := func() workspace.ID { return workspace.NewID() }
	old, newer, inflight, doomed := mk(), mk(), mk(), mk()
	s.mu.Lock()
	for _, id := range []workspace.ID{old, newer, inflight, doomed} {
		s.live[id] = &wsEntry{}
	}
	s.confirm(s.live[old])
	start := s.gen
	s.confirm(s.live[newer]) // finished while the List ran
	s.live[inflight].refs = 1
	s.live[doomed].doomed = true
	s.mu.Unlock()
	s.prune(start, map[workspace.ID]bool{})
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.live[old] != nil {
		t.Error("unlisted old confirmed entry kept")
	}
	for name, id := range map[string]workspace.ID{"newer": newer, "inflight": inflight, "doomed": doomed} {
		if s.live[id] == nil {
			t.Errorf("%s entry pruned", name)
		}
	}
}

// A destroy of an absent id drops the lock around the executor call. A create admitted in that
// window must keep its entry (now doomed, so createDone destroys it), not have it forgotten.
func TestWorkspaceDestroyAbsentRacesNewCreate(t *testing.T) {
	f := newFailEx()
	f.destroyErr = workspace.ErrNotFound
	dGate := make(chan struct{})
	f.dRelease = dGate
	h := newHarness(t, wsYAML, f.Executor, func(cfg *Config) { cfg.Workspace = f })
	var co, do sync.Once
	t.Cleanup(func() { co.Do(func() { close(f.cRelease) }); do.Do(func() { close(dGate) }) })
	id := workspace.NewID()
	destroyed := make(chan struct{})
	go func() {
		h.try(transport.WorkspaceRequest{Op: transport.OpDestroy, ID: string(id)}, nil)
		close(destroyed)
	}()
	recv(t, f.dEntered, "destroy entered")
	created := make(chan struct{})
	go func() { h.try(createReq(id), nil); close(created) }()
	recv(t, f.entered, "create entered")
	do.Do(func() { close(dGate) })
	<-destroyed
	d, cf, ok := h.entry(id)
	if !ok || !d || cf {
		t.Fatalf("new create's entry: ok=%v doomed=%v confirmed=%v, want present and doomed", ok, d, cf)
	}
	co.Do(func() { close(f.cRelease) })
	<-created
	recv(t, f.dEntered, "deferred destroy")
	waitFor(t, "entry released after deferred destroy", func() bool { _, _, ok := h.entry(id); return !ok })
}

// panicEx panics in Destroy while armed.
type panicEx struct {
	*fake.Executor
	armed atomic.Bool
}

func (p *panicEx) Destroy(ctx context.Context, id workspace.ID) error {
	if p.armed.Load() {
		panic("destroy boom")
	}
	return p.Executor.Destroy(ctx, id)
}

// A panic in the executor's Destroy fails the destroy, keeps the slot, leaves no stuck
// destroying mark, and a later destroy works.
func TestWorkspaceDestroyPanicSettles(t *testing.T) {
	p := &panicEx{Executor: fake.New()}
	h := newHarness(t, wsYAML, p.Executor, func(cfg *Config) { cfg.Workspace = p })
	id := h.create()
	p.armed.Store(true)
	h.want(transport.WorkspaceRequest{Op: transport.OpDestroy, ID: string(id)}, nil, transport.CodeInternal)
	h.runnr.ws.mu.Lock()
	e := h.runnr.ws.live[id]
	stuck := e == nil || e.destroying
	h.runnr.ws.mu.Unlock()
	if stuck {
		t.Fatalf("entry missing or still destroying after panic (entry=%v)", e)
	}
	if n := h.slots(); n != 1 {
		t.Fatalf("slots = %d after panicking destroy, want 1", n)
	}
	p.armed.Store(false)
	h.want(transport.WorkspaceRequest{Op: transport.OpDestroy, ID: string(id)}, nil, transport.CodeOK)
	if n := h.slots(); n != 0 {
		t.Fatalf("slots = %d after later destroy, want 0", n)
	}
}

// A second normal-path destroy of a confirmed entry waits for the first and answers its
// result: one executor call, the slot held until the executor returns and freed exactly once,
// new ops on the entry refused meanwhile.
func TestWorkspaceDestroyingConcurrentDestroy(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "ok"
		if fail {
			name = "failure"
		}
		t.Run(name, func(t *testing.T) {
			c := newCtlEx()
			h := newHarness(t, wsYAML, c.Executor, func(cfg *Config) { cfg.Workspace = c })
			c.openGate()()
			id := h.create() // concurrent: 2
			dGate := make(chan struct{})
			var once sync.Once
			openD := func() { once.Do(func() { close(dGate) }) }
			t.Cleanup(openD)
			var wantErr error
			if fail {
				wantErr = errors.New("boom")
			}
			c.set(wantErr, dGate)

			code := make(chan string, 2)
			destroy := func() {
				go func() {
					res, _, err := h.try(transport.WorkspaceRequest{Op: transport.OpDestroy, ID: string(id)}, nil)
					if err != nil {
						code <- err.Error()
						return
					}
					code <- res.Code
				}()
			}
			destroy()
			recv(t, c.dEntered, "first destroy in the executor")
			waitFor(t, "destroying mark", func() bool {
				h.runnr.ws.mu.Lock()
				defer h.runnr.ws.mu.Unlock()
				e := h.runnr.ws.live[id]
				return e != nil && e.destroying
			})
			destroy() // concurrent destroy of the same entry
			select {
			case c := <-code:
				t.Fatalf("second destroy answered %q before the first finished", c)
			case <-time.After(150 * time.Millisecond):
			}
			if n := h.slots(); n != 1 {
				t.Fatalf("slots = %d while destroying, want 1", n)
			}
			// New ops on the entry are refused.
			h.want(transport.WorkspaceRequest{Op: transport.OpExec, ID: string(id), Argv: []string{"x"}}, nil, transport.CodeNotFound)
			h.want(transport.WorkspaceRequest{Op: transport.OpRead, ID: string(id), Path: "x"}, nil, transport.CodeNotFound)
			if _, _, err := h.try(createReq(id), nil); !busy(err) {
				t.Fatalf("create of the destroying id: %v, want busy", err)
			}
			if n := c.destroys(); n != 1 {
				t.Fatalf("executor destroys = %d, want 1", n)
			}
			openD()
			want := transport.CodeOK
			if fail {
				want = transport.CodeInternal
			}
			for i := 0; i < 2; i++ {
				select {
				case got := <-code:
					if got != want {
						t.Fatalf("destroy answered %q, want %q", got, want)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("destroy did not answer")
				}
			}
			if n := c.destroys(); n != 1 {
				t.Fatalf("executor destroys = %d after both answered, want 1", n)
			}
			wantSlots := 0
			if fail {
				wantSlots = 1 // container may still exist: slot kept, mark released
			}
			if n := h.slots(); n != wantSlots {
				t.Fatalf("slots = %d, want %d", n, wantSlots)
			}
			if fail {
				if d, _, ok := h.entry(id); !ok || d {
					t.Fatalf("entry ok=%v doomed=%v after failed destroy", ok, d)
				}
				c.set(nil, nil)
				h.want(transport.WorkspaceRequest{Op: transport.OpDestroy, ID: string(id)}, nil, transport.CodeOK)
				if n := h.slots(); n != 0 {
					t.Fatalf("slots = %d after retry, want 0", n)
				}
			}
		})
	}
}
