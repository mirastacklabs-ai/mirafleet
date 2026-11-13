package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/central/registry"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// opsEnv is an env with one workspace fleet, one runner and an open workspace owned by ann.
func opsEnv(t *testing.T, guarded bool, mut func(*store.WorkspaceConfig)) (*env, auth.Principal, store.Workspace) {
	t.Helper()
	e := newEnv(t)
	e.wsFleet("dev", guarded, mut)
	e.wsRunner("dev", rspec{slots: 4, secure: "kata"})
	op := e.operator("ann")
	confirm := ""
	if guarded {
		confirm = "dev"
	}
	w, _, err := e.svc.OpenWorkspace(e.ctx, op, SourceAPI, "dev", "s1", "", nil, confirm)
	if err != nil {
		t.Fatal(err)
	}
	return e, op, w
}

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// auditText renders every audit row so tests can scan for leaked values.
func (e *env) auditText() string {
	var b strings.Builder
	for _, a := range e.audit() {
		fmt.Fprintf(&b, "%+v\n", a)
	}
	return b.String()
}

func wsExecOK(code int) func(context.Context, int64, transport.WorkspaceRequest, io.Reader, io.Writer) (*transport.WorkspaceResult, error) {
	return func(_ context.Context, _ int64, req transport.WorkspaceRequest, _ io.Reader, _ io.Writer) (*transport.WorkspaceResult, error) {
		if req.Op != transport.OpExec {
			return &transport.WorkspaceResult{Code: transport.CodeOK}, nil
		}
		return &transport.WorkspaceResult{Code: transport.CodeOK, Exec: &transport.WorkspaceExec{ExitCode: code,
			Stdout: "out", Stderr: "err", Truncated: true, DurationMs: 42}}, nil
	}
}

func TestExecWorkspaceHappy(t *testing.T) {
	e, op, w := opsEnv(t, false, nil)
	var got transport.WorkspaceRequest
	var dl time.Time
	var stdinGot []byte
	e.live.wsFn = func(ctx context.Context, id int64, req transport.WorkspaceRequest, body io.Reader, out io.Writer) (*transport.WorkspaceResult, error) {
		if req.Op != transport.OpExec {
			return &transport.WorkspaceResult{Code: transport.CodeOK}, nil
		}
		got = req
		dl, _ = ctx.Deadline()
		stdinGot, _ = io.ReadAll(body)
		return wsExecOK(3)(ctx, id, req, body, out)
	}
	e.svc.now = func() time.Time { return t0.Add(time.Hour) }
	res, err := e.svc.ExecWorkspace(e.ctx, op, SourceAPI, w.WsID, []string{"go", "test", "./..."}, "src", map[string]string{"LANG": "C", "CI": "1"},
		0, strings.NewReader("in"), 2, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 || res.Stdout != "out" || res.Stderr != "err" || !res.Truncated || res.DurationMS != 42 || res.RequestID == "" {
		t.Fatalf("result: %+v", res)
	}
	if got.ID != w.WsID || got.Cwd != "/workspace/src" || got.Env["LANG"] != "C" || got.StdinLen != 2 || got.TimeoutMs != 120000 ||
		string(stdinGot) != "in" || fmt.Sprint(got.Argv) != "[go test ./...]" {
		t.Fatalf("request: %+v", got)
	}
	if d := time.Until(dl); d < 120*time.Second || d > 120*time.Second+execMargin {
		t.Fatalf("deadline in %v", d)
	}
	rows := e.wsAudit(api.MethodWorkspaceExec)
	if len(rows) != 2 || rows[0].Outcome != OutcomeStart || rows[1].Outcome != OutcomeOK || rows[0].RequestID != res.RequestID ||
		rows[1].RequestID != res.RequestID || rows[0].Workspace != w.WsID || rows[1].ExitCode == nil || *rows[1].ExitCode != 3 {
		t.Fatalf("audit: %+v", rows)
	}
	p := rows[0].Params
	if p["argv"] != `["go","test","./..."]` || p["cwd"] != "/workspace/src" || p["env_names"] != "CI,LANG" {
		t.Fatalf("start params: %+v", p)
	}
	if got, _ := e.db.GetWorkspace(e.ctx, w.WsID); !got.LastUsedAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("last_used %v", got.LastUsedAt)
	}
}

