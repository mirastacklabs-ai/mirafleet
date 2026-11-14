// Package api defines the wire protocol of api@mirafleet (operators) and the local admin
// socket: length-prefixed JSON frames, one request per channel. Pure Go, no unix-only calls.
package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
)

// Version is the protocol version; SupportedVersions lists every version this build speaks.
const Version = 1

var SupportedVersions = []int{1}

// Limits (docs/M2.md §1.4).
const (
	MaxRequestBytes = 64 << 10
	// MaxResultBytes mirrors actions.MaxOutput (stdout+stderr combined); not imported to keep
	// this package dependency-free.
	MaxResultBytes   = 1 << 20
	MaxResponseBytes = MaxResultBytes + 64<<10

	ListTimeout          = 10 * time.Second
	DefaultActionTimeout = 2 * time.Minute
	MaxActionTimeout     = 10 * time.Minute

	MaxChannelsPerConn = 8

	MaxParams     = 32
	MaxParamKey   = 64
	MaxParamValue = 4 << 10

	maxIDLen = 128
)

// RequestTimeout is how long a peer has to deliver a complete request. Variable so tests can
// shorten it.
var RequestTimeout = 10 * time.Second

// Method names.
const (
	// Operator methods (api@mirafleet).
	MethodHello       = "hello"
	MethodFleetsList  = "fleets.list"
	MethodRunnersList = "runners.list"
	MethodActionRun   = "action.run"

	// Workspace operator methods (api@mirafleet; docs/M3.md §1.1).
	MethodWorkspaceOpen      = "workspace.open"
	MethodWorkspaceExec      = "workspace.exec"
	MethodWorkspaceReadFile  = "workspace.read_file"
	MethodWorkspaceWriteFile = "workspace.write_file"
	MethodWorkspaceClose     = "workspace.close"
	MethodWorkspaceList      = "workspace.list"

	// Workspace admin methods (admin socket only).
	MethodFleetWorkspaceSet  = "fleet.workspace_set"
	MethodWorkspaceAdminList = "workspace.admin_list"
	MethodWorkspaceKill      = "workspace.kill"

	// Admin-only methods (local admin socket / GUI service; never on api@mirafleet).
	MethodFleetCreate          = "fleet.create"
	MethodTokenCreate          = "token.create"
	MethodTokenList            = "token.list"
	MethodTokenRevoke          = "token.revoke"
	MethodRunnerLifecycle      = "runner.lifecycle"
	MethodOperatorCode         = "operator.code"
	MethodOperatorRotateSecret = "operator.rotate_secret"
	MethodOperatorList         = "operator.list"
	MethodOperatorRevoke       = "operator.revoke"
	MethodAuditList            = "audit.list"

	// Enrol methods (join@mirafleet / register@mirafleet, user "enroll" only).
	MethodEnrollJoin     = "enroll.join"
	MethodEnrollRegister = "enroll.register"
)

// IsEnrollMethod reports whether m is routable on the enroll channels. Disjoint from
// IsOperatorMethod and IsAdminMethod.
func IsEnrollMethod(m string) bool {
	switch m {
	case MethodEnrollJoin, MethodEnrollRegister:
		return true
	}
	return false
}

// EnrollJoinParams is the enroll.join request (channel join@mirafleet).
type EnrollJoinParams struct {
	Token  string            `json:"token"`
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
}

// EnrollJoinResult reports the runner's resulting status ("pending" or "active").
type EnrollJoinResult struct {
	Fleet  string `json:"fleet"`
	Runner string `json:"runner"`
	Status string `json:"status"`
}

// EnrollRegisterParams is the enroll.register request (channel register@mirafleet).
type EnrollRegisterParams struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

// EnrollRegisterResult reports the operator's resulting status ("active").
type EnrollRegisterResult struct {
	Operator string `json:"operator"`
	Status   string `json:"status"`
}

