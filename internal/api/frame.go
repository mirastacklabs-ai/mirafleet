package api

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
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
