package osb

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/wsconfig"
)

const testRunner = "runner-a"

// efake is a stateful OpenSandbox over mTLS for executor tests. It ignores the metadata list
// filter on purpose: the executor must not trust it.
type efake struct {
	srv *httptest.Server
	pki *pki

	mu        sync.Mutex
	n         int
	sbs       map[string]*Sandbox
	files     map[string]map[string][]byte // sandbox id -> path -> data
	creates   []CreateRequest
	deletes   []string
	cmds      []CommandRequest
	cancelled []string
	getCount  int

	createState string                                       // state a new sandbox settles in; default Running
	script      func(w http.ResponseWriter, r *http.Request) // /command handler; default prints hi
	leak        bool                                         // answer errors with both secrets in the body
}

func newEFake(t *testing.T) (*efake, *pki) {
	t.Helper()
	p := newPKI(t)
	f := &efake{pki: p, sbs: map[string]*Sandbox{}, files: map[string]map[string][]byte{}}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(f.handle))
	pool := x509.NewCertPool()
	pool.AddCert(p.caCert)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{p.serverCert}, ClientCAs: pool,
		ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS13}
	srv.Config.ErrorLog = nil
	srv.StartTLS()
	t.Cleanup(srv.Close)
	f.srv = srv
	return f, p
}