// IsOperatorMethod reports whether m is routable on api@mirafleet. Backed by a switch so the
// set is immutable at runtime.
func IsOperatorMethod(m string) bool {
	switch m {
	case MethodHello, MethodFleetsList, MethodRunnersList, MethodActionRun,
		MethodWorkspaceOpen, MethodWorkspaceExec, MethodWorkspaceReadFile,
		MethodWorkspaceWriteFile, MethodWorkspaceClose, MethodWorkspaceList:
		return true
	}
	return false
}

// IsAdminMethod reports whether m is routable only on the admin socket / GUI service. Disjoint
// from IsOperatorMethod.
func IsAdminMethod(m string) bool {
	switch m {
	case MethodFleetCreate, MethodTokenCreate, MethodTokenList, MethodTokenRevoke,
		MethodRunnerLifecycle, MethodOperatorCode, MethodOperatorRotateSecret,
		MethodOperatorList, MethodOperatorRevoke, MethodAuditList,
		MethodFleetWorkspaceSet, MethodWorkspaceAdminList, MethodWorkspaceKill:
		return true
	}
	return false
}

// Error codes.
const (
	CodeBadRequest         = "bad_request"
	CodeUnauthenticated    = "unauthenticated"
	CodeForbidden          = "forbidden"
	CodeNotFound           = "not_found"
	CodeConflict           = "conflict"
	CodeUnavailable        = "unavailable"
	CodeTooLarge           = "too_large"
	CodeTimeout            = "timeout"
	CodeUnsupportedVersion = "unsupported_version"
	CodeInternal           = "internal"
)

// Error is the wire error and a Go error.
type Error struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Supported []int  `json:"supported,omitempty"` // set for unsupported_version
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Errorf builds an Error with the given code.
func Errorf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Internal returns a generic internal error; the cause is deliberately dropped so it can never
// reach the peer. Callers log err server-side.
func Internal(_ error) *Error { return &Error{Code: CodeInternal, Message: "internal error"} }

// AsError converts any error to a wire Error: nil stays nil, *Error passes through, anything
// else becomes a generic internal error (no message leak).
func AsError(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return Internal(err)
}

// Request is one API call.
type Request struct {
	V      int             `json:"v"`
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Response carries exactly one of Result or Error.
type Response struct {
	V      int             `json:"v"`
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

// NewResult builds a success response.
func NewResult(id string, result any) (*Response, error) {
	b, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	if len(b) == 0 || string(b) == "null" {
		b = []byte("{}")
	}
	return &Response{V: Version, ID: id, Result: b}, nil
}

// NewError builds an error response.
func NewError(id string, e *Error) *Response {
	return &Response{V: Version, ID: id, Error: e}
}

// Marshal encodes the response, enforcing exactly one of result/error.
func (r *Response) Marshal() ([]byte, error) {
	if (r.Error == nil) == (len(r.Result) == 0) {
		return nil, errors.New("api: response must carry exactly one of result or error")
	}
	return json.Marshal(r)
}

// WriteTimeout bounds WriteResponse on writers that support SetWriteDeadline. Variable so
// tests can shorten it.
var WriteTimeout = 10 * time.Second

type writeDeadliner interface{ SetWriteDeadline(time.Time) error }

// WriteResponse marshals and frames a response. An oversized response is replaced by a
// too_large error so the peer still gets an answer. When w supports SetWriteDeadline (net.Conn)
// the write is bounded by WriteTimeout and the deadline cleared afterwards. ssh.Channel has no
// write deadline: callers on channels must rely on the request context / closing the channel or
// connection to unblock a stalled write.
func WriteResponse(w io.Writer, r *Response) error {
	b, err := r.Marshal()
	if err != nil {
		return err
	}
	if len(b) > MaxResponseBytes {
		b, err = NewError(r.ID, Errorf(CodeTooLarge, "response exceeds %d bytes", MaxResponseBytes)).Marshal()
		if err != nil {
			return err
		}
	}
	if d, ok := w.(writeDeadliner); ok {
		if e := d.SetWriteDeadline(time.Now().Add(WriteTimeout)); e == nil {
			defer d.SetWriteDeadline(time.Time{})
		}
	}
	return WriteFrame(w, b)
}

// decodeStrict decodes exactly one JSON value into v, rejecting unknown fields and any trailing
// data (including a stray closing brace, which Decoder.More does not detect).
func decodeStrict(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data")
	}
	return nil
}

// DecodeParams strictly decodes a request's params into v (a pointer to a params struct):
// unknown fields and trailing data are bad_request. Missing or null params are allowed and leave
// v untouched (zero value), so methods without params (and optional params) need no special case;
// handlers whose params are mandatory must validate the fields they need.
func DecodeParams(raw json.RawMessage, v any) *Error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := decodeStrict(raw, v); err != nil {
		return Errorf(CodeBadRequest, "invalid params")
	}
	return nil
}

