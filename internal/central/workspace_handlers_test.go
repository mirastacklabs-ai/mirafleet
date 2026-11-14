package central

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/central/registry"
	"github.com/mirastacklabs-ai/mirafleet/internal/keys"
	"github.com/mirastacklabs-ai/mirafleet/internal/policy"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

const (
	wsImg      = "docker.io/library/golang:1.27"
	wsDevFleet = "dev"
	wsGdFleet  = "gd" // guarded (confirm) fleet
)

// wsLive is a registry.Live whose workspace calls hit an in-memory fake runner: a store-seeded
// central with a fake Live, as the service tests do. Everything else delegates to the real
// registry.
type wsLive struct {
	registry.Live
	snap []registry.SessionInfo

	mu    sync.Mutex
	files map[string]map[string][]byte // ws id -> path -> bytes
	calls []string
	stdin []string

	refuseDestroy bool // the runner answers destroy with busy
}

func (l *wsLive) Snapshot() []registry.SessionInfo { return l.snap }

func (l *wsLive) opCalls(op string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, c := range l.calls {
		if c == op {
			n++
		}
	}
	return n
}

func (l *wsLive) Workspace(ctx context.Context, id int64, req transport.WorkspaceRequest, body io.Reader, out io.Writer) (*transport.WorkspaceResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, req.Op)
	fs := l.files[req.ID]
	switch req.Op {
	case transport.OpCreate:
		l.files[req.ID] = map[string][]byte{}
		return &transport.WorkspaceResult{Code: transport.CodeOK}, nil
	case transport.OpExec:
		var in []byte
		if req.StdinLen > 0 {
			in = make([]byte, req.StdinLen)
			if _, err := io.ReadFull(body, in); err != nil {
				return nil, err
			}
		}
		l.stdin = append(l.stdin, string(in))
		return &transport.WorkspaceResult{Code: transport.CodeOK, Exec: &transport.WorkspaceExec{
			Stdout: strings.Join(req.Argv, " ") + "|" + string(in), DurationMs: 3}}, nil
	case transport.OpWrite:
		data := make([]byte, req.Length)
		if _, err := io.ReadFull(body, data); err != nil {
			return nil, err
		}
		fs[req.Path] = data
		return &transport.WorkspaceResult{Code: transport.CodeOK, File: &transport.WorkspaceFile{Size: int64(len(data))}}, nil
	case transport.OpRead:
		data, ok := fs[req.Path]
		if !ok {
			return &transport.WorkspaceResult{Code: transport.CodeNotFound, Message: "no such file"}, nil
		}
		end := int64(len(data))
		if req.Offset > end {
			req.Offset = end
		}
		chunk := data[req.Offset:end]
		if int64(len(chunk)) > req.Limit {
			chunk = chunk[:req.Limit]
		}
		if _, err := out.Write(chunk); err != nil {
			return nil, err
		}
		return &transport.WorkspaceResult{Code: transport.CodeOK, File: &transport.WorkspaceFile{Size: int64(len(data)),
			Mode: 0o644, Length: int64(len(chunk)), EOF: req.Offset+int64(len(chunk)) >= int64(len(data))}}, nil
	case transport.OpDestroy:
		if l.refuseDestroy {
			return &transport.WorkspaceResult{Code: transport.CodeBusy}, nil
		}
		delete(l.files, req.ID)
		return &transport.WorkspaceResult{Code: transport.CodeOK}, nil
	}
	return &transport.WorkspaceResult{Code: transport.CodeOK}, nil
}

type wsEnv struct {
	*env
	svc   *service.Service
	live  *wsLive
	alice store.Operator
	bob   store.Operator
}

