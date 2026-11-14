package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/central/registry"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
)

var execMargin = 15 * time.Second // added to the exec timeout for the central-side deadline (a var for tests)

const (
	// fileOpTimeout bounds read_file and write_file (M3.md 1.9).
	fileOpTimeout = 60 * time.Second
	// maxAuditArgv is the clip of the argv JSON in the audit row (M3.md 1.8).
	maxAuditArgv = 4 << 10
	// maxAuditEnvNames bounds the audited env name list; at most MaxEnvVars (32) names of
	// at most 64 bytes each exist, so this is only a safety net.
	maxAuditEnvNames = 4 << 10
)

// wsTarget loads the workspace for an owner-only op: policy (active operator), ownership
// (not_found for non-owners and unknown ids alike), state open (else conflict) and the fleet with
// its workspace config.
func (s *Service) wsTarget(ctx context.Context, p auth.Principal, src Source, method, wsID string) (store.Workspace, store.Fleet, store.WorkspaceConfig, error) {
	if err := s.requireOperator(ctx, p, src, method); err != nil {
		return store.Workspace{}, store.Fleet{}, store.WorkspaceConfig{}, err
	}
	w, err := s.ownedWorkspace(ctx, p, src, method, wsID)
	if err != nil {
		return store.Workspace{}, store.Fleet{}, store.WorkspaceConfig{}, err
	}
	if w.State != store.WorkspaceOpen {
		return store.Workspace{}, store.Fleet{}, store.WorkspaceConfig{}, api.Errorf(api.CodeConflict, "workspace is %s", w.State)
	}
	f, err := s.st.GetFleet(ctx, w.Fleet)
	if err != nil {
		return store.Workspace{}, store.Fleet{}, store.WorkspaceConfig{}, s.fail("get fleet", err)
	}
	var cfg store.WorkspaceConfig
	if f.Workspace != nil {
		cfg = *f.Workspace
	} else if cfg, err = s.st.GetFleetWorkspace(ctx, w.Fleet); err != nil {
		return store.Workspace{}, store.Fleet{}, store.WorkspaceConfig{}, s.fail("get fleet workspace", err)
	}
	return w, f, cfg, nil
}

// denyConfirm audits a refused confirm (no params) and returns bad_request.
func (s *Service) denyConfirm(ctx context.Context, p auth.Principal, src Source, method string, w store.Workspace) error {
	den := s.wsEvent(p, src, method, w.Fleet, w.Runner, w.WsID, "")
	den.Subject, den.Outcome, den.Message = w.Session, OutcomeDenied, "confirm missing or wrong"
	s.recordBestEffort(ctx, den)
	return badRequest(api.MsgConfirmRequired)
}

// refuseInput audits a refused request (message only, never values) and returns bad_request.
func (s *Service) refuseInput(ctx context.Context, p auth.Principal, src Source, method string, w store.Workspace, msg string) error {
	den := s.wsEvent(p, src, method, w.Fleet, w.Runner, w.WsID, "")
	den.Subject, den.Outcome, den.Message = w.Session, OutcomeDenied, clip(msg, 200)
	s.recordBestEffort(ctx, den)
	return badRequest("%s", clip(msg, 200))
}

