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
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.w.Write(append(b, '\n'))
}

func (a *Writer) Close() error {
	if a == nil || a.c == nil {
		return nil
	}
	return a.c.Close()
}