// ParseRequest decodes and validates one request payload. The returned error is always *Error.
// req is non-nil only on success; use PeekID for the id of an error reply.
func ParseRequest(payload []byte) (*Request, *Error) {
	var head struct {
		V json.RawMessage `json:"v"`
	}
	if err := json.Unmarshal(payload, &head); err != nil {
		return nil, Errorf(CodeBadRequest, "malformed request")
	}
	var v int
	if len(head.V) == 0 || string(head.V) == "null" || json.Unmarshal(head.V, &v) != nil {
		return nil, Errorf(CodeBadRequest, "missing or invalid protocol version")
	}
	if v != Version {
		return nil, &Error{Code: CodeUnsupportedVersion,
			Message:   fmt.Sprintf("unsupported protocol version %d; supported: %v", v, SupportedVersions),
			Supported: SupportedVersions}
	}
	var req Request
	if err := decodeStrict(payload, &req); err != nil {
		return nil, Errorf(CodeBadRequest, "malformed request")
	}
	if req.ID == "" || len(req.ID) > maxIDLen {
		return nil, Errorf(CodeBadRequest, "id must be 1-%d bytes", maxIDLen)
	}
	if req.Method == "" {
		return nil, Errorf(CodeBadRequest, "method required")
	}
	return &req, nil
}

// PeekID best-effort extracts the id of a payload, for error replies. Returns "" if absent.
func PeekID(payload []byte) string {
	var p struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(payload, &p)
	if len(p.ID) > maxIDLen {
		return ""
	}
	return p.ID
}

type readDeadliner interface{ SetReadDeadline(time.Time) error }

// ReadRequest reads one framed request, applying the RequestTimeout arrival deadline via
// SetReadDeadline (cleared on success). It only works for readers that support deadlines
// (net.Conn); a reader without SetReadDeadline gets NO time limit. ssh.Channel is such a reader:
// every api@ / enroll handler MUST use ReadRequestTimeout instead.
// The error, when non-nil, is an *Error suitable for the peer, except io.EOF for a peer that
// closed without sending anything. id is the best-effort request id for error replies.
func ReadRequest(r io.Reader) (req *Request, id string, err error) {
	return readRequest(r, nil)
}

// ReadRequestTimeout is ReadRequest for readers without a read deadline (ssh.Channel). It arms a
// RequestTimeout timer that closes closer (typically the channel) to unblock the read, stops the
// timer on completion, and reports a timeout *Error if the timer fired. If r also supports
// SetReadDeadline that is used too. After a timeout closer has been closed, so the error reply
// may not be deliverable; callers should just tear down. Every api@ / enroll handler must use
// this function.
func ReadRequestTimeout(r io.Reader, closer io.Closer) (req *Request, id string, err error) {
	return readRequest(r, closer)
}