func (f *efake) executor(t *testing.T, p *pki, secure string) workspace.Executor {
	t.Helper()
	kf := filepath.Join(p.dir, "api.key")
	if err := os.WriteFile(kf, []byte(testAPIKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &wsconfig.Config{
		Executor: wsconfig.ExecOpenSandbox,
		Limits:   wsconfig.Limits{MaxFile: 1 << 10},
		OpenSandbox: &wsconfig.OpenSandbox{
			Server: "https://" + f.srv.Listener.Addr().String(), CAFile: p.caFile,
			ClientCert: p.clientCertFile, ClientKey: p.clientKey, APIKeyFile: kf, SecureRuntime: secure,
		},
	}
	ex, err := NewExecutor(cfg, testRunner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ex.Close() })
	return ex
}

func (f *efake) add(id string, meta map[string]string, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sbs[id] = &Sandbox{ID: id, Metadata: meta, Status: Status{State: state}, CreatedAt: "2026-11-13T10:00:00Z"}
}

func (f *efake) handle(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(apiKeyHeader) != testAPIKey {
		http.Error(w, "bad key", 401)
		return
	}
	p := r.URL.Path
	if strings.HasPrefix(p, "/sandboxes/") {
		parts := strings.SplitN(strings.TrimPrefix(p, "/sandboxes/"), "/proxy/44772", 2)
		if len(parts) == 2 && r.Header.Get(execdTokHeader) == testExecdTok {
			f.execd(w, r, parts[0], parts[1])
			return
		}
		http.Error(w, "bad token", 401)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case p == "/v1/sandboxes" && r.Method == "POST":
		var cr CreateRequest
		_ = json.NewDecoder(r.Body).Decode(&cr)
		f.creates = append(f.creates, cr)
		if f.leak {
			http.Error(w, "boom key="+testAPIKey+" tok="+testExecdTok, 500)
			return
		}
		f.n++
		id := fmt.Sprintf("sb-%d", f.n)
		st := f.createState
		if st == "" {
			st = "Running"
		}
		f.sbs[id] = &Sandbox{ID: id, Metadata: cr.Metadata, Status: Status{State: st}, CreatedAt: "2026-11-13T10:00:00Z"}
		jsonOut(w, 202, Sandbox{ID: id, Status: Status{State: "Pending"}, Metadata: cr.Metadata})
	case p == "/v1/sandboxes" && r.Method == "GET":
		var items []Sandbox
		for _, sb := range f.sbs {
			items = append(items, *sb)
		}
		jsonOut(w, 200, map[string]any{"items": items, "pagination": map[string]any{"page": 1, "hasNextPage": false}})
	case strings.HasPrefix(p, "/v1/sandboxes/") && strings.HasSuffix(p, "/endpoints/44772"):
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/v1/sandboxes/"), "/endpoints/44772")
		if f.sbs[id] == nil {
			http.NotFound(w, r)
			return
		}
		jsonOut(w, 200, map[string]any{"endpoint": "x:1/sandboxes/" + id + "/proxy/44772",
			"headers": map[string]string{execdTokHeader: testExecdTok}})
	case strings.HasPrefix(p, "/v1/sandboxes/") && r.Method == "GET":
		sb := f.sbs[strings.TrimPrefix(p, "/v1/sandboxes/")]
		if sb == nil {
			http.NotFound(w, r)
			return
		}
		f.getCount++
		jsonOut(w, 200, sb)
	case strings.HasPrefix(p, "/v1/sandboxes/") && r.Method == "DELETE":
		id := strings.TrimPrefix(p, "/v1/sandboxes/")
		if f.leak {
			http.Error(w, "boom key="+testAPIKey+" tok="+testExecdTok, 500)
			return
		}
		if f.sbs[id] == nil {
			http.NotFound(w, r)
			return
		}
		f.deletes = append(f.deletes, id)
		delete(f.sbs, id)
		w.WriteHeader(204)
	default:
		http.NotFound(w, r)
	}
}

func (f *efake) execd(w http.ResponseWriter, r *http.Request, id, p string) {
	f.mu.Lock()
	if f.sbs[id] == nil {
		f.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	script := f.script
	f.mu.Unlock()
	switch {
	case p == "/command" && r.Method == "POST":
		var cr CommandRequest
		_ = json.NewDecoder(r.Body).Decode(&cr)
		f.mu.Lock()
		f.cmds = append(f.cmds, cr)
		leak := f.leak
		f.mu.Unlock()
		if leak {
			http.Error(w, "boom key="+testAPIKey+" tok="+testExecdTok, 500)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		if script != nil {
			script(w, r)
			return
		}
		io.WriteString(w, `data: {"type":"init","text":"cmd-1"}`+"\n\n"+
			`data: {"type":"stdout","text":"hi\n"}`+"\n\n"+`data: {"type":"execution_complete","exit_code":0}`+"\n\n")
	case p == "/command" && r.Method == "DELETE":
		f.mu.Lock()
		f.cancelled = append(f.cancelled, r.URL.Query().Get("id"))
		f.mu.Unlock()
		w.WriteHeader(200)
	case p == "/v1/filesystem/1000/1000/files/info":
		path := r.URL.Query().Get("path")
		f.mu.Lock()
		data, ok := f.files[id][path]
		f.mu.Unlock()
		if !ok {
			http.Error(w, "no such file", 404)
			return
		}
		jsonOut(w, 200, map[string]FileInfo{path: {Path: path, Type: "file", Size: int64(len(data)), Mode: 640, ModifiedAt: "2026-11-13T10:00:00Z"}})
	case p == "/v1/filesystem/1000/1000/files/download":
		f.mu.Lock()
		data, ok := f.files[id][r.URL.Query().Get("path")]
		f.mu.Unlock()
		if !ok {
			http.Error(w, "no such file", 404)
			return
		}
		var a, b int
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &a, &b); err == nil {
			if b >= len(data) {
				b = len(data) - 1
			}
			w.WriteHeader(206)
			w.Write(data[a : b+1])
			return
		}
		w.Write(data)
	case p == "/v1/filesystem/1000/1000/files/upload":
		mr, err := r.MultipartReader()
		if err != nil {
			http.Error(w, "x", 400)
			return
		}
		var meta UploadMeta
		var body []byte
		for {
			part, err := mr.NextPart()
			if err != nil {
				break
			}
			b, _ := io.ReadAll(part)
			if part.FormName() == "metadata" {
				_ = json.Unmarshal(b, &meta)
			} else {
				body = b
			}
		}
		f.mu.Lock()
		if f.files[id] == nil {
			f.files[id] = map[string][]byte{}
		}
		f.files[id][meta.Path] = body
		f.mu.Unlock()
		jsonOut(w, 200, map[string]any{})
	default:
		http.NotFound(w, r)
	}
}

func (f *efake) only(t *testing.T) *Sandbox {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sbs) != 1 {
		t.Fatalf("want one sandbox, have %d", len(f.sbs))
	}
	for _, sb := range f.sbs {
		return sb
	}
	return nil
}

func spec(id workspace.ID) workspace.Spec {
	return workspace.Spec{ID: id, Image: "docker.io/library/alpine:3.20", Limits: workspace.Limits{CPUMilli: 500, MemoryBytes: 256 << 20}}
}

func setup(t *testing.T, secure string) (*efake, workspace.Executor, workspace.ID) {
	t.Helper()
	createPoll = 5 * time.Millisecond
	f, p := newEFake(t)
	ex := f.executor(t, p, secure)
	id := workspace.NewID()
	if err := ex.Create(ctx, spec(id)); err != nil {
		t.Fatal(err)
	}
	return f, ex, id
}

func TestExecutorName(t *testing.T) {
	f, p := newEFake(t)
	if n := f.executor(t, p, "").Name(); n != "opensandbox" {
		t.Fatal(n)
	}
}

func TestCreateMapping(t *testing.T) {
	f, _, id := setup(t, "")
	if len(f.creates) != 1 {
		t.Fatal(len(f.creates))
	}
	c := f.creates[0]
	if c.Image.URI != "docker.io/library/alpine:3.20" || strings.Join(c.Entrypoint, " ") != "sleep infinity" {
		t.Fatalf("%+v", c)
	}
	if c.ResourceLimits["cpu"] != "500m" || c.ResourceLimits["memory"] != "268435456" || c.Timeout != 60 {
		t.Fatalf("%+v", c)
	}
	if c.Metadata[MetaWorkspace] != string(id) || c.Metadata[MetaRunner] != testRunner {
		t.Fatalf("%v", c.Metadata)
	}
	if c.NetworkPolicy == nil || c.NetworkPolicy.DefaultAction != "deny" || len(c.NetworkPolicy.Egress) != 0 {
		t.Fatalf("policy %+v", c.NetworkPolicy)
	}
	v := c.Volumes
	if len(v) != 1 || v[0].MountPath != "/workspace" || v[0].PVC == nil || v[0].PVC.ClaimName != "mf-"+string(id) ||
		!v[0].PVC.DeleteOnSandboxTermination || !v[0].PVC.CreateIfNotExists {
		t.Fatalf("volumes %+v", v)
	}
}

func TestCreateEgressAndMaxAge(t *testing.T) {
	createPoll = 5 * time.Millisecond
	f, p := newEFake(t)
	ex := f.executor(t, p, "")
	s := spec(workspace.NewID())
	s.MaxAge = 2 * time.Hour
	s.Network = workspace.Network{Mode: workspace.NetEgress, Allow: []workspace.EgressRule{{Host: "pypi.org", Port: 443}, {Host: "github.com", Port: 443}}}
	if err := ex.Create(ctx, s); err != nil {
		t.Fatal(err)
	}
	c := f.creates[0]
	if c.Timeout != 7200 || c.NetworkPolicy.DefaultAction != "deny" || len(c.NetworkPolicy.Egress) != 2 ||
		c.NetworkPolicy.Egress[0] != (EgressRule{"allow", "github.com"}) {
		t.Fatalf("%+v", c)
	}
}

func TestCreateIdempotent(t *testing.T) {
	f, ex, id := setup(t, "")
	if err := ex.Create(ctx, spec(id)); err != nil {
		t.Fatal(err)
	}
	if len(f.creates) != 1 {
		t.Fatalf("second create posted again: %d", len(f.creates))
	}
}

func TestCreateGuardedNeedsSecureRuntime(t *testing.T) {
	f, p := newEFake(t)
	s := spec(workspace.NewID())
	s.RequireSecureRuntime = true
	err := f.executor(t, p, "").Create(ctx, s)
	if !errors.Is(err, workspace.ErrRefused) || len(f.creates) != 0 {
		t.Fatalf("err %v creates %d", err, len(f.creates))
	}
	createPoll = 5 * time.Millisecond
	if err := f.executor(t, p, "gvisor").Create(ctx, s); err != nil {
		t.Fatal(err)
	}
}

func TestCreateFailureCleansUp(t *testing.T) {
	createPoll = 5 * time.Millisecond
	f, p := newEFake(t)
	f.createState = "Failed"
	err := f.executor(t, p, "").Create(ctx, spec(workspace.NewID()))
	if !errors.Is(err, workspace.ErrUnavailable) {
		t.Fatal(err)
	}
	if len(f.deletes) != 1 || len(f.sbs) != 0 {
		t.Fatalf("not cleaned: deletes %v left %d", f.deletes, len(f.sbs))
	}
}

func TestCreateCancelCleansUp(t *testing.T) {
	createPoll = 5 * time.Millisecond
	f, p := newEFake(t)
	f.createState = "Pending"
	c, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := f.executor(t, p, "").Create(c, spec(workspace.NewID()))
	if err == nil {
		t.Fatal("want error")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sbs) != 0 {
		t.Fatalf("sandbox left behind: %d", len(f.sbs))
	}
}

func TestExecOutputAndExitCode(t *testing.T) {
	f, ex, id := setup(t, "")
	f.script = func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `data: {"type":"init","text":"cmd-9"}`+"\n\n"+
			`data: {"type":"stdout","text":"out"}`+"\n\n"+`data: {"type":"stderr","text":"err"}`+"\n\n"+
			`data: {"type":"error","error":{"ename":"CommandExecError","evalue":"3","exit_code":3}}`+"\n\n")
	}
	var out, errb bytes.Buffer
	res, err := ex.Exec(ctx, id, workspace.ExecRequest{Argv: []string{"sh", "-c", "x"}, Env: map[string]string{"A": "b"}, Stdout: &out, Stderr: &errb})
	if err != nil || res.ExitCode != 3 || res.TimedOut || out.String() != "out" || errb.String() != "err" {
		t.Fatalf("%v %+v %q %q", err, res, out.String(), errb.String())
	}
	cm := f.cmds[0]
	if cm.Cwd != "/workspace" || cm.Envs["A"] != "b" || *cm.UID != 1000 || *cm.GID != 1000 || cm.Argv[0] != "sh" || cm.Timeout != 0 {
		t.Fatalf("%+v", cm)
	}
	// success path
	f.script = nil
	out.Reset()
	res, err = ex.Exec(ctx, id, workspace.ExecRequest{Argv: []string{"echo"}, Cwd: "/workspace/sub", Stdout: &out})
	if err != nil || res.ExitCode != 0 || out.String() != "hi\n" || f.cmds[1].Cwd != "/workspace/sub" {
		t.Fatalf("%v %+v %q", err, res, out.String())
	}
}

