//go:build unix

package central

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/actions"
	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/apiclient"
	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/central/registry"
	"github.com/mirastacklabs-ai/mirafleet/internal/keys"
	"github.com/mirastacklabs-ai/mirafleet/internal/mcpserver"
	"github.com/mirastacklabs-ai/mirafleet/internal/opconf"
	"github.com/mirastacklabs-ai/mirafleet/internal/orch"
	"github.com/mirastacklabs-ai/mirafleet/internal/policy"
	"github.com/mirastacklabs-ai/mirafleet/internal/runner"
	"github.com/mirastacklabs-ai/mirafleet/internal/secrets"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/store/sqlite"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/fake"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/wsconfig"
)

// Helpers of TestM7Done (m7_done_test.go) and the host-mode glue tests.

// m7Ring adapts a secrets.Key to service.KeyRing (the same adapter cmd/mirafleet uses).
type m7Ring struct{ k secrets.Key }

func (r m7Ring) Seal(pt, aad []byte) ([]byte, error) { return secrets.Seal(r.k, pt, aad) }
func (r m7Ring) Open(ct, aad []byte) ([]byte, error) { return secrets.Open(r.k, ct, aad) }
func (r m7Ring) Check() []byte                       { return r.k.Check() }
func (r m7Ring) VerifyCheck(stored []byte) bool      { return r.k.VerifyCheck(stored) }

// newM7Env is newEnvOpts with a service that holds credentials under key, delivers them through
// the registry, and runs on the injected clock. tune may adjust the credentials config.
func newM7Env(t *testing.T, clock func() time.Time, key secrets.Key, tune func(*service.CredentialsConfig)) *env {
	t.Helper()
	var reg *registry.Registry
	var svc *service.Service
	e := newEnvOpts(t, 15*time.Second, func(c *Config) {
		r, err := registry.New(registry.Options{StaleAfter: 45 * time.Second, Logf: c.Logf})
		must(t, err)
		cc := service.CredentialsConfig{Key: m7Ring{key}, Sender: r}
		if tune != nil {
			tune(&cc)
		}
		s, err := service.New(service.Config{Store: c.Store, Policy: policy.TablePolicy{}, Live: r,
			HostKey: c.HostKey.PublicKey(), Now: clock, OperatorConns: c.Operators, Logf: c.Logf,
			ReaperInterval: time.Hour, Credentials: cc})
		must(t, err)
		c.Registry, c.Service = r, s
		reg, svc = r, s
	})
	e.reg, e.svc = reg, svc
	return e
}

// m7AdminBody is one admin-socket call with a body frame behind the request (credential.set),
// the same wire as cmd/mirafleet's adminCallBody.
func m7AdminBody(sock, method string, params any, body []byte, result any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	id := fmt.Sprintf("%x", time.Now().UnixNano())
	payload, err := json.Marshal(api.Request{V: api.Version, ID: id, Method: method, Params: raw})
	if err != nil {
		return err
	}
	c, err := net.DialTimeout("unix", sock, 5*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	if err := api.WriteFrame(c, payload); err != nil {
		return err
	}
	werr := api.WriteBody(c, body)
	b, err := api.ReadFrame(c, api.MaxResponseBytes)
	if err != nil {
		if werr != nil {
			err = werr
		}
		return err
	}
	var resp api.Response
	if err := json.Unmarshal(b, &resp); err != nil {
		return err
	}
	if resp.Error != nil {
		return resp.Error
	}
	if resp.ID != id {
		return errors.New("admin: response id mismatch")
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(resp.Result, result)
}

// m7Ctx is a context that ends after 60 s (or with the test).
func m7Ctx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// ---- the host executor stand-in ----

// m7Host is the "host" executor of TestM7Done: a wrapper around the fake executor (host-mode
// behaviour on) that adds what the real host executor does and the fake does not: a foreground
// bash with a persisted cwd and saved output (runner.BashExecutor), and session directories keyed
// by the session or workdir label, so a reopen reattaches the same directory (hostx does this
// with <root>/sessions/<slug> and <root>/work/<name>). Each outer workspace owns one inner fake
// whose id is derived from the label; two workspaces with the same label share one directory.
// Its commands are a tiny language: "m7:cd D", "m7:pwd", "m7:big" (5 MiB), "m7:hang" (runs into
// its timeout), "m7:build" (a background job that waits for gate), "m7:forever"; anything else
// prints the command line.
type m7Host struct {
	root string

	mu       sync.Mutex
	inner    map[workspace.ID]*m7Inner
	cwd      map[workspace.ID]string
	timeouts []time.Duration
	gate     chan struct{} // closed to let m7:build finish
	big      []byte
}

type m7Inner struct {
	ex *fake.Executor
	id workspace.ID
}

const m7BigSize = 5 << 20

func newM7Host(t *testing.T) *m7Host {
	t.Helper()
	// A short path: unix socket paths (the per-session agent) are limited to about 100 bytes.
	root, err := os.MkdirTemp("", "m7h")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(root) })
	h := &m7Host{root: root, inner: map[workspace.ID]*m7Inner{}, cwd: map[workspace.ID]string{}, gate: make(chan struct{})}
	h.big = make([]byte, 0, m7BigSize)
	for i := 0; len(h.big) < m7BigSize; i++ {
		h.big = append(h.big, fmt.Sprintf("big line %07d %s\n", i, strings.Repeat("x", 40))...)
	}
	h.big = h.big[:m7BigSize]
	return h
}

