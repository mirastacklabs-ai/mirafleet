package mcpserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/orch"
)

// wsRig is a fakeBackend with a dev (open) and a prod (guarded) fleet, session-keyed workspace
// ids, and the workspace tools served over a pipe.
type wsRig struct {
	f *fakeBackend
	h *harness

	mu    sync.Mutex
	byses map[string]string
	n     int
}

func newWSRig(t *testing.T, defaultFleet string) *wsRig {
	t.Helper()
	r := &wsRig{byses: map[string]string{}}
	f := &fakeBackend{Contexts: []string{"main"}}
	r.f = f
	f.ResolveFn = func(_ context.Context, c, fl string) (orch.Target, error) {
		tg := orch.Target{Context: c, ContextSource: orch.SrcArgument, Fleet: fl, FleetSource: orch.SrcArgument}
		if c == "" {
			tg.Context, tg.ContextSource = "main", orch.SrcOnly
		}
		if fl == "" && defaultFleet != "" {
			tg.Fleet, tg.FleetSource = defaultFleet, orch.SrcProject
		} else if fl == "" {
			tg.FleetSource = orch.SrcNone
		}
		return tg, nil
	}
	f.FleetsListFn = func(context.Context, string) (*api.FleetsListResult, error) {
		return &api.FleetsListResult{Fleets: []api.FleetInfo{
			{Name: "dev", Mode: "workspace"}, {Name: "prod", Mode: "workspace", Confirm: true},
		}}, nil
	}
	f.WorkspaceOpenFn = func(_ context.Context, _ string, p api.WorkspaceOpenParams) (*api.WorkspaceOpenResult, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		id, reused := r.byses[p.Session]
		if !reused {
			r.n++
			id = fmt.Sprintf("ws-%d", r.n)
			r.byses[p.Session] = id
		}
		return &api.WorkspaceOpenResult{Workspace: api.WorkspaceInfo{ID: id, Fleet: p.Fleet, Runner: "build-1", Session: p.Session, State: "open"}, Reused: reused}, nil
	}
	r.h = startTools(t, WorkspaceTools(f))
	return r
}

func (r *wsRig) call(t *testing.T, tool, args string) (map[string]any, string, string) {
	t.Helper()
	res := callJSON(t, r.h, tool, args)
	if res.IsError {
		c, m := errorOf(t, res)
		return nil, c, m
	}
	var v map[string]any
	okJSON(t, res, &v)
	if _, ok := v["where"]; !ok {
		t.Fatalf("%s: no where in %v", tool, v)
	}
	return v, "", ""
}

func (r *wsRig) mustCall(t *testing.T, tool, args string) map[string]any {
	t.Helper()
	v, c, m := r.call(t, tool, args)
	if v == nil {
		t.Fatalf("%s %s: %s: %s", tool, args, c, m)
	}
	return v
}

func (r *wsRig) wantErr(t *testing.T, tool, args, code, contains string) {
	t.Helper()
	v, c, m := r.call(t, tool, args)
	if v != nil || c != code || !strings.Contains(m, contains) {
		t.Fatalf("%s %s: got %v %q %q; want %s containing %q", tool, args, v, c, m, code, contains)
	}
}

func hexSum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func TestWSToolSet(t *testing.T) {
	r := newWSRig(t, "dev")
	lt, err := r.h.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, tl := range lt.Tools {
		got[tl.Name] = true
		if tl.OutputSchema == nil || tl.Annotations == nil {
			t.Errorf("%s: missing output schema or annotations", tl.Name)
		}
	}
	for _, n := range []string{"workspace_open", "exec", "read_file", "write_file", "workspace_close"} {
		if !got[n] {
			t.Errorf("missing tool %s", n)
		}
	}
}

