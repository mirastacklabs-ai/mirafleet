package orch

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func encLen(t testing.TB, s string) int {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return len(b) - 2
}

func checkSanitized(t testing.TB, s string) {
	t.Helper()
	if !utf8.ValidString(s) {
		t.Fatalf("invalid UTF-8: %q", s)
	}
	for _, r := range s {
		if r == 0x1b || r == '\r' || r == 0x7f || (r < 0x20 && r != '\n' && r != '\t') || (r >= 0x80 && r <= 0x9f) ||
			unicode.Is(unicode.Cf, r) {
			t.Fatalf("control character %U in %q", r, s)
		}
	}
}

func TestSanitize(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"plain\ttext\n", "plain\ttext\n"},
		{"a\r\nb\r\n", "a\nb\n"},
		{"\x1b[31mred\x1b[0m", "red"},
		{"\x1b[1;32;40mx\x1b[K", "x"},
		{"\x1b]0;title\x07after", "after"},
		{"\x1b]8;;http://x\x1b\\link\x1b]8;;\x1b\\", "link"},
		{"bell\x07nul\x00bs\x08", "bellnulbs"},
		{"bad\xffbyte", "bad�byte"},
		{"\xe2\x82", "��"},
		{"del\x7fx", "delx"},
		{"c1\u009bx", "c1x"},
		{"\x1b(Bx", "x"},
		{"trailing\x1b", "trailing"},
		{"\x1b]unterminated", "]unterminated"},
		{"héllo 世界 🙂", "héllo 世界 🙂"},
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

func TestCapOutputNoTruncation(t *testing.T) {
	c := CapOutput([]byte("out\n"), []byte("err\n"), 1000)
	if c.Truncated || c.OmittedBytes != 0 || c.Stdout != "out\n" || c.Stderr != "err\n" {
		t.Fatalf("%+v", c)
	}
}

func TestCapOutputHeadTailAndMarker(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&sb, "line %04d\n", i)
	}
	in := []byte(sb.String())
	c := CapOutput(in, nil, 3000)
	if !c.Truncated || c.OmittedBytes <= 0 {
		t.Fatalf("%+v", c)
	}
	if !strings.HasPrefix(c.Stdout, "line 0000\n") || !strings.HasSuffix(c.Stdout, "line 4999\n") {
		t.Fatal("head and tail must be kept")
	}
	marker := fmt.Sprintf("[mirafleet: %d bytes omitted]", c.OmittedBytes)
	i := strings.Index(c.Stdout, marker)
	if i < 0 {
		t.Fatalf("marker %q missing", marker)
	}
	// 1/3 head, 2/3 tail (within a line or two).
	head, tail := i, len(c.Stdout)-i-len(marker)
	if head*2 > tail+200 || tail < head*2-200 {
		t.Fatalf("head=%d tail=%d, want about 1:2", head, tail)
	}
	if n := encLen(t, c.Stdout); n > 3000 {
		t.Fatalf("encoded %d > 3000", n)
	}
	if len(in)-c.OmittedBytes != len(c.Stdout)-len(markerFor(c.OmittedBytes)) {
		t.Fatal("omitted bytes do not add up")
	}
}

func TestCapOutputStderrKeepsAThird(t *testing.T) {
	big := []byte(strings.Repeat("o", 100000))
	errs := []byte(strings.Repeat("e", 100000))
	const budget = 6000
	c := CapOutput(big, errs, budget)
	if got := strings.Count(c.Stderr, "e"); got < budget/3-100 {
		t.Fatalf("stderr kept %d, want about %d", got, budget/3)
	}
	if n := encLen(t, c.Stdout) + encLen(t, c.Stderr); n > budget {
		t.Fatalf("total %d > %d", n, budget)
	}
	// Small stderr is kept whole and stdout gets the rest.
	c = CapOutput(big, []byte("boom\n"), budget)
	if c.Stderr != "boom\n" || encLen(t, c.Stdout)+5 > budget {
		t.Fatalf("stderr = %q", c.Stderr)
	}
	// Small stdout is kept whole and stderr gets the rest.
	c = CapOutput([]byte("ok"), errs, budget)
	if c.Stdout != "ok" || encLen(t, c.Stderr) > budget-2 {
		t.Fatal("stdout not kept")
	}
}