func readRequest(r io.Reader, closer io.Closer) (req *Request, id string, err error) {
	if d, ok := r.(readDeadliner); ok {
		if e := d.SetReadDeadline(time.Now().Add(RequestTimeout)); e == nil {
			defer d.SetReadDeadline(time.Time{})
		}
	}
	var t *time.Timer
	if closer != nil {
		t = time.AfterFunc(RequestTimeout, func() { closer.Close() })
	}
	payload, ferr := ReadFrame(r, MaxRequestBytes)
	// Stop right after the read: if it reports false the timer already fired (or is firing), so
	// the closer may be closed and the read must count as timed out. If true, the timer can
	// never close the channel after this function returns a request.
	if t != nil && !t.Stop() {
		return nil, "", Errorf(CodeTimeout, "request not received within %s", RequestTimeout)
	}
	switch {
	case ferr == nil:
	case ferr == io.EOF:
		return nil, "", io.EOF
	case errors.Is(ferr, ErrFrameTooLarge):
		return nil, "", Errorf(CodeTooLarge, "request exceeds %d bytes", MaxRequestBytes)
	case errors.Is(ferr, ErrEmptyFrame):
		return nil, "", Errorf(CodeBadRequest, "empty request")
	default:
		var ne net.Error
		if errors.As(ferr, &ne) && ne.Timeout() {
			return nil, "", Errorf(CodeTimeout, "request not received within %s", RequestTimeout)
		}
		return nil, "", Errorf(CodeBadRequest, "truncated request")
	}
	req, perr := ParseRequest(payload)
	if perr != nil {
		return nil, PeekID(payload), perr
	}
	return req, req.ID, nil
}

// ValidateActionParams enforces the param count and size limits.
func ValidateActionParams(p map[string]string) *Error {
	if len(p) > MaxParams {
		return Errorf(CodeBadRequest, "at most %d params", MaxParams)
	}
	for k, v := range p {
		if k == "" || len(k) > MaxParamKey {
			return Errorf(CodeBadRequest, "param key must be 1-%d bytes", MaxParamKey)
		}
		if len(v) > MaxParamValue {
			return Errorf(CodeBadRequest, "param value over %d bytes", MaxParamValue)
		}
	}
	return nil
}

// ValidateTimeoutS rejects a negative timeout_s; 0 means "default".
func ValidateTimeoutS(timeoutS int) *Error {
	if timeoutS < 0 {
		return Errorf(CodeBadRequest, "timeout_s must be >= 0")
	}
	return nil
}

// ActionTimeout returns the effective action.run timeout: the default when timeoutS<=0 (callers
// reject negatives first with ValidateTimeoutS), capped at MaxActionTimeout. The clamp happens
// in seconds so huge values cannot overflow time.Duration.
func ActionTimeout(timeoutS int) time.Duration {
	if timeoutS <= 0 {
		return DefaultActionTimeout
	}
	if timeoutS >= int(MaxActionTimeout/time.Second) {
		return MaxActionTimeout
	}
	return time.Duration(timeoutS) * time.Second
}

// Admin-only (docs/M2.md §1.2-§1.4). No DTO ever carries a token secret or its hash, except
// TokenCreateResult.Token, returned exactly once.

type FleetCreateParams struct {
	Name    string `json:"name"`
	Mode    string `json:"mode,omitempty"` // "actions" (default) or "workspace"
	Confirm bool   `json:"confirm,omitempty"`
	// Workspace is the workspace config; required-or-defaulted when mode is "workspace",
	// rejected otherwise (validated by the service).
	Workspace *WorkspaceConfig `json:"workspace,omitempty"`
}
type FleetCreateResult struct {
	Fleet FleetInfo `json:"fleet"`
}