func TestWSImplicitOpenAndReuse(t *testing.T) {
	r := newWSRig(t, "dev")
	v := r.mustCall(t, "exec", `{"command":"echo hi"}`)
	if v["workspace"] != "ws-1" {
		t.Fatalf("%v", v)
	}
	w := v["where"].(map[string]any)
	if w["fleet"] != "dev" || w["fleet_source"] != orch.SrcProject {
		t.Fatalf("where %v", w)
	}
	r.mustCall(t, "exec", `{"argv":["ls","-l"]}`)
	if n := len(r.f.calls("WorkspaceOpen")); n != 1 {
		t.Fatalf("%d opens, want 1", n)
	}
	ex := r.f.calls("WorkspaceExec")
	if len(ex) != 2 || ex[1].(api.WorkspaceExecParams).Workspace != "ws-1" {
		t.Fatalf("%v", ex)
	}
	op := r.f.calls("WorkspaceOpen")[0].(api.WorkspaceOpenParams)
	if op.Fleet != "dev" || !strings.HasPrefix(op.Session, "p:") || op.Confirm != "" {
		t.Fatalf("%+v", op)
	}
}

func TestWSImplicitOpenRefusedOnGuarded(t *testing.T) {
	r := newWSRig(t, "dev")
	r.wantErr(t, "exec", `{"command":"id","fleet":"prod"}`, "bad_request", "workspace_open first")
	r.wantErr(t, "read_file", `{"path":"etc/hostname","fleet":"prod"}`, "bad_request", "workspace_open first")
	r.wantErr(t, "write_file", `{"path":"a","content":"x","fleet":"prod"}`, "bad_request", "workspace_open first")
	if n := len(r.f.calls("WorkspaceOpen")); n != 0 {
		t.Fatalf("%d opens on a guarded fleet", n)
	}
	// An explicit workspace_open with confirm binds it; exec then works with confirm.
	r.mustCall(t, "workspace_open", `{"fleet":"prod","confirm":"prod"}`)
	r.mustCall(t, "exec", `{"command":"id","fleet":"prod","confirm":"prod"}`)
}

func TestWSExecArgs(t *testing.T) {
	r := newWSRig(t, "dev")
	r.mustCall(t, "exec", `{"command":"echo hi && true"}`)
	r.mustCall(t, "exec", `{"argv":["/usr/bin/env","x"],"cwd":"src","timeout_s":5,"stdin":"in"}`)
	ex := r.f.calls("WorkspaceExec")
	if got := ex[0].(api.WorkspaceExecParams).Argv; strings.Join(got, "|") != "/bin/sh|-c|echo hi && true" {
		t.Fatalf("%q", got)
	}
	p := ex[1].(api.WorkspaceExecParams)
	if strings.Join(p.Argv, "|") != "/usr/bin/env|x" || p.Cwd != "src" || p.TimeoutS != 5 {
		t.Fatalf("%+v", p)
	}
	r.wantErr(t, "exec", `{"command":"a","argv":["b"]}`, "bad_request", "exactly one")
	r.wantErr(t, "exec", `{}`, "bad_request", "exactly one")
	r.wantErr(t, "exec", `{"command":""}`, "bad_request", "empty")
	r.wantErr(t, "exec", `{"argv":[]}`, "bad_request", "")
	r.wantErr(t, "exec", `{"command":"a","timeout_s":-1}`, "bad_request", "timeout_s")
	r.wantErr(t, "exec", `{"command":"a","local_path":"~/.ssh"}`, "bad_request", "unknown or mis-cased")
	r.wantErr(t, "exec", `{"command":"a","stdin":"`+strings.Repeat("x", api.MaxFileChunk+1)+`"}`, "too_large", "stdin")
	r.wantErr(t, "exec", `{"command":"a","cwd":"../x"}`, "bad_request", "")
}

func TestWSExecStdinReachesBackend(t *testing.T) {
	r := newWSRig(t, "dev")
	var got []byte
	r.f.WorkspaceExecFn = func(_ context.Context, _ string, _ api.WorkspaceExecParams, in []byte) (*api.WorkspaceExecResult, error) {
		got = in
		return &api.WorkspaceExecResult{Stdout: "ok", DurationMS: 1500}, nil
	}
	v := r.mustCall(t, "exec", `{"command":"cat","stdin":"hello"}`)
	if string(got) != "hello" || v["duration"] != "1.5s" {
		t.Fatalf("%q %v", got, v)
	}
}

