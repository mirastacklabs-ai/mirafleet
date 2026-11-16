package gui

import (
	"container/list"
	"context"
	"errors"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sync"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
)

// Limits.
const (
	loginBurst        = 5
	loginRefill       = 12 * time.Second // 5 per minute
	lockoutAfter      = 10               // consecutive failures
	lockoutFor        = 15 * time.Minute
	globalFailLimit   = 100 // failures in globalWindow switch on the slow-down
	globalWindow      = 10 * time.Minute
	globalSlowGap     = 5 * time.Second
	limiterMaxEntries = 4096

	// MaxFormBytes caps every GUI request body.
	MaxFormBytes = 64 << 10
)

// Reasons Allow refuses.
const (
	DenyRate     = "rate"
	DenyLockout  = "lockout"
	DenySlowdown = "slowdown"
)

// LimiterKey maps a client address ("ip", "ip:port" or "[ip]:port") to its bucket key: IPv4 as
// is, IPv6 masked to its /64, IPv4-mapped IPv6 as IPv4. Unparseable input maps to itself.
func LimiterKey(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		addr = h
	}
	ip, err := netip.ParseAddr(addr)
	if err != nil {
		return addr
	}
	ip = ip.WithZone("").Unmap()
	if ip.Is6() {
		p, _ := ip.Prefix(64)
		return p.Addr().String() + "/64"
	}
	return ip.String()
}

// lockKey maps a client address to its lockout key: the exact address (a /128 for IPv6), so one
// client's failures do not lock out the other hosts of its /64. The rate bucket stays per /64.
func lockKey(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		addr = h
	}
	ip, err := netip.ParseAddr(addr)
	if err != nil {
		return "L|" + addr
	}
	return "L|" + ip.WithZone("").Unmap().String()
}

// RetryAfterSeconds rounds d up to whole seconds, at least 1, for a Retry-After header.
func RetryAfterSeconds(d time.Duration) int {
	n := int((d + time.Second - 1) / time.Second)
	if n < 1 {
		n = 1
	}
	return n
}

type ipState struct {
	key         string
	tokens      float64
	last        time.Time
	consecutive int
	lockedUntil time.Time
	elem        *list.Element
}

// LoginLimiter implements the per-IP token bucket, lockout and global slow-down.
// Maps are capped at 4096 entries with LRU eviction. Safe for concurrent use.
type LoginLimiter struct {
	now func() time.Time
	// OnSlowdown (optional) is called once each time the global slow-down switches on.
	OnSlowdown func()

	mu         sync.Mutex
	byKey      map[string]*ipState
	lru        *list.List // front = most recent
	fails      []time.Time
	slow       bool
	lastGlobal time.Time
}

// NewLoginLimiter returns a limiter reading time from now.
func NewLoginLimiter(now func() time.Time) *LoginLimiter {
	return &LoginLimiter{now: now, byKey: map[string]*ipState{}, lru: list.New()}
}

func (l *LoginLimiter) get(key string, now time.Time) *ipState {
	if s, ok := l.byKey[key]; ok {
		l.lru.MoveToFront(s.elem)
		return s
	}
	for len(l.byKey) >= limiterMaxEntries {
		// Evict the least recent entry that is not under a lockout; only when every
		// entry is locked does the least recent one go.
		old := l.lru.Back()
		for e := old; e != nil; e = e.Prev() {
			if !now.Before(e.Value.(*ipState).lockedUntil) {
				old = e
				break
			}
		}
		s := old.Value.(*ipState)
		l.lru.Remove(old)
		delete(l.byKey, s.key)
	}
	s := &ipState{key: key, tokens: loginBurst, last: now}
	s.elem = l.lru.PushFront(s)
	l.byKey[key] = s
	return s
}

// Len returns the number of tracked buckets (tests, metrics).
func (l *LoginLimiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.byKey)
}

// Allow consumes one attempt for addr. When refused it returns the reason and how long to wait.
func (l *LoginLimiter) Allow(addr string) (ok bool, reason string, retryAfter time.Duration) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if ls, ok := l.byKey[lockKey(addr)]; ok && now.Before(ls.lockedUntil) {
		l.lru.MoveToFront(ls.elem)
		return false, DenyLockout, ls.lockedUntil.Sub(now)
	}
	s := l.get(LimiterKey(addr), now)
	if el := now.Sub(s.last); el > 0 {
		s.tokens += float64(el) / float64(loginRefill)
		if s.tokens > loginBurst {
			s.tokens = loginBurst
		}
		s.last = now
	}
	if s.tokens < 1 {
		return false, DenyRate, time.Duration((1 - s.tokens) * float64(loginRefill))
	}
	l.pruneFails(now)
	if l.slow {
		if gap := now.Sub(l.lastGlobal); gap < globalSlowGap {
			s.tokens-- // a slowed-down attempt still counts against the client's own bucket
			return false, DenySlowdown, globalSlowGap - gap
		}
		l.lastGlobal = now
	}
	s.tokens--
	return true, "", 0
}