// classify maps the outcome of a Live.Workspace call (other than a successful result) to the
// audit outcome/message and the operator-facing error. It returns retErr == nil only for a
// result with code ok. dctx is the call's deadline context, ctx the request's.
func classify(ctx, dctx context.Context, res *transport.WorkspaceResult, lerr error, logf func(string, ...any)) (outcome, msg string, retErr error) {
	if lerr != nil {
		var ke *registry.KickError
		switch {
		case errors.As(lerr, &ke):
			return OutcomeCancelled, ke.Reason, api.Errorf(api.CodeUnavailable, "%s", ke.Reason)
		case errors.Is(lerr, registry.ErrDisabled):
			return OutcomeError, "runner is disabled", api.Errorf(api.CodeUnavailable, "runner is disabled")
		case errors.Is(lerr, registry.ErrPending):
			return OutcomeError, "runner is pending approval", api.Errorf(api.CodeUnavailable, "runner is pending approval")
		case errors.Is(lerr, registry.ErrBusy):
			return OutcomeError, "runner busy", api.Errorf(api.CodeUnavailable, "runner busy")
		case errors.Is(lerr, registry.ErrNoExecutor):
			return OutcomeError, "runner has no workspace executor", api.Errorf(api.CodeUnavailable, "runner has no workspace executor")
		case errors.Is(dctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil:
			return OutcomeTimeout, "timed out", api.Errorf(api.CodeTimeout, "workspace operation timed out")
		case ctx.Err() != nil:
			return OutcomeCancelled, "request cancelled", api.Errorf(api.CodeUnavailable, "request cancelled")
		}
		logf("service: workspace call failed: %v", lerr)
		return OutcomeError, "runner unavailable", api.Errorf(api.CodeUnavailable, "runner unavailable")
	}
	if res == nil {
		return OutcomeError, "empty result", api.Internal(errors.New("nil workspace result"))
	}
	switch res.Code {
	case transport.CodeOK:
		return OutcomeOK, "", nil
	case transport.CodeBusy:
		return OutcomeError, "runner busy", api.Errorf(api.CodeUnavailable, "runner busy")
	case transport.CodeTimeout:
		return OutcomeTimeout, "timed out", api.Errorf(api.CodeTimeout, "timed out")
	}
	m := clip(res.Message, 200)
	return OutcomeError, "runner: " + res.Code + ": " + m, runnerResultError(res.Code, m)
}

// touch records activity on the workspace; failures are only logged.
func (s *Service) touch(ctx context.Context, wsID string) {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), outcomeWriteTimeout)
	defer cancel()
	if err := s.st.TouchWorkspace(wctx, wsID, s.now()); err != nil {
		s.logf("service: touch workspace %s: %v", wsID, err)
	}
}

// startRow writes the start audit row, failing closed.
func (s *Service) startRow(ctx context.Context, ev store.AuditEvent) error {
	if _, err := s.st.RecordAudit(ctx, ev); err != nil {
		s.logf("service: start audit for %s failed, refusing %s: %v", ev.RequestID, ev.Action, err)
		return api.Errorf(api.CodeUnavailable, "audit unavailable")
	}
	return nil
}