func TestWSEnvRefusedAndDecoysNeverForwarded(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "decoy-anthropic-9f3a")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "decoy-aws-77c1")
	t.Setenv("GITHUB_TOKEN", "decoy-gh-12ab")
	r := newWSRig(t, "dev")
	r.wantErr(t, "exec", `{"command":"env","env":{"ANTHROPIC_API_KEY":"x"}}`, "bad_request", "")
	r.mustCall(t, "exec", `{"command":"env","env":{"FOO":"bar"}}`)
	r.mustCall(t, "workspace_open", `{}`)
	r.mustCall(t, "write_file", `{"path":"a.txt","content":"hi"}`)
	r.mustCall(t, "read_file", `{"path":"a.txt"}`)
	r.mustCall(t, "workspace_close", `{}`)
	if n := len(r.f.calls("WorkspaceExec")); n != 1 {
		t.Fatalf("%d execs reached the backend, want 1 (the refused one must not)", n)
	}
	all, err := json.Marshal(r.f.Calls)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"decoy-anthropic-9f3a", "decoy-aws-77c1", "decoy-gh-12ab", "ANTHROPIC_API_KEY"} {
		if bytes.Contains(all, []byte(d)) {
			t.Errorf("%q reached a backend request: %s", d, all)
		}
	}
}

func TestWSExecCapsNoisyOutput(t *testing.T) {
	r := newWSRig(t, "dev")
	noisy := strings.Repeat("\x1b[31mline <&> \"q\"\x1b[0m\r\n", 10000)
	r.f.WorkspaceExecFn = func(context.Context, string, api.WorkspaceExecParams, []byte) (*api.WorkspaceExecResult, error) {
		return &api.WorkspaceExecResult{Stdout: noisy, Stderr: "err\n"}, nil
	}
	res := callJSON(t, r.h, "exec", `{"command":"noisy"}`)
	var v struct {
		Stdout, Stderr string
		Truncated      bool
		OmittedBytes   int `json:"omitted_bytes"`
	}
	text := okJSON(t, res, &v)
	if !v.Truncated || v.OmittedBytes == 0 || strings.ContainsRune(v.Stdout, 0x1b) || !strings.Contains(v.Stdout, "[mirafleet:") {
		t.Fatalf("truncated=%v omitted=%d", v.Truncated, v.OmittedBytes)
	}
	enc, _ := json.Marshal(v.Stdout + v.Stderr)
	if len(enc)-2 > r.f.OutputCap() {
		t.Fatalf("encoded %d > cap %d", len(enc), r.f.OutputCap())
	}
	if len(text) > r.f.OutputCap()+2048 {
		t.Fatalf("whole result %d bytes", len(text))
	}
}

func TestWSExecSurfacesRunnerOmitted(t *testing.T) {
	r := newWSRig(t, "dev")
	r.f.WorkspaceExecFn = func(context.Context, string, api.WorkspaceExecParams, []byte) (*api.WorkspaceExecResult, error) {
		return &api.WorkspaceExecResult{Stdout: "tail", Omitted: 5000, Ref: "out-1"}, nil
	}
	var v struct {
		Truncated    bool
		OmittedBytes int `json:"omitted_bytes"`
		Ref, Note    string
	}
	okJSON(t, callJSON(t, r.h, "exec", `{"command":"big"}`), &v)
	if !v.Truncated || v.OmittedBytes != 5000 || v.Ref != "out-1" || !strings.Contains(v.Note, "read_output") {
		t.Fatalf("%+v", v)
	}
}

func TestWSReadFilePaging(t *testing.T) {
	r := newWSRig(t, "dev")
	data := []byte(strings.Repeat("héllo wörld, ünï\n", 20)) // multi-byte runes everywhere
	r.f.WorkspaceReadFileFn = func(_ context.Context, _ string, p api.WorkspaceReadFileParams) (*api.WorkspaceReadFileResult, []byte, error) {
		end := int(p.Offset) + p.Limit
		if end > len(data) {
			end = len(data)
		}
		return &api.WorkspaceReadFileResult{Size: int64(len(data)), Offset: p.Offset, Length: end - int(p.Offset), EOF: end == len(data)}, data[p.Offset:end], nil
	}
	var got []byte
	var off int64
	pages := 0
	for {
		v := r.mustCall(t, "read_file", fmt.Sprintf(`{"path":"f.txt","offset":%d,"limit":37}`, off))
		pages++
		page := []byte(v["content"].(string))
		raw := data[off : off+int64(v["length"].(float64))]
		if v["sha256"] != hexSum(raw) || string(page) != string(raw) {
			t.Fatalf("page %d mismatch: %v", pages, v)
		}
		got = append(got, raw...)
		if v["eof"] == true {
			if _, more := v["next_offset"]; more {
				t.Fatalf("eof with next_offset: %v", v)
			}
			break
		}
		off = int64(v["next_offset"].(float64))
		if pages > 100 {
			t.Fatal("no progress")
		}
	}
	if !bytes.Equal(got, data) || pages < 5 {
		t.Fatalf("reassembled %d bytes in %d pages", len(got), pages)
	}
	if p := r.f.calls("WorkspaceReadFile")[0].(api.WorkspaceReadFileParams); p.Workspace != "ws-1" {
		t.Fatalf("%+v", p)
	}
}