func (l *LoginLimiter) pruneFails(now time.Time) {
	i := 0
	for i < len(l.fails) && now.Sub(l.fails[i]) > globalWindow {
		i++
	}
	l.fails = l.fails[i:]
	if l.slow && len(l.fails) <= globalFailLimit {
		l.slow = false
	}
}

// Failure records a failed attempt. justLocked is true exactly when this failure starts a
// lockout (the caller writes one gui.lockout audit row then).
func (l *LoginLimiter) Failure(addr string) (justLocked bool) {
	now := l.now()
	l.mu.Lock()
	var notify func()
	s := l.get(lockKey(addr), now)
	if !now.Before(s.lockedUntil) {
		s.consecutive++
		if s.consecutive >= lockoutAfter {
			s.lockedUntil = now.Add(lockoutFor)
			s.consecutive = 0
			justLocked = true
		}
	}
	l.pruneFails(now)
	l.fails = append(l.fails, now)
	if len(l.fails) > globalFailLimit+1 {
		l.fails = l.fails[len(l.fails)-(globalFailLimit+1):]
	}
	if !l.slow && len(l.fails) > globalFailLimit {
		l.slow = true
		l.lastGlobal = now
		notify = l.OnSlowdown
	}
	l.mu.Unlock()
	if notify != nil {
		notify()
	}
	return justLocked
}

// Success resets the consecutive-failure count for addr.
func (l *LoginLimiter) Success(addr string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if s, ok := l.byKey[lockKey(addr)]; ok {
		s.consecutive = 0
	}
}

// ErrHashBusy means no hash slot freed up in time; answer 503.
var ErrHashBusy = errors.New("gui: password hashing busy")

// HashSem bounds concurrent Argon2 work (at most 2 at once, 5 s wait, then 503).
// The login handlers wrap every login and password verification/hash in Acquire.
type HashSem struct {
	slots chan struct{}
	wait  time.Duration
	after func(time.Duration) <-chan time.Time
}

// NewHashSem returns a semaphore of n slots that waits up to wait. after is time.After in
// production (nil means time.After) and injectable in tests.
func NewHashSem(n int, wait time.Duration, after func(time.Duration) <-chan time.Time) *HashSem {
	if after == nil {
		after = time.After
	}
	return &HashSem{slots: make(chan struct{}, n), wait: wait, after: after}
}

// DefaultHashSem is the production configuration: 2 slots, 5 s.
func DefaultHashSem() *HashSem { return NewHashSem(2, 5*time.Second, nil) }

// Acquire takes a slot and returns its release func (call exactly once, idempotent). It returns
// ErrHashBusy after the wait, or ctx.Err() if ctx ends first.
func (h *HashSem) Acquire(ctx context.Context) (release func(), err error) {
	select {
	case h.slots <- struct{}{}:
	default:
		select {
		case h.slots <- struct{}{}:
		case <-h.after(h.wait):
			return nil, ErrHashBusy
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	var once sync.Once
	return func() { once.Do(func() { <-h.slots }) }, nil
}

// ParseForm applies the request hygiene: urlencoded content type only (415),
// body capped at MaxFormBytes before parsing (413), then ParseForm (400 on malformed). It
// returns the parsed values and 0, or nil and the HTTP status to answer with.
func ParseForm(w http.ResponseWriter, r *http.Request) (url.Values, int) {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil ||
		mt != "application/x-www-form-urlencoded" {
		return nil, http.StatusUnsupportedMediaType
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxFormBytes)
	if err := r.ParseForm(); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, http.StatusRequestEntityTooLarge
		}
		return nil, http.StatusBadRequest
	}
	return r.PostForm, 0
}

// PasswordTooLong reports whether a submitted password exceeds auth.MaxPasswordBytes. The login
// handler must call it after ParseForm and before the user lookup or any hashing.
func PasswordTooLong(pw string) bool { return len(pw) > auth.MaxPasswordBytes }
