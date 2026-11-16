package transport

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"unicode/utf8"
)

// ChanWorkspace is the central->runner channel type for workspace operations.
const ChanWorkspace = "workspace@mirafleet"

// Workspace operations.
const (
	OpCreate  = "create"
	OpExec    = "exec"
	OpRead    = "read"
	OpWrite   = "write"
	OpDestroy = "destroy"
	OpList    = "list"

	// Dev-workspace operations: file tools, background jobs, output paging and
	// credential delivery. Shapes and validation live in devws.go.
	OpFS         = "fs"
	OpJobStart   = "job_start"
	OpJobStatus  = "job_status"
	OpJobTail    = "job_tail"
	OpJobCancel  = "job_cancel"
	OpOutputRead = "output_read"
	OpCredsSync  = "creds_sync"
)

// Limits of the workspace protocol. The runner re-validates everything; these only bound the wire.
const (
	MaxWorkspaceRequestBytes = 64 << 10 // channel-open extra data
	MaxBodyBytes             = 1 << 20  // raw body (stdin, file chunk) per request
	MaxWorkspaceTimeoutMs    = 24 * 60 * 60 * 1000
	MaxWorkspaceAgeMs        = 365 * 24 * 60 * 60 * 1000 // spec max_age_ms upper bound (365 days)
	MaxArgvItems             = 256
	MaxArgvBytes             = 32 << 10
	MaxPathBytes             = 4 << 10
	MaxEnvVars               = 32
	MaxEnvNameBytes          = 64
	MaxEnvValueBytes         = 4 << 10
	MaxImageBytes            = 256
	MaxSpecLabels            = 16
	MaxSpecLabelBytes        = 256
	MaxEgressRules           = 64
	MaxHostBytes             = 253
	MaxListEntries           = 4096
	MaxMessageBytes          = 4 << 10
	maxFileMode              = 0o777 // permission bits only: no setuid/setgid/sticky

	// MaxWorkspaceResultBytes is the largest encoded WorkspaceResult frame: the API result cap
	// (1 MiB) plus 64 KiB of framing. Runner and central both use it (not the 8 MiB exec cap).
	MaxWorkspaceResultBytes = MaxBodyBytes + 64<<10

	// MaxHeartbeatImages and MaxHeartbeatImageBytes bound Heartbeat.Images.
	MaxHeartbeatImages     = 32
	MaxHeartbeatImageBytes = 256
	MaxHeartbeatSlots      = 1024
	maxHeartbeatNameBytes  = 64
)

// Result codes of WorkspaceResult.Code.
const (
	CodeOK          = "ok"
	CodeBadRequest  = "bad_request"
	CodeRefused     = "refused"
	CodeNotFound    = "not_found"
	CodeBusy        = "busy"
	CodeTooLarge    = "too_large"
	CodeQuota       = "quota"
	CodeUnavailable = "unavailable"
	CodeUnsupported = "unsupported"
	CodeTimeout     = "timeout"
	CodeInternal    = "internal"
)

var validCodes = map[string]bool{
	CodeOK: true, CodeBadRequest: true, CodeRefused: true, CodeNotFound: true, CodeBusy: true,
	CodeTooLarge: true, CodeQuota: true, CodeUnavailable: true, CodeUnsupported: true,
	CodeTimeout: true, CodeInternal: true,
}

// EgressRule is one allowed destination of a workspace in network mode "egress".
type EgressRule struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

// WorkspaceSpec mirrors the executor Spec as plain JSON-able fields.
type WorkspaceSpec struct {
	Image                string            `json:"image"`
	Pull                 string            `json:"pull,omitempty"`
	CPUMilli             int64             `json:"cpu_milli,omitempty"`
	MemoryBytes          int64             `json:"memory_bytes,omitempty"`
	Pids                 int64             `json:"pids,omitempty"`
	TmpfsBytes           int64             `json:"tmpfs_bytes,omitempty"`
	DiskBytes            int64             `json:"disk_bytes,omitempty"`
	NetworkMode          string            `json:"network_mode,omitempty"` // "none" (default) or "egress"
	Allow                []EgressRule      `json:"allow,omitempty"`
	RequireSecureRuntime bool              `json:"require_secure_runtime,omitempty"`
	Labels               map[string]string `json:"labels,omitempty"`
	MaxAgeMs             int64             `json:"max_age_ms,omitempty"`
}

