package transport

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"testing"
)

func goodEvent() Event {
	return Event{Type: EventCredentialUse, Session: "s1", Credential: "deploy", Host: "gitlab.test",
		Status: EventOK, Count: 3, WindowMs: 1700000000000, WindowS: 60}
}

func TestEventBatchRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := &EventBatch{Events: []Event{goodEvent(), goodEvent()}, Dropped: 4}
	if err := WriteEventBatch(&buf, in); err != nil {
		t.Fatal(err)
	}
	if err := WriteEventBatch(&buf, &EventBatch{Dropped: 1}); err != nil {
		t.Fatal(err)
	}
	out, err := ReadEventBatch(&buf)
	if err != nil || len(out.Events) != 2 || out.Dropped != 4 {
		t.Fatalf("got %+v %v", out, err)
	}
	if out, err = ReadEventBatch(&buf); err != nil || out.Dropped != 1 {
		t.Fatalf("dropped-only batch: %+v %v", out, err)
	}
	if _, err = ReadEventBatch(&buf); err != io.EOF {
		t.Fatalf("clean end: %v", err)
	}
}

func TestEventBatchLimits(t *testing.T) {
	many := make([]Event, MaxBatchEvents+1)
	for i := range many {
		many[i] = goodEvent()
	}
	if err := (&EventBatch{Events: many}).Validate(); err == nil {
		t.Error("129 events accepted")
	}
	if err := (&EventBatch{Events: many[:MaxBatchEvents]}).Validate(); err != nil {
		t.Errorf("128 events: %v", err)
	}
	mut := func(f func(*Event)) *EventBatch { e := goodEvent(); f(&e); return &EventBatch{Events: []Event{e}} }
	for name, b := range map[string]*EventBatch{
		"empty":       {},
		"neg dropped": {Dropped: -1},
		"type":        mut(func(e *Event) { e.Type = "credential.read" }),
		"session":     mut(func(e *Event) { e.Session = "" }),
		"credential":  mut(func(e *Event) { e.Credential = "a/b" }),
		"host":        mut(func(e *Event) { e.Host = "" }),
		"host long":   mut(func(e *Event) { e.Host = strings.Repeat("a", MaxEventFieldLen+1) }),
		"status":      mut(func(e *Event) { e.Status = "weird" }),
		"count 0":     mut(func(e *Event) { e.Count = 0 }),
		"window":      mut(func(e *Event) { e.WindowS = 0 }),
		"window big":  mut(func(e *Event) { e.WindowS = 7200 }),
		"window neg":  mut(func(e *Event) { e.WindowMs = -1 }),
	} {
		if err := b.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Writer refuses an oversized frame.
	long := goodEvent()
	long.Host = strings.Repeat("h", MaxEventFieldLen)
	long.Session = strings.Repeat("s", MaxSessionIDBytes)
	big := &EventBatch{}
	for i := 0; i < MaxBatchEvents; i++ {
		long.Credential = "c" + strings.Repeat("x", 60)
		big.Events = append(big.Events, long)
	}
	if err := WriteEventBatch(io.Discard, big); err == nil {
		t.Error("oversized batch written")
	}
}

func TestReadEventBatchHostile(t *testing.T) {
	frame := func(payload string) *bytes.Reader {
		var h [4]byte
		binary.BigEndian.PutUint32(h[:], uint32(len(payload)))
		return bytes.NewReader(append(h[:], payload...))
	}
	ev := `{"type":"credential.use","session":"s","credential":"c","host":"h.test","status":"ok","count":1,"window_ms":1,"window_s":60}`
	for name, r := range map[string]io.Reader{
		"zero length":   bytes.NewReader([]byte{0, 0, 0, 0}),
		"huge length":   bytes.NewReader([]byte{0xff, 0xff, 0xff, 0xff}),
		"over limit":    bytes.NewReader([]byte{0, 1, 0, 1}),
		"short header":  bytes.NewReader([]byte{0, 0}),
		"short body":    bytes.NewReader([]byte{0, 0, 0, 9, '{'}),
		"unknown field": frame(`{"events":[` + ev + `],"x":1}`),
		"case":          frame(`{"Events":[` + ev + `]}`),
		"event case":    frame(`{"events":[` + strings.Replace(ev, `"type"`, `"Type"`, 1) + `]}`),
		"event extra":   frame(`{"events":[` + strings.Replace(ev, `}`, `,"url":"x"}`, 1) + `]}`),
		"duplicate":     frame(`{"events":[` + ev + `],"events":[]}`),
		"not json":      frame(`nope`),
	} {
		if _, err := ReadEventBatch(r); err == nil || err == io.EOF {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := ReadEventBatch(frame(`{"events":[` + ev + `]}`)); err != nil {
		t.Errorf("valid frame refused: %v", err)
	}
}