func newWSEnv(t *testing.T) *wsEnv {
	t.Helper()
	w := &wsEnv{live: &wsLive{files: map[string]map[string][]byte{}}}
	w.env = newEnvOpts(t, 15*time.Second, func(c *Config) {
		w.live.Live = c.Registry
		svc, err := service.New(service.Config{Store: c.Store, Policy: policy.TablePolicy{}, Live: w.live,
			HostKey: c.HostKey.PublicKey(), Now: time.Now, OperatorConns: c.Operators, Logf: c.Logf, ReaperInterval: time.Hour})
		must(t, err)
		w.svc, c.Service = svc, svc
	})
	ctx := context.Background()
	cfg := store.WorkspaceConfig{DefaultImage: wsImg, Images: []string{wsImg, "docker.io/library/node:*"}}
	for _, f := range []struct {
		name    string
		confirm bool
	}{{wsDevFleet, false}, {wsGdFleet, true}} {
		_, err := w.svc.CreateFleet(ctx, w.admin, service.SourceCLI, f.name, store.ModeWorkspace, f.confirm, &cfg)
		must(t, err)
		r := w.enrol(f.name, "ws-"+f.name)
		w.live.snap = append(w.live.snap, registry.SessionInfo{ID: r.ID, Name: r.Name, Fleet: f.name,
			Status: registry.StatusActive, Online: true, Slots: 4, Executor: "containerd", SecureRuntime: "gvisor",
			Images: []string{"docker.io/library/golang:*", "docker.io/library/node:*"}})
	}
	w.alice, w.bob = w.addOperator("alice"), w.addOperator("bob")
	return w
}

// enrol creates an active runner in fleet through the store.
func (e *env) enrol(fleet, name string) store.Runner {
	e.t.Helper()
	ctx := context.Background()
	pub, err := keys.Generate(e.dir, name, name)
	must(e.t, err)
	pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pub))
	must(e.t, err)
	e.tokenN++
	tokID := fmt.Sprintf("%016x", e.tokenN+1000)
	secret := sha256.Sum256([]byte("secret-" + tokID))
	now := time.Now()
	_, err = e.db.CreateToken(ctx, store.JoinToken{TokenID: tokID, SecretHash: secret[:], Fleet: fleet,
		AutoApprove: true, MaxUses: 1, ExpiresAt: now.Add(time.Hour), CreatedBy: "test"}, now, auditEv("token.create"))
	must(e.t, err)
	r, err := e.db.Enroll(ctx, tokID, secret[:], now,
		store.Runner{Name: name, PubKey: pk.Marshal(), Fingerprint: ssh.FingerprintSHA256(pk)}, auditEv("runner.enrol"))
	must(e.t, err)
	return r
}

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func writeParams(ws, path string, data []byte) api.WorkspaceWriteFileParams {
	return api.WorkspaceWriteFileParams{Workspace: ws, Path: path, Length: len(data), SHA256: sum(data)}
}