// WorkspaceRequest is sent as the extra data of a workspace@mirafleet channel open.
type WorkspaceRequest struct {
	Op        string            `json:"op"`
	ID        string            `json:"id,omitempty"`
	Spec      *WorkspaceSpec    `json:"spec,omitempty"`
	Argv      []string          `json:"argv,omitempty"`
	Cwd       string            `json:"cwd,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	StdinLen  int64             `json:"stdin_len,omitempty"`
	Path      string            `json:"path,omitempty"`
	Offset    int64             `json:"offset,omitempty"`
	Limit     int64             `json:"limit,omitempty"`
	Truncate  bool              `json:"truncate,omitempty"`
	Mode      int64             `json:"mode,omitempty"`
	Length    int64             `json:"length,omitempty"`
	TimeoutMs int64             `json:"timeout_ms,omitempty"`

	// Delete (M7, destroy on a host runner only): also delete the session directory.
	Delete bool `json:"delete,omitempty"`

	// Dev-workspace payloads (M7): exactly the one matching Op is set. creds_sync carries its
	// secrets in a body of StdinLen bytes (see CredsSyncBody), never in this request.
	FS  *FSOp     `json:"fs,omitempty"`
	Job *JobOp    `json:"job,omitempty"`
	Out *OutputOp `json:"out,omitempty"`
}

// WorkspaceExec is the exec outcome in a WorkspaceResult.
type WorkspaceExec struct {
	ExitCode   int    `json:"exit_code"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	Truncated  bool   `json:"truncated,omitempty"`
	TimedOut   bool   `json:"timed_out,omitempty"`
	DurationMs int64  `json:"duration_ms"`

	// M7 (additive): full output is kept on the runner. Ref names it for output_read; the byte
	// counts are the true totals; Omitted is how many bytes the inline text left out.
	Ref         string `json:"ref,omitempty"`
	StdoutBytes int64  `json:"stdout_bytes,omitempty"`
	StderrBytes int64  `json:"stderr_bytes,omitempty"`
	Omitted     int64  `json:"omitted,omitempty"`
	// Cwd is the session's working directory after the command (host mode), a runner path.
	Cwd string `json:"cwd,omitempty"`
}

// WorkspaceFile describes a file op outcome. For read, Length raw bytes follow the result frame.
type WorkspaceFile struct {
	Size   int64 `json:"size"`
	Mode   int64 `json:"mode"`
	Length int64 `json:"length"`
	EOF    bool  `json:"eof,omitempty"`
}

// WorkspaceInfo is one entry of a list result.
type WorkspaceInfo struct {
	ID        string `json:"id"`
	Image     string `json:"image"`
	State     string `json:"state"`
	CreatedMs int64  `json:"created_ms"`
	DiskBytes int64  `json:"disk_bytes"`
}

// WorkspaceResult is the runner's single length-prefixed reply frame.
type WorkspaceResult struct {
	Code       string          `json:"code"`
	Message    string          `json:"message,omitempty"`
	Exec       *WorkspaceExec  `json:"exec,omitempty"`
	File       *WorkspaceFile  `json:"file,omitempty"`
	List       []WorkspaceInfo `json:"list,omitempty"`
	DurationMs int64           `json:"duration_ms"`

	// M7 results; the one matching the request's op is set. For an fs result with Length > 0,
	// that many raw body bytes follow the frame.
	FS     *FSResult        `json:"fs,omitempty"`
	Job    *JobResult       `json:"job,omitempty"`
	Jobs   []JobResult      `json:"jobs,omitempty"`
	Output *OutputResult    `json:"output,omitempty"`
	Creds  *CredsSyncResult `json:"creds,omitempty"`
}

func validWSID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func checkText(what string, s string, max int) error {
	if len(s) > max {
		return fmt.Errorf("%s longer than %d bytes", what, max)
	}
	if strings.IndexByte(s, 0) >= 0 {
		return fmt.Errorf("%s contains NUL", what)
	}
	if !utf8.ValidString(s) {
		return fmt.Errorf("%s is not valid UTF-8", what)
	}
	return nil
}

func inRange(what string, v, max int64) error {
	if v < 0 || v > max {
		return fmt.Errorf("%s out of range", what)
	}
	return nil
}