func TestExecWorkspaceOutcomes(t *testing.T) {
	cases := []struct {
		name    string
		fn      func(context.Context, int64, transport.WorkspaceRequest, io.Reader, io.Writer) (*transport.WorkspaceResult, error)
		wantErr string
		outcome string
	}{
		{"runner timed out", func(context.Context, int64, transport.WorkspaceRequest, io.Reader, io.Writer) (*transport.WorkspaceResult, error) {
			return &transport.WorkspaceResult{Code: transport.CodeOK, Exec: &transport.WorkspaceExec{ExitCode: -1, TimedOut: true}}, nil
		}, "", OutcomeTimeout},
		{"disabled", func(context.Context, int64, transport.WorkspaceRequest, io.Reader, io.Writer) (*transport.WorkspaceResult, error) {
			return nil, registry.ErrDisabled
		}, api.CodeUnavailable, OutcomeError},
		{"pending", func(context.Context, int64, transport.WorkspaceRequest, io.Reader, io.Writer) (*transport.WorkspaceResult, error) {
			return nil, registry.ErrPending
		}, api.CodeUnavailable, OutcomeError},
		{"busy", func(context.Context, int64, transport.WorkspaceRequest, io.Reader, io.Writer) (*transport.WorkspaceResult, error) {
			return nil, registry.ErrBusy
		}, api.CodeUnavailable, OutcomeError},
		{"busy code", func(context.Context, int64, transport.WorkspaceRequest, io.Reader, io.Writer) (*transport.WorkspaceResult, error) {
			return &transport.WorkspaceResult{Code: transport.CodeBusy}, nil
		}, api.CodeUnavailable, OutcomeError},
		{"kick", func(context.Context, int64, transport.WorkspaceRequest, io.Reader, io.Writer) (*transport.WorkspaceResult, error) {
			return nil, &registry.KickError{Reason: "runner revoked"}
		}, api.CodeUnavailable, OutcomeCancelled},
		{"runner refused", func(context.Context, int64, transport.WorkspaceRequest, io.Reader, io.Writer) (*transport.WorkspaceResult, error) {
			return &transport.WorkspaceResult{Code: transport.CodeRefused, Message: "nope"}, nil
		}, api.CodeBadRequest, OutcomeError},
		{"runner not found", func(context.Context, int64, transport.WorkspaceRequest, io.Reader, io.Writer) (*transport.WorkspaceResult, error) {
			return &transport.WorkspaceResult{Code: transport.CodeNotFound}, nil
		}, api.CodeNotFound, OutcomeError},
		{"runner timeout code", func(context.Context, int64, transport.WorkspaceRequest, io.Reader, io.Writer) (*transport.WorkspaceResult, error) {
			return &transport.WorkspaceResult{Code: transport.CodeTimeout}, nil
		}, api.CodeTimeout, OutcomeTimeout},
		{"transport failure", func(context.Context, int64, transport.WorkspaceRequest, io.Reader, io.Writer) (*transport.WorkspaceResult, error) {
			return nil, fmt.Errorf("conn reset")
		}, api.CodeUnavailable, OutcomeError},
		{"deadline", func(ctx context.Context, _ int64, _ transport.WorkspaceRequest, _ io.Reader, _ io.Writer) (*transport.WorkspaceResult, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}, api.CodeTimeout, OutcomeTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, op, w := opsEnv(t, false, nil)
			e.live.wsFn = tc.fn
			if tc.name == "deadline" {
				restore := execMarginForTest(time.Millisecond)
				defer restore()
				// 1 s exec cap so the central deadline fires quickly.
				cur, _ := e.db.GetFleetWorkspace(e.ctx, "dev")
				cur.ExecDefault, cur.ExecMax = time.Second, time.Second
				if _, err := e.svc.SetFleetWorkspace(e.ctx, e.admin, SourceCLI, "dev", cur); err != nil {
					t.Fatal(err)
				}
			}
			_, err := e.svc.ExecWorkspace(e.ctx, op, SourceAPI, w.WsID, []string{"true"}, "", nil, 0, nil, 0, "")
			if code(t, err) != tc.wantErr {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
			rows := e.wsAudit(api.MethodWorkspaceExec)
			if len(rows) != 2 || rows[1].Outcome != tc.outcome || rows[0].RequestID != rows[1].RequestID {
				t.Fatalf("audit: %+v", rows)
			}
		})
	}
}

func TestExecWorkspaceCancelled(t *testing.T) {
	e, op, w := opsEnv(t, false, nil)
	ctx, cancel := context.WithCancel(e.ctx)
	e.live.wsFn = func(c context.Context, _ int64, _ transport.WorkspaceRequest, _ io.Reader, _ io.Writer) (*transport.WorkspaceResult, error) {
		cancel()
		<-c.Done()
		return nil, c.Err()
	}
	_, err := e.svc.ExecWorkspace(ctx, op, SourceAPI, w.WsID, []string{"sleep", "9"}, "", nil, 0, nil, 0, "")
	if code(t, err) != api.CodeUnavailable {
		t.Fatalf("err = %v", err)
	}
	rows := e.wsAudit(api.MethodWorkspaceExec)
	if len(rows) != 2 || rows[1].Outcome != OutcomeCancelled {
		t.Fatalf("audit: %+v", rows)
	}
}

