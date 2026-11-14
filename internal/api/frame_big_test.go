package api

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

func TestReadFrameLargeIncremental(t *testing.T) {
	payload := bytes.Repeat([]byte{7}, 300<<10)
	var buf bytes.Buffer
	hdr := make([]byte, 4)
	binary.BigEndian.PutUint32(hdr, uint32(len(payload)))
	buf.Write(hdr)
	buf.Write(payload)
	got, err := ReadFrame(&buf, 1<<20)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("round trip: %d bytes, %v", len(got), err)
	}
	// Announced 900 KiB, only 10 bytes follow: unexpected EOF, nothing returned.
	binary.BigEndian.PutUint32(hdr, 900<<10)
	short := append(append([]byte(nil), hdr...), make([]byte, 10)...)
	if b, err := ReadFrame(bytes.NewReader(short), 1<<20); !errors.Is(err, io.ErrUnexpectedEOF) || b != nil {
		t.Fatalf("short: %v %d", err, len(b))
	}
	if b, err := readBody(bytes.NewReader(short), 1<<20); !errors.Is(err, io.ErrUnexpectedEOF) || b != nil {
		t.Fatalf("short body: %v %d", err, len(b))
	}
}