// ExecWorkspace runs argv in the caller's workspace (M3.md 1.1, 1.8, 1.9). Checks, in order:
// policy (active operator), ownership (not_found otherwise), state open (else conflict), input
// (argv, cwd, env, stdin length), guarded confirm, then the timeout is derived from the fleet's
// exec_default/exec_max (api.WorkspaceExecTimeout). The env must pass workspace.FilterEnv with the
// hard denylist: a denied or malformed name is refused as bad_request naming the variable, never
// its value. A "start" audit row (argv JSON clipped to 4 KiB, cwd, env NAMES only, stdin byte
// count) is written BEFORE dispatch (fail closed); the outcome row (ok/error/cancelled/timeout)
// has the same request id. Runner status gating (disabled, pending) is the registry's. stdin
// supplies stdinLen bytes when stdinLen > 0.
func (s *Service) ExecWorkspace(ctx context.Context, p auth.Principal, src Source, wsID string, argv []string, cwd string,
	env map[string]string, timeoutS int, stdin io.Reader, stdinLen int64, confirm string) (api.WorkspaceExecResult, error) {

	w, f, cfg, err := s.wsTarget(ctx, p, src, api.MethodWorkspaceExec, wsID)
	if err != nil {
		return api.WorkspaceExecResult{}, err
	}
	if e := api.ValidateArgv(argv); e != nil {
		return api.WorkspaceExecResult{}, e
	}
	cleanCwd := ""
	if cwd != "" {
		if cleanCwd, err = workspace.CleanPath(cwd); err != nil {
			return api.WorkspaceExecResult{}, badRequest("%v", err)
		}
	}
	names := make([]string, 0, len(env))
	for k := range env {
		names = append(names, k)
	}
	sort.Strings(names)
	// Exactly the requested names are "allowed" here: the fleet/runner env_allow is the runner's
	// job; central enforces the shape limits and the denylist, which wins over any allowlist.
	fenv, err := workspace.FilterEnv(env, names)
	if err != nil {
		return api.WorkspaceExecResult{}, s.refuseInput(ctx, p, src, api.MethodWorkspaceExec, w, err.Error())
	}
	deferred, _ := stdin.(DeferredBody)
	if stdinLen == StdinDeferred {
		if deferred == nil {
			return api.WorkspaceExecResult{}, badRequest("invalid stdin length")
		}
	} else if stdinLen < 0 || stdinLen > api.MaxFileChunk || (stdinLen > 0 && stdin == nil) {
		return api.WorkspaceExecResult{}, badRequest("invalid stdin length")
	}
	if f.Confirm && confirm != f.Name {
		return api.WorkspaceExecResult{}, s.denyConfirm(ctx, p, src, api.MethodWorkspaceExec, w)
	}
	timeout, terr := api.WorkspaceExecTimeout(timeoutS, int(cfg.ExecDefault/time.Second), int(cfg.ExecMax/time.Second))
	if terr != nil {
		return api.WorkspaceExecResult{}, terr
	}
	if stdinLen == StdinDeferred {
		// Every gating check has passed: only now may the caller read the body from the wire.
		var berr error
		if stdin, stdinLen, berr = deferred.Body(); berr != nil {
			return api.WorkspaceExecResult{}, berr
		}
		if stdinLen < 0 || stdinLen > api.MaxFileChunk || (stdinLen > 0 && stdin == nil) {
			return api.WorkspaceExecResult{}, badRequest("invalid stdin length")
		}
	}
	rid, err := newRequestID()
	if err != nil {
		return api.WorkspaceExecResult{}, s.fail("request id", err)
	}

	argvJSON, err := json.Marshal(argv)
	if err != nil {
		return api.WorkspaceExecResult{}, s.fail("encode argv", err)
	}
	params := map[string]string{"argv": clip(string(argvJSON), maxAuditArgv)}
	if len(argvJSON) > maxAuditArgv {
		params["argv_clipped"] = "true"
	}
	if cleanCwd != "" {
		params["cwd"] = clip(cleanCwd, 512)
	}
	if len(names) > 0 {
		params["env_names"] = clip(strings.Join(names, ","), maxAuditEnvNames)
	}
	if stdinLen > 0 {
		params["stdin_bytes"] = strconv.FormatInt(stdinLen, 10)
	}
	params["timeout_s"] = strconv.Itoa(int(timeout / time.Second))

	start := s.now()
	ev := s.wsEvent(p, src, api.MethodWorkspaceExec, w.Fleet, w.Runner, w.WsID, rid)
	ev.Time, ev.Subject, ev.Outcome, ev.Params = start, w.Session, OutcomeStart, params
	if err := s.startRow(ctx, ev); err != nil {
		return api.WorkspaceExecResult{}, err
	}

	s.touch(ctx, w.WsID)
	defer s.beginOp(w.WsID)()
	dctx, cancel := context.WithTimeout(ctx, timeout+execMargin)
	defer cancel()
	res, lerr := s.live.Workspace(dctx, w.RunnerID, transport.WorkspaceRequest{Op: transport.OpExec, ID: w.WsID,
		Argv: argv, Cwd: cleanCwd, Env: fenv, StdinLen: stdinLen, TimeoutMs: timeout.Milliseconds()}, stdin, nil)

	end := ev
	end.Time = s.now()
	end.Params = nil
	end.DurationMS = end.Time.Sub(start).Milliseconds()
	outcome, msg, retErr := classify(ctx, dctx, res, lerr, s.logf)
	var out api.WorkspaceExecResult
	if retErr == nil && res.Exec == nil {
		outcome, msg, retErr = OutcomeError, "runner sent no exec result", api.Internal(errors.New("exec result missing"))
	}
	if retErr == nil {
		x := res.Exec
		code := x.ExitCode
		end.ExitCode = &code
		if x.DurationMs > 0 {
			end.DurationMS = x.DurationMs
		}
		if x.TimedOut {
			outcome, msg = OutcomeTimeout, "timed out"
		}
		out = api.WorkspaceExecResult{ExitCode: x.ExitCode, Stdout: x.Stdout, Stderr: x.Stderr,
			Truncated: x.Truncated, TimedOut: x.TimedOut, DurationMS: end.DurationMS, RequestID: rid}
	}
	end.Outcome, end.Message = outcome, msg
	s.recordBestEffort(ctx, end)
	if res != nil && lerr == nil {
		s.touch(ctx, w.WsID)
	}
	if retErr != nil {
		return api.WorkspaceExecResult{}, retErr
	}
	return out, nil
}

