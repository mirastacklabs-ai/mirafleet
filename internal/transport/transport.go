// Package transport defines the wire vocabulary shared by central and runners:
// SSH channel/request names and the JSON messages carried over them.
package transport

import (
	"encoding/json"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Action discovery bounds (Heartbeat.Actions). Informational only: the runner's actions.yaml
// stays the security boundary.
const (
	MaxHeartbeatActions     = 128
	MaxActionNameBytes      = 64
	MaxActionParams         = 32
	MaxActionPatternBytes   = 256
	MaxActionDefaultBytes   = 256
	maxActionParamNameBytes = 64
	// MaxActionsBytes caps the JSON-encoded size of Heartbeat.Actions, so discovery can neither
	// overflow the SSH packet limit nor blow up runners.list.
	MaxActionsBytes = 16 << 10
)

// ActionsEncodedSize returns the JSON-encoded size of an actions list.
func ActionsEncodedSize(as []ActionInfo) int {
	b, _ := json.Marshal(as)
	return len(b)
}

// ActionParam describes one action parameter. Default is nil when the parameter is required.
type ActionParam struct {
	Name    string  `json:"name"`
	Pattern string  `json:"pattern"`
	Default *string `json:"default,omitempty"`
}

// ActionInfo describes one action a runner offers (no argv, paths or environment).
type ActionInfo struct {
	Name   string        `json:"name"`
	Params []ActionParam `json:"params,omitempty"`
}

func printable(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// ValidateAction bounds one ActionInfo.
func ValidateAction(a ActionInfo) error {
	if a.Name == "" || len(a.Name) > MaxActionNameBytes || !printable(a.Name) {
		return errors.New("heartbeat action name invalid")
	}
	if len(a.Params) > MaxActionParams {
		return errors.New("heartbeat action has too many params")
	}
	for _, p := range a.Params {
		if p.Name == "" || len(p.Name) > maxActionParamNameBytes || !printable(p.Name) {
			return errors.New("heartbeat action param name invalid")
		}
		if len(p.Pattern) > MaxActionPatternBytes || !printable(p.Pattern) {
			return errors.New("heartbeat action pattern invalid")
		}
		if p.Default != nil && (len(*p.Default) > MaxActionDefaultBytes || !printable(*p.Default)) {
			return errors.New("heartbeat action default invalid")
		}
	}
	return nil
}

// SSH names. Central->runner channels and runner->central global requests.
// ChanWorkspace (workspace@mirafleet) is defined in workspace.go with the workspace messages.
const (
	ChanExec     = "exec@mirafleet"
	ReqHeartbeat = "heartbeat@mirafleet"
	// ReqRefused is the central->runner global request that carries the version-skew refusal
	// message (docs/M6.md 2.2). The payload is the plain message text; the runner replies true.
	ReqRefused = "refused@mirafleet"
)

// Channel types opened toward central (operators on api@, enrolling peers on join@/register@).
const (
	ChanAPI      = "api@mirafleet"
	ChanJoin     = "join@mirafleet"
	ChanRegister = "register@mirafleet"
)

// SSH users: the user name binds the role a key may authenticate as (docs/M2.md §1.3).
const (
	UserOperator = "operator"
	UserEnroll   = "enroll"
	UserRunner   = "runner"
)

// MaxResultBytes is the largest encoded ExecResult central will read. Runners cap stdout+stderr
// combined at 1 MiB; JSON escaping can expand a byte to at most 6 bytes (\u00XX / \ufffd), so the
// worst case is ~6 MiB plus a few bytes of framing, safely under this limit.
const MaxResultBytes = 8 << 20

// Heartbeat is the JSON payload of a heartbeat@mirafleet global request.
type Heartbeat struct {
	Hostname string            `json:"hostname"`
	OS       string            `json:"os"`
	Arch     string            `json:"arch"`
	Version  string            `json:"version"`
	Labels   map[string]string `json:"labels,omitempty"`
	// Proto is the runner's protocol level (internal/protover). Central checks it on the first
	// heartbeat; missing counts as level 0 and is refused.
	Proto int `json:"proto,omitempty"`

	// Workspace fields (M3), all optional: M2 runners omit them, and zero values mean "no workspace
	// executor". Bounded by Validate.
	Slots         int      `json:"slots,omitempty"`          // concurrent workspaces the runner accepts
	Executor      string   `json:"executor,omitempty"`       // "containerd" or "opensandbox"
	SecureRuntime string   `json:"secure_runtime,omitempty"` // "gvisor", "kata" or empty
	Images        []string `json:"images,omitempty"`         // advertised image patterns, <= 32 entries of <= 256 bytes

	// Actions (M5, additive): the actions an actions-mode runner offers, for discovery only.
	// <= 128 actions; see ValidateAction. M2/M3 runners omit it.
	Actions []ActionInfo `json:"actions,omitempty"`

	// M7 (additive; see devws.go): Features lists what the runner supports (fs, jobs, creds,
	// events, host), Credentials the credentials it holds (names, kinds, hosts, status; never a
	// value), Busy the number of running background jobs (a workspace with one is in use).
	Features    []string              `json:"features,omitempty"`
	Credentials []HeartbeatCredential `json:"credentials,omitempty"`
	Busy        int                   `json:"busy,omitempty"`
}

// Validate bounds the optional workspace fields of a heartbeat.
func (h *Heartbeat) Validate() error {
	if h.Slots < 0 || h.Slots > MaxHeartbeatSlots {
		return errors.New("heartbeat slots out of range")
	}
	// Executor is a name, not an enum: central places workspaces by exact match against the fleet's
	// configured executor, so only the shape is checked here.
	if h.Executor != "" && (len(h.Executor) > 32 || !heartbeatCharsOK(h.Executor, "-") ||
		h.Executor[0] < 'a' || h.Executor[0] > 'z' || strings.ToLower(h.Executor) != h.Executor) {
		return errors.New("heartbeat executor has an invalid name")
	}
	if len(h.SecureRuntime) > maxHeartbeatNameBytes {
		return errors.New("heartbeat secure_runtime too long")
	}
	if !heartbeatCharsOK(h.SecureRuntime, "._-/:") {
		return errors.New("heartbeat secure_runtime has invalid characters")
	}
	if len(h.Images) > MaxHeartbeatImages {
		return errors.New("too many heartbeat images")
	}
	for _, im := range h.Images {
		if len(im) > MaxHeartbeatImageBytes {
			return errors.New("heartbeat image too long")
		}
		if im == "" || !heartbeatCharsOK(im, "._-/:@*") {
			return errors.New("heartbeat image has invalid characters")
		}
	}
	if len(h.Actions) > MaxHeartbeatActions {
		return errors.New("too many heartbeat actions")
	}
	for _, a := range h.Actions {
		if err := ValidateAction(a); err != nil {
			return err
		}
	}
	if len(h.Actions) > 0 && ActionsEncodedSize(h.Actions) > MaxActionsBytes {
		return errors.New("heartbeat actions too large")
	}
	return h.validateDev()
}

// heartbeatCharsOK reports whether s consists only of ASCII letters, digits and the extra bytes.
func heartbeatCharsOK(s, extra string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte(extra, c) >= 0) {
			return false
		}
	}
	return true
}

// ExecRequest is sent as the extra data of an exec@mirafleet channel open.
type ExecRequest struct {
	Action string            `json:"action"`
	Params map[string]string `json:"params,omitempty"`
}

// ExecResult is written by the runner to the channel (one JSON document) before it closes it.
// Refused means the actions guard rejected the request (unknown action, bad param); nothing ran.
type ExecResult struct {
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`

	StdoutTruncated bool   `json:"stdout_truncated,omitempty"`
	StderrTruncated bool   `json:"stderr_truncated,omitempty"`
	ExitCode        int    `json:"exit_code"`
	TimedOut        bool   `json:"timed_out,omitempty"`
	Refused         bool   `json:"refused,omitempty"`
	Message         string `json:"message,omitempty"`
	DurationMS      int64  `json:"duration_ms"`
}