func TestWorkspaceMethodsEndToEnd(t *testing.T) {
	w := newWSEnv(t)
	alice, bob := w.opClient("alice"), w.opClient("bob")
	ctx := tctx(t)

	if l, err := alice.WorkspaceList(ctx, ""); err != nil || len(l.Workspaces) != 0 {
		t.Fatalf("empty list: %+v %v", l, err)
	}
	op, err := alice.WorkspaceOpen(ctx, api.WorkspaceOpenParams{Fleet: wsDevFleet, Session: "s1"})
	must(t, err)
	if op.Reused || op.Workspace.State != "open" || op.Workspace.Fleet != wsDevFleet || op.Workspace.Session != "s1" ||
		op.Workspace.Image != wsImg || op.Workspace.Runner != "ws-dev" || len(op.Workspace.ID) != 32 || op.Workspace.Operator != "" {
		t.Fatalf("open: %+v", op)
	}
	id := op.Workspace.ID
	again, err := alice.WorkspaceOpen(ctx, api.WorkspaceOpenParams{Fleet: wsDevFleet, Session: "s1"})
	must(t, err)
	if !again.Reused || again.Workspace.ID != id {
		t.Fatalf("sticky open: %+v", again)
	}
	if _, err := alice.WorkspaceOpen(ctx, api.WorkspaceOpenParams{Fleet: wsDevFleet, Session: "bad session!"}); err == nil {
		t.Fatal("invalid session accepted")
	}

	// exec without and with stdin
	r, err := alice.WorkspaceExec(ctx, api.WorkspaceExecParams{Workspace: id, Argv: []string{"echo", "hi"}}, nil)
	must(t, err)
	if r.ExitCode != 0 || r.Stdout != "echo hi|" || r.RequestID == "" {
		t.Fatalf("exec: %+v", r)
	}
	r, err = alice.WorkspaceExec(ctx, api.WorkspaceExecParams{Workspace: id, Argv: []string{"cat"}, Stdin: true}, []byte("piped"))
	must(t, err)
	if r.Stdout != "cat|piped" {
		t.Fatalf("exec stdin: %+v", r)
	}
	r, err = alice.WorkspaceExec(ctx, api.WorkspaceExecParams{Workspace: id, Argv: []string{"cat"}, Stdin: true}, nil)
	must(t, err)
	if r.Stdout != "cat|" {
		t.Fatalf("exec empty stdin: %+v", r)
	}

	// write then read (including a full 1 MiB chunk and an empty read at EOF)
	big := bytes.Repeat([]byte("0123456789abcdef"), api.MaxFileChunk/16)
	wr, err := alice.WorkspaceWriteFile(ctx, writeParams(id, "big.bin", big), big)
	must(t, err)
	if wr.Size != int64(len(big)) {
		t.Fatalf("write size %d", wr.Size)
	}
	rf, body, err := alice.WorkspaceReadFile(ctx, api.WorkspaceReadFileParams{Workspace: id, Path: "big.bin"})
	must(t, err)
	if !bytes.Equal(body, big) || rf.Length != len(big) || rf.Size != int64(len(big)) || !rf.EOF || rf.Mode != 0o644 {
		t.Fatalf("read: %+v len=%d", rf, len(body))
	}
	rf, body, err = alice.WorkspaceReadFile(ctx, api.WorkspaceReadFileParams{Workspace: id, Path: "big.bin", Offset: int64(len(big))})
	must(t, err)
	if len(body) != 0 || rf.Length != 0 || !rf.EOF {
		t.Fatalf("read at EOF: %+v %d", rf, len(body))
	}
	_, err = alice.WorkspaceWriteFile(ctx, writeParams(id, "empty", nil), nil)
	must(t, err)
	// missing file: runner error, no body
	_, _, err = alice.WorkspaceReadFile(ctx, api.WorkspaceReadFileParams{Workspace: id, Path: "nope"})
	if err == nil {
		t.Fatal("read of a missing file succeeded")
	}
	// bad sha256: refused before the runner is contacted
	before := w.live.opCalls(transport.OpWrite)
	p := writeParams(id, "x", []byte("abc"))
	p.SHA256 = sum([]byte("other"))
	_, err = alice.WorkspaceWriteFile(ctx, p, []byte("abc"))
	wantCode(t, err, api.CodeBadRequest)
	if w.live.opCalls(transport.OpWrite) != before {
		t.Fatal("runner contacted for a corrupted chunk")
	}
	// body length differing from length is refused
	p = writeParams(id, "x", []byte("abc"))
	_, err = alice.WorkspaceWriteFile(ctx, p, []byte("abcd"))
	wantCode(t, err, api.CodeTooLarge)

	// list is the caller's own; bob sees none of it and every call on it is not_found
	l, err := alice.WorkspaceList(ctx, wsDevFleet)
	must(t, err)
	if len(l.Workspaces) != 1 || l.Workspaces[0].ID != id {
		t.Fatalf("list: %+v", l)
	}
	if l, err := bob.WorkspaceList(ctx, ""); err != nil || len(l.Workspaces) != 0 {
		t.Fatalf("bob list: %+v %v", l, err)
	}
	_, err = bob.WorkspaceExec(ctx, api.WorkspaceExecParams{Workspace: id, Argv: []string{"id"}}, nil)
	wantCode(t, err, api.CodeNotFound)
	_, _, err = bob.WorkspaceReadFile(ctx, api.WorkspaceReadFileParams{Workspace: id, Path: "big.bin"})
	wantCode(t, err, api.CodeNotFound)
	_, err = bob.WorkspaceWriteFile(ctx, writeParams(id, "x", []byte("abc")), []byte("abc"))
	wantCode(t, err, api.CodeNotFound)
	wantCode(t, bob.WorkspaceClose(ctx, id), api.CodeNotFound)

	must(t, alice.WorkspaceClose(ctx, id))
	if l, err := alice.WorkspaceList(ctx, ""); err != nil || len(l.Workspaces) != 0 {
		t.Fatalf("list after close: %+v %v", l, err)
	}
	_, err = alice.WorkspaceExec(ctx, api.WorkspaceExecParams{Workspace: id, Argv: []string{"id"}}, nil)
	wantCode(t, err, api.CodeConflict)
}

