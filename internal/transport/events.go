package transport

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ChanEvents is the channel a host-mode runner opens toward central to send aggregated usage
// events. It is one-way: the runner writes length-prefixed EventBatch frames
// (4-byte big-endian length, then JSON), central reads and never replies. Runner role only.
const ChanEvents = "events@mirafleet"

// Event limits.
const (
	MaxBatchEvents    = 128
	MaxBatchBytes     = 64 << 10 // encoded EventBatch
	MaxEventFieldLen  = 253
	MaxEventCount     = 1 << 31
	eventWindowMaxSec = 3600
)

// Event types.
const (
	EventCredentialUse = "credential.use"
)

// Event statuses: how a credential use ended.
const (
	EventOK      = "ok"
	EventDenied  = "denied"
	EventError   = "error"
	EventExpired = "expired"
)

// Event is one aggregate: Count uses of a credential for a host by a session within the window
// starting at WindowMs (unix ms) and lasting WindowS seconds (60 by the runner). It carries no
// URL, path, header or value.
type Event struct {
	Type       string `json:"type"`
	Session    string `json:"session"`
	Credential string `json:"credential"`
	Host       string `json:"host"`
	Status     string `json:"status"`
	Count      int64  `json:"count"`
	WindowMs   int64  `json:"window_ms"`
	WindowS    int    `json:"window_s"`
}

// EventBatch is one frame. Dropped counts events the runner discarded locally (caps) since its
// previous batch; central records it as an events.dropped audit row.
type EventBatch struct {
	Events  []Event `json:"events"`
	Dropped int64   `json:"dropped,omitempty"`
}

func (e *Event) validate() error {
	if e.Type != EventCredentialUse {
		return errors.New("unknown event type")
	}
	if e.Session == "" || len(e.Session) > MaxSessionIDBytes || !printable(e.Session) {
		return errors.New("bad event session")
	}
	if !credName.MatchString(e.Credential) {
		return errors.New("bad event credential")
	}
	if e.Host == "" || len(e.Host) > MaxEventFieldLen || !printable(e.Host) {
		return errors.New("bad event host")
	}
	switch e.Status {
	case EventOK, EventDenied, EventError, EventExpired:
	default:
		return errors.New("bad event status")
	}
	if e.Count < 1 || e.Count > MaxEventCount {
		return errors.New("bad event count")
	}
	if e.WindowMs < 0 || e.WindowS < 1 || e.WindowS > eventWindowMaxSec {
		return errors.New("bad event window")
	}
	return nil
}

// MaxSessionIDBytes bounds an event's session id (api.MaxSessionLen).
const MaxSessionIDBytes = 128

// Validate bounds the batch.
func (b *EventBatch) Validate() error {
	if len(b.Events) > MaxBatchEvents {
		return fmt.Errorf("more than %d events in a batch", MaxBatchEvents)
	}
	if b.Dropped < 0 {
		return errors.New("negative dropped count")
	}
	if len(b.Events) == 0 && b.Dropped == 0 {
		return errors.New("empty batch")
	}
	for i := range b.Events {
		if err := b.Events[i].validate(); err != nil {
			return err
		}
	}
	return nil
}

// WriteEventBatch validates b and writes it as one frame of at most MaxBatchBytes.
func WriteEventBatch(w io.Writer, b *EventBatch) error {
	if err := b.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(b)
	if err != nil {
		return err
	}
	if len(data) > MaxBatchBytes {
		return fmt.Errorf("event batch %d bytes exceeds limit %d", len(data), MaxBatchBytes)
	}
	buf := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(buf, uint32(len(data)))
	copy(buf[4:], data)
	_, err = w.Write(buf)
	return err
}

// ReadEventBatch reads one frame (io.EOF on a clean end). The length is checked before anything
// is allocated; decoding is strict and the batch is validated.
func ReadEventBatch(r io.Reader) (*EventBatch, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		if err == io.EOF {
			return nil, io.EOF
		}
		return nil, fmt.Errorf("read event header: %w", err)
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > MaxBatchBytes {
		return nil, fmt.Errorf("event frame length %d out of range", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("read event body: %w", err)
	}
	var b EventBatch
	if err := strictUnmarshal(buf, &b); err != nil {
		return nil, fmt.Errorf("bad event batch: %w", err)
	}
	if err := b.Validate(); err != nil {
		return nil, err
	}
	return &b, nil
}