func TestExecWorkspaceTimeoutCapping(t *testing.T) {
	cases := []struct {
		name     string
		mut      func(*store.WorkspaceConfig)
		timeoutS int
		wantMs   int64
	}{
		{"fleet default", nil, 0, 120000},
		{"within max", nil, 600, 600000},
		{"capped by default exec_max", nil, 86400, 1800000},
		{"custom default", func(c *store.WorkspaceConfig) { c.ExecDefault, c.ExecMax = 30*time.Second, time.Hour }, 0, 30000},
		{"custom max caps", func(c *store.WorkspaceConfig) { c.ExecDefault, c.ExecMax = 30*time.Second, 5*time.Minute }, 3600, 300000},
		{"custom max raises beyond old default", func(c *store.WorkspaceConfig) { c.ExecDefault, c.ExecMax = 30*time.Second, 4*time.Hour }, 7200, 7200000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, op, w := opsEnv(t, false, tc.mut)
			var got transport.WorkspaceRequest
			var dl time.Time
			e.live.wsFn = func(ctx context.Context, id int64, req transport.WorkspaceRequest, b io.Reader, o io.Writer) (*transport.WorkspaceResult, error) {
				if req.Op == transport.OpExec {
					got = req
					dl, _ = ctx.Deadline()
				}
				return wsExecOK(0)(ctx, id, req, b, o)
			}
			if _, err := e.svc.ExecWorkspace(e.ctx, op, SourceAPI, w.WsID, []string{"true"}, "", nil, tc.timeoutS, nil, 0, ""); err != nil {
				t.Fatal(err)
			}
			if got.TimeoutMs != tc.wantMs {
				t.Fatalf("timeout_ms = %d, want %d", got.TimeoutMs, tc.wantMs)
			}
			want := time.Duration(tc.wantMs)*time.Millisecond + execMargin
			if d := time.Until(dl); d > want || d < want-5*time.Second {
				t.Fatalf("deadline in %v, want about %v", d, want)
			}
		})
	}
	// A negative timeout is refused.
	e, op, w := opsEnv(t, false, nil)
	if c := code(t, errOf(e.svc.ExecWorkspace(e.ctx, op, SourceAPI, w.WsID, []string{"true"}, "", nil, -1, nil, 0, ""))); c != api.CodeBadRequest {
		t.Fatalf("negative timeout: %q", c)
	}
}