// Validate checks the shape of a request: known op, required fields for the op, non-negative
// lengths and sizes within limits. Semantics (path escapes, env policy, image allowlist) belong to
// the runner's guard.
func (r *WorkspaceRequest) Validate() error {
	switch r.Op {
	case OpCreate, OpExec, OpRead, OpWrite, OpDestroy, OpList:
	case OpFS, OpJobStart, OpJobStatus, OpJobTail, OpJobCancel, OpOutputRead, OpCredsSync:
		return r.validateDev()
	default:
		return errors.New("unknown op")
	}
	if r.FS != nil || r.Job != nil || r.Out != nil {
		return errors.New("dev payload set on a legacy op")
	}
	if r.Delete && r.Op != OpDestroy {
		return errors.New("delete is only valid for destroy")
	}
	if r.ID != "" && !validWSID(r.ID) {
		return errors.New("bad id")
	}
	if r.Op != OpList && r.ID == "" {
		return errors.New("id required")
	}
	if r.Op != OpCreate && r.Spec != nil {
		return errors.New("spec only valid for create")
	}
	if r.Op == OpCreate {
		if r.Spec == nil {
			return errors.New("spec required")
		}
		if err := r.Spec.validate(); err != nil {
			return err
		}
	}
	if len(r.Argv) > MaxArgvItems {
		return errors.New("too many argv items")
	}
	total := 0
	for _, a := range r.Argv {
		total += len(a)
		if strings.IndexByte(a, 0) >= 0 {
			return errors.New("argv contains NUL")
		}
		if !utf8.ValidString(a) {
			return errors.New("argv is not valid UTF-8")
		}
	}
	if total > MaxArgvBytes {
		return errors.New("argv too large")
	}
	if r.Op == OpExec && len(r.Argv) == 0 {
		return errors.New("argv required")
	}
	if r.Op != OpExec && (len(r.Argv) > 0 || r.Cwd != "" || len(r.Env) > 0 || r.StdinLen != 0) {
		return errors.New("exec fields set on non-exec op")
	}
	if err := checkText("cwd", r.Cwd, MaxPathBytes); err != nil {
		return err
	}
	if err := checkText("path", r.Path, MaxPathBytes); err != nil {
		return err
	}
	if (r.Op == OpRead || r.Op == OpWrite) && r.Path == "" {
		return errors.New("path required")
	}
	if r.Op != OpRead && r.Op != OpWrite && (r.Path != "" || r.Offset != 0 || r.Limit != 0 || r.Truncate || r.Mode != 0 || r.Length != 0) {
		return errors.New("file fields set on non-file op")
	}
	if r.Op == OpRead && (r.Truncate || r.Mode != 0 || r.Length != 0) {
		return errors.New("write fields set on read")
	}
	if r.Op == OpWrite && r.Limit != 0 {
		return errors.New("limit set on write")
	}
	if len(r.Env) > MaxEnvVars {
		return errors.New("too many env vars")
	}
	for k, v := range r.Env {
		if k == "" || len(k) > MaxEnvNameBytes || strings.ContainsAny(k, "=\x00") {
			return errors.New("bad env name")
		}
		if err := checkText("env value", v, MaxEnvValueBytes); err != nil {
			return err
		}
	}
	for _, c := range []struct {
		what string
		v    int64
		max  int64
	}{
		{"stdin_len", r.StdinLen, MaxBodyBytes},
		{"length", r.Length, MaxBodyBytes},
		{"limit", r.Limit, MaxBodyBytes},
		{"offset", r.Offset, 1<<62 - 1},
		{"mode", r.Mode, maxFileMode},
		{"timeout_ms", r.TimeoutMs, MaxWorkspaceTimeoutMs},
	} {
		if err := inRange(c.what, c.v, c.max); err != nil {
			return err
		}
	}
	return nil
}

func (s *WorkspaceSpec) validate() error {
	if s.Image == "" {
		return errors.New("image required")
	}
	if err := checkText("image", s.Image, MaxImageBytes); err != nil {
		return err
	}
	if err := checkText("pull", s.Pull, 32); err != nil {
		return err
	}
	switch s.NetworkMode {
	case "", "none", "egress":
	default:
		return errors.New("bad network_mode")
	}
	for _, v := range []int64{s.CPUMilli, s.MemoryBytes, s.Pids, s.TmpfsBytes, s.DiskBytes, s.MaxAgeMs} {
		if v < 0 {
			return errors.New("negative spec limit")
		}
	}
	if s.MaxAgeMs > MaxWorkspaceAgeMs {
		return errors.New("max_age_ms above 365 days")
	}
	if len(s.Allow) > MaxEgressRules {
		return errors.New("too many egress rules")
	}
	for _, a := range s.Allow {
		if a.Host == "" {
			return errors.New("egress host required")
		}
		if err := checkText("egress host", a.Host, MaxHostBytes); err != nil {
			return err
		}
		if a.Port < 1 || a.Port > 65535 {
			return errors.New("bad egress port")
		}
	}
	if len(s.Labels) > MaxSpecLabels {
		return errors.New("too many labels")
	}
	for k, v := range s.Labels {
		if k == "" {
			return errors.New("empty label name")
		}
		if err := checkText("label", k, MaxSpecLabelBytes); err != nil {
			return err
		}
		if err := checkText("label", v, MaxSpecLabelBytes); err != nil {
			return err
		}
	}
	return nil
}

// strictUnmarshal decodes exactly one JSON value, refusing unknown fields (exact-case keys, no
// duplicates) and trailing data.
func strictUnmarshal(b []byte, v any) error {
	if err := checkExactKeys(b, reflect.TypeOf(v)); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data after JSON value")
	}
	return nil
}