func TestWorkspaceGuardedFleetConfirm(t *testing.T) {
	w := newWSEnv(t)
	alice := w.opClient("alice")
	ctx := tctx(t)
	_, err := alice.WorkspaceOpen(ctx, api.WorkspaceOpenParams{Fleet: wsGdFleet, Session: "g"})
	wantCode(t, err, api.CodeBadRequest)
	op, err := alice.WorkspaceOpen(ctx, api.WorkspaceOpenParams{Fleet: wsGdFleet, Session: "g", Confirm: wsGdFleet})
	must(t, err)
	id := op.Workspace.ID
	// Without confirm the write is refused; the body was never needed.
	_, err = alice.WorkspaceWriteFile(ctx, writeParams(id, "f", []byte("x")), []byte("x"))
	wantCode(t, err, api.CodeBadRequest)
	_, err = alice.WorkspaceExec(ctx, api.WorkspaceExecParams{Workspace: id, Argv: []string{"id"}, Stdin: true}, []byte("x"))
	wantCode(t, err, api.CodeBadRequest)
	if n := w.live.opCalls(transport.OpWrite) + w.live.opCalls(transport.OpExec); n != 0 {
		t.Fatalf("%d runner calls on refused guarded requests", n)
	}
	p := writeParams(id, "f", []byte("x"))
	p.Confirm = wsGdFleet
	_, err = alice.WorkspaceWriteFile(ctx, p, []byte("x"))
	must(t, err)
}

// countingReader counts the bytes pulled from it.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func bodyFrame(b []byte) []byte {
	out := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(out, uint32(len(b)))
	copy(out[4:], b)
	return out
}

