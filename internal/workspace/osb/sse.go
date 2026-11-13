package osb

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Default SSE bounds.
const (
	DefaultMaxLine  = 1 << 20 // 1 MiB per line
	DefaultMaxEvent = 4 << 20 // 4 MiB per event
)

// SSE errors.
var (
	ErrLineTooLong    = errors.New("osb: sse line exceeds limit")
	ErrEventTooLarge  = errors.New("osb: sse event exceeds limit")
	ErrMalformedEvent = errors.New("osb: malformed sse event")
)

// Event types sent by execd's /command.
const (
	EventInit           = "init"
	EventStdout         = "stdout"
	EventStderr         = "stderr"
	EventError          = "error"
	EventComplete       = "execution_complete"
	EventStatus         = "status"
	EventResult         = "result"
	EventExecutionCount = "execution_count"
	EventPing           = "ping"
)

// ErrorInfo is the `error` object of an `error` event.
type ErrorInfo struct {
	Name      string   `json:"ename"`
	Value     string   `json:"evalue"`
	Traceback []string `json:"traceback"`
	ExitCode  *int     `json:"exit_code"`
}

// Event is one decoded execd stream event.
type Event struct {
	Type          string     `json:"type"`
	Text          string     `json:"text"`
	ID            string     `json:"id"`
	ExitCode      *int       `json:"exit_code"`
	ExecutionTime int64      `json:"execution_time"`
	Timestamp     int64      `json:"timestamp"`
	Error         *ErrorInfo `json:"error"`
}

// SessionID returns the command id of an init event. execd carries it in `text` today; an `id`
// field is accepted too.
func (e Event) SessionID() string {
	if e.ID != "" {
		return e.ID
	}
	return e.Text
}

// SSEReader is a strict, bounded Server-Sent-Events parser. Lines end in LF or CRLF. Each event
// must carry a JSON `data` payload; a missing/invalid payload, an unfinished event at EOF, an
// oversize line or an oversize event is an error and ends the stream.
type SSEReader struct {
	br       *bufio.Reader
	maxLine  int
	maxEvent int
	err      error
}

// NewSSEReader wraps r. Non-positive limits fall back to the defaults.
func NewSSEReader(r io.Reader, maxLine, maxEvent int) *SSEReader {
	if maxLine <= 0 {
		maxLine = DefaultMaxLine
	}
	if maxEvent <= 0 {
		maxEvent = DefaultMaxEvent
	}
	return &SSEReader{br: bufio.NewReaderSize(r, 4096), maxLine: maxLine, maxEvent: maxEvent}
}

// readLine returns the next line without its terminator. io.EOF only at a clean boundary.
func (s *SSEReader) readLine() ([]byte, error) {
	var acc []byte
	for {
		chunk, err := s.br.ReadSlice('\n')
		if len(acc)+len(chunk) > s.maxLine+2 { // +2: CRLF
			return nil, ErrLineTooLong
		}
		switch {
		case err == nil:
			line := append(acc, chunk...)
			line = line[:len(line)-1]
			line = bytes.TrimSuffix(line, []byte{'\r'})
			if len(line) > s.maxLine {
				return nil, ErrLineTooLong
			}
			return line, nil
		case errors.Is(err, bufio.ErrBufferFull):
			acc = append(acc, chunk...)
		case errors.Is(err, io.EOF):
			if len(acc)+len(chunk) == 0 {
				return nil, io.EOF
			}
			return nil, io.ErrUnexpectedEOF // partial line
		default:
			return nil, err
		}
	}
}

// Next returns the next event, io.EOF at a clean end of stream. After any other error every
// later call returns the same error.
func (s *SSEReader) Next() (Event, error) {
	if s.err != nil {
		return Event{}, s.err
	}
	ev, err := s.next()
	if err != nil {
		s.err = err
	}
	return ev, err
}

func (s *SSEReader) next() (Event, error) {
	var (
		data    []byte
		name    string
		hasData bool
		size    int
		pending bool
	)
	for {
		line, err := s.readLine()
		if err != nil {
			if errors.Is(err, io.EOF) && pending {
				return Event{}, io.ErrUnexpectedEOF
			}
			return Event{}, err
		}
		if len(line) == 0 {
			if !hasData {
				data, name, size, pending = nil, "", 0, false
				continue // nothing to dispatch
			}
			var ev Event
			if err := json.Unmarshal(data, &ev); err != nil {
				return Event{}, fmt.Errorf("%w: invalid JSON payload (%d bytes)", ErrMalformedEvent, len(data))
			}
			if ev.Type == "" {
				ev.Type = name
			}
			if ev.Type == "" {
				return Event{}, fmt.Errorf("%w: no event type", ErrMalformedEvent)
			}
			return ev, nil
		}
		if line[0] == ':' {
			continue // comment / keep-alive
		}
		size += len(line) + 1
		if size > s.maxEvent {
			return Event{}, ErrEventTooLarge
		}
		pending = true
		field, value := string(line), []byte(nil)
		if i := bytes.IndexByte(line, ':'); i >= 0 {
			field = string(line[:i])
			value = line[i+1:]
			if len(value) > 0 && value[0] == ' ' {
				value = value[1:]
			}
		}
		switch field {
		case "data":
			if hasData {
				data = append(data, '\n')
			}
			data = append(data, value...)
			hasData = true
		case "event":
			name = string(value)
		default: // id, retry and unknown fields are ignored per the SSE spec
		}
	}
}