// EncodeWorkspaceRequest validates r and returns the channel-open extra data.
func EncodeWorkspaceRequest(r *WorkspaceRequest) ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	if len(b) > MaxWorkspaceRequestBytes {
		return nil, fmt.Errorf("workspace request larger than %d bytes", MaxWorkspaceRequestBytes)
	}
	return b, nil
}

// DecodeWorkspaceRequest strictly decodes and validates channel-open extra data.
func DecodeWorkspaceRequest(b []byte) (*WorkspaceRequest, error) {
	if len(b) > MaxWorkspaceRequestBytes {
		return nil, fmt.Errorf("workspace request larger than %d bytes", MaxWorkspaceRequestBytes)
	}
	var r WorkspaceRequest
	if err := strictUnmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("bad workspace request: %w", err)
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return &r, nil
}

// Validate checks the result's shape.
func (r *WorkspaceResult) Validate() error {
	if !validCodes[r.Code] {
		return errors.New("unknown result code")
	}
	if len(r.Message) > MaxMessageBytes {
		return errors.New("message too long")
	}
	if len(r.List) > MaxListEntries {
		return errors.New("list too long")
	}
	if f := r.File; f != nil && (f.Size < 0 || f.Mode < 0 || f.Length < 0 || f.Length > MaxBodyBytes) {
		return errors.New("bad file result")
	}
	return r.validateDevResult()
}

// WriteResult writes res as one frame: a 4-byte big-endian length, then the JSON. The encoded size
// must not exceed max (MaxWorkspaceResultBytes if max <= 0).
func WriteResult(w io.Writer, res *WorkspaceResult, max int) error {
	if max <= 0 {
		max = MaxWorkspaceResultBytes
	}
	b, err := json.Marshal(res)
	if err != nil {
		return err
	}
	if len(b) > max {
		return fmt.Errorf("result frame %d bytes exceeds limit %d", len(b), max)
	}
	buf := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(buf, uint32(len(b)))
	copy(buf[4:], b)
	_, err = w.Write(buf)
	return err
}

// ReadResult reads one result frame. The length is checked against max (MaxWorkspaceResultBytes
// if max <= 0) before anything is allocated; decoding is strict.
func ReadResult(r io.Reader, max int) (*WorkspaceResult, error) {
	if max <= 0 {
		max = MaxWorkspaceResultBytes
	}
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, fmt.Errorf("read result header: %w", err)
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if uint64(n) > uint64(max) {
		return nil, fmt.Errorf("result frame %d bytes exceeds limit %d", n, max)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("read result body: %w", err)
	}
	var res WorkspaceResult
	if err := strictUnmarshal(buf, &res); err != nil {
		return nil, fmt.Errorf("bad workspace result: %w", err)
	}
	if err := res.Validate(); err != nil {
		return nil, err
	}
	return &res, nil
}

// ErrBodyLength reports a raw body that is shorter or longer than announced.
var ErrBodyLength = errors.New("body length mismatch")

// CopyBody copies exactly n raw bytes from src to dst (n must be within [0, MaxBodyBytes]) and then
// requires src to be at EOF: a short body or any extra byte yields ErrBodyLength. The peer must
// CloseWrite after the body, otherwise the EOF check waits for it.
func CopyBody(dst io.Writer, src io.Reader, n int64) error {
	if n < 0 || n > MaxBodyBytes {
		return fmt.Errorf("body length %d out of range", n)
	}
	got, err := io.Copy(dst, io.LimitReader(src, n))
	if err != nil {
		return err
	}
	if got != n {
		return fmt.Errorf("%w: got %d of %d bytes", ErrBodyLength, got, n)
	}
	var one [1]byte
	if k, err := src.Read(one[:]); k > 0 {
		return fmt.Errorf("%w: more than %d bytes", ErrBodyLength, n)
	} else if err != nil && err != io.EOF {
		return err
	}
	return nil
}

// WriteBody streams exactly n bytes from src to dst. src running short, or holding more than n
// bytes, yields ErrBodyLength (nothing beyond n is written).
func WriteBody(dst io.Writer, src io.Reader, n int64) error {
	if n < 0 || n > MaxBodyBytes {
		return fmt.Errorf("body length %d out of range", n)
	}
	got, err := io.Copy(dst, io.LimitReader(src, n))
	if err != nil {
		return err
	}
	if got != n {
		return fmt.Errorf("%w: source had %d of %d bytes", ErrBodyLength, got, n)
	}
	var one [1]byte
	if k, _ := src.Read(one[:]); k > 0 {
		return fmt.Errorf("%w: source has more than %d bytes", ErrBodyLength, n)
	}
	return nil
}