// Handler level: a body frame must stay unread whenever gating fails, and be read once the call
// is authorised (positive control).
func TestWorkspaceBodyNeverReadWhenGatingFails(t *testing.T) {
	w := newWSEnv(t)
	ctx := tctx(t)
	opP := func(o store.Operator) auth.Principal {
		return auth.Principal{Kind: auth.KindOperator, ID: o.ID, Name: o.Name}
	}
	mk := func(p auth.Principal, frame []byte) (*callCtx, *countingReader) {
		cr := &countingReader{r: bytes.NewReader(frame)}
		return &callCtx{p: p, src: service.SourceAPI, svc: w.svc, st: w.db, logf: w.logf, state: &connState{},
			body: &callBody{r: cr, closer: io.NopCloser(nil)}}, cr
	}
	open := func(o store.Operator, fleet, session, confirm string) string {
		res, _, err := w.svc.OpenWorkspace(ctx, opP(o), service.SourceAPI, fleet, session, "", nil, confirm)
		must(t, err)
		return res.WsID
	}
	id := open(w.alice, wsDevFleet, "s", "")
	gid := open(w.alice, wsGdFleet, "g", wsGdFleet)
	frame := bodyFrame([]byte("abc"))
	req := func(method string, p any) *api.Request {
		b, _ := json.Marshal(p)
		return &api.Request{V: api.Version, ID: "x", Method: method, Params: b}
	}
	good := writeParams(id, "f", []byte("abc"))
	badSha := good
	badSha.SHA256 = "XYZ"
	hugeLen := good
	hugeLen.Length = api.MaxFileChunk + 1
	badMode := good
	badMode.Mode = 0o4777
	noPath := good
	noPath.Path = ""
	guarded := writeParams(gid, "f", []byte("abc")) // confirm missing

	cases := []struct {
		name   string
		who    store.Operator
		method string
		h      handler
		params any
		want   string
	}{
		{"write by non-owner", w.bob, api.MethodWorkspaceWriteFile, handleWorkspaceWriteFile, good, api.CodeNotFound},
		{"write unknown workspace", w.alice, api.MethodWorkspaceWriteFile, handleWorkspaceWriteFile, writeParams(strings.Repeat("0", 32), "f", nil), api.CodeNotFound},
		{"write guarded without confirm", w.alice, api.MethodWorkspaceWriteFile, handleWorkspaceWriteFile, guarded, api.CodeBadRequest},
		{"write bad sha", w.alice, api.MethodWorkspaceWriteFile, handleWorkspaceWriteFile, badSha, api.CodeBadRequest},
		{"write length over chunk", w.alice, api.MethodWorkspaceWriteFile, handleWorkspaceWriteFile, hugeLen, api.CodeBadRequest},
		{"write bad mode", w.alice, api.MethodWorkspaceWriteFile, handleWorkspaceWriteFile, badMode, api.CodeBadRequest},
		{"write no path", w.alice, api.MethodWorkspaceWriteFile, handleWorkspaceWriteFile, noPath, api.CodeBadRequest},
		{"exec stdin by non-owner", w.bob, api.MethodWorkspaceExec, handleWorkspaceExec,
			api.WorkspaceExecParams{Workspace: id, Argv: []string{"cat"}, Stdin: true}, api.CodeNotFound},
		{"exec stdin guarded without confirm", w.alice, api.MethodWorkspaceExec, handleWorkspaceExec,
			api.WorkspaceExecParams{Workspace: gid, Argv: []string{"cat"}, Stdin: true}, api.CodeBadRequest},
		{"exec stdin empty argv", w.alice, api.MethodWorkspaceExec, handleWorkspaceExec,
			api.WorkspaceExecParams{Workspace: id, Stdin: true}, api.CodeBadRequest},
		{"exec stdin denied env", w.alice, api.MethodWorkspaceExec, handleWorkspaceExec,
			api.WorkspaceExecParams{Workspace: id, Argv: []string{"cat"}, Stdin: true, Env: map[string]string{"ANTHROPIC_API_KEY": "x"}}, api.CodeBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cc, cr := mk(opP(tc.who), frame)
			_, err := tc.h(ctx, cc, req(tc.method, tc.params))
			wantCode(t, err, tc.want)
			if cr.n != 0 {
				t.Fatalf("handler read %d body bytes before authorising", cr.n)
			}
		})
	}

	// Positive controls: the same calls from the owner do consume the frame.
	cc, cr := mk(opP(w.alice), frame)
	if _, err := handleWorkspaceWriteFile(ctx, cc, req(api.MethodWorkspaceWriteFile, good)); err != nil || cr.n != len(frame) {
		t.Fatalf("authorised write: err=%v read=%d", err, cr.n)
	}
	cc, cr = mk(opP(w.alice), frame)
	if _, err := handleWorkspaceExec(ctx, cc, req(api.MethodWorkspaceExec, api.WorkspaceExecParams{Workspace: id, Argv: []string{"cat"}, Stdin: true})); err != nil || cr.n != len(frame) {
		t.Fatalf("authorised exec: err=%v read=%d", err, cr.n)
	}
	if got := w.live.stdin[len(w.live.stdin)-1]; got != "abc" {
		t.Fatalf("stdin %q", got)
	}
}

// rawBodyCall sends a request frame plus raw bytes (a body frame or fragment) on a fresh channel
// and returns the response.
func rawBodyCall(t *testing.T, cl *ssh.Client, method string, params any, raw []byte) *api.Response {
	t.Helper()
	ch, reqs, err := cl.OpenChannel(transport.ChanAPI, nil)
	must(t, err)
	go ssh.DiscardRequests(reqs)
	defer ch.Close()
	req := api.Request{V: api.Version, ID: "b1", Method: method}
	req.Params, _ = json.Marshal(params)
	b, _ := json.Marshal(req)
	must(t, api.WriteFrame(ch, b))
	// A refused call may be answered and closed while the body is still going out; the write
	// error is irrelevant, the response is what is checked.
	go ch.Write(raw)
	done := make(chan *api.Response, 1)
	go func() {
		frame, err := api.ReadFrame(ch, api.MaxResponseBytes)
		if err != nil {
			done <- nil
			return
		}
		var resp api.Response
		json.Unmarshal(frame, &resp)
		done <- &resp
	}()
	select {
	case r := <-done:
		if r == nil {
			t.Fatal("no response")
		}
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("central did not answer without the body")
		return nil
	}
}

