// Package transport defines the wire vocabulary shared by central and runners:
// SSH channel/request names and the JSON messages carried over them.
package transport

// SSH names. Central->runner channels and runner->central global requests.
const (
	ChanExec     = "exec@mirafleet"
	ReqHeartbeat = "heartbeat@mirafleet"
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
