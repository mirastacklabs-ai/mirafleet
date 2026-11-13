package workspace

import (
	"encoding/json"
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"
)

func encLen(t *testing.T, s string) int {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return len(b) - 2
}

func TestCaptureSmall(t *testing.T) {
	c := NewCapture(1000)
	c.Write([]byte("hello "))
	c.Write([]byte("world"))
	s, tr := c.Result()
	if s != "hello world" || tr || c.Total() != 11 {
		t.Fatalf("%q %v", s, tr)
	}
}

func TestCaptureHeadTail(t *testing.T) {
	c := NewCapture(300)
	in := strings.Repeat("A", 1000) + strings.Repeat("B", 1000) + strings.Repeat("C", 1000)
	for i := 0; i < len(in); i += 100 {
		c.Write([]byte(in[i : i+100]))
	}
	s, tr := c.Result()
	if !tr || !strings.HasPrefix(s, "AAAA") || !strings.HasSuffix(s, "CCCC") || !strings.Contains(s, "omitted") {
		t.Fatalf("%q", s)
	}
	if n := encLen(t, s); n > 300 {
		t.Fatalf("encoded %d > 300", n)
	}
}

func TestCaptureBudgetRandom(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	budgets := []int{0, 10, 64, 100, 257, 1000, 4096}
	alpha := []string{"<>&\"\\\n", "héllo  €\U0001F600<"}
	for iter := 0; iter < 300; iter++ {
		budget := budgets[iter%len(budgets)]
		c := NewCapture(budget)
		var total int
		for k := rng.Intn(20); k >= 0; k-- {
			p := make([]byte, rng.Intn(3000))
			switch rng.Intn(4) {
			case 0:
				rng.Read(p) // binary
			case 1:
				for i := range p {
					p[i] = byte(rng.Intn(0x20)) // control characters
				}
			default:
				a := []rune(alpha[rng.Intn(2)])
				p = p[:0]
				for i := rng.Intn(1500); i > 0; i-- {
					p = utf8.AppendRune(p, a[rng.Intn(len(a))])
				}
			}
			total += len(p)
			// Random split points exercise multi-byte sequences across writes.
			for len(p) > 0 {
				n := 1 + rng.Intn(len(p))
				c.Write(p[:n])
				p = p[n:]
			}
		}
		s, tr := c.Result()
		if !utf8.ValidString(s) {
			t.Fatalf("iter %d: invalid UTF-8", iter)
		}
		if n := encLen(t, s); n > budget {
			t.Fatalf("iter %d: encoded %d > budget %d (truncated=%v)", iter, n, budget, tr)
		}
		if EncodedLen(s) < encLen(t, s) {
			t.Fatalf("EncodedLen underestimates")
		}
		if c.Total() != int64(total) {
			t.Fatal("total")
		}
	}
}

func TestCaptureSplitRune(t *testing.T) {
	c := NewCapture(100)
	for _, x := range []byte("a€b") {
		c.Write([]byte{x})
	}
	if s, _ := c.Result(); s != "a€b" {
		t.Fatalf("%q", s)
	}
	c = NewCapture(100)
	c.Write([]byte("a\xe2\x82"))
	if s, _ := c.Result(); s != "a�" {
		t.Fatalf("%q", s)
	}
}