func m7LabelID(label string) workspace.ID {
	s := sha256.Sum256([]byte(label))
	return workspace.ID(hex.EncodeToString(s[:16]))
}

func (h *m7Host) Name() string   { return wsconfig.ExecHost }
func (h *m7Host) HostMode() bool { return true }
func (h *m7Host) Close() error   { return nil }

func (h *m7Host) get(id workspace.ID) (*m7Inner, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	in := h.inner[id]
	if in == nil {
		return nil, workspace.ErrNotFound
	}
	return in, nil
}

func (h *m7Host) Create(ctx context.Context, s workspace.Spec) error {
	h.mu.Lock()
	if _, ok := h.inner[s.ID]; ok {
		h.mu.Unlock()
		return nil
	}
	label := "session:" + s.Labels["mirafleet.session"]
	if wd := s.Labels["mirafleet.workdir"]; wd != "" {
		label = "workdir:" + wd
	} else if s.Labels["mirafleet.session"] == "" {
		label = "id:" + string(s.ID)
	}
	ex := fake.New()
	ex.SessionRoot = h.root
	ex.ExecFunc = h.execFunc
	in := &m7Inner{ex: ex, id: m7LabelID(label)}
	h.inner[s.ID] = in
	h.mu.Unlock()
	sp := s
	sp.ID = in.id
	if err := ex.Create(ctx, sp); err != nil {
		h.mu.Lock()
		delete(h.inner, s.ID)
		h.mu.Unlock()
		return err
	}
	return nil
}

func (h *m7Host) Exec(ctx context.Context, id workspace.ID, r workspace.ExecRequest) (workspace.ExecResult, error) {
	in, err := h.get(id)
	if err != nil {
		return workspace.ExecResult{}, err
	}
	return in.ex.Exec(ctx, in.id, r)
}

func (h *m7Host) ReadFile(ctx context.Context, id workspace.ID, p string, off, n int64) (io.ReadCloser, workspace.FileInfo, error) {
	in, err := h.get(id)
	if err != nil {
		return nil, workspace.FileInfo{}, err
	}
	return in.ex.ReadFile(ctx, in.id, p, off, n)
}

func (h *m7Host) WriteFile(ctx context.Context, id workspace.ID, p string, r io.Reader, off int64, truncate bool, mode fs.FileMode) (workspace.FileInfo, error) {
	in, err := h.get(id)
	if err != nil {
		return workspace.FileInfo{}, err
	}
	return in.ex.WriteFile(ctx, in.id, p, r, off, truncate, mode)
}

func (h *m7Host) Destroy(ctx context.Context, id workspace.ID) error {
	in, err := h.get(id)
	if err != nil {
		return nil
	}
	err = in.ex.Destroy(ctx, in.id)
	h.mu.Lock()
	delete(h.inner, id)
	delete(h.cwd, id)
	h.mu.Unlock()
	return err
}

func (h *m7Host) DestroyDelete(ctx context.Context, id workspace.ID) error {
	in, err := h.get(id)
	if err != nil {
		return nil
	}
	err = in.ex.DestroyDelete(ctx, in.id)
	h.mu.Lock()
	delete(h.inner, id)
	delete(h.cwd, id)
	h.mu.Unlock()
	return err
}

