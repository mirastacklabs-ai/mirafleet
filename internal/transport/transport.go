// Package transport defines the wire vocabulary shared by central and runners:
// SSH channel/request names and the JSON messages carried over them.
package transport

import "errors"

// SSH names. Central->runner channels and runner->central global requests.
// ChanWorkspace (workspace@mirafleet) is defined in workspace.go with the workspace messages.
const (
	ChanExec     = "exec@mirafleet"
	ReqHeartbeat = "heartbeat@mirafleet"
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

	// Workspace fields (M3), all optional: M2 runners omit them, and zero values mean "no workspace
	// executor". Bounded by Validate.
	Slots         int      `json:"slots,omitempty"`          // concurrent workspaces the runner accepts
	Executor      string   `json:"executor,omitempty"`       // "containerd" or "opensandbox"
	SecureRuntime string   `json:"secure_runtime,omitempty"` // "gvisor", "kata" or empty
	Images        []string `json:"images,omitempty"`         // advertised image patterns, <= 32 entries of <= 256 bytes
}

// Validate bounds the optional workspace fields of a heartbeat.
func (h *Heartbeat) Validate() error {
	if h.Slots < 0 || h.Slots > MaxHeartbeatSlots {
		return errors.New("heartbeat slots out of range")
	}
	if len(h.Executor) > maxHeartbeatNameBytes || len(h.SecureRuntime) > maxHeartbeatNameBytes {
		return errors.New("heartbeat executor/secure_runtime too long")
	}
	if len(h.Images) > MaxHeartbeatImages {
		return errors.New("too many heartbeat images")
	}
	for _, im := range h.Images {
		if len(im) > MaxHeartbeatImageBytes {
			return errors.New("heartbeat image too long")
		}
	}
	return nil
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