// Over the wire: an unauthorised caller sends a full 1 MiB body; central answers not_found
// without waiting for it, and the connection stays usable. An oversize announced body is refused
// from its 4-byte header alone, before any allocation or payload read.
func TestWorkspaceBodyOverTheWire(t *testing.T) {
	w := newWSEnv(t)
	ctx := tctx(t)
	alice, bob := w.opClient("alice"), w.opClient("bob")
	op, err := alice.WorkspaceOpen(ctx, api.WorkspaceOpenParams{Fleet: wsDevFleet, Session: "s"})
	must(t, err)
	id := op.Workspace.ID

	bobSSH := bobRaw(t, w)
	big := bytes.Repeat([]byte("z"), api.MaxFileChunk)
	resp := rawBodyCall(t, bobSSH, api.MethodWorkspaceWriteFile, writeParams(id, "f", big), bodyFrame(big))
	if resp.Error == nil || resp.Error.Code != api.CodeNotFound {
		t.Fatalf("non-owner write: %+v", resp)
	}
	resp = rawBodyCall(t, bobSSH, api.MethodWorkspaceExec, api.WorkspaceExecParams{Workspace: id, Argv: []string{"cat"}, Stdin: true}, bodyFrame(big))
	if resp.Error == nil || resp.Error.Code != api.CodeNotFound {
		t.Fatalf("non-owner exec: %+v", resp)
	}
	_, err = bob.WorkspaceList(ctx, "")
	must(t, err) // the connection is intact
	if w.live.opCalls(transport.OpWrite) != 0 {
		t.Fatal("runner saw a write")
	}

	// Oversize announced body from the owner: header only, no payload ever sent.
	aliceSSH := aliceRaw(t, w)
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], api.MaxFileChunk+1)
	resp = rawBodyCall(t, aliceSSH, api.MethodWorkspaceWriteFile, writeParams(id, "f", []byte("abc")), hdr[:])
	if resp.Error == nil || resp.Error.Code != api.CodeTooLarge {
		t.Fatalf("oversize write body: %+v", resp)
	}
	resp = rawBodyCall(t, aliceSSH, api.MethodWorkspaceExec, api.WorkspaceExecParams{Workspace: id, Argv: []string{"cat"}, Stdin: true}, hdr[:])
	if resp.Error == nil || resp.Error.Code != api.CodeTooLarge {
		t.Fatalf("oversize stdin body: %+v", resp)
	}
	// A body announcing more than length (but within a chunk) is too_large as well.
	binary.BigEndian.PutUint32(hdr[:], 4096)
	resp = rawBodyCall(t, aliceSSH, api.MethodWorkspaceWriteFile, writeParams(id, "f", []byte("abc")), hdr[:])
	if resp.Error == nil || resp.Error.Code != api.CodeTooLarge {
		t.Fatalf("body larger than length: %+v", resp)
	}
	if w.live.opCalls(transport.OpWrite) != 0 || w.live.opCalls(transport.OpExec) != 0 {
		t.Fatal("runner saw calls for refused bodies")
	}
}

func aliceRaw(t *testing.T, w *wsEnv) *ssh.Client { return opRaw(t, w, "alice") }
func bobRaw(t *testing.T, w *wsEnv) *ssh.Client   { return opRaw(t, w, "bob") }

func opRaw(t *testing.T, w *wsEnv, name string) *ssh.Client {
	t.Helper()
	cl, err := w.dial(transport.UserOperator, name)
	must(t, err)
	t.Cleanup(func() { cl.Close() })
	return cl
}