// countWriter counts the bytes written through it.
type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	c.n += int64(n)
	return n, err
}

// capBuffer collects at most max bytes and fails the write beyond that.
type capBuffer struct {
	b   []byte
	max int
}

func (c *capBuffer) Write(p []byte) (int, error) {
	if len(c.b)+len(p) > c.max {
		return 0, errors.New("read reply exceeds the chunk limit")
	}
	c.b = append(c.b, p...)
	return len(p), nil
}

// ReadWorkspaceFile reads up to limit bytes (0 or above api.MaxFileChunk means MaxFileChunk) of
// path from offset and streams them to out. The result's Length is the number of bytes written to
// out. Owner only, workspace open; the runner's status gating is the registry's. The audit rows
// carry the path and byte count, never content.
func (s *Service) ReadWorkspaceFile(ctx context.Context, p auth.Principal, src Source, wsID, path string, offset int64, limit int,
	out io.Writer) (api.WorkspaceReadFileResult, error) {

	w, _, _, err := s.wsTarget(ctx, p, src, api.MethodWorkspaceReadFile, wsID)
	if err != nil {
		return api.WorkspaceReadFileResult{}, err
	}
	clean, err := cleanFilePath(path)
	if err != nil {
		return api.WorkspaceReadFileResult{}, err
	}
	if offset < 0 || limit < 0 {
		return api.WorkspaceReadFileResult{}, badRequest("offset and limit must be >= 0")
	}
	if limit == 0 || limit > api.MaxFileChunk {
		limit = api.MaxFileChunk
	}
	if out == nil {
		out = io.Discard
	}
	// C-17: the reply is buffered (at most limit <= MaxFileChunk bytes) and checked against the
	// runner's stated length BEFORE anything reaches out, so an inconsistent reply sends nothing.
	buf := &capBuffer{max: limit}
	rid, err := newRequestID()
	if err != nil {
		return api.WorkspaceReadFileResult{}, s.fail("request id", err)
	}
	params := map[string]string{"path": clip(clean, 512), "offset": strconv.FormatInt(offset, 10), "limit": strconv.Itoa(limit)}
	start := s.now()
	ev := s.wsEvent(p, src, api.MethodWorkspaceReadFile, w.Fleet, w.Runner, w.WsID, rid)
	ev.Time, ev.Subject, ev.Outcome, ev.Params = start, w.Session, OutcomeStart, params
	if err := s.startRow(ctx, ev); err != nil {
		return api.WorkspaceReadFileResult{}, err
	}

	s.touch(ctx, w.WsID)
	defer s.beginOp(w.WsID)()
	dctx, cancel := context.WithTimeout(ctx, fileOpTimeout)
	defer cancel()
	cw := &countWriter{w: buf}
	res, lerr := s.live.Workspace(dctx, w.RunnerID, transport.WorkspaceRequest{Op: transport.OpRead, ID: w.WsID,
		Path: clean, Offset: offset, Limit: int64(limit), TimeoutMs: fileOpTimeout.Milliseconds()}, nil, cw)

	end := ev
	end.Time = s.now()
	end.DurationMS = end.Time.Sub(start).Milliseconds()
	outcome, msg, retErr := classify(ctx, dctx, res, lerr, s.logf)
	var result api.WorkspaceReadFileResult
	if retErr == nil && (res.File == nil || res.File.Length != cw.n) {
		outcome, msg, retErr = OutcomeError, "runner read result inconsistent", api.Internal(errors.New("read result inconsistent"))
	}
	if retErr == nil {
		if _, werr := out.Write(buf.b); werr != nil {
			outcome, msg, retErr = OutcomeError, "write to caller failed", api.Internal(werr)
		}
	}
	if retErr == nil {
		f := res.File
		result = api.WorkspaceReadFileResult{Size: f.Size, Offset: offset, Length: int(f.Length), Mode: uint32(f.Mode), EOF: f.EOF}
	}
	end.Params = map[string]string{"path": params["path"], "bytes": strconv.FormatInt(cw.n, 10)}
	end.Outcome, end.Message = outcome, msg
	s.recordBestEffort(ctx, end)
	if res != nil && lerr == nil {
		s.touch(ctx, w.WsID)
	}
	if retErr != nil {
		return api.WorkspaceReadFileResult{}, retErr
	}
	return result, nil
}