type TokenCreateParams struct {
	Fleet   string            `json:"fleet"`
	Labels  map[string]string `json:"labels,omitempty"`
	Approve bool              `json:"approve,omitempty"` // join as active instead of pending
	Uses    int               `json:"uses,omitempty"`    // 0 = single use
	TTLS    int               `json:"ttl_s,omitempty"`   // 0 = default 24h
}
type TokenCreateResult struct {
	ID      string    `json:"id"`
	Token   string    `json:"token"` // full mfj1.* string, shown once
	Expires time.Time `json:"expires"`
}
type TokenInfo struct {
	ID      string            `json:"id"`
	Fleet   string            `json:"fleet"`
	Labels  map[string]string `json:"labels,omitempty"`
	Approve bool              `json:"approve"`
	MaxUses int               `json:"max_uses"`
	Uses    int               `json:"uses"`
	Expires time.Time         `json:"expires"`
	Revoked bool              `json:"revoked"`
}
type TokenListParams struct {
	Fleet string `json:"fleet,omitempty"` // empty = all fleets
}
type TokenListResult struct {
	Tokens []TokenInfo `json:"tokens"`
}
type TokenRevokeParams struct {
	ID string `json:"id"`
}

// Runner lifecycle ops.
const (
	OpApprove = "approve"
	OpDisable = "disable"
	OpEnable  = "enable"
	OpRevoke  = "revoke"
)

type RunnerLifecycleParams struct {
	Fleet  string `json:"fleet"`
	Runner string `json:"runner"`
	Op     string `json:"op"`
}

// operator.code and operator.rotate_secret take no params.
type OperatorCodeResult struct {
	Code            string `json:"code"` // shown, never audited; valid for its 60 s window
	SecondsLeft     int    `json:"seconds_left"`
	HostFingerprint string `json:"host_fingerprint"` // SHA-256 of central's host key
}
type OperatorRotateSecretResult struct{}
type OperatorInfo struct {
	Name        string     `json:"name"`
	Fingerprint string     `json:"fingerprint,omitempty"`
	Status      string     `json:"status"`
	LastSeen    *time.Time `json:"last_seen,omitempty"`
}
type OperatorListResult struct {
	Operators []OperatorInfo `json:"operators"`
}
type OperatorRevokeParams struct {
	Name string `json:"name"`
}

type AuditListParams struct {
	SinceID int64 `json:"since_id,omitempty"`
	Limit   int   `json:"limit,omitempty"`
}

// AuditEvent mirrors store.AuditEvent.
type AuditEvent struct {
	ID         int64             `json:"id"`
	Time       time.Time         `json:"time"`
	Source     string            `json:"source"`
	ActorKind  string            `json:"actor_kind"`
	Actor      string            `json:"actor"`
	Remote     string            `json:"remote,omitempty"`
	Fleet      string            `json:"fleet,omitempty"`
	Runner     string            `json:"runner,omitempty"`
	Subject    string            `json:"subject,omitempty"`
	Action     string            `json:"action"`
	Params     map[string]string `json:"params,omitempty"`
	Outcome    string            `json:"outcome,omitempty"`
	ExitCode   *int              `json:"exit_code,omitempty"`
	DurationMS int64             `json:"duration_ms,omitempty"`
	Message    string            `json:"message,omitempty"`
	RequestID  string            `json:"request_id,omitempty"`
}
type AuditListResult struct {
	Events []AuditEvent `json:"events"`
}

// Params and results.

type HelloParams struct {
	Client string `json:"client,omitempty"` // e.g. "mcp"; recorded as a hint, not trusted
}
type HelloResult struct {
	Operator string `json:"operator"`
	Versions []int  `json:"versions"`
	Server   string `json:"server,omitempty"`
}

type FleetInfo struct {
	Name    string `json:"name"`
	Mode    string `json:"mode"`
	Confirm bool   `json:"confirm"`
	Runners int    `json:"runners"` // non-revoked runner count
	// Workspace is set for workspace-mode fleets only (additive, docs/M3.md §1.1).
	Workspace *FleetWorkspaceInfo `json:"workspace,omitempty"`
}

// FleetWorkspaceInfo is the operator-visible subset of a fleet's workspace config.
type FleetWorkspaceInfo struct {
	DefaultImage string   `json:"default_image"`
	Images       []string `json:"images"`
	Network      string   `json:"network"` // "none" or "egress"
	// Config is the complete config, set on the admin socket only (the CLI's `fleet workspace
	// set` needs it to send a full replace). Operators never see it.
	Config *WorkspaceConfig `json:"config,omitempty"`
}
type FleetsListResult struct {
	Fleets []FleetInfo `json:"fleets"`
}

