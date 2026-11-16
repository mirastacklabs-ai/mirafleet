package mcpserver

import (
	"strings"
	"testing"
)

// Ids are keyed the way the SDK normalises them (float64 -> int64, decoded strings).
func TestIDKey(t *testing.T) {
	same := [][]string{
		{`1`, `1.0`, `1e0`, `10e-1`},
		{`"A"`, `"A"`},
		{`"/"`, `"\/"`},
		{`"caf` + "é" + `"`, `"café"`, `"café"`}, // raw vs ensure_ascii escapes
		{"\"a b\"", `"a b"`},
		{`9007199254740993`, `9007199254740992`}, // beyond 2^53: both go through float64 like the SDK
	}
	for _, g := range same {
		want, ok := idKey([]byte(g[0]))
		if !ok {
			t.Fatalf("idKey(%s) not ok", g[0])
		}
		for _, s := range g[1:] {
			if got, _ := idKey([]byte(s)); got != want {
				t.Errorf("idKey(%s) = %q, want %q (same as %s)", s, got, want, g[0])
			}
		}
	}
	if a, _ := idKey([]byte(`1`)); a == mustKey(t, `"1"`) {
		t.Error(`number 1 and string "1" must differ`)
	}
	for _, bad := range []string{`null`, `{}`, `[1]`, `true`, `nope`} {
		if _, ok := idKey([]byte(bad)); ok {
			t.Errorf("idKey(%s) should not be an id", bad)
		}
	}
}

func mustKey(t *testing.T, s string) string {
	t.Helper()
	k, ok := idKey([]byte(s))
	if !ok {
		t.Fatalf("idKey(%s)", s)
	}
	return k
}

func TestPumpIDNormalisedAcrossReply(t *testing.T) {
	r := newRig(t)
	r.send(t, `{"jsonrpc":"2.0","id":1.0,"method":"ping"}`+"\n"+`{"jsonrpc":"2.0","id":"A","method":"ping"}`+"\n")
	r.inW.Close()
	r.stillOpen(t)
	r.reply(t, result("1"))
	r.stillOpen(t)
	r.reply(t, `{"jsonrpc":"2.0","id":"A","result":{}}`+"\n")
	r.released(t)
}

// Keys match exactly and the last duplicate wins, like the SDK's decoder.
func TestScanHeaderKeys(t *testing.T) {
	cases := []struct {
		name, in string
		tracked  string // expected tracked id key, "" for none
	}{
		{"exact", `{"id":1,"method":"ping"}`, "n:1"},
		{"wrong_case_method", `{"id":1,"Method":"ping"}`, ""},
		{"wrong_case_id", `{"ID":1,"method":"ping"}`, ""},
		{"dup_id_last_wins", `{"id":1,"id":2,"method":"ping"}`, "n:2"},
		{"dup_method_last_wins_drained", `{"method":"tools/call","method":"ping","id":4}`, "n:4"},
		{"dup_method_last_wins_toolscall", `{"method":"ping","method":"tools/call","id":4}`, ""},
		{"nested_keys_ignored", `{"params":{"id":9,"method":"ping"},"id":5,"method":"ping"}`, "n:5"},
		{"payload_first", `{"params":{"a":[1,{"x":"}"}]},"id":6,"method":"tools/list"}`, "n:6"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := newTracker()
			tr.onInput([]byte(c.in))
			if c.tracked == "" {
				if tr.size() != 0 {
					t.Fatalf("tracked %v, want nothing", tr.ids)
				}
				return
			}
			if _, ok := tr.ids[c.tracked]; !ok || tr.size() != 1 {
				t.Fatalf("tracked %v, want only %s", tr.ids, c.tracked)
			}
		})
	}
}

// The SDK answers a batch only when all of it is done, so a batch with a tools/call is not drained.
func TestPumpBatchWithToolsCallNotHeld(t *testing.T) {
	r := newRig(t)
	r.send(t, "["+call("1", "tools/list")+","+call("2", "tools/call")+"]\n")
	r.inW.Close()
	r.released(t)
}

// A big frame's buffer is not kept for the rest of the session.
func TestFramerReleasesBuffer(t *testing.T) {
	n := 0
	f := &framer{max: 1 << 30, fn: func([]byte) { n++ }}
	f.feed([]byte(`{"id":1,"method":"ping","params":{"s":"` + strings.Repeat("x", 300<<10) + `"}}` + "\n"))
	if n != 1 {
		t.Fatalf("callbacks = %d", n)
	}
	if cap(f.buf) > keepBuf {
		t.Fatalf("kept %d bytes after a large frame (limit %d)", cap(f.buf), keepBuf)
	}
	f.feed([]byte(`{"id":2,"method":"ping"}` + "\n"))
	if n != 2 {
		t.Fatalf("callbacks = %d after the next frame", n)
	}
}