func cleanFilePath(path string) (string, error) {
	if path == "" {
		return "", badRequest("path required")
	}
	clean, err := workspace.CleanPath(path)
	if err != nil {
		return "", badRequest("%v", err)
	}
	return clean, nil
}

// WriteWorkspaceFile writes length bytes from body to path (M3.md 1.1). Checks, in order: policy,
// ownership, state open, guarded confirm, path, offset, mode (api.ValidateFileMode, <= 0777), length
// (<= api.MaxFileChunk) and sha256 shape.
//
// Integrity: the body (at most 1 MiB, so buffering is cheap) is read fully into memory and its
// SHA-256 compared with sha256hex BEFORE anything is sent to the runner. A mismatch (or a short
// body) is refused as bad_request and the runner is never contacted, so a corrupted chunk can
// never be committed. Only then is the start audit row written (fail closed) and the verified
// bytes streamed. The body is read only after the gating checks pass. Audit rows carry the path,
// byte count and sha256, never content.
func (s *Service) WriteWorkspaceFile(ctx context.Context, p auth.Principal, src Source, wsID, path string, offset int64, truncate bool,
	mode uint32, length int, sha256hex string, body io.Reader, confirm string) (api.WorkspaceWriteFileResult, error) {

	w, f, _, err := s.wsTarget(ctx, p, src, api.MethodWorkspaceWriteFile, wsID)
	if err != nil {
		return api.WorkspaceWriteFileResult{}, err
	}
	if f.Confirm && confirm != f.Name {
		return api.WorkspaceWriteFileResult{}, s.denyConfirm(ctx, p, src, api.MethodWorkspaceWriteFile, w)
	}
	clean, err := cleanFilePath(path)
	if err != nil {
		return api.WorkspaceWriteFileResult{}, err
	}
	if offset < 0 {
		return api.WorkspaceWriteFileResult{}, badRequest("offset must be >= 0")
	}
	if e := api.ValidateFileMode(mode); e != nil {
		return api.WorkspaceWriteFileResult{}, e
	}
	if length < 0 || length > api.MaxFileChunk {
		return api.WorkspaceWriteFileResult{}, badRequest("length must be 0..%d", api.MaxFileChunk)
	}
	want, err := hex.DecodeString(sha256hex)
	if err != nil || len(want) != sha256.Size || sha256hex != strings.ToLower(sha256hex) {
		return api.WorkspaceWriteFileResult{}, badRequest("sha256 must be 64 lowercase hex characters")
	}
	if length > 0 && body == nil {
		return api.WorkspaceWriteFileResult{}, badRequest("body required")
	}
	rid, err := newRequestID()
	if err != nil {
		return api.WorkspaceWriteFileResult{}, s.fail("request id", err)
	}
	params := map[string]string{"path": clip(clean, 512), "bytes": strconv.Itoa(length), "sha256": sha256hex,
		"offset": strconv.FormatInt(offset, 10)}
	if truncate {
		params["truncate"] = "true"
	}
	if mode != 0 {
		params["mode"] = strconv.FormatUint(uint64(mode), 8)
	}

	// Read and verify before anything else happens. A failed check is audited as one error row.
	buf := make([]byte, length)
	if length > 0 {
		if _, rerr := io.ReadFull(body, buf); rerr != nil {
			return api.WorkspaceWriteFileResult{}, s.writeRefused(ctx, p, src, w, rid, params, "body shorter than length")
		}
	}
	if got := sha256.Sum256(buf); subtle.ConstantTimeCompare(got[:], want) != 1 {
		return api.WorkspaceWriteFileResult{}, s.writeRefused(ctx, p, src, w, rid, params, "sha256 mismatch")
	}

	start := s.now()
	ev := s.wsEvent(p, src, api.MethodWorkspaceWriteFile, w.Fleet, w.Runner, w.WsID, rid)
	ev.Time, ev.Subject, ev.Outcome, ev.Params = start, w.Session, OutcomeStart, params
	if err := s.startRow(ctx, ev); err != nil {
		return api.WorkspaceWriteFileResult{}, err
	}

	s.touch(ctx, w.WsID)
	defer s.beginOp(w.WsID)()
	dctx, cancel := context.WithTimeout(ctx, fileOpTimeout)
	defer cancel()
	res, lerr := s.live.Workspace(dctx, w.RunnerID, transport.WorkspaceRequest{Op: transport.OpWrite, ID: w.WsID,
		Path: clean, Offset: offset, Truncate: truncate, Mode: int64(mode), Length: int64(length),
		TimeoutMs: fileOpTimeout.Milliseconds()}, bytes.NewReader(buf), nil)

	end := ev
	end.Time = s.now()
	end.DurationMS = end.Time.Sub(start).Milliseconds()
	outcome, msg, retErr := classify(ctx, dctx, res, lerr, s.logf)
	var result api.WorkspaceWriteFileResult
	if retErr == nil && res.File == nil {
		outcome, msg, retErr = OutcomeError, "runner sent no file result", api.Internal(errors.New("write result missing"))
	}
	if retErr == nil {
		result = api.WorkspaceWriteFileResult{Size: res.File.Size}
	}
	end.Outcome, end.Message = outcome, msg
	s.recordBestEffort(ctx, end)
	if res != nil && lerr == nil {
		s.touch(ctx, w.WsID)
	}
	if retErr != nil {
		return api.WorkspaceWriteFileResult{}, retErr
	}
	return result, nil
}

