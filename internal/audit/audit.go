// Package audit writes JSON-lines audit records. A nil *Writer is a valid no-op sink.
package audit

import (
	"encoding/json"
	"io"
	"os"
	"sync"
	"time"
)

// Event is one audit record. Fields not relevant to a side are left empty.
type Event struct {
	Time       time.Time         `json:"time"`
	Runner     string            `json:"runner,omitempty"`
	Action     string            `json:"action"`
	Params     map[string]string `json:"params,omitempty"`
	Outcome    string            `json:"outcome"` // start | ok | failed | refused | timeout | error
	ExitCode   int               `json:"exit_code"`
	DurationMS int64             `json:"duration_ms"`
	Message    string            `json:"message,omitempty"`

	// Central-side attribution; all omitted when empty so runner JSONL is unchanged.
	Source    string `json:"source,omitempty"`
	ActorKind string `json:"actor_kind,omitempty"`
	Actor     string `json:"actor,omitempty"`
	Remote    string `json:"remote,omitempty"`
	Fleet     string `json:"fleet,omitempty"`
	Subject   string `json:"subject,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

type Writer struct {
	mu sync.Mutex
	w  io.Writer
	c  io.Closer
}

func New(w io.Writer) *Writer { return &Writer{w: w} }

// Open appends to path, creating it with mode 0600.
func Open(path string) (*Writer, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &Writer{w: f, c: f}, nil
}

func (a *Writer) Log(e Event) {
	if a == nil {
		return
	}
	_ = a.write(e)
}

func (a *Writer) write(e Event) error {
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	_, err = a.w.Write(append(b, 0x0a))
	return err
}

func (a *Writer) Close() error {
	if a == nil || a.c == nil {
		return nil
	}
	return a.c.Close()
}