func TestExecStdinUnsupported(t *testing.T) {
	_, ex, id := setup(t, "")
	_, err := ex.Exec(ctx, id, workspace.ExecRequest{Argv: []string{"cat"}, Stdin: strings.NewReader("x")})
	if !errors.Is(err, workspace.ErrUnsupported) {
		t.Fatal(err)
	}
}

func hang(w http.ResponseWriter, r *http.Request) {
	io.WriteString(w, `data: {"type":"init","text":"cmd-hang"}`+"\n\n")
	w.(http.Flusher).Flush()
	<-r.Context().Done()
}

func waitFor(t *testing.T, f *efake, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		f.mu.Lock()
		ok := cond()
		f.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met")
}

func TestExecCancel(t *testing.T) {
	f, ex, id := setup(t, "")
	f.script = hang
	c, cancel := context.WithCancel(context.Background())
	go func() {
		waitFor(t, f, func() bool { return len(f.cmds) == 1 })
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	res, err := ex.Exec(c, id, workspace.ExecRequest{Argv: []string{"sleep", "99"}})
	if !errors.Is(err, context.Canceled) || res.TimedOut {
		t.Fatalf("%v %+v", err, res)
	}
	waitFor(t, f, func() bool { return len(f.cancelled) == 1 })
	if f.cancelled[0] != "cmd-hang" {
		t.Fatal(f.cancelled)
	}
}

func TestExecTimeout(t *testing.T) {
	f, ex, id := setup(t, "")
	f.script = hang
	c, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	res, err := ex.Exec(c, id, workspace.ExecRequest{Argv: []string{"sleep", "99"}})
	if err != nil || !res.TimedOut {
		t.Fatalf("%v %+v", err, res)
	}
	waitFor(t, f, func() bool { return len(f.cancelled) == 1 })
	if f.cmds[0].Timeout <= 0 || f.cmds[0].Timeout > 300 {
		t.Fatalf("timeout ms %d", f.cmds[0].Timeout)
	}
}

func TestFiles(t *testing.T) {
	f, ex, id := setup(t, "")
	sb := f.only(t)
	fi, err := ex.WriteFile(ctx, id, "a.txt", strings.NewReader("hello world"), 0, true, 0o600)
	if err != nil || fi.Size != 11 {
		t.Fatalf("%v %+v", err, fi)
	}
	if string(f.files[sb.ID]["/workspace/a.txt"]) != "hello world" {
		t.Fatal("not stored")
	}
	rc, info, err := ex.ReadFile(ctx, id, "/workspace/a.txt", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "hello world" || info.Size != 11 || info.Mode != fs.FileMode(0o640) || info.ModTime.IsZero() {
		t.Fatalf("%q %+v", b, info)
	}
	rc, _, err = ex.ReadFile(ctx, id, "a.txt", 6, 3)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(rc)
	rc.Close()
	if string(b) != "wor" {
		t.Fatalf("range %q", b)
	}
	// offset write without truncate splices.
	if _, err = ex.WriteFile(ctx, id, "a.txt", strings.NewReader("WORLD!!"), 6, false, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := string(f.files[sb.ID]["/workspace/a.txt"]); got != "hello WORLD!!" {
		t.Fatalf("splice %q", got)
	}
	if _, _, err = ex.ReadFile(ctx, id, "missing", 0, 0); !errors.Is(err, workspace.ErrNotFound) {
		t.Fatal(err)
	}
	if _, _, err = ex.ReadFile(ctx, id, "../etc/passwd", 0, 0); !errors.Is(err, workspace.ErrRefused) {
		t.Fatal(err)
	}
	// sizes (MaxFile = 1 KiB)
	if _, err = ex.WriteFile(ctx, id, "big", bytes.NewReader(make([]byte, 2000)), 0, true, 0); !errors.Is(err, workspace.ErrTooLarge) {
		t.Fatalf("write: %v", err)
	}
	f.mu.Lock()
	f.files[sb.ID]["/workspace/huge"] = make([]byte, 5000)
	f.mu.Unlock()
	if _, _, err = ex.ReadFile(ctx, id, "huge", 0, 0); !errors.Is(err, workspace.ErrTooLarge) {
		t.Fatalf("read: %v", err)
	}
	if rc, _, err = ex.ReadFile(ctx, id, "huge", 0, 100); err != nil {
		t.Fatalf("bounded read: %v", err)
	}
	rc.Close()
}

func TestDestroyAndList(t *testing.T) {
	f, ex, id := setup(t, "")
	l, err := ex.List(ctx)
	if err != nil || len(l) != 1 || l[0].ID != id || l[0].State != workspace.StateRunning || l[0].Created.IsZero() {
		t.Fatalf("%v %+v", err, l)
	}
	if err := ex.Destroy(ctx, id); err != nil {
		t.Fatal(err)
	}
	if len(f.deletes) != 1 {
		t.Fatal(f.deletes)
	}
	if err := ex.Destroy(ctx, id); err != nil { // absent = nil
		t.Fatal(err)
	}
	if l, _ = ex.List(ctx); len(l) != 0 {
		t.Fatal(l)
	}
}

func TestForeignSandboxesUntouched(t *testing.T) {
	createPoll = 5 * time.Millisecond
	f, p := newEFake(t)
	ex := f.executor(t, p, "")
	id := workspace.NewID()
	f.add("other-runner", map[string]string{MetaWorkspace: string(id), MetaRunner: "runner-b"}, "Running")
	f.add("no-meta", nil, "Running")
	f.add("ws-only", map[string]string{MetaWorkspace: string(id)}, "Running")
	f.add("runner-only", map[string]string{MetaRunner: testRunner}, "Running") // ours by runner, but no workspace id

	if err := ex.Destroy(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := ex.Exec(ctx, id, workspace.ExecRequest{Argv: []string{"id"}}); !errors.Is(err, workspace.ErrNotFound) {
		t.Fatalf("exec: %v", err)
	}
	if _, _, err := ex.ReadFile(ctx, id, "a", 0, 0); !errors.Is(err, workspace.ErrNotFound) {
		t.Fatalf("read: %v", err)
	}
	if _, err := ex.WriteFile(ctx, id, "a", strings.NewReader("x"), 0, true, 0); !errors.Is(err, workspace.ErrNotFound) {
		t.Fatalf("write: %v", err)
	}
	if len(f.deletes) != 0 || len(f.cmds) != 0 || len(f.files) != 0 {
		t.Fatalf("foreign sandbox touched: deletes %v cmds %d files %d", f.deletes, len(f.cmds), len(f.files))
	}
	l, err := ex.List(ctx)
	if err != nil || len(l) != 0 {
		t.Fatalf("listed foreign: %v %+v", err, l)
	}
	// Our own workspace id can be created next to the foreign look-alikes and is the only one used.
	if err := ex.Create(ctx, spec(id)); err != nil {
		t.Fatal(err)
	}
	if err := ex.Destroy(ctx, id); err != nil {
		t.Fatal(err)
	}
	if len(f.deletes) != 1 || f.sbs["other-runner"] == nil || f.sbs["no-meta"] == nil || f.sbs["ws-only"] == nil {
		t.Fatalf("deletes %v", f.deletes)
	}
}

func TestListRefusesEmptyFilter(t *testing.T) {
	f, _ := newEFake(t)
	f.add("x", nil, "Running")
	c := NewClientForTest(t, f)
	for _, m := range []map[string]string{nil, {}} {
		if _, err := c.ListSandboxes(ctx, ListOptions{Metadata: m}); err == nil {
			t.Fatal("empty filter accepted")
		}
	}
}

func NewClientForTest(t *testing.T, f *efake) *Client {
	t.Helper()
	kf := filepath.Join(t.TempDir(), "k")
	if err := os.WriteFile(kf, []byte(testAPIKey), 0o600); err != nil {
		t.Fatal(err)
	}
	// reuse the pki from the fake's server certificate chain
	p := f.pki
	c, err := NewClient(Config{BaseURL: "https://" + f.srv.Listener.Addr().String(), CAFile: p.caFile,
		CertFile: p.clientCertFile, KeyFile: p.clientKey, APIKeyFile: kf})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSecretsNeverInExecutorErrors(t *testing.T) {
	createPoll = 5 * time.Millisecond
	f, p := newEFake(t)
	ex := f.executor(t, p, "")
	id := workspace.NewID()
	f.leak = true
	var errs []error
	errs = append(errs, ex.Create(ctx, spec(id)))
	f.leak = false
	if err := ex.Create(ctx, spec(id)); err != nil {
		t.Fatal(err)
	}
	f.leak = true
	_, err := ex.Exec(ctx, id, workspace.ExecRequest{Argv: []string{"id"}})
	errs = append(errs, err, ex.Destroy(ctx, id))
	for i, err := range errs {
		if err == nil {
			t.Fatalf("%d: expected an error", i)
		}
		// The execd token is only known to the client once an endpoint was fetched (i.e. from Exec on).
		if s := err.Error(); strings.Contains(s, testAPIKey) || (i > 0 && strings.Contains(s, testExecdTok)) {
			t.Fatalf("%d: secret in error: %v", i, err)
		}
	}
}