func (h *m7Host) List(ctx context.Context) ([]workspace.Info, error) {
	h.mu.Lock()
	snap := make(map[workspace.ID]*m7Inner, len(h.inner))
	for k, v := range h.inner {
		snap[k] = v
	}
	h.mu.Unlock()
	var out []workspace.Info
	for outer, in := range snap {
		infos, err := in.ex.List(ctx)
		if err != nil {
			return nil, err
		}
		for _, i := range infos {
			i.ID = outer
			out = append(out, i)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (h *m7Host) SessionDir(id workspace.ID) (string, error) {
	in, err := h.get(id)
	if err != nil {
		return "", err
	}
	return in.ex.SessionDir(in.id)
}

func (h *m7Host) SetSessionEnv(id workspace.ID, env map[string]string) {
	if in, err := h.get(id); err == nil {
		in.ex.SetSessionEnv(in.id, env)
	}
}

func (h *m7Host) SessionEnv(id workspace.ID) map[string]string {
	if in, err := h.get(id); err == nil {
		return in.ex.SessionEnv(in.id)
	}
	return nil
}

// WorkDir is the directory the file tools see for the outer workspace.
func (h *m7Host) WorkDir(id workspace.ID) string {
	d, err := h.SessionDir(id)
	if err != nil {
		return ""
	}
	return filepath.Join(d, "work")
}

func (h *m7Host) FS(ctx context.Context, id workspace.ID, op transport.FSOp, body []byte) (transport.FSResult, []byte, error) {
	in, err := h.get(id)
	if err != nil {
		return transport.FSResult{}, nil, err
	}
	return in.ex.FS(ctx, in.id, op, body)
}

func (h *m7Host) JobStart(ctx context.Context, id workspace.ID, op transport.JobOp) (transport.JobResult, error) {
	in, err := h.get(id)
	if err != nil {
		return transport.JobResult{}, err
	}
	return in.ex.JobStart(ctx, in.id, op)
}

func (h *m7Host) JobStatus(ctx context.Context, id workspace.ID, job string) ([]transport.JobResult, error) {
	in, err := h.get(id)
	if err != nil {
		return nil, err
	}
	return in.ex.JobStatus(ctx, in.id, job)
}

func (h *m7Host) JobTail(ctx context.Context, id workspace.ID, job, filter string) (transport.JobResult, error) {
	in, err := h.get(id)
	if err != nil {
		return transport.JobResult{}, err
	}
	return in.ex.JobTail(ctx, in.id, job, filter)
}

func (h *m7Host) JobCancel(ctx context.Context, id workspace.ID, job string) (transport.JobResult, error) {
	in, err := h.get(id)
	if err != nil {
		return transport.JobResult{}, err
	}
	return in.ex.JobCancel(ctx, in.id, job)
}

func (h *m7Host) OutputRead(ctx context.Context, id workspace.ID, op transport.OutputOp) (transport.OutputResult, error) {
	in, err := h.get(id)
	if err != nil {
		return transport.OutputResult{}, err
	}
	return in.ex.OutputRead(ctx, in.id, op)
}

func (h *m7Host) Busy() int {
	h.mu.Lock()
	seen := map[*fake.Executor]bool{}
	for _, in := range h.inner {
		seen[in.ex] = true
	}
	h.mu.Unlock()
	n := 0
	for ex := range seen {
		n += ex.Busy()
	}
	return n
}

// execFunc runs the jobs (the fake runs them through ExecFunc as bash -lc COMMAND).
func (h *m7Host) execFunc(ctx context.Context, id workspace.ID, r workspace.ExecRequest) (workspace.ExecResult, error) {
	cmd := ""
	if len(r.Argv) == 3 {
		cmd = r.Argv[2]
	}
	switch cmd {
	case "m7:build":
		io.WriteString(r.Stdout, "step1\n")
		select {
		case <-h.gate:
			io.WriteString(r.Stdout, "step2\n")
		case <-ctx.Done():
			return workspace.ExecResult{ExitCode: -1}, ctx.Err()
		}
	case "m7:forever":
		io.WriteString(r.Stdout, "started\n")
		<-ctx.Done()
		return workspace.ExecResult{ExitCode: -1}, ctx.Err()
	default:
		io.WriteString(r.Stdout, cmd+"\n")
	}
	return workspace.ExecResult{}, nil
}

var errM7Hang = errors.New("m7: hang")

// Bash implements runner.BashExecutor.
func (h *m7Host) Bash(ctx context.Context, id workspace.ID, command string, timeout time.Duration, requestID string) (transport.WorkspaceExec, error) {
	in, err := h.get(id)
	if err != nil {
		return transport.WorkspaceExec{}, err
	}
	h.mu.Lock()
	h.timeouts = append(h.timeouts, timeout)
	cwd := h.cwd[id]
	if cwd == "" {
		cwd = "/work"
	}
	h.mu.Unlock()
	var out []byte
	we := transport.WorkspaceExec{}
	switch {
	case strings.HasPrefix(command, "m7:cd "):
		cwd = path.Clean(path.Join(cwd, strings.TrimPrefix(command, "m7:cd ")))
	case command == "m7:pwd":
		out = []byte(cwd + "\n")
	case command == "m7:big":
		out = h.big
	case command == "m7:hang":
		// A real executor would wait for the deadline and kill the process group; the wrapper
		// reports that outcome at once so the test does not wait on a wall clock.
		we.TimedOut, we.ExitCode = true, -1
	default:
		out = []byte(command + "\n")
	}
	h.mu.Lock()
	h.cwd[id] = cwd
	h.mu.Unlock()
	we.Cwd = cwd
	we.StdoutBytes = int64(len(out))
	const inline = 64 << 10
	if len(out) > inline {
		in.ex.SetOutput(in.id, requestID, out)
		we.Ref, we.Truncated, we.Omitted = requestID, true, int64(len(out)-inline)
		out = out[:inline]
	}
	we.Stdout = string(out)
	return we, nil
}

func (h *m7Host) recordedTimeouts() []time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]time.Duration(nil), h.timeouts...)
}