func TestCapOutputWorstCaseEncoding(t *testing.T) {
	// Every rune is 6 bytes once JSON-encoded (HTML escaping) or multi-byte UTF-8.
	for _, in := range []string{
		strings.Repeat("<>&", 5000), strings.Repeat(" ", 4000), strings.Repeat("世", 4000),
		strings.Repeat("\"\\\n\t", 4000), strings.Repeat("🙂", 4000),
	} {
		for _, budget := range []int{0, 1, 40, 100, 4096, 30720} {
			c := CapOutput([]byte(in), []byte(in), budget)
			if n := encLen(t, c.Stdout) + encLen(t, c.Stderr); n > budget {
				t.Fatalf("budget %d: encoded %d", budget, n)
			}
			checkSanitized(t, c.Stdout)
			checkSanitized(t, c.Stderr)
		}
	}
}

func FuzzSanitize(f *testing.F) {
	for _, s := range []string{"", "a\r\nb", "\x1b[31m", "\x1b]0;t\x07", "\xff\xfe", "\x1b", "\x1b[", "\x1b]", "\x1b\x1b[1m", "\x1b[3\x011m", "é\u009b", "a\xe2\x80\xaeb", "\xf3\xa0\x81\x81x", "\xe2\x80\x8b\xe2\x80\x8d\xef\xbb\xbf", "\x1b]aaa\x1b", "\x1b]\x1b]\x07"} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		got := Sanitize(b)
		checkSanitized(t, got)
		if again := Sanitize([]byte(got)); again != got {
			t.Fatalf("not idempotent: %q -> %q", got, again)
		}
		if len(got) > 4*len(b) { // an invalid byte grows to 3, a 2-byte format character to 8
			t.Fatalf("output grew: %d -> %d", len(b), len(got))
		}
	})
}

func FuzzCapOutput(f *testing.F) {
	f.Add([]byte("hello"), []byte("world"), 10)
	f.Add([]byte(strings.Repeat("a\xe2\x80\xae\xe2\x80\x8b\xf3\xa0\x81\x81", 100)), []byte("\xe2\x80\x8d\xef\xbb\xbfx"), 200)
	f.Add([]byte(strings.Repeat("<>&\"\n", 200)), []byte("\xffe "), 300)
	f.Add([]byte(strings.Repeat("世", 500)), []byte(strings.Repeat("x", 500)), 4096)
	f.Fuzz(func(t *testing.T, so, se []byte, budget int) {
		if budget < 0 {
			budget = -budget
		}
		budget %= 20000
		c := CapOutput(so, se, budget)
		checkSanitized(t, c.Stdout)
		checkSanitized(t, c.Stderr)
		b, err := json.Marshal(c) // fields marshal as strings under their Go names
		if err != nil {
			t.Fatal(err)
		}
		_ = b
		if n := encLen(t, c.Stdout) + encLen(t, c.Stderr); n > budget {
			t.Fatalf("encoded %d > budget %d", n, budget)
		}
		cs, ce := Sanitize(so), Sanitize(se)
		if cost(cs)+cost(ce) <= budget {
			if c.Truncated || c.Stdout != cs || c.Stderr != ce {
				t.Fatalf("needlessly truncated: %+v", c)
			}
		} else if !c.Truncated || c.OmittedBytes <= 0 {
			t.Fatalf("over budget but not marked truncated: %+v", c)
		}
		if c.OmittedBytes < 0 || c.OmittedBytes > len(cs)+len(ce) {
			t.Fatalf("omitted %d out of range", c.OmittedBytes)
		}
	})
}