func TestWSReadFileDefaultLimitIsOutputCap(t *testing.T) {
	r := newWSRig(t, "dev")
	r.f.Cap = 4096
	r.f.WorkspaceReadFileFn = func(_ context.Context, _ string, p api.WorkspaceReadFileParams) (*api.WorkspaceReadFileResult, []byte, error) {
		return &api.WorkspaceReadFileResult{Size: 1 << 20, Length: p.Limit}, bytes.Repeat([]byte("a"), p.Limit), nil
	}
	v := r.mustCall(t, "read_file", `{"path":"big"}`)
	if v["length"] != float64(4096) || v["next_offset"] != float64(4096) || v["eof"] != false {
		t.Fatalf("%v", v)
	}
	r.mustCall(t, "read_file", `{"path":"big","limit":2000000}`) // clamped to the frame limit
	if p := r.f.calls("WorkspaceReadFile")[1].(api.WorkspaceReadFileParams); p.Limit != api.MaxFileChunk {
		t.Fatalf("%+v", p)
	}
}

func TestWSReadFileEncodings(t *testing.T) {
	r := newWSRig(t, "dev")
	bin := []byte{0x00, 0xff, 0xfe, 'a', 0x80}
	r.f.WorkspaceReadFileFn = func(context.Context, string, api.WorkspaceReadFileParams) (*api.WorkspaceReadFileResult, []byte, error) {
		return &api.WorkspaceReadFileResult{Size: int64(len(bin)), Length: len(bin), EOF: true}, bin, nil
	}
	r.wantErr(t, "read_file", `{"path":"b"}`, "bad_request", "not valid UTF-8")
	_, _, m := r.call(t, "read_file", `{"path":"b"}`)
	if !strings.Contains(m, "5 bytes") || !strings.Contains(m, hexSum(bin)) || !strings.Contains(m, "base64") {
		t.Fatalf("%s", m)
	}
	v := r.mustCall(t, "read_file", `{"path":"b","encoding":"base64"}`)
	dec, err := base64.StdEncoding.DecodeString(v["content"].(string))
	if err != nil || !bytes.Equal(dec, bin) || v["sha256"] != hexSum(bin) || v["encoding"] != "base64" {
		t.Fatalf("%v %v", v, err)
	}
	r.wantErr(t, "read_file", `{"path":"b","encoding":"hex"}`, "bad_request", "encoding")
	r.wantErr(t, "read_file", `{"path":""}`, "bad_request", "path")
}

func TestWSReadFileSanitisesText(t *testing.T) {
	r := newWSRig(t, "dev")
	body := []byte("a\x1b[31mred\x1b[0m\r\nb‮c")
	r.f.WorkspaceReadFileFn = func(context.Context, string, api.WorkspaceReadFileParams) (*api.WorkspaceReadFileResult, []byte, error) {
		return &api.WorkspaceReadFileResult{Size: int64(len(body)), Length: len(body), EOF: true}, body, nil
	}
	v := r.mustCall(t, "read_file", `{"path":"f"}`)
	c := v["content"].(string)
	if strings.ContainsRune(c, 0x1b) || strings.ContainsRune(c, '\r') || strings.ContainsRune(c, 0x202e) || v["sha256"] != hexSum(body) {
		t.Fatalf("%q", c)
	}
}