type RunnersListParams struct {
	Fleet string `json:"fleet"`
}
type RunnerInfo struct {
	Name     string            `json:"name"`
	Status   string            `json:"status"`
	Online   bool              `json:"online"`
	Labels   map[string]string `json:"labels,omitempty"`
	LastSeen *time.Time        `json:"last_seen,omitempty"`
	// Workspace-mode runners only (additive): total and currently free workspace slots, and the
	// executor ("containerd" or "opensandbox"). Omitted when zero/empty.
	Slots     int    `json:"slots,omitempty"`
	FreeSlots int    `json:"free_slots,omitempty"`
	Executor  string `json:"executor,omitempty"`
	// Actions-mode runners (additive, informational: actions.yaml on the runner is the boundary).
	Actions []ActionInfo `json:"actions,omitempty"`
	// ActionsOmitted: the runner advertises actions, but the response's action budget was used up
	// by earlier runners; query it alone or ask for fewer runners to see them.
	ActionsOmitted bool `json:"actions_omitted,omitempty"`
}

// ActionParamInfo describes one action parameter. Default is absent when required.
type ActionParamInfo struct {
	Name    string  `json:"name"`
	Pattern string  `json:"pattern"`
	Default *string `json:"default,omitempty"`
}

// ActionInfo is an action a runner advertises: name and parameter shapes only.
type ActionInfo struct {
	Name   string            `json:"name"`
	Params []ActionParamInfo `json:"params,omitempty"`
}
type RunnersListResult struct {
	Runners []RunnerInfo `json:"runners"`
}

type ActionRunParams struct {
	Fleet    string            `json:"fleet"`
	Runner   string            `json:"runner"`
	Action   string            `json:"action"`
	Params   map[string]string `json:"params,omitempty"`
	TimeoutS int               `json:"timeout_s,omitempty"`
	// Confirm must equal the fleet name on guarded fleets.
	Confirm string `json:"confirm,omitempty"`
}
type ActionRunResult struct {
	ExitCode   int    `json:"exit_code"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	Truncated  bool   `json:"truncated,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	RequestID  string `json:"request_id,omitempty"`
}

// Workspace limits (docs/M3.md §1.9).
const (
	MaxArgvElems  = 256
	MaxArgvBytes  = 32 << 10
	MaxPathBytes  = 4 << 10
	MaxEnvVars    = 32
	MaxEnvValue   = 4 << 10
	MaxFileChunk  = 1 << 20 // body frame limit
	MaxSessionLen = 128

	// DefaultWorkspaceExecTimeout and DefaultWorkspaceExecMax are the per-fleet defaults
	// (exec_default_s 120, exec_max_s 1800); HardWorkspaceExecCeiling is the absolute cap in code.
	DefaultWorkspaceExecTimeout = 2 * time.Minute
	DefaultWorkspaceExecMax     = 30 * time.Minute
	HardWorkspaceExecCeiling    = 24 * time.Hour
)

// WorkspaceExecTimeout returns the effective workspace.exec timeout from the request's timeout_s
// and the fleet's exec_default_s / exec_max_s (0 or negative fleet values mean the built-in
// defaults). A negative timeoutS is bad_request; 0 means the fleet default. The result is capped
// at the fleet max and at HardWorkspaceExecCeiling. The runner's own limit is applied separately.
// Clamping is done in seconds so huge values cannot overflow time.Duration.
func WorkspaceExecTimeout(timeoutS, defaultS, maxS int) (time.Duration, *Error) {
	if e := ValidateTimeoutS(timeoutS); e != nil {
		return 0, e
	}
	ceiling := int(HardWorkspaceExecCeiling / time.Second)
	if maxS <= 0 {
		maxS = int(DefaultWorkspaceExecMax / time.Second)
	}
	if maxS > ceiling {
		maxS = ceiling
	}
	if defaultS <= 0 {
		defaultS = int(DefaultWorkspaceExecTimeout / time.Second)
	}
	s := timeoutS
	if s == 0 {
		s = defaultS
	}
	if s > maxS {
		s = maxS
	}
	return time.Duration(s) * time.Second, nil
}