func TestExecWorkspaceDeniedEnv(t *testing.T) {
	e, op, w := opsEnv(t, false, nil)
	const secret = "sk-ant-TOPSECRETVALUE"
	for _, name := range []string{"ANTHROPIC_API_KEY", "GITHUB_TOKEN", "HTTPS_PROXY", "AWS_REGION", "lower"} {
		_, err := e.svc.ExecWorkspace(e.ctx, op, SourceAPI, w.WsID, []string{"env"}, "", map[string]string{"LANG": "C", name: secret}, 0, nil, 0, "")
		if code(t, err) != api.CodeBadRequest || !strings.Contains(err.Error(), name) || strings.Contains(err.Error(), secret) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	if n := len(e.wsCalls("exec")); n != 0 {
		t.Fatalf("%d exec dispatches", n)
	}
	if txt := e.auditText(); strings.Contains(txt, secret) {
		t.Fatalf("env value in audit:\n%s", txt)
	}
	if rows := e.wsAudit(api.MethodWorkspaceExec); len(rows) != 5 || rows[0].Outcome != OutcomeDenied {
		t.Fatalf("expected refusal rows: %+v", rows)
	}
	// Too many variables.
	many := map[string]string{}
	for i := 0; i < 33; i++ {
		many[fmt.Sprintf("V%d", i)] = "x"
	}
	if c := code(t, errOf(e.svc.ExecWorkspace(e.ctx, op, SourceAPI, w.WsID, []string{"env"}, "", many, 0, nil, 0, ""))); c != api.CodeBadRequest {
		t.Fatalf("too many env: %q", c)
	}
}

func TestExecWorkspaceEnvValuesNeverAudited(t *testing.T) {
	e, op, w := opsEnv(t, false, nil)
	e.live.wsFn = wsExecOK(0)
	const val = "very-private-value-123"
	if _, err := e.svc.ExecWorkspace(e.ctx, op, SourceAPI, w.WsID, []string{"env"}, "", map[string]string{"LANG": val}, 0, nil, 0, ""); err != nil {
		t.Fatal(err)
	}
	txt := e.auditText()
	if strings.Contains(txt, val) || !strings.Contains(txt, "LANG") {
		t.Fatalf("audit:\n%s", txt)
	}
}

func TestExecWorkspaceInputValidation(t *testing.T) {
	e, op, w := opsEnv(t, false, nil)
	long := make([]string, 300)
	for i := range long {
		long[i] = "a"
	}
	for name, c := range map[string]struct {
		argv  []string
		cwd   string
		stdin io.Reader
		n     int64
	}{
		"empty argv":    {argv: nil},
		"too many argv": {argv: long},
		"empty arg0":    {argv: []string{""}},
		"nul":           {argv: []string{"a\x00b"}},
		"cwd escape":    {argv: []string{"ls"}, cwd: "../../etc"},
		"cwd abs":       {argv: []string{"ls"}, cwd: "/etc"},
		"stdin missing": {argv: []string{"ls"}, n: 5},
		"stdin huge":    {argv: []string{"ls"}, stdin: strings.NewReader(""), n: api.MaxFileChunk + 1},
	} {
		if cd := code(t, errOf(e.svc.ExecWorkspace(e.ctx, op, SourceAPI, w.WsID, c.argv, c.cwd, nil, 0, c.stdin, c.n, ""))); cd != api.CodeBadRequest {
			t.Errorf("%s: %q", name, cd)
		}
	}
	if n := len(e.wsCalls("exec")); n != 0 {
		t.Fatalf("%d exec dispatches", n)
	}
}

func TestExecWorkspaceArgvClipped(t *testing.T) {
	e, op, w := opsEnv(t, false, nil)
	e.live.wsFn = wsExecOK(0)
	argv := []string{"echo", strings.Repeat("x", 20000)}
	if _, err := e.svc.ExecWorkspace(e.ctx, op, SourceAPI, w.WsID, argv, "", nil, 0, nil, 0, ""); err != nil {
		t.Fatal(err)
	}
	p := e.wsAudit(api.MethodWorkspaceExec)[0].Params
	if len(p["argv"]) > maxAuditArgv || p["argv_clipped"] != "true" {
		t.Fatalf("argv param len %d clipped=%q", len(p["argv"]), p["argv_clipped"])
	}
}

func TestExecWorkspaceStartRowFailClosed(t *testing.T) {
	e, op, w := opsEnv(t, false, nil)
	e.svc.st = &failAudit{Store: e.svc.st, outcome: OutcomeStart}
	_, err := e.svc.ExecWorkspace(e.ctx, op, SourceAPI, w.WsID, []string{"true"}, "", nil, 0, nil, 0, "")
	if code(t, err) != api.CodeUnavailable {
		t.Fatalf("err = %v", err)
	}
	if n := len(e.wsCalls("exec")); n != 0 {
		t.Fatalf("%d exec dispatches despite missing start row", n)
	}
	// The same for write and read.
	b := []byte("x")
	if c := code(t, errOf(e.svc.WriteWorkspaceFile(e.ctx, op, SourceAPI, w.WsID, "f", 0, false, 0, 1, sum(b), bytes.NewReader(b), ""))); c != api.CodeUnavailable {
		t.Fatalf("write: %q", c)
	}
	if c := code(t, errOf(e.svc.ReadWorkspaceFile(e.ctx, op, SourceAPI, w.WsID, "f", 0, 0, io.Discard))); c != api.CodeUnavailable {
		t.Fatalf("read: %q", c)
	}
	if n := len(e.wsCalls("write")) + len(e.wsCalls("read")); n != 0 {
		t.Fatalf("%d file dispatches", n)
	}
}

func TestExecWorkspaceOutcomeAuditFailureStillReturnsResult(t *testing.T) {
	e, op, w := opsEnv(t, false, nil)
	e.live.wsFn = wsExecOK(0)
	e.svc.st = &failAudit{Store: e.svc.st, outcome: OutcomeOK}
	if _, err := e.svc.ExecWorkspace(e.ctx, op, SourceAPI, w.WsID, []string{"true"}, "", nil, 0, nil, 0, ""); err != nil {
		t.Fatal(err)
	}
}

func TestGuardedConfirm(t *testing.T) {
	e, op, w := opsEnv(t, true, nil)
	e.live.wsFn = fileOK
	b := []byte("hi")
	for _, bad := range []string{"", "other", "DEV"} {
		if c := code(t, errOf(e.svc.ExecWorkspace(e.ctx, op, SourceAPI, w.WsID, []string{"true"}, "", nil, 0, nil, 0, bad))); c != api.CodeBadRequest {
			t.Errorf("exec confirm %q: %q", bad, c)
		}
		if c := code(t, errOf(e.svc.WriteWorkspaceFile(e.ctx, op, SourceAPI, w.WsID, "f", 0, false, 0, 2, sum(b), bytes.NewReader(b), bad))); c != api.CodeBadRequest {
			t.Errorf("write confirm %q: %q", bad, c)
		}
	}
	if n := len(e.wsCalls("exec")) + len(e.wsCalls("write")); n != 0 {
		t.Fatalf("%d dispatches without confirm", n)
	}
	den := 0
	for _, a := range e.audit() {
		if a.Outcome == OutcomeDenied && (a.Action == api.MethodWorkspaceExec || a.Action == api.MethodWorkspaceWriteFile) {
			den++
		}
	}
	if den != 6 {
		t.Fatalf("denied rows = %d", den)
	}
	if _, err := e.svc.ExecWorkspace(e.ctx, op, SourceAPI, w.WsID, []string{"true"}, "", nil, 0, nil, 0, "dev"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.WriteWorkspaceFile(e.ctx, op, SourceAPI, w.WsID, "f", 0, false, 0, 2, sum(b), bytes.NewReader(b), "dev"); err != nil {
		t.Fatal(err)
	}
	// Reads need no confirm.
	if _, err := e.svc.ReadWorkspaceFile(e.ctx, op, SourceAPI, w.WsID, "f", 0, 0, io.Discard); err != nil {
		t.Fatalf("read: %v", err)
	}
}

func TestReadWorkspaceFile(t *testing.T) {
	e, op, w := opsEnv(t, false, nil)
	var got transport.WorkspaceRequest
	const content = "SECRET-FILE-CONTENT"
	e.live.wsFn = func(_ context.Context, _ int64, req transport.WorkspaceRequest, _ io.Reader, out io.Writer) (*transport.WorkspaceResult, error) {
		if req.Op != transport.OpRead {
			return &transport.WorkspaceResult{Code: transport.CodeOK}, nil
		}
		got = req
		out.Write([]byte(content))
		return &transport.WorkspaceResult{Code: transport.CodeOK, File: &transport.WorkspaceFile{Size: 100, Mode: 0o644, Length: int64(len(content))}}, nil
	}
	e.svc.now = func() time.Time { return t0.Add(time.Minute) }
	var buf bytes.Buffer
	res, err := e.svc.ReadWorkspaceFile(e.ctx, op, SourceAPI, w.WsID, "a/../b.txt", 7, 1<<30, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if buf.String() != content || res.Length != len(content) || res.Size != 100 || res.Offset != 7 || res.Mode != 0o644 {
		t.Fatalf("result %+v body %q", res, buf.String())
	}
	if got.Path != "/workspace/b.txt" || got.Offset != 7 || got.Limit != api.MaxFileChunk {
		t.Fatalf("request: %+v", got)
	}
	rows := e.wsAudit(api.MethodWorkspaceReadFile)
	if len(rows) != 2 || rows[0].Outcome != OutcomeStart || rows[1].Outcome != OutcomeOK || rows[1].Params["bytes"] != fmt.Sprint(len(content)) ||
		rows[1].Params["path"] != "/workspace/b.txt" || rows[0].RequestID != rows[1].RequestID {
		t.Fatalf("audit: %+v", rows)
	}
	if strings.Contains(e.auditText(), content) {
		t.Fatal("file content in audit")
	}
	if g, _ := e.db.GetWorkspace(e.ctx, w.WsID); !g.LastUsedAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("last_used %v", g.LastUsedAt)
	}
	for name, c := range map[string]struct {
		path  string
		off   int64
		limit int
	}{"empty path": {"", 0, 0}, "escape": {"../x", 0, 0}, "nul": {"a\x00", 0, 0}, "neg offset": {"f", -1, 0}, "neg limit": {"f", 0, -1}} {
		if cd := code(t, errOf(e.svc.ReadWorkspaceFile(e.ctx, op, SourceAPI, w.WsID, c.path, c.off, c.limit, io.Discard))); cd != api.CodeBadRequest {
			t.Errorf("%s: %q", name, cd)
		}
	}
	// A runner that reports a length different from what it sent is an internal error.
	e.live.wsFn = func(_ context.Context, _ int64, req transport.WorkspaceRequest, _ io.Reader, out io.Writer) (*transport.WorkspaceResult, error) {
		return &transport.WorkspaceResult{Code: transport.CodeOK, File: &transport.WorkspaceFile{Length: 5}}, nil
	}
	if cd := code(t, errOf(e.svc.ReadWorkspaceFile(e.ctx, op, SourceAPI, w.WsID, "f", 0, 0, io.Discard))); cd != api.CodeInternal {
		t.Errorf("inconsistent: %q", cd)
	}
}

func TestWriteWorkspaceFile(t *testing.T) {
	e, op, w := opsEnv(t, false, nil)
	var got transport.WorkspaceRequest
	var gotBody []byte
	e.live.wsFn = func(_ context.Context, _ int64, req transport.WorkspaceRequest, body io.Reader, _ io.Writer) (*transport.WorkspaceResult, error) {
		if req.Op != transport.OpWrite {
			return &transport.WorkspaceResult{Code: transport.CodeOK}, nil
		}
		got = req
		gotBody, _ = io.ReadAll(body)
		return &transport.WorkspaceResult{Code: transport.CodeOK, File: &transport.WorkspaceFile{Size: 777}}, nil
	}
	e.svc.now = func() time.Time { return t0.Add(time.Minute) }
	data := []byte("PRIVATE-CONTENT-BYTES")
	res, err := e.svc.WriteWorkspaceFile(e.ctx, op, SourceAPI, w.WsID, "src/main.go", 10, true, 0o755, len(data), sum(data), bytes.NewReader(data), "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Size != 777 || !bytes.Equal(gotBody, data) {
		t.Fatalf("res %+v body %q", res, gotBody)
	}
	if got.Path != "/workspace/src/main.go" || got.Offset != 10 || !got.Truncate || got.Mode != 0o755 || got.Length != int64(len(data)) {
		t.Fatalf("request: %+v", got)
	}
	rows := e.wsAudit(api.MethodWorkspaceWriteFile)
	if len(rows) != 2 || rows[0].Outcome != OutcomeStart || rows[1].Outcome != OutcomeOK || rows[0].RequestID != rows[1].RequestID {
		t.Fatalf("audit: %+v", rows)
	}
	if p := rows[0].Params; p["path"] != "/workspace/src/main.go" || p["bytes"] != fmt.Sprint(len(data)) || p["sha256"] != sum(data) {
		t.Fatalf("params: %+v", p)
	}
	if strings.Contains(e.auditText(), string(data)) {
		t.Fatal("file content in audit")
	}
	if g, _ := e.db.GetWorkspace(e.ctx, w.WsID); !g.LastUsedAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("last_used %v", g.LastUsedAt)
	}
	// An empty write is valid.
	if _, err := e.svc.WriteWorkspaceFile(e.ctx, op, SourceAPI, w.WsID, "empty", 0, true, 0, 0, sum(nil), nil, ""); err != nil {
		t.Fatalf("empty write: %v", err)
	}
}

func TestWriteWorkspaceFileRefusals(t *testing.T) {
	e, op, w := opsEnv(t, false, nil)
	e.live.wsFn = fileOK
	data := []byte("hello")
	good := sum(data)
	other := sum([]byte("other"))
	call := func(path string, off int64, mode uint32, length int, hash string, body io.Reader) error {
		return errOf(e.svc.WriteWorkspaceFile(e.ctx, op, SourceAPI, w.WsID, path, off, false, mode, length, hash, body, ""))
	}
	cases := map[string]error{
		"sha mismatch":     call("f", 0, 0, 5, other, bytes.NewReader(data)),
		"sha mismatch len": call("f", 0, 0, 4, good, bytes.NewReader(data)),
		"short body":       call("f", 0, 0, 9, good, bytes.NewReader(data)),
		"setuid mode":      call("f", 0, 0o4755, 5, good, bytes.NewReader(data)),
		"sticky mode":      call("f", 0, 0o1777, 5, good, bytes.NewReader(data)),
		"too long":         call("f", 0, 0, api.MaxFileChunk+1, good, bytes.NewReader(data)),
		"negative length":  call("f", 0, 0, -1, good, bytes.NewReader(data)),
		"bad hash":         call("f", 0, 0, 5, "xyz", bytes.NewReader(data)),
		"upper hash":       call("f", 0, 0, 5, strings.ToUpper(good), bytes.NewReader(data)),
		"empty path":       call("", 0, 0, 5, good, bytes.NewReader(data)),
		"escape":           call("../../etc/passwd", 0, 0, 5, good, bytes.NewReader(data)),
		"neg offset":       call("f", -1, 0, 5, good, bytes.NewReader(data)),
		"nil body":         call("f", 0, 0, 5, good, nil),
	}
	for name, err := range cases {
		if c := code(t, err); c != api.CodeBadRequest {
			t.Errorf("%s: %q", name, c)
		}
	}
	if n := len(e.wsCalls("write")); n != 0 {
		t.Fatalf("%d write dispatches", n)
	}
	// A mismatch is audited as one error row.
	found := false
	for _, a := range e.wsAudit(api.MethodWorkspaceWriteFile) {
		if a.Outcome == OutcomeError && a.Message == "sha256 mismatch" && a.RequestID != "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("mismatch not audited: %+v", e.wsAudit(api.MethodWorkspaceWriteFile))
	}
	// Exactly 0o777 is fine.
	if err := call("f", 0, 0o777, 5, good, bytes.NewReader(data)); err != nil {
		t.Fatalf("0777: %v", err)
	}
}

func TestWriteBodyNotReadBeforeGating(t *testing.T) {
	e, op, w := opsEnv(t, true, nil)
	r := &countReader{}
	_, err := e.svc.WriteWorkspaceFile(e.ctx, op, SourceAPI, w.WsID, "f", 0, false, 0, 10, sum(nil), r, "")
	if code(t, err) != api.CodeBadRequest || r.n != 0 {
		t.Fatalf("err %v, body reads %d", err, r.n)
	}
	other := e.operator("bob")
	_, err = e.svc.WriteWorkspaceFile(e.ctx, other, SourceAPI, w.WsID, "f", 0, false, 0, 10, sum(nil), r, "dev")
	if code(t, err) != api.CodeNotFound || r.n != 0 {
		t.Fatalf("non-owner: err %v, body reads %d", err, r.n)
	}
}

type countReader struct{ n int }

func (c *countReader) Read([]byte) (int, error) { c.n++; return 0, io.EOF }

func TestWorkspaceOpsNonOwnerAndUnknown(t *testing.T) {
	e, _, w := opsEnv(t, false, nil)
	bob := e.operator("bob")
	b := []byte("x")
	unknown := strings.Repeat("a", 32)
	for _, id := range []string{w.WsID, unknown, "short", ""} {
		calls := map[string]error{
			"exec":  errOf(e.svc.ExecWorkspace(e.ctx, bob, SourceAPI, id, []string{"true"}, "", nil, 0, nil, 0, "")),
			"read":  errOf(e.svc.ReadWorkspaceFile(e.ctx, bob, SourceAPI, id, "f", 0, 0, io.Discard)),
			"write": errOf(e.svc.WriteWorkspaceFile(e.ctx, bob, SourceAPI, id, "f", 0, false, 0, 1, sum(b), bytes.NewReader(b), "")),
		}
		for name, err := range calls {
			if c := code(t, err); c != api.CodeNotFound {
				t.Errorf("%s on %q: %q", name, id, c)
			}
		}
	}
	if n := len(e.wsCalls("exec")) + len(e.wsCalls("read")) + len(e.wsCalls("write")); n != 0 {
		t.Fatalf("%d dispatches for non-owner", n)
	}
	// Admins and non-operators are denied outright.
	if c := code(t, errOf(e.svc.ExecWorkspace(e.ctx, e.admin, SourceCLI, w.WsID, []string{"true"}, "", nil, 0, nil, 0, ""))); c != api.CodeForbidden {
		t.Errorf("admin exec: %q", c)
	}
	if c := code(t, errOf(e.svc.ListWorkspaces(e.ctx, e.admin, SourceCLI, ""))); c != api.CodeForbidden {
		t.Errorf("admin list: %q", c)
	}
}

func TestWorkspaceOpsRequireOpenState(t *testing.T) {
	e, op, w := opsEnv(t, false, nil)
	b := []byte("x")
	check := func(want string) {
		t.Helper()
		for name, err := range map[string]error{
			"exec":  errOf(e.svc.ExecWorkspace(e.ctx, op, SourceAPI, w.WsID, []string{"true"}, "", nil, 0, nil, 0, "")),
			"read":  errOf(e.svc.ReadWorkspaceFile(e.ctx, op, SourceAPI, w.WsID, "f", 0, 0, io.Discard)),
			"write": errOf(e.svc.WriteWorkspaceFile(e.ctx, op, SourceAPI, w.WsID, "f", 0, false, 0, 1, sum(b), bytes.NewReader(b), "")),
		} {
			if c := code(t, err); c != want {
				t.Errorf("%s: %q, want %q", name, c, want)
			}
		}
	}
	// Runner offline for destroy: the workspace stays closing.
	e.live.wsFn = func(context.Context, int64, transport.WorkspaceRequest, io.Reader, io.Writer) (*transport.WorkspaceResult, error) {
		return nil, fmt.Errorf("offline")
	}
	cw, err := e.svc.CloseWorkspace(e.ctx, op, SourceAPI, w.WsID)
	if err != nil || cw.State != store.WorkspaceClosing {
		t.Fatalf("close: %+v %v", cw, err)
	}
	check(api.CodeConflict)
	e.live.wsFn = nil
	if cw, err = e.svc.CloseWorkspace(e.ctx, op, SourceAPI, w.WsID); err != nil || cw.State != store.WorkspaceClosed {
		t.Fatalf("close again: %+v %v", cw, err)
	}
	check(api.CodeConflict)
	if n := len(e.wsCalls("exec")) + len(e.wsCalls("read")) + len(e.wsCalls("write")); n != 0 {
		t.Fatalf("%d dispatches", n)
	}
}

func TestListWorkspaces(t *testing.T) {
	e := newEnv(t)
	e.wsFleet("dev", false, nil)
	e.wsFleet("qa", false, nil)
	e.wsRunner("dev", rspec{slots: 4})
	e.wsRunner("qa", rspec{slots: 4})
	ann, bob := e.operator("ann"), e.operator("bob")
	mk := func(p auth.Principal, fleet, session string) store.Workspace {
		w, _, err := e.svc.OpenWorkspace(e.ctx, p, SourceAPI, fleet, session, "", nil, "")
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	a1, a2 := mk(ann, "dev", "s1"), mk(ann, "qa", "s1")
	b1 := mk(bob, "dev", "s1")
	closed := mk(ann, "dev", "s2")
	if _, err := e.svc.CloseWorkspace(e.ctx, ann, SourceAPI, closed.WsID); err != nil {
		t.Fatal(err)
	}
	before := len(e.wsAudit(api.MethodWorkspaceList))
	ws, err := e.svc.ListWorkspaces(e.ctx, ann, SourceAPI, "")
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, w := range ws {
		ids[w.WsID] = true
		if w.OperatorID != ann.ID || w.Fleet == "" || w.Runner == "" {
			t.Fatalf("row: %+v", w)
		}
	}
	if len(ws) != 2 || !ids[a1.WsID] || !ids[a2.WsID] || ids[b1.WsID] || ids[closed.WsID] {
		t.Fatalf("list: %+v", ws)
	}
	if ws, err = e.svc.ListWorkspaces(e.ctx, ann, SourceAPI, "qa"); err != nil || len(ws) != 1 || ws[0].WsID != a2.WsID {
		t.Fatalf("fleet filter: %+v %v", ws, err)
	}
	if c := code(t, errOf(e.svc.ListWorkspaces(e.ctx, ann, SourceAPI, "bad name!"))); c != api.CodeBadRequest {
		t.Fatalf("bad fleet: %q", c)
	}
	rows := e.wsAudit(api.MethodWorkspaceList)
	if len(rows) != before+2 || rows[before].Outcome != OutcomeOK || rows[before].Actor != "ann" || rows[before+1].Fleet != "qa" {
		t.Fatalf("audit: %+v", rows)
	}
	// Fail closed when the audit row cannot be written.
	e.svc.st = &failAudit{Store: e.svc.st, outcome: OutcomeOK}
	if c := code(t, errOf(e.svc.ListWorkspaces(e.ctx, ann, SourceAPI, ""))); c != api.CodeUnavailable {
		t.Fatalf("list without audit: %q", c)
	}
}

func TestWorkspaceOpsDisabledRunnerRefusesExecAndWrite(t *testing.T) {
	e, op, w := opsEnv(t, false, nil)
	// A disabled runner still serves read (M3.md 1.5); create, exec and write are refused.
	e.live.wsFn = func(ctx context.Context, id int64, req transport.WorkspaceRequest, b io.Reader, o io.Writer) (*transport.WorkspaceResult, error) {
		if req.Op == transport.OpRead {
			return fileOK(ctx, id, req, b, o)
		}
		return nil, registry.ErrDisabled
	}
	b := []byte("x")
	if c := code(t, errOf(e.svc.ExecWorkspace(e.ctx, op, SourceAPI, w.WsID, []string{"true"}, "", nil, 0, nil, 0, ""))); c != api.CodeUnavailable {
		t.Errorf("exec: %q", c)
	}
	if c := code(t, errOf(e.svc.WriteWorkspaceFile(e.ctx, op, SourceAPI, w.WsID, "f", 0, false, 0, 1, sum(b), bytes.NewReader(b), ""))); c != api.CodeUnavailable {
		t.Errorf("write: %q", c)
	}
	// No touch on a refused op: the workspace is not "used".
	if g, _ := e.db.GetWorkspace(e.ctx, w.WsID); !g.LastUsedAt.Equal(w.LastUsedAt) {
		t.Fatalf("last_used moved to %v", g.LastUsedAt)
	}
	if _, err := e.svc.ReadWorkspaceFile(e.ctx, op, SourceAPI, w.WsID, "f", 0, 0, io.Discard); err != nil {
		t.Errorf("read on disabled runner: %v", err)
	}
}

// execMarginForTest shortens the central-side exec deadline margin for one test.
func execMarginForTest(d time.Duration) func() {
	old := execMargin
	execMargin = d
	return func() { execMargin = old }
}

// fileOK answers write with a size and exec with exit 0.
func fileOK(ctx context.Context, id int64, req transport.WorkspaceRequest, b io.Reader, o io.Writer) (*transport.WorkspaceResult, error) {
	switch req.Op {
	case transport.OpWrite:
		return &transport.WorkspaceResult{Code: transport.CodeOK, File: &transport.WorkspaceFile{Size: req.Length}}, nil
	case transport.OpRead:
		return &transport.WorkspaceResult{Code: transport.CodeOK, File: &transport.WorkspaceFile{}}, nil
	}
	return wsExecOK(0)(ctx, id, req, b, o)
}