func TestWSWriteFileChunks(t *testing.T) {
	r := newWSRig(t, "dev")
	data := bytes.Repeat([]byte("0123456789abcdef"), (3<<19)/16) // 1.5 MiB
	var mu sync.Mutex
	var bodies [][]byte
	r.f.WorkspaceWriteFileFn = func(_ context.Context, _ string, p api.WorkspaceWriteFileParams, d []byte) (*api.WorkspaceWriteFileResult, error) {
		mu.Lock()
		bodies = append(bodies, append([]byte(nil), d...))
		mu.Unlock()
		return &api.WorkspaceWriteFileResult{Size: p.Offset + int64(len(d))}, nil
	}
	args, _ := json.Marshal(map[string]any{"path": "out.bin", "content": base64.StdEncoding.EncodeToString(data), "encoding": "base64", "mode": 0o600})
	v := r.mustCall(t, "write_file", string(args))
	if v["bytes_written"] != float64(len(data)) || v["sha256"] != hexSum(data) || v["size"] != float64(len(data)) {
		t.Fatalf("%v", v)
	}
	cs := r.f.calls("WorkspaceWriteFile")
	if len(cs) != 2 {
		t.Fatalf("%d chunks", len(cs))
	}
	c0, c1 := cs[0].(api.WorkspaceWriteFileParams), cs[1].(api.WorkspaceWriteFileParams)
	if c0.Offset != 0 || !c0.Truncate || c0.Length != api.MaxFileChunk || c0.SHA256 != hexSum(data[:api.MaxFileChunk]) || c0.Mode != 0o600 {
		t.Fatalf("%+v", c0)
	}
	if c1.Offset != api.MaxFileChunk || c1.Truncate || c1.Length != len(data)-api.MaxFileChunk || c1.SHA256 != hexSum(data[api.MaxFileChunk:]) {
		t.Fatalf("%+v", c1)
	}
	if !bytes.Equal(append(bodies[0], bodies[1]...), data) {
		t.Fatal("bodies differ")
	}
}

func TestWSWriteFileOffsetEmptyAndFailure(t *testing.T) {
	r := newWSRig(t, "dev")
	r.mustCall(t, "write_file", `{"path":"e","content":""}`)
	r.mustCall(t, "write_file", `{"path":"e","content":"tail","offset":10}`)
	cs := r.f.calls("WorkspaceWriteFile")
	e, o := cs[0].(api.WorkspaceWriteFileParams), cs[1].(api.WorkspaceWriteFileParams)
	if !e.Truncate || e.Length != 0 || e.SHA256 != hexSum(nil) {
		t.Fatalf("empty write %+v", e)
	}
	if o.Truncate || o.Offset != 10 || o.Length != 4 {
		t.Fatalf("offset write %+v", o)
	}
	// Second chunk fails: the error says how much landed.
	n := 0
	r.f.WorkspaceWriteFileFn = func(_ context.Context, _ string, p api.WorkspaceWriteFileParams, d []byte) (*api.WorkspaceWriteFileResult, error) {
		n++
		if n == 2 {
			return nil, &Error{Code: "unavailable", Message: "runner went away"}
		}
		return &api.WorkspaceWriteFileResult{Size: int64(len(d))}, nil
	}
	big := strings.Repeat("x", api.MaxFileChunk+5)
	r.wantErr(t, "write_file", `{"path":"big","content":"`+big+`"}`, "unavailable", fmt.Sprintf("%d of %d bytes were written", api.MaxFileChunk, len(big)))
	r.wantErr(t, "write_file", `{"path":"x","content":"!!","encoding":"base64"}`, "bad_request", "base64")
	r.wantErr(t, "write_file", `{"path":"x","content":"a","mode":4095}`, "bad_request", "")
	r.wantErr(t, "write_file", `{"path":"x"}`, "bad_request", "required")
	r.wantErr(t, "write_file", `{"path":"x","content":"a","offset":-1}`, "bad_request", "offset")
}

func TestWSWriteFileTooLarge(t *testing.T) {
	r := newWSRig(t, "dev")
	big := base64.StdEncoding.EncodeToString(make([]byte, MaxWriteBytes+1))
	r.wantErr(t, "write_file", `{"path":"x","encoding":"base64","content":"`+big+`"}`, "too_large", "exceeds")
	if n := len(r.f.calls("WorkspaceWriteFile")); n != 0 {
		t.Fatalf("%d writes", n)
	}
}