// ValidateArgv delegates to workspace.ValidArgv (1..256 elements, 32 KiB, no NUL, argv[0] set).
func ValidateArgv(argv []string) *Error {
	if err := workspace.ValidArgv(argv); err != nil {
		return Errorf(CodeBadRequest, "%v", err)
	}
	return nil
}

// ValidatePath checks a workspace cwd or file path with workspace.CleanPath (the one shared
// implementation; "" means the workspace root).
func ValidatePath(p string) *Error {
	if _, err := workspace.CleanPath(p); err != nil {
		return Errorf(CodeBadRequest, "%v", err)
	}
	return nil
}

// MaxFileMode is the largest file mode accepted on write: permission bits only, so no
// setuid, setgid or sticky bit can be requested.
const MaxFileMode = 0o777

// ValidateFileMode rejects a write mode above MaxFileMode (0 means the default).
func ValidateFileMode(mode uint32) *Error {
	if mode > MaxFileMode {
		return Errorf(CodeBadRequest, "mode %#o: only permission bits (0777) are allowed", mode)
	}
	return nil
}

// ValidateEnv checks env with workspace.FilterEnv, allowing exactly the given names: it fails
// closed on malformed, oversized or denylisted names. The fleet env_allow is applied by the runner.
func ValidateEnv(env map[string]string) *Error {
	allow := make([]string, 0, len(env))
	for k := range env {
		allow = append(allow, k)
	}
	if _, err := workspace.FilterEnv(env, allow); err != nil {
		return Errorf(CodeBadRequest, "%v", err)
	}
	return nil
}

// WorkspaceInfo describes one workspace. Operator is set only on the admin listing.
type WorkspaceInfo struct {
	ID       string    `json:"id"`
	Fleet    string    `json:"fleet"`
	Runner   string    `json:"runner"`
	Session  string    `json:"session"`
	Image    string    `json:"image"`
	State    string    `json:"state"`
	Operator string    `json:"operator,omitempty"`
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"last_used"`
	Expires  time.Time `json:"expires"`
}

type WorkspaceOpenParams struct {
	Fleet   string            `json:"fleet"`
	Session string            `json:"session"`
	Image   string            `json:"image,omitempty"` // must be in the fleet allowlist
	Labels  map[string]string `json:"labels,omitempty"`
	Confirm string            `json:"confirm,omitempty"` // fleet name on guarded fleets
}
type WorkspaceOpenResult struct {
	Workspace WorkspaceInfo `json:"workspace"`
	Reused    bool          `json:"reused"`
}

// WorkspaceExecParams: when Stdin is true a body frame (<= MaxFileChunk) follows the request.
type WorkspaceExecParams struct {
	Workspace string            `json:"workspace"`
	Argv      []string          `json:"argv"`
	Cwd       string            `json:"cwd,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	TimeoutS  int               `json:"timeout_s,omitempty"` // 0 = fleet exec_default_s
	Stdin     bool              `json:"stdin,omitempty"`
	Confirm   string            `json:"confirm,omitempty"`
}
type WorkspaceExecResult struct {
	ExitCode   int    `json:"exit_code"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	Truncated  bool   `json:"truncated,omitempty"`
	TimedOut   bool   `json:"timed_out,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	RequestID  string `json:"request_id,omitempty"`
}

type WorkspaceReadFileParams struct {
	Workspace string `json:"workspace"`
	Path      string `json:"path"`
	Offset    int64  `json:"offset,omitempty"`
	Limit     int    `json:"limit,omitempty"` // bytes, 0 = MaxFileChunk; capped at MaxFileChunk
}

