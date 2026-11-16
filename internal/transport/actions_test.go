package transport

import (
	"encoding/json"
	"strings"
	"testing"
)

func sp(s string) *string { return &s }

func TestHeartbeatActionsBounds(t *testing.T) {
	ok := func(name string, a []ActionInfo, want bool) {
		t.Helper()
		err := (&Heartbeat{Hostname: "h", Actions: a}).Validate()
		if (err == nil) != want {
			t.Errorf("%s: err=%v want ok=%v", name, err, want)
		}
	}
	one := func(p ActionParam) []ActionInfo {
		return []ActionInfo{{Name: "a", Params: []ActionParam{{Name: "p", Pattern: "x"}, p}}}
	}
	many := func(n int) []ActionInfo {
		out := make([]ActionInfo, n)
		for i := range out {
			out[i] = ActionInfo{Name: "a"}
		}
		return out
	}
	ok("none", nil, true)
	ok("128 actions", many(128), true)
	ok("129 actions", many(129), false)
	ok("name 64", []ActionInfo{{Name: strings.Repeat("n", 64)}}, true)
	ok("name 65", []ActionInfo{{Name: strings.Repeat("n", 65)}}, false)
	ok("empty name", []ActionInfo{{Name: ""}}, false)
	ok("pattern 256", one(ActionParam{Name: "q", Pattern: strings.Repeat("x", 256)}), true)
	ok("pattern 257", one(ActionParam{Name: "q", Pattern: strings.Repeat("x", 257)}), false)
	ok("default 256", one(ActionParam{Name: "q", Pattern: "x", Default: sp(strings.Repeat("x", 256))}), true)
	ok("default 257", one(ActionParam{Name: "q", Pattern: "x", Default: sp(strings.Repeat("x", 257))}), false)
	ok("empty default", one(ActionParam{Name: "q", Pattern: "x", Default: sp("")}), true)
	ps := func(n int) []ActionInfo {
		a := ActionInfo{Name: "a"}
		for i := 0; i < n; i++ {
			a.Params = append(a.Params, ActionParam{Name: "p", Pattern: "x"})
		}
		return []ActionInfo{a}
	}
	ok("32 params", ps(32), true)
	ok("33 params", ps(33), false)
	ok("utf8 printable", []ActionInfo{{Name: "café"}}, true)
	for _, bad := range []string{"a\x00b", "a\nb", "a\x1bb", "a\x7fb", "a\xffb"} {
		ok("name "+bad, []ActionInfo{{Name: bad}}, false)
		ok("pattern "+bad, one(ActionParam{Name: "q", Pattern: bad}), false)
		ok("default "+bad, one(ActionParam{Name: "q", Pattern: "x", Default: sp(bad)}), false)
		ok("param name "+bad, one(ActionParam{Name: bad, Pattern: "x"}), false)
	}
}

// An older heartbeat (no actions key) decodes and validates; empty actions marshal to no key.
func TestHeartbeatWithoutActions(t *testing.T) {
	var hb Heartbeat
	if err := json.Unmarshal([]byte(`{"hostname":"h","os":"linux","arch":"amd64","version":"v","slots":2}`), &hb); err != nil {
		t.Fatal(err)
	}
	if err := hb.Validate(); err != nil || hb.Actions != nil {
		t.Fatalf("%v %+v", err, hb)
	}
	b, _ := json.Marshal(hb)
	if strings.Contains(string(b), "actions") {
		t.Fatalf("empty actions marshalled: %s", b)
	}
}

func TestHeartbeatActionsWire(t *testing.T) {
	hb := Heartbeat{Hostname: "h", Actions: []ActionInfo{{Name: "a", Params: []ActionParam{{Name: "n", Pattern: "[0-9]+", Default: sp("5")}, {Name: "m", Pattern: "x"}}}}}
	b, _ := json.Marshal(hb)
	if !strings.Contains(string(b), `"actions":[{"name":"a","params":[{"name":"n","pattern":"[0-9]+","default":"5"},{"name":"m","pattern":"x"}]}]`) {
		t.Fatalf("wire: %s", b)
	}
}

// bigActions builds a valid list (<= MaxHeartbeatActions) whose encoded size is exactly want.
func bigActions(t *testing.T, want int) []ActionInfo {
	t.Helper()
	filler := func(n int) ActionInfo {
		return ActionInfo{Name: "a" + strings.Repeat("n", n%50), Params: []ActionParam{
			{Name: "p", Pattern: strings.Repeat("x", 60), Default: sp(strings.Repeat("y", 10))}}}
	}
	var as []ActionInfo
	for len(as) < MaxHeartbeatActions && ActionsEncodedSize(append(as[:len(as):len(as)], filler(len(as)))) <= want-400 {
		as = append(as, filler(len(as)))
	}
	mk := func(l1, l2 int) []ActionInfo {
		x := ActionInfo{Name: "z", Params: []ActionParam{{Name: "p", Pattern: strings.Repeat("x", l1), Default: sp(strings.Repeat("y", l2))}}}
		return append(as[:len(as):len(as)], x)
	}
	delta := want - ActionsEncodedSize(mk(0, 0))
	l1 := min(delta, MaxActionPatternBytes)
	c := mk(l1, delta-l1)
	if delta < 0 || delta-l1 > MaxActionDefaultBytes || ActionsEncodedSize(c) != want {
		t.Fatalf("cannot build actions of %d bytes", want)
	}
	return c
}

func TestHeartbeatActionsByteCap(t *testing.T) {
	at := bigActions(t, MaxActionsBytes)
	if err := (&Heartbeat{Hostname: "h", Actions: at}).Validate(); err != nil {
		t.Fatalf("list at the cap refused: %v", err)
	}
	over := bigActions(t, MaxActionsBytes+1)
	if err := (&Heartbeat{Hostname: "h", Actions: over}).Validate(); err == nil {
		t.Fatal("list one byte over the cap accepted")
	}
}
