package orch

import (
	"strings"
	"testing"
	"time"
)

func TestSanitizeInvisible(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"a\U000E0041\U000E0042b", "ab"}, // tag characters vanish
		{"\U000E0000\U000E007F", ""},     // both ends of the tag block
		{"x\u202Ey", "x<U+202E>y"},       // bidi override
		{"\u202A\u202B\u202C\u202D", "<U+202A><U+202B><U+202C><U+202D>"},
		{"\u2066\u2067\u2068\u2069", "<U+2066><U+2067><U+2068><U+2069>"},
		{"\u200E\u200F\u061C", "<U+200E><U+200F><U+061C>"},
		{"a\u200Bb\u200Cc\u200Dd\u2060e\uFEFFf", "a<U+200B>b<U+200C>c<U+200D>d<U+2060>e<U+FEFF>f"},
		{"keep\n\ttabs", "keep\n\ttabs"},
		{"\u00ADsoft", "<U+00AD>soft"},
		{"\x1b[31m\u200B\x1b[0m", "<U+200B>"},
		{"<U+200B>", "<U+200B>"}, // already visible text is untouched
	} {
		got := Sanitize([]byte(tc.in))
		if got != tc.want {
			t.Errorf("Sanitize(%q) = %q, want %q", tc.in, got, tc.want)
		}
		checkSanitized(t, got)
		if again := Sanitize([]byte(got)); again != got {
			t.Errorf("not idempotent: %q -> %q", got, again)
		}
	}
}

func TestCapOutputInvisibleCostHolds(t *testing.T) {
	in := []byte(strings.Repeat("a\u202E\u200B\U000E0041\"<", 300))
	for _, budget := range []int{0, 50, 200, 1000, 5000} {
		c := CapOutput(in, in, budget)
		if n := encLen(t, c.Stdout) + encLen(t, c.Stderr); n > budget {
			t.Fatalf("budget %d: encoded %d", budget, n)
		}
	}
}

func TestSanitizeOSCLinear(t *testing.T) {
	for _, unit := range []string{"\x1b]", "\x1bP", "\x1b]a\x1b", "\x1b]\x1b\\\x1b]"} {
		in := []byte(strings.Repeat(unit, (1<<20)/len(unit)))
		start := time.Now()
		got := Sanitize(in)
		if d := time.Since(start); d > time.Second {
			t.Fatalf("%q: Sanitize took %v", unit, d)
		}
		checkSanitized(t, got)
	}
	// Terminator just inside and just outside the window.
	in := "\x1b]" + strings.Repeat("x", maxOSC-3) + "\x07tail"
	if got := Sanitize([]byte(in)); got != "tail" {
		t.Fatalf("terminator inside window: %q", got)
	}
	in = "\x1b]" + strings.Repeat("x", maxOSC-2) + "\x07tail"
	if got := Sanitize([]byte(in)); !strings.HasSuffix(got, "tail") || len(got) < maxOSC {
		t.Fatalf("terminator outside window dropped text: %d bytes", len(got))
	}
}