// WorkspaceReadFileResult is followed by exactly one body frame holding Length bytes.
type WorkspaceReadFileResult struct {
	Size   int64  `json:"size"` // total file size
	Offset int64  `json:"offset"`
	Length int    `json:"length"`
	Mode   uint32 `json:"mode"` // permission bits
	EOF    bool   `json:"eof"`
}

// WorkspaceWriteFileParams is followed by one body frame of exactly Length bytes whose SHA-256
// (lowercase hex) must equal SHA256.
type WorkspaceWriteFileParams struct {
	Workspace string `json:"workspace"`
	Path      string `json:"path"`
	Offset    int64  `json:"offset,omitempty"`
	Truncate  bool   `json:"truncate,omitempty"`
	Mode      uint32 `json:"mode,omitempty"` // permission bits for a new file, 0 = 0644
	Length    int    `json:"length"`
	SHA256    string `json:"sha256"`
	Confirm   string `json:"confirm,omitempty"`
}
type WorkspaceWriteFileResult struct {
	Size int64 `json:"size"` // file size after the write
}

type WorkspaceCloseParams struct {
	Workspace string `json:"workspace"`
}
type WorkspaceCloseResult struct{}

type WorkspaceListParams struct {
	Fleet string `json:"fleet,omitempty"`
}
type WorkspaceListResult struct {
	Workspaces []WorkspaceInfo `json:"workspaces"` // the caller's own only
}

// WorkspaceConfig is a fleet's workspace config (mirrors store.WorkspaceConfig). Units: CPUMilli
// millicores, *MB mebibytes, *S seconds. fleet.workspace_set is a full replace: the complete
// config is sent and nothing is merged. Defaults for omitted fields apply on fleet.create only.
type WorkspaceConfig struct {
	Executor     string   `json:"executor,omitempty"`      // "containerd" (default) or "opensandbox"
	DefaultImage string   `json:"default_image,omitempty"` // must be in Images
	Images       []string `json:"images,omitempty"`        // allowlist: exact refs or repo@sha256:* / repo:*
	Pull         string   `json:"pull,omitempty"`          // if-not-present | always | never
	Network      string   `json:"network,omitempty"`       // none | egress
	Egress       []string `json:"egress,omitempty"`        // host[:port], *.domain:443, cidr:port
	CPUMilli     int      `json:"cpu_milli,omitempty"`
	MemoryMB     int      `json:"memory_mb,omitempty"`
	Pids         int      `json:"pids,omitempty"`
	TmpfsMB      int      `json:"tmpfs_mb,omitempty"`
	DiskMB       int      `json:"disk_mb,omitempty"`
	IdleTTLS     int      `json:"idle_ttl_s,omitempty"`
	MaxTTLS      int      `json:"max_ttl_s,omitempty"`
	ExecDefaultS int      `json:"exec_default_s,omitempty"` // default 120
	ExecMaxS     int      `json:"exec_max_s,omitempty"`     // default 1800, ceiling 86400
}

type FleetWorkspaceSetParams struct {
	Fleet     string          `json:"fleet"`
	Workspace WorkspaceConfig `json:"workspace"`
}
type FleetWorkspaceSetResult struct {
	Fleet FleetInfo `json:"fleet"`
}

type WorkspaceAdminListParams struct {
	Fleet string `json:"fleet,omitempty"`
}
type WorkspaceAdminListResult struct {
	Workspaces []WorkspaceInfo `json:"workspaces"` // all operators, live and recent
}

type WorkspaceKillParams struct {
	ID string `json:"id"`
}

// WorkspaceKillResult carries the resulting state: "closed", or "closing" when the runner could not be
// reached and the reaper will finish the destroy.
type WorkspaceKillResult struct {
	State string `json:"state"`
}

var sessionRE = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// ValidateSession checks a workspace session id: 1..MaxSessionLen characters of A-Za-z0-9._:-.
func ValidateSession(s string) *Error {
	if !sessionRE.MatchString(s) {
		return Errorf(CodeBadRequest, "invalid session")
	}
	return nil
}