// A slow body sender is cut off by the body timer after authorisation.
func TestWorkspaceSlowBodyTimesOut(t *testing.T) {
	old := api.BodyTimeout
	api.BodyTimeout = 200 * time.Millisecond
	t.Cleanup(func() { api.BodyTimeout = old })
	w := newWSEnv(t)
	ctx := tctx(t)
	alice := w.opClient("alice")
	op, err := alice.WorkspaceOpen(ctx, api.WorkspaceOpenParams{Fleet: wsDevFleet, Session: "s"})
	must(t, err)
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], 3)
	ch, reqs, err := aliceRaw(t, w).OpenChannel(transport.ChanAPI, nil)
	must(t, err)
	go ssh.DiscardRequests(reqs)
	defer ch.Close()
	rq := api.Request{V: api.Version, ID: "s1", Method: api.MethodWorkspaceWriteFile}
	rq.Params, _ = json.Marshal(writeParams(op.Workspace.ID, "f", []byte("abc")))
	b, _ := json.Marshal(rq)
	must(t, api.WriteFrame(ch, b))
	_, err = ch.Write(hdr[:]) // header, then silence
	must(t, err)
	guard := time.AfterFunc(5*time.Second, func() { ch.Close() }) // a hang must fail, not stall
	defer guard.Stop()
	// Central closes the channel on timeout; either a timeout response or EOF is acceptable, a
	// hang is not.
	if frame, err := api.ReadFrame(ch, api.MaxResponseBytes); err == nil {
		var resp api.Response
		json.Unmarshal(frame, &resp)
		if resp.Error == nil || resp.Error.Code != api.CodeTimeout {
			t.Fatalf("slow body: %+v", resp)
		}
	}
	if !guard.Stop() {
		t.Fatal("central did not cut off the slow body sender")
	}
	if w.live.opCalls(transport.OpWrite) != 0 {
		t.Fatal("runner saw a write")
	}
}

// Workspace operator methods are served on api@ only; the admin methods are never reachable there.
func TestWorkspaceAdminMethodsRefusedOnAPI(t *testing.T) {
	w := newWSEnv(t)
	cl := w.opClient("alice")
	ctx := tctx(t)
	before := len(w.auditRows())
	for _, m := range []string{api.MethodFleetWorkspaceSet, api.MethodWorkspaceAdminList, api.MethodWorkspaceKill} {
		wantCode(t, cl.Call(ctx, m, map[string]string{"fleet": "dev", "id": strings.Repeat("0", 32)}, nil), api.CodeForbidden)
		wantCode(t, cl.Call(ctx, m, nil, nil), api.CodeForbidden)
	}
	if n := len(w.auditRows()); n != before {
		t.Fatalf("audit grew by %d on forbidden methods", n-before)
	}
	// Method sets stay disjoint.
	for _, m := range []string{api.MethodFleetWorkspaceSet, api.MethodWorkspaceAdminList, api.MethodWorkspaceKill} {
		if api.IsOperatorMethod(m) || !api.IsAdminMethod(m) {
			t.Fatalf("%s misclassified", m)
		}
	}
	for _, m := range []string{api.MethodWorkspaceOpen, api.MethodWorkspaceExec, api.MethodWorkspaceReadFile,
		api.MethodWorkspaceWriteFile, api.MethodWorkspaceClose, api.MethodWorkspaceList} {
		if !api.IsOperatorMethod(m) || api.IsAdminMethod(m) {
			t.Fatalf("%s misclassified", m)
		}
		if _, ok := adminTable().handlers[m]; ok || adminTable().allowed(m) {
			t.Fatalf("%s served on the admin table", m)
		}
		if _, ok := operatorTable().handlers[m]; !ok {
			t.Fatalf("%s missing on the operator table", m)
		}
	}
	for _, m := range []string{api.MethodFleetWorkspaceSet, api.MethodWorkspaceAdminList, api.MethodWorkspaceKill} {
		if _, ok := operatorTable().handlers[m]; ok || operatorTable().allowed(m) {
			t.Fatalf("%s served on the operator table", m)
		}
		if _, ok := adminTable().handlers[m]; !ok {
			t.Fatalf("%s missing on the admin table", m)
		}
	}
}

