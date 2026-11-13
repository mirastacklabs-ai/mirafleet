package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestJSONLBackwardCompatible(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	var buf bytes.Buffer
	var s Sink = New(&buf)
	err := s.Record(context.Background(), Event{Time: ts, Runner: "r", Action: "a", Outcome: "ok", DurationMS: 3})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"time":"2026-01-02T03:04:05Z","runner":"r","action":"a","outcome":"ok","exit_code":0,"duration_ms":3}` + "\n"
	if buf.String() != want {
		t.Fatalf("got  %s\nwant %s", buf.String(), want)
	}
}

func TestNewFieldsSerialise(t *testing.T) {
	var buf bytes.Buffer
	w := New(&buf)
	w.Log(Event{Action: "x", Outcome: "ok", Source: "api", ActorKind: "operator", Actor: "a", Remote: "r", Fleet: "f", Subject: "s", RequestID: "q"})
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"source", "actor_kind", "actor", "remote", "fleet", "subject", "request_id"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing %s", k)
		}
	}
}

func TestNilWriterRecord(t *testing.T) {
	var w *Writer
	if err := w.Record(context.Background(), Event{}); err != nil {
		t.Fatal(err)
	}
}

func TestRecordHonoursCancelledContext(t *testing.T) {
	var buf bytes.Buffer
	w := New(&buf)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.Record(ctx, Event{Action: "a"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("wrote after cancel: %q", buf.String())
	}
}