// writeRefused audits a write refused before dispatch (one error row, never content).
func (s *Service) writeRefused(ctx context.Context, p auth.Principal, src Source, w store.Workspace, rid string, params map[string]string, msg string) error {
	ev := s.wsEvent(p, src, api.MethodWorkspaceWriteFile, w.Fleet, w.Runner, w.WsID, rid)
	ev.Subject, ev.Outcome, ev.Message, ev.Params = w.Session, OutcomeError, msg, params
	s.recordBestEffort(ctx, ev)
	return badRequest("%s", msg)
}

// ListWorkspaces returns the caller's own live workspaces (opening, open, closing), optionally of
// one fleet. It is audited with one row (M3.md 1.8); if that row cannot be written the call fails
// closed with unavailable.
func (s *Service) ListWorkspaces(ctx context.Context, p auth.Principal, src Source, fleet string) ([]store.Workspace, error) {
	if err := s.requireOperator(ctx, p, src, api.MethodWorkspaceList); err != nil {
		return nil, err
	}
	if fleet != "" && !store.ValidFleetName(fleet) {
		return nil, badRequest("invalid fleet name")
	}
	ws, err := s.st.ListWorkspaces(ctx, store.WorkspaceFilter{Fleet: fleet, OperatorID: p.ID,
		States: []store.WorkspaceState{store.WorkspaceOpening, store.WorkspaceOpen, store.WorkspaceClosing}, Limit: 1000})
	if err != nil {
		return nil, s.fail("list workspaces", err)
	}
	ev := s.event(p, src, api.MethodWorkspaceList)
	ev.Fleet, ev.Message = fleet, strconv.Itoa(len(ws))+" workspaces"
	if _, err := s.st.RecordAudit(ctx, ev); err != nil {
		s.logf("service: audit of workspace list failed: %v", err)
		return nil, api.Errorf(api.CodeUnavailable, "audit unavailable")
	}
	return ws, nil
}
