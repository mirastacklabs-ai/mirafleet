package osb

import (
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

func readAll(t *testing.T, in string, maxLine, maxEvent int) ([]Event, error) {
	t.Helper()
	r := NewSSEReader(iotest.OneByteReader(strings.NewReader(in)), maxLine, maxEvent)
	var evs []Event
	for {
		ev, err := r.Next()
		if err == io.EOF {
			return evs, nil
		}
		if err != nil {
			return evs, err
		}
		evs = append(evs, ev)
	}
}

func TestSSEHappy(t *testing.T) {
	in := ": keepalive\r\n\r\n" +
		"data: {\"type\":\"init\",\"text\":\"c1\"}\r\n\r\n" +
		"data: {\"type\":\"stdout\",\n" +
		"data: \"text\":\"a\\nb\"}\n\n" + // multi-line data joins with \n
		"id: 7\nretry: 5\nunknown: x\nevent: ignored-name\ndata: {\"type\":\"stderr\",\"text\":\"e\"}\n\n" +
		"event: error\ndata: {\"error\":{\"ename\":\"CommandExecError\",\"evalue\":\"3\",\"exit_code\":3}}\n\n" +
		"\n\n" +
		"data:{\"type\":\"execution_complete\"}\n\n"
	evs, err := readAll(t, in, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 5 {
		t.Fatalf("got %d events: %+v", len(evs), evs)
	}
	if evs[0].SessionID() != "c1" || evs[1].Text != "a\nb" || evs[2].Type != EventStderr {
		t.Fatalf("%+v", evs)
	}
	if evs[3].Type != EventError || evs[3].Error == nil || *evs[3].Error.ExitCode != 3 {
		t.Fatalf("error event %+v", evs[3])
	}
	if evs[4].Type != EventComplete || evs[4].ExitCode != nil {
		t.Fatalf("complete %+v", evs[4])
	}
}

func TestSSEInitID(t *testing.T) {
	evs, err := readAll(t, "data: {\"type\":\"init\",\"id\":\"zz\"}\n\n", 0, 0)
	if err != nil || evs[0].SessionID() != "zz" {
		t.Fatalf("%v %+v", err, evs)
	}
}

func TestSSEBounds(t *testing.T) {
	long := "data: {\"type\":\"stdout\",\"text\":\"" + strings.Repeat("x", 300) + "\"}\n\n"
	if _, err := readAll(t, long, 100, 0); !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("want line limit, got %v", err)
	}
	// no newline at all: bounded memory, still ErrLineTooLong
	if _, err := readAll(t, strings.Repeat("a", 100000), 1000, 0); !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("want line limit, got %v", err)
	}
	many := strings.Repeat("data: {\"a\":1}\n", 50) + "\n"
	if _, err := readAll(t, many, 100, 200); !errors.Is(err, ErrEventTooLarge) {
		t.Fatalf("want event limit, got %v", err)
	}
	// exactly at the line limit is fine
	line := "data: " + strings.Repeat("y", 20)
	if _, err := readAll(t, line+"\n\n", len(line), 0); errors.Is(err, ErrLineTooLong) {
		t.Fatalf("limit is inclusive: %v", err)
	}
}

func TestSSEMalformed(t *testing.T) {
	cases := map[string]error{
		"data: not json\n\n":                         ErrMalformedEvent,
		"data: {\"text\":\"no type\"}\n\n":           ErrMalformedEvent,
		"data: {\"type\":\"stdout\",\"text\":1}\n\n": ErrMalformedEvent,
		"data: {\"type\":\"stdout\"}\n":              io.ErrUnexpectedEOF, // unfinished event
		"data: {\"type\":\"stdout\"}":                io.ErrUnexpectedEOF, // partial line
		"data: {\"type\":\"stdout\"}\n\ndata: {":     io.ErrUnexpectedEOF,
	}
	for in, want := range cases {
		_, err := readAll(t, in, 0, 0)
		if !errors.Is(err, want) {
			t.Errorf("%q: want %v got %v", in, want, err)
		}
	}
	// error is sticky and the payload is not echoed
	r := NewSSEReader(strings.NewReader("data: SECRET-OUTPUT\n\n"), 0, 0)
	_, e1 := r.Next()
	_, e2 := r.Next()
	if e1 == nil || e1 != e2 || strings.Contains(e1.Error(), "SECRET-OUTPUT") {
		t.Fatalf("%v %v", e1, e2)
	}
}

func TestSSEEmptyAndComments(t *testing.T) {
	evs, err := readAll(t, ": a\n\n\n: b\n", 0, 0)
	if err != nil || len(evs) != 0 {
		t.Fatalf("%v %v", err, evs)
	}
}