func TestWSCloseReturnsStateAndUnbinds(t *testing.T) {
	r := newWSRig(t, "dev")
	r.mustCall(t, "exec", `{"command":"a"}`)
	r.f.WorkspaceCloseFn = func(context.Context, string, string) (*api.WorkspaceCloseResult, error) {
		return &api.WorkspaceCloseResult{State: "closing"}, nil
	}
	v := r.mustCall(t, "workspace_close", `{}`)
	if v["state"] != "closing" || v["workspace"] != "ws-1" {
		t.Fatalf("%v", v)
	}
	if got := r.f.calls("WorkspaceClose"); len(got) != 1 || got[0] != "ws-1" {
		t.Fatalf("%v", got)
	}
	// Unbound: close with nothing bound is refused without a backend call, and the next exec
	// opens a fresh workspace (the fake reuses the id for the same session, but it is a new open).
	r.wantErr(t, "workspace_close", `{}`, "bad_request", "workspace_open first")
	r.mustCall(t, "exec", `{"command":"b"}`)
	if n := len(r.f.calls("WorkspaceOpen")); n != 2 {
		t.Fatalf("%d opens, want 2", n)
	}
	r.f.WorkspaceCloseFn = nil
	r.f.WorkspaceListFn = func(context.Context, string, string) (*api.WorkspaceListResult, error) {
		return &api.WorkspaceListResult{Workspaces: []api.WorkspaceInfo{{ID: "ws-9", Fleet: "dev", State: "open"}}}, nil
	}
	if v := r.mustCall(t, "workspace_close", `{"workspace":"ws-9"}`); v["state"] != "closed" || v["workspace"] != "ws-9" {
		t.Fatalf("%v", v)
	}
	if got := r.f.calls("WorkspaceClose"); got[len(got)-1] != "ws-9" {
		t.Fatalf("%v", got)
	}
	// The bound workspace is untouched by closing another id.
	r.mustCall(t, "exec", `{"command":"c"}`)
	if n := len(r.f.calls("WorkspaceOpen")); n != 2 {
		t.Fatalf("%d opens, want 2", n)
	}
}

func TestWSSessionReadAtEveryOpen(t *testing.T) {
	r := newWSRig(t, "dev")
	r.f.Session = "cc:session-one"
	v := r.mustCall(t, "workspace_open", `{}`)
	if v["session_source"] != "flag" || v["workspace"].(map[string]any)["id"] != "ws-1" {
		t.Fatalf("%v", v)
	}
	r.mustCall(t, "exec", `{"command":"a"}`)
	r.f.Session = "cc:session-two" // /clear
	v = r.mustCall(t, "workspace_open", `{}`)
	if v["workspace"].(map[string]any)["id"] != "ws-2" || v["reused"] != false {
		t.Fatalf("%v", v)
	}
	r.mustCall(t, "exec", `{"command":"b"}`)
	ex := r.f.calls("WorkspaceExec")
	if ex[0].(api.WorkspaceExecParams).Workspace != "ws-1" || ex[1].(api.WorkspaceExecParams).Workspace != "ws-2" {
		t.Fatalf("%v", ex)
	}
	ops := r.f.calls("WorkspaceOpen")
	if ops[0].(api.WorkspaceOpenParams).Session != "cc:session-one" || ops[1].(api.WorkspaceOpenParams).Session != "cc:session-two" {
		t.Fatalf("%v", ops)
	}
	// Same session again: reattaches.
	if v = r.mustCall(t, "workspace_open", `{}`); v["reused"] != true {
		t.Fatalf("%v", v)
	}
}

