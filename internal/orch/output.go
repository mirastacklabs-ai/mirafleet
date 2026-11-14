package orch

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Sanitize returns b as valid UTF-8 text that is safe to hand to a model and a terminal-less
// reader: invalid bytes become U+FFFD (one per byte), ANSI escape sequences (CSI, OSC and other
// ESC sequences) are removed, C0 controls except \n and \t, DEL and the C1 controls are dropped,
// and \r\n becomes \n (a lone \r is dropped). Unicode tag characters (U+E0000-E007F) are dropped
// and every other format character (category Cf: bidi overrides and isolates, zero-width
// characters, BOM, ...) is replaced by a visible <U+XXXX>, so nothing invisible can hide text.
// The result never contains ESC or \r, and Sanitize(Sanitize(x)) == Sanitize(x).
//
// The cost is linear in len(b): the OSC terminator search is cached.
func Sanitize(b []byte) string {
	var sb strings.Builder
	sb.Grow(len(b))
	var tc termCache
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c == 0x1b:
			i += escLen(b, i, &tc)
			continue
		case c == '\n' || c == '\t':
			sb.WriteByte(c)
			i++
			continue
		case c < 0x20 || c == 0x7f: // other C0 (incl. \r) and DEL
			i++
			continue
		case c < 0x80:
			sb.WriteByte(c)
			i++
			continue
		}
		r, n := utf8.DecodeRune(b[i:])
		switch {
		case r == utf8.RuneError && n == 1:
			sb.WriteString("�")
		case r >= 0x80 && r <= 0x9f: // C1 controls
		case r >= 0xE0000 && r <= 0xE007F: // tag characters
		case unicode.Is(unicode.Cf, r):
			fmt.Fprintf(&sb, "<U+%04X>", r)
		default:
			sb.Write(b[i : i+n])
		}
		i += n
	}
	return sb.String()
}

// maxOSC bounds how far an OSC terminator is searched; an unterminated one drops only the ESC.
const maxOSC = 4096

// termCache remembers the last terminator search so that scanning stays linear overall: the
// first BEL or ESC \ at or after from is the same for every later query that starts at or
// before it.
type termCache struct {
	from, res int // res is -1 when there is no terminator at or after from
	valid     bool
}

// next returns the index of the first terminator (BEL, or ESC followed by '\') at or after from,
// and its length; -1 when there is none.
func (tc *termCache) next(b []byte, from int) (int, int) {
	if !tc.valid || from < tc.from || (tc.res >= 0 && tc.res < from) {
		tc.from, tc.res, tc.valid = from, -1, true
		for j := from; j < len(b); j++ {
			if b[j] == 0x07 || (b[j] == 0x1b && j+1 < len(b) && b[j+1] == '\\') {
				tc.res = j
				break
			}
		}
	}
	if tc.res < 0 {
		return -1, 0
	}
	if b[tc.res] == 0x07 {
		return tc.res, 1
	}
	return tc.res, 2
}

// escLen returns how many bytes of the escape sequence at b[i] (an ESC) to drop (at least 1).
func escLen(b []byte, i int, tc *termCache) int {
	s := b[i:]
	if len(s) < 2 {
		return 1
	}
	switch s[1] {
	case '[': // CSI: params 0x30-0x3f, intermediates 0x20-0x2f, final 0x40-0x7e
		i := 2
		for i < len(s) && s[i] >= 0x30 && s[i] <= 0x3f {
			i++
		}
		for i < len(s) && s[i] >= 0x20 && s[i] <= 0x2f {
			i++
		}
		if i < len(s) && s[i] >= 0x40 && s[i] <= 0x7e {
			return i + 1
		}
		return 1
	case ']', 'P', 'X', '^', '_': // OSC, DCS, SOS, PM, APC: up to BEL or ST (ESC \)
		if t, n := tc.next(b, i+2); t >= 0 && t-i < maxOSC {
			return t - i + n
		}
		return 1
	}
	// Other escapes: intermediates 0x20-0x2f then one final byte 0x30-0x7e.
	j := 1
	for j < len(s) && s[j] >= 0x20 && s[j] <= 0x2f {
		j++
	}
	if j < len(s) && s[j] >= 0x30 && s[j] <= 0x7e {
		return j + 1
	}
	return 1
}

// Capped is stdout and stderr after CapOutput.
type Capped struct {
	Stdout       string
	Stderr       string
	Truncated    bool
	OmittedBytes int // sanitised UTF-8 bytes dropped, both streams together
}

// jsonCost is the worst-case encoded size of r inside a JSON string: encoding/json escapes ",
// \, \n, \t as two bytes and <, >, & and U+2028/9 as six (its default HTML escaping). Sanitised
// text has no other control characters.
func jsonCost(r rune) int {
	switch r {
	case '"', '\\', '\n', '\t':
		return 2
	case '<', '>', '&', ' ', ' ':
		return 6
	}
	return utf8.RuneLen(r)
}

func cost(s string) int {
	n := 0
	for _, r := range s {
		n += jsonCost(r)
	}
	return n
}

func markerFor(n int) string { return fmt.Sprintf("\n[mirafleet: %d bytes omitted]\n", n) }

// CapOutput sanitises both streams and fits them in budget, counted as the JSON-encoded size of
// Stdout plus Stderr (so it holds after encoding, even with the default HTML escaping). A stream
// that does not fit keeps 1/3 head and 2/3 tail with a "[mirafleet: N bytes omitted]" marker in
// between, cut on rune boundaries. The budget is shared; a non-empty stderr keeps at least 1/3 of
// it. With a budget too small for a marker the stream is dropped entirely (Truncated is set).
func CapOutput(stdout, stderr []byte, budget int) Capped {
	so, se := Sanitize(stdout), Sanitize(stderr)
	if budget < 0 {
		budget = 0
	}
	co, ce := cost(so), cost(se)
	if co+ce <= budget {
		return Capped{Stdout: so, Stderr: se}
	}
	// Shares: stderr gets what stdout leaves unused, but at least a third of the budget.
	eAlloc := budget / 3
	if m := budget - co; m > eAlloc {
		eAlloc = m
	}
	if eAlloc > ce {
		eAlloc = ce
	}
	oAlloc := budget - eAlloc
	var out Capped
	var n1, n2 int
	out.Stdout, n1 = fit(so, co, oAlloc)
	out.Stderr, n2 = fit(se, ce, eAlloc)
	out.OmittedBytes = n1 + n2
	out.Truncated = n1+n2 > 0
	return out
}

// fit shrinks s (encoded size c) to at most alloc; it returns the result and the bytes omitted.
func fit(s string, c, alloc int) (string, int) {
	if c <= alloc {
		return s, 0
	}
	avail := alloc - cost(markerFor(len(s))) // upper bound: the real N is at most len(s)
	if avail < 0 {
		return "", len(s)
	}
	headB := avail / 3
	tailB := avail - headB
	hEnd, used := 0, 0
	for i, r := range s {
		w := jsonCost(r)
		if used+w > headB {
			break
		}
		used += w
		hEnd = i + utf8.RuneLen(r)
	}
	tStart, used := len(s), 0
	for i := len(s); i > hEnd; {
		r, n := utf8.DecodeLastRuneInString(s[:i])
		w := jsonCost(r)
		if used+w > tailB {
			break
		}
		used += w
		i -= n
		tStart = i
	}
	omitted := tStart - hEnd
	return s[:hEnd] + markerFor(omitted) + s[tStart:], omitted
}
