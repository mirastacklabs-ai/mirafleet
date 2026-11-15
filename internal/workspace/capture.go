package workspace

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// EncodedLen returns an upper bound (exact except for \b and \f, which it over-counts) of the
// bytes s occupies inside a JSON string literal (quotes excluded), as produced by
// encoding/json with its default HTML escaping. s must be valid UTF-8; invalid bytes are
// counted as the 6-byte � escape encoding/json emits for them.
func EncodedLen(s string) int {
	n := 0
	for i := 0; i < len(s); {
		r, w := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && w == 1 {
			n += 6
		} else {
			n += runeCost(r)
		}
		i += w
	}
	return n
}

func runeCost(r rune) int {
	switch {
	case r == '"' || r == '\\' || r == '\n' || r == '\r' || r == '\t':
		return 2
	case r < 0x20, r == '<', r == '>', r == '&', r == 0x2028, r == 0x2029:
		return 6
	}
	return utf8.RuneLen(r)
}

// markerReserve is the budget held back for the truncation marker.
const markerReserve = 64

// Capture is an io.Writer that keeps the head and the tail of what is written, so that the
// JSON-encoded form of its Result (without surrounding quotes) never exceeds the budget,
// whatever bytes are written. Input is sanitised to valid UTF-8 (invalid bytes become U+FFFD).
// Not safe for concurrent use. Write never fails.
type Capture struct {
	headBudget, tailBudget int
	budget                 int

	head     []byte
	headCost int
	headDone bool

	tail     []rune
	tailOff  int // tail[tailOff:] is live; the prefix is reclaimed lazily
	tailCost int

	pending []byte // incomplete UTF-8 prefix carried to the next Write
	total   int64  // raw bytes written
	dropped int64  // sanitised bytes dropped from the middle
}

// NewCapture returns a Capture whose encoded result is at most budget bytes.
func NewCapture(budget int) *Capture {
	if budget < 0 {
		budget = 0
	}
	avail := budget - markerReserve
	if avail < 0 {
		avail = 0
	}
	return &Capture{budget: budget, headBudget: avail / 2, tailBudget: avail - avail/2}
}

// Total returns the number of raw bytes written.
func (c *Capture) Total() int64 { return c.total }

// Write implements io.Writer.
func (c *Capture) Write(p []byte) (int, error) {
	c.total += int64(len(p))
	b := p
	if len(c.pending) > 0 {
		b = append(c.pending, p...)
		c.pending = nil
	}
	for len(b) > 0 {
		if !utf8.FullRune(b) {
			c.pending = append([]byte(nil), b...)
			break
		}
		r, w := utf8.DecodeRune(b)
		c.add(r)
		b = b[w:]
	}
	return len(p), nil
}

func (c *Capture) add(r rune) {
	cost := runeCost(r)
	if !c.headDone {
		if c.headCost+cost <= c.headBudget {
			c.head = utf8.AppendRune(c.head, r)
			c.headCost += cost
			return
		}
		c.headDone = true
	}
	if cost > c.tailBudget {
		c.dropped += int64(utf8.RuneLen(r))
		return
	}
	c.tail = append(c.tail, r)
	c.tailCost += cost
	for c.tailCost > c.tailBudget {
		old := c.tail[c.tailOff]
		c.tailOff++
		c.tailCost -= runeCost(old)
		c.dropped += int64(utf8.RuneLen(old))
	}
	if c.tailOff > 4096 && c.tailOff > len(c.tail)/2 {
		c.tail = append(c.tail[:0], c.tail[c.tailOff:]...)
		c.tailOff = 0
	}
}

// Result returns the captured text and whether bytes were dropped. It flushes a trailing
// incomplete UTF-8 sequence as U+FFFD and may be called once output is complete.
func (c *Capture) Result() (string, bool) {
	if len(c.pending) > 0 {
		c.pending = nil
		c.add(utf8.RuneError)
	}
	var sb strings.Builder
	sb.Write(c.head)
	truncated := c.dropped > 0
	if truncated {
		m := "\n...[" + strconv.FormatInt(c.dropped, 10) + " bytes omitted]...\n"
		if EncodedLen(m) <= markerReserve && c.budget >= markerReserve {
			sb.WriteString(m)
		}
	}
	sb.WriteString(string(c.tail[c.tailOff:]))
	return sb.String(), truncated
}

// Dropped returns the sanitised bytes left out of the middle (valid once output is complete).
func (c *Capture) Dropped() int64 { return c.dropped }
