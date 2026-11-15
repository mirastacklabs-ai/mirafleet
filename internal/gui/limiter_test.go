package gui

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)} }
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *fakeClock) Add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func TestLimiterKey(t *testing.T) {
	cases := map[string]string{
		"192.0.2.1:5555":            "192.0.2.1",
		"192.0.2.1":                 "192.0.2.1",
		"[2001:db8:1:2:aaaa::1]:80": "2001:db8:1:2::/64",
		"2001:db8:1:2:bbbb:cccc::9": "2001:db8:1:2::/64",
		"2001:db8:1:3::1":           "2001:db8:1:3::/64",
		"::ffff:192.0.2.1":          "192.0.2.1",
		"fe80::1%eth0":              "fe80::/64",
		"not an ip":                 "not an ip",
	}
	for in, want := range cases {
		if got := LimiterKey(in); got != want {
			t.Errorf("LimiterKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLimiterBucket(t *testing.T) {
	c := newClock()
	l := NewLoginLimiter(c.Now)
	for i := 0; i < 5; i++ {
		if ok, _, _ := l.Allow("192.0.2.1:1"); !ok {
			t.Fatalf("attempt %d refused", i)
		}
	}
	ok, why, ra := l.Allow("192.0.2.1:2")
	if ok || why != DenyRate || ra <= 0 || ra > 12*time.Second {
		t.Fatalf("6th: %v %q %v", ok, why, ra)
	}
	if ok, _, _ := l.Allow("192.0.2.2"); !ok { // other IP unaffected
		t.Fatal("other IP refused")
	}
	c.Add(12 * time.Second)
	if ok, _, _ := l.Allow("192.0.2.1"); !ok {
		t.Fatal("no refill after 12s")
	}
	if ok, _, _ := l.Allow("192.0.2.1"); ok {
		t.Fatal("only one token should have refilled")
	}
}

func TestLimiterIPv6Bucketing(t *testing.T) {
	c := newClock()
	l := NewLoginLimiter(c.Now)
	for i := 0; i < 5; i++ {
		l.Allow(fmt.Sprintf("[2001:db8:0:1::%x]:1", i+1)) // same /64, different hosts
	}
	if ok, why, _ := l.Allow("[2001:db8:0:1:ffff::1]:1"); ok || why != DenyRate {
		t.Fatalf("same /64 not shared: %v %q", ok, why)
	}
	if ok, _, _ := l.Allow("[2001:db8:0:2::1]:1"); !ok {
		t.Fatal("different /64 shared a bucket")
	}
}

func TestLimiterLockoutAndRelease(t *testing.T) {
	c := newClock()
	l := NewLoginLimiter(c.Now)
	const ip = "192.0.2.9"
	locked := 0
	for i := 0; i < 10; i++ {
		c.Add(15 * time.Second) // stay inside the bucket
		if ok, _, _ := l.Allow(ip); !ok {
			t.Fatalf("attempt %d refused", i)
		}
		if l.Failure(ip) {
			locked++
		}
	}
	if locked != 1 {
		t.Fatalf("lockouts = %d", locked)
	}
	ok, why, ra := l.Allow(ip)
	if ok || why != DenyLockout || ra < 14*time.Minute || ra > 15*time.Minute {
		t.Fatalf("locked: %v %q %v", ok, why, ra)
	}
	if l.Failure(ip) { // failures during the lockout do not re-fire the audit signal
		t.Fatal("re-locked while locked")
	}
	c.Add(15*time.Minute + time.Second)
	if ok, _, _ := l.Allow(ip); !ok {
		t.Fatal("not released after 15 min")
	}
}

func TestLimiterSuccessResetsCount(t *testing.T) {
	c := newClock()
	l := NewLoginLimiter(c.Now)
	const ip = "192.0.2.7"
	for round := 0; round < 3; round++ {
		for i := 0; i < 9; i++ {
			c.Add(15 * time.Second)
			if l.Failure(ip) {
				t.Fatal("locked early")
			}
		}
		l.Success(ip)
	}
	c.Add(15 * time.Second)
	if ok, _, _ := l.Allow(ip); !ok {
		t.Fatal("refused")
	}
}

func TestLimiterGlobalSlowdown(t *testing.T) {
	c := newClock()
	l := NewLoginLimiter(c.Now)
	var warned int
	l.OnSlowdown = func() { warned++ }
	for i := 0; i < 100; i++ { // 100 failures from distinct IPs: not yet slow
		l.Failure(fmt.Sprintf("10.0.%d.%d", i/200, i%200+1))
	}
	if ok, _, _ := l.Allow("198.51.100.1"); !ok {
		t.Fatal("slow too early")
	}
	l.Failure("10.9.9.9") // 101st
	if warned != 1 {
		t.Fatalf("warned = %d", warned)
	}
	ok, why, ra := l.Allow("198.51.100.2")
	if ok || why != DenySlowdown || ra <= 0 || ra > 5*time.Second {
		t.Fatalf("slow: %v %q %v", ok, why, ra)
	}
	c.Add(5 * time.Second)
	if ok, _, _ := l.Allow("198.51.100.3"); !ok {
		t.Fatal("one attempt per 5s should pass")
	}
	if ok, why, _ := l.Allow("198.51.100.4"); ok || why != DenySlowdown {
		t.Fatal("second within 5s passed")
	}
	c.Add(globalWindow + time.Second) // failures age out
	if ok, _, _ := l.Allow("198.51.100.5"); !ok {
		t.Fatal("still slow after window")
	}
	if ok, _, _ := l.Allow("198.51.100.6"); !ok {
		t.Fatal("slow-down not released")
	}
}

func TestLimiterMapCap(t *testing.T) {
	c := newClock()
	l := NewLoginLimiter(c.Now)
	for i := 0; i < limiterMaxEntries+500; i++ {
		l.Allow(fmt.Sprintf("10.%d.%d.%d", i>>16&255, i>>8&255, i&255))
	}
	if n := l.Len(); n != limiterMaxEntries {
		t.Fatalf("len = %d", n)
	}
	if len(l.byKey) != l.lru.Len() {
		t.Fatal("map and list diverged")
	}
}

func TestHashSem(t *testing.T) {
	timeout := make(chan time.Time)
	var waited time.Duration
	s := NewHashSem(2, 5*time.Second, func(d time.Duration) <-chan time.Time { waited = d; return timeout })
	ctx := context.Background()
	r1, err := s.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := s.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Third waits; releasing a slot lets it in.
	got := make(chan error, 1)
	go func() {
		r, err := s.Acquire(ctx)
		if err == nil {
			r()
		}
		got <- err
	}()
	r1()
	r1() // idempotent: must not free a second slot
	if err := <-got; err != nil {
		t.Fatal(err)
	}
	// Both slots held again (r2 + new one), third times out.
	r3, err := s.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	go func() { timeout <- time.Time{} }()
	if _, err := s.Acquire(ctx); err != ErrHashBusy {
		t.Fatalf("err = %v", err)
	}
	if waited != 5*time.Second {
		t.Fatalf("waited %v", waited)
	}
	// Context cancel while waiting.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.Acquire(cctx); err != context.Canceled {
		t.Fatalf("err = %v", err)
	}
	r2()
	r3()
	if len(s.slots) != 0 {
		t.Fatal("slots leaked")
	}
}

func postForm(body, ctype string) (*httptest.ResponseRecorder, *http.Request) {
	r := httptest.NewRequest("POST", "/login", strings.NewReader(body))
	r.Header.Set("Content-Type", ctype)
	return httptest.NewRecorder(), r
}

func TestParseFormCapsAndPasswordLength(t *testing.T) {
	w, r := postForm("username=a&password=b", "application/x-www-form-urlencoded; charset=utf-8")
	v, st := ParseForm(w, r)
	if st != 0 || v.Get("username") != "a" {
		t.Fatalf("%v %d", v, st)
	}
	if w, r = postForm("x=1", "text/plain"); true {
		if _, st := ParseForm(w, r); st != http.StatusUnsupportedMediaType {
			t.Fatalf("ctype %d", st)
		}
	}
	w, r = postForm("password="+strings.Repeat("a", MaxFormBytes), "application/x-www-form-urlencoded")
	if _, st := ParseForm(w, r); st != http.StatusRequestEntityTooLarge {
		t.Fatalf("size %d", st)
	}
	if PasswordTooLong(strings.Repeat("a", 256)) || !PasswordTooLong(strings.Repeat("a", 257)) {
		t.Fatal("password bound")
	}
}

func TestLimiterSlowdownConsumesToken(t *testing.T) {
	c := newClock()
	l := NewLoginLimiter(c.Now)
	for i := 0; i < 101; i++ {
		l.Failure(fmt.Sprintf("10.0.%d.%d", i/200, i%200+1))
	}
	const ip = "198.51.100.77"
	for i := 0; i < loginBurst; i++ { // all refused by the slow-down, all spend a token
		if ok, why, _ := l.Allow(ip); ok || why != DenySlowdown {
			t.Fatalf("attempt %d: %v %q", i, ok, why)
		}
	}
	if ok, why, _ := l.Allow(ip); ok || why != DenyRate {
		t.Fatalf("after burst: %v %q (want rate)", ok, why)
	}
}

func TestLimiterLockoutPerAddressRatePerPrefix(t *testing.T) {
	c := newClock()
	l := NewLoginLimiter(c.Now)
	const bad, neighbour = "2001:db8:1:2::1", "2001:db8:1:2::2"
	for i := 0; i < lockoutAfter; i++ {
		c.Add(15 * time.Second)
		l.Allow(bad)
		l.Failure(bad)
	}
	if ok, why, _ := l.Allow(bad); ok || why != DenyLockout {
		t.Fatalf("bad host: %v %q", ok, why)
	}
	if ok, _, _ := l.Allow(neighbour); !ok {
		t.Fatal("neighbour in the same /64 locked out")
	}
	// The rate bucket is shared across the /64.
	c2 := newClock()
	l2 := NewLoginLimiter(c2.Now)
	for i := 0; i < loginBurst; i++ {
		l2.Allow(fmt.Sprintf("2001:db8:9:9::%x", i+1))
	}
	if ok, why, _ := l2.Allow("2001:db8:9:9::ff"); ok || why != DenyRate {
		t.Fatalf("shared /64 bucket: %v %q", ok, why)
	}
}

func TestRetryAfterSeconds(t *testing.T) {
	cases := map[time.Duration]int{0: 1, -time.Second: 1, time.Millisecond: 1, time.Second: 1,
		time.Second + time.Millisecond: 2, 4500 * time.Millisecond: 5, 15 * time.Minute: 900}
	for d, want := range cases {
		if got := RetryAfterSeconds(d); got != want {
			t.Errorf("RetryAfterSeconds(%v) = %d, want %d", d, got, want)
		}
	}
}