func TestWSConfirmPassThroughNeverCached(t *testing.T) {
	r := newWSRig(t, "dev")
	r.f.WorkspaceOpenFn = func(_ context.Context, _ string, p api.WorkspaceOpenParams) (*api.WorkspaceOpenResult, error) {
		if p.Confirm != p.Fleet {
			return nil, &Error{Code: "bad_request", Message: "fleet requires confirm to equal the fleet name"}
		}
		return &api.WorkspaceOpenResult{Workspace: api.WorkspaceInfo{ID: "ws-p", Fleet: p.Fleet}}, nil
	}
	want := "fleet `prod` in context `main` (only context) is guarded. Ask the user. Only after the user explicitly agrees, repeat the call with `confirm` set to the fleet name."
	r.wantErr(t, "workspace_open", `{"fleet":"prod"}`, "bad_request", want)
	r.mustCall(t, "workspace_open", `{"fleet":"prod","confirm":"prod"}`)
	r.wantErr(t, "workspace_open", `{"fleet":"prod"}`, "bad_request", want) // not remembered
	ops := r.f.calls("WorkspaceOpen")
	var confirms []string
	for _, o := range ops {
		confirms = append(confirms, o.(api.WorkspaceOpenParams).Confirm)
	}
	if strings.Join(confirms, ",") != ",prod," {
		t.Fatalf("%q", confirms)
	}
	// exec and write_file carry the argument verbatim, and nothing when it is absent.
	r.f.WorkspaceExecFn = func(_ context.Context, _ string, p api.WorkspaceExecParams, _ []byte) (*api.WorkspaceExecResult, error) {
		if p.Confirm != "prod" {
			return nil, &Error{Code: "bad_request", Message: "fleet requires confirm to equal the fleet name"}
		}
		return &api.WorkspaceExecResult{}, nil
	}
	r.wantErr(t, "exec", `{"command":"id","fleet":"prod"}`, "bad_request", want)
	r.mustCall(t, "exec", `{"command":"id","fleet":"prod","confirm":"prod"}`)
	r.wantErr(t, "exec", `{"command":"id","fleet":"prod"}`, "bad_request", want)
	r.mustCall(t, "write_file", `{"path":"p","content":"x","fleet":"prod","confirm":"prod"}`)
	r.mustCall(t, "write_file", `{"path":"p","content":"x","fleet":"prod"}`)
	w := r.f.calls("WorkspaceWriteFile")
	if w[0].(api.WorkspaceWriteFileParams).Confirm != "prod" || w[1].(api.WorkspaceWriteFileParams).Confirm != "" {
		t.Fatalf("%v", w)
	}
}

func TestWSOpenFormsAndExplicitWorkspace(t *testing.T) {
	r := newWSRig(t, "")
	r.wantErr(t, "workspace_open", `{}`, "bad_request", "no fleet selected")
	r.wantErr(t, "exec", `{"command":"a"}`, "bad_request", "no fleet selected")
	v := r.mustCall(t, "workspace_open", `{"fleet":"dev","image":"alpine:3","labels":{"gpu":"yes"},"context":"main"}`)
	if v["reused"] != false || v["session_source"] != "process" {
		t.Fatalf("%v", v)
	}
	op := r.f.calls("WorkspaceOpen")[0].(api.WorkspaceOpenParams)
	if op.Image != "alpine:3" || op.Labels["gpu"] != "yes" {
		t.Fatalf("%+v", op)
	}
	// An explicit workspace id needs no fleet and never opens one.
	r.f.WorkspaceListFn = func(context.Context, string, string) (*api.WorkspaceListResult, error) {
		return &api.WorkspaceListResult{Workspaces: []api.WorkspaceInfo{{ID: "ws-7", Fleet: "dev", State: "open"}}}, nil
	}
	r.mustCall(t, "exec", `{"command":"a","workspace":"ws-7"}`)
	if n := len(r.f.calls("WorkspaceOpen")); n != 1 {
		t.Fatalf("%d opens", n)
	}
	if p := r.f.calls("WorkspaceExec")[0].(api.WorkspaceExecParams); p.Workspace != "ws-7" {
		t.Fatalf("%+v", p)
	}
	r.wantErr(t, "exec", `{"command":"a","workspace":"a b"}`, "bad_request", "workspace id")
	r.wantErr(t, "workspace_open", `{"fleet":"Bad Name"}`, "bad_request", "fleet name")
}

func TestWSStaleBindingDropped(t *testing.T) {
	r := newWSRig(t, "dev")
	r.mustCall(t, "exec", `{"command":"a"}`)
	first := true
	r.f.WorkspaceExecFn = func(context.Context, string, api.WorkspaceExecParams, []byte) (*api.WorkspaceExecResult, error) {
		if first {
			first = false
			return nil, &Error{Code: "not_found", Message: "no such workspace"}
		}
		return &api.WorkspaceExecResult{}, nil
	}
	r.wantErr(t, "exec", `{"command":"b"}`, "not_found", "")
	r.mustCall(t, "exec", `{"command":"c"}`)
	if n := len(r.f.calls("WorkspaceOpen")); n != 2 {
		t.Fatalf("%d opens, want a fresh one after not_found", n)
	}
}