// E-04: a runner that refuses the destroy leaves the row closing; close answers ok with
// state "closing" (not unavailable), and a close that succeeds carries no state.
func TestWorkspaceCloseRunnerRefusesReportsClosing(t *testing.T) {
	w := newWSEnv(t)
	cl := w.opClient("alice")
	ctx := tctx(t)
	var open api.WorkspaceOpenResult
	must(t, cl.Call(ctx, api.MethodWorkspaceOpen, api.WorkspaceOpenParams{Fleet: wsDevFleet, Session: "s"}, &open))
	w.live.mu.Lock()
	w.live.refuseDestroy = true
	w.live.mu.Unlock()
	var res api.WorkspaceCloseResult
	must(t, cl.Call(ctx, api.MethodWorkspaceClose, api.WorkspaceCloseParams{Workspace: open.Workspace.ID}, &res))
	if res.State != "closing" {
		t.Fatalf("state %q, want closing", res.State)
	}
	w.live.mu.Lock()
	w.live.refuseDestroy = false
	w.live.mu.Unlock()
	var done api.WorkspaceCloseResult
	must(t, cl.Call(ctx, api.MethodWorkspaceClose, api.WorkspaceCloseParams{Workspace: open.Workspace.ID}, &done))
	if done.State != "" {
		t.Fatalf("closed: state %q, want empty", done.State)
	}
}

// DTOs carry only the documented fields; nothing internal.
func TestWorkspaceDTOsHaveNoInternalFields(t *testing.T) {
	now := time.Now()
	row := store.Workspace{ID: 7, WsID: strings.Repeat("a", 32), FleetID: 3, Fleet: "dev", RunnerID: 9, Runner: "r1",
		OperatorID: 5, Operator: "alice", Session: "s", Image: "img", Spec: `{"secret":"x"}`, State: store.WorkspaceOpen,
		Reason: "internal reason", CreatedAt: now, LastUsedAt: now, ExpiresAt: now, ClosedAt: now}
	keysOf := func(v any) []string {
		b, err := json.Marshal(v)
		must(t, err)
		var m map[string]any
		must(t, json.Unmarshal(b, &m))
		var ks []string
		for k := range m {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		return ks
	}
	opKeys := "created expires fleet id image last_used runner session state"
	if got := strings.Join(keysOf(wsInfo(row, false)), " "); got != opKeys {
		t.Fatalf("operator DTO keys: %s", got)
	}
	if got := strings.Join(keysOf(wsInfo(row, true)), " "); got != "created expires fleet id image last_used operator runner session state" {
		t.Fatalf("admin DTO keys: %s", got)
	}
	b, _ := json.Marshal(api.WorkspaceOpenResult{Workspace: wsInfo(row, false)})
	for _, bad := range []string{"secret", "internal reason", "spec", "operator_id", "runner_id", "closed", "reason"} {
		if strings.Contains(string(b), bad) {
			t.Fatalf("DTO leaks %q: %s", bad, b)
		}
	}
	// Same through the wire, over the real handler.
	w := newWSEnv(t)
	cl := w.opClient("alice")
	ctx := tctx(t)
	var raw json.RawMessage
	must(t, cl.Call(ctx, api.MethodWorkspaceOpen, api.WorkspaceOpenParams{Fleet: wsDevFleet, Session: "s"}, &raw))
	var res map[string]json.RawMessage
	must(t, json.Unmarshal(raw, &res))
	var ws map[string]any
	must(t, json.Unmarshal(res["workspace"], &ws))
	var got []string
	for k := range ws {
		got = append(got, k)
	}
	sort.Strings(got)
	if strings.Join(got, " ") != opKeys {
		t.Fatalf("wire keys: %v", got)
	}
}

func TestWorkspaceHandlerTimeouts(t *testing.T) {
	if operatorTimeout(api.MethodActionRun) != 0 || operatorTimeout(api.MethodWorkspaceList) != api.ListTimeout ||
		operatorTimeout(api.MethodWorkspaceOpen) < 5*time.Minute || operatorTimeout(api.MethodWorkspaceExec) <= api.HardWorkspaceExecCeiling ||
		operatorTimeout(api.MethodWorkspaceReadFile) < 60*time.Second || operatorTimeout(api.MethodWorkspaceWriteFile) < 60*time.Second ||
		operatorTimeout(api.MethodWorkspaceClose) < 60*time.Second || operatorTimeout(api.MethodFleetsList) != api.ListTimeout {
		t.Fatal("unexpected per-method timeouts")
	}
}