// ---- the central ----

// m7Sender records every creds_sync body central sends and forwards it to the registry.
type m7Sender struct {
	inner service.CredentialSender

	mu    sync.Mutex
	last  map[int64]map[string]string // runner id -> credential name -> value of the latest sync
	count map[int64]int
}

func newM7Sender() *m7Sender {
	return &m7Sender{last: map[int64]map[string]string{}, count: map[int64]int{}}
}

func (s *m7Sender) SyncCredentials(ctx context.Context, id int64, body *transport.CredsSyncBody) error {
	err := s.inner.SyncCredentials(ctx, id, body)
	if err == nil {
		set := map[string]string{}
		for _, c := range body.Credentials {
			set[c.Name] = c.Value
		}
		s.mu.Lock()
		s.last[id] = set
		s.count[id]++
		s.mu.Unlock()
	}
	return err
}

// held returns the sorted credential names of the latest sync to the runner (nil: none yet).
func (s *m7Sender) held(id int64) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	set, ok := s.last[id]
	if !ok {
		return nil
	}
	out := []string{}
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (s *m7Sender) value(id int64, name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last[id][name]
}

// newM7EnvDB is newM4Env with credentials: a central on a SQLite file whose path is known, a
// service on the injected clock that holds credentials under key, and a recording sender.
func newM7EnvDB(t *testing.T, clock func() time.Time, key secrets.Key, tune func(*service.CredentialsConfig)) (*env, string, *m7Sender) {
	t.Helper()
	ctx := context.Background()
	e := &env{t: t, dir: t.TempDir(), ids: map[string]int64{}, admin: auth.LocalAdmin(os.Getuid())}
	_, err := keys.Generate(e.dir, "host", "host")
	must(t, err)
	e.hostSigner, err = keys.LoadSigner(filepath.Join(e.dir, "host"))
	must(t, err)
	e.knownHosts = filepath.Join(e.dir, "known_hosts")
	must(t, os.WriteFile(e.knownHosts, []byte("* "+keys.PubLine(e.hostSigner.PublicKey(), "")+"\n"), 0o600))
	e.acts, err = actions.Parse([]byte(m4Actions))
	must(t, err)
	dbPath := filepath.Join(t.TempDir(), "mirafleet.db")
	e.db, err = sqlite.Open(ctx, dbPath)
	must(t, err)
	t.Cleanup(func() { e.db.Close() })
	_, err = e.db.CreateFleet(ctx, store.Fleet{Name: testFleet, Mode: store.ModeActions}, time.Now(), auditEv("fleet.create"))
	must(t, err)
	e.reg, err = registry.New(registry.Options{StaleAfter: 45 * time.Second, Logf: e.logf})
	must(t, err)
	e.ops = NewOperatorConns()
	snd := newM7Sender()
	snd.inner = e.reg
	cc := service.CredentialsConfig{Key: m7Ring{key}, Sender: snd}
	if tune != nil {
		tune(&cc)
	}
	e.svc, err = service.New(service.Config{Store: e.db, Policy: policy.TablePolicy{}, Live: e.reg,
		HostKey: e.hostSigner.PublicKey(), Now: clock, OperatorConns: e.ops, Logf: e.logf,
		ReaperInterval: time.Hour, Credentials: cc})
	must(t, err)
	e.startCentral("127.0.0.1:0")
	return e, dbPath, snd
}

