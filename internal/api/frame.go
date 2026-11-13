package api

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// Framing errors.
var (
	ErrFrameTooLarge = errors.New("api: frame exceeds size limit")
	ErrEmptyFrame    = errors.New("api: zero-length frame")
)

// ReadFrame reads one frame: a 4-byte big-endian length followed by that many payload bytes.
// The length is checked against limit BEFORE any payload allocation. A clean EOF before the first
// header byte returns io.EOF; a short header or payload returns io.ErrUnexpectedEOF; a zero
// length returns ErrEmptyFrame; a length over limit returns ErrFrameTooLarge (nothing more is read).
func ReadFrame(r io.Reader, limit int) ([]byte, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("api: invalid frame limit %d", limit)
	}
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err // io.EOF if nothing read, io.ErrUnexpectedEOF if partial
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 {
		return nil, ErrEmptyFrame
	}
	if uint64(n) > uint64(limit) {
		return nil, ErrFrameTooLarge
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return buf, nil
}

// WriteFrame writes payload as one frame. Empty payloads and payloads over MaxResponseBytes
// are refused. Header and payload go out in a single Write.
func WriteFrame(w io.Writer, payload []byte) error {
	return writeFrameMax(w, payload, MaxResponseBytes)
}

func writeFrameMax(w io.Writer, payload []byte, limit int) error {
	if len(payload) == 0 {
		return ErrEmptyFrame
	}
	if len(payload) > limit {
		return ErrFrameTooLarge
	}
	buf := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(buf, uint32(len(payload)))
	copy(buf[4:], payload)
	_, err := w.Write(buf)
	return err
}

// BodyTimeout is how long a peer has to deliver a complete body frame once central asks for it
// (docs/M3.md §1.1: 30 s). Variable so tests can shorten it.
var BodyTimeout = 30 * time.Second

// WriteBody writes one body frame: a 4-byte big-endian length and the bytes, in a single Write.
// Unlike WriteFrame a zero-length body is valid (empty file / empty stdin). More than MaxFileChunk
// bytes returns ErrFrameTooLarge.
func WriteBody(w io.Writer, b []byte) error {
	if len(b) > MaxFileChunk {
		return ErrFrameTooLarge
	}
	buf := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(buf, uint32(len(b)))
	copy(buf[4:], b)
	_, err := w.Write(buf)
	return err
}

// ReadBodyTimeout reads one body frame of at most max bytes (capped at MaxFileChunk; max<0 is
// treated as 0). The length is checked against max BEFORE allocating, and nothing more is read
// when it is over. It arms a BodyTimeout timer that closes closer (typically the channel) to
// unblock a stalled sender, exactly like ReadRequestTimeout; if r supports SetReadDeadline that is
// used too. Errors are *Error: too_large (length over max), timeout (slow sender; closer has been
// closed), bad_request (truncated). Call it only after authorisation checks have passed.
func ReadBodyTimeout(r io.Reader, closer io.Closer, max int) ([]byte, error) {
	if max < 0 {
		max = 0
	}
	if max > MaxFileChunk {
		max = MaxFileChunk
	}
	if d, ok := r.(readDeadliner); ok {
		if e := d.SetReadDeadline(time.Now().Add(BodyTimeout)); e == nil {
			defer d.SetReadDeadline(time.Time{})
		}
	}
	var t *time.Timer
	if closer != nil {
		t = time.AfterFunc(BodyTimeout, func() { closer.Close() })
	}
	b, err := readBody(r, max)
	if t != nil && !t.Stop() {
		return nil, Errorf(CodeTimeout, "body not received within %s", BodyTimeout)
	}
	switch {
	case err == nil:
		return b, nil
	case errors.Is(err, ErrFrameTooLarge):
		return nil, Errorf(CodeTooLarge, "body exceeds %d bytes", max)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return nil, Errorf(CodeTimeout, "body not received within %s", BodyTimeout)
	}
	return nil, Errorf(CodeBadRequest, "truncated body")
}

func readBody(r io.Reader, max int) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if uint64(n) > uint64(max) {
		return nil, ErrFrameTooLarge
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return buf, nil
}