// m7StartRunner runs a runner over h until the returned stop function is called (or the test ends).
func m7StartRunner(t *testing.T, e *env, name string, h *m7Host, wc *wsconfig.Config) (stop func()) {
	t.Helper()
	cfg := runner.Config{
		Central: e.addr, Name: name, Signer: e.signer(name), KnownHosts: e.knownHosts,
		Workspace: h, WorkspaceConfig: wc, Labels: map[string]string{"role": name}, Version: "test",
		Logf:           e.logf,
		HeartbeatEvery: 50 * time.Millisecond, HeartbeatTimeout: 10 * time.Second,
		BackoffMin: 20 * time.Millisecond, BackoffMax: 100 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { runner.New(cfg).Run(ctx); close(done) }()
	var once sync.Once
	stop = func() { once.Do(func() { cancel(); <-done }) }
	t.Cleanup(stop)
	return stop
}

// m7Operator registers operator name (key generated now) and returns its orch config directory.
func m7Operator(t *testing.T, e *env, name string) string {
	t.Helper()
	dir := t.TempDir()
	pub, err := keys.Generate(dir, opconf.KeyName, name)
	must(t, err)
	pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pub))
	must(t, err)
	_, _, err = e.db.RegisterOperator(context.Background(), name, pk.Marshal(), ssh.FingerprintSHA256(pk), time.Now(), auditEv("operator.register"))
	must(t, err)
	must(t, os.WriteFile(opconf.KnownHostsPath(dir), []byte(apiclient.KnownHostsLine(e.addr, e.hostSigner.PublicKey())+"\n"), 0o600))
	must(t, opconf.SaveContexts(dir, &opconf.Contexts{Current: "work", Contexts: []opconf.Context{{Name: "work", Addr: e.addr}}}))
	return dir
}

// m7OperatorClient dials api@ as the operator whose config directory is dir.
func m7OperatorClient(t *testing.T, e *env, dir string) *apiclient.Client {
	t.Helper()
	signer, err := keys.LoadSigner(opconf.KeyPath(dir))
	must(t, err)
	cl, err := apiclient.Dial(m7Ctx(t), e.addr, transport.UserOperator, signer, ssh.FixedHostKey(e.hostSigner.PublicKey()))
	must(t, err)
	t.Cleanup(func() { cl.Close() })
	return cl
}

// m7Image is an MCP image content block of a tool result.
type m7Image struct {
	MIME string
	Data []byte
}

// m7CallImages calls a tool and returns the decoded text result and its image blocks.
func m7CallImages(t *testing.T, s *m5Server, name string, args map[string]any) (m5Res, []m7Image) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := s.cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	must(t, err)
	var imgs []m7Image
	var text *mcp.TextContent
	for _, c := range res.Content {
		switch x := c.(type) {
		case *mcp.TextContent:
			text = x
		case *mcp.ImageContent:
			imgs = append(imgs, m7Image{MIME: x.MIMEType, Data: x.Data})
		}
	}
	if text == nil {
		t.Fatalf("%s: no text content", name)
	}
	s.sink.add(text.Text)
	out := m5Res{IsErr: res.IsError, Text: text.Text}
	must(t, json.Unmarshal([]byte(text.Text), &out.M))
	return out, imgs
}

// m7Backend is the orchestrator as `mirafleet mcp` serves it: with the project directory that
// upload and download are confined to (cmd/mirafleet projectOrch).
type m7Backend struct {
	*orch.Orch
	dir string
}

func (b m7Backend) ProjectDir() string { return b.dir }

// m7Start is m5Start with that backend.
func m7Start(t *testing.T, cfg orch.Config, rec *m5Rec, sink *m5Sink) *m5Server {
	t.Helper()
	cfg.Dial = m5Dial(rec)
	cfg.KeepaliveInterval = time.Hour
	o, err := orch.New(cfg)
	must(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	s := &m5Server{t: t, o: o, cancel: cancel, done: make(chan struct{}), sink: sink, serr: new(error)}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	s.inW = inW
	b := m7Backend{Orch: o, dir: cfg.ProjectDir}
	tools := append(mcpserver.ListTools(b), mcpserver.WorkspaceTools(b)...)
	go func() {
		*s.serr = mcpserver.Serve(ctx, inR, outW, mcpserver.Config{Name: "mirafleet", Version: "test", Tools: tools})
		outW.Close()
		close(s.done)
	}()
	client := mcp.NewClient(&mcp.Implementation{Name: "m7-test", Version: "0"}, nil)
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
