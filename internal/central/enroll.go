package central

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// serveEnroll is the enroll role: one connection lives at most Config.EnrollDeadline from
// accept, carries exactly one channel (join@ or register@) with one framed request and one
// framed response, then is closed. Failures are logged by the service and here, never audited.
func (s *Server) serveEnroll(sconn *ssh.ServerConn, p auth.Principal, accepted time.Time, chans <-chan ssh.NewChannel, reqs <-chan *ssh.Request) {
	if !s.limiter.allow(p.Remote) {
		s.cfg.Logf("enroll connection from %s refused: rate limit exceeded", p.Remote)
		return
	}
	left := s.cfg.EnrollDeadline - time.Since(accepted)
	if left <= 0 {
		left = time.Millisecond
	}
	deadline := time.AfterFunc(left, func() {
		s.cfg.Logf("enroll connection from %s closed: deadline", p.Remote)
		sconn.Close()
	})
	defer deadline.Stop()
	go ssh.DiscardRequests(reqs)

	key, err := enrollKey(sconn, p)
	if err != nil {
		s.cfg.Logf("enroll connection from %s: %v", p.Remote, err)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { sconn.Wait(); cancel() }()

	var once sync.Once
	var wg sync.WaitGroup
	defer wg.Wait()
	for nch := range chans {
		first := false
		once.Do(func() { first = true })
		if !first {
			nch.Reject(ssh.Prohibited, "only one channel per enroll connection")
			continue
		}
		typ := nch.ChannelType()
		if !channelAllowed(auth.KindEnroll, typ) {
			// The one allowed channel was wasted on a bad type: done.
			nch.Reject(ssh.UnknownChannelType, "channel type not allowed")
			go sconn.Close()
			continue
		}
		ch, creqs, err := nch.Accept()
		if err != nil {
			go sconn.Close()
			continue
		}
		go ssh.DiscardRequests(creqs)
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.serveEnrollChannel(ctx, sconn, ch, typ, p, key)
			// One request, one response, then the connection is closed.
			ch.Close()
			sconn.Close()
		}()
	}
}

// enrollKey recovers the key presented at authentication (stored by the auth callback) and
// cross-checks it against the authenticated principal's fingerprint; a mismatch fails closed (D-07).
func enrollKey(sconn *ssh.ServerConn, p auth.Principal) (ssh.PublicKey, error) {
	if sconn.Permissions == nil {
		return nil, errors.New("missing permissions")
	}
	b, err := base64.StdEncoding.DecodeString(sconn.Permissions.Extensions[extPubKey])
	if err != nil {
		return nil, errors.New("missing presented key")
	}
	k, err := ssh.ParsePublicKey(b)
	if err != nil || k.Type() != ssh.KeyAlgoED25519 {
		return nil, errors.New("invalid presented key")
	}
	if p.Fingerprint == "" || ssh.FingerprintSHA256(k) != p.Fingerprint {
		return nil, errors.New("presented key does not match the authenticated key")
	}
	return k, nil
}

func (s *Server) serveEnrollChannel(ctx context.Context, sconn *ssh.ServerConn, ch ssh.Channel, typ string, p auth.Principal, key ssh.PublicKey) {
	req, id, err := api.ReadRequestTimeout(ch, ch)
	if err != nil {
		if !errors.Is(err, io.EOF) {
			s.cfg.Logf("enroll from %s: bad request: %v", p.Remote, err)
			writeResponseTimeout(ch, ch, api.NewError(id, api.AsError(err)), sconn)
		}
		return
	}
	stop := context.AfterFunc(ctx, func() { ch.Close() })
	defer stop()
	resp := s.enrollCall(ctx, typ, p, key, req)
	if resp.Error != nil {
		s.cfg.Logf("enroll from %s: %s refused (%s)", p.Remote, req.Method, resp.Error.Code)
	}
	if err := writeResponseTimeout(ch, ch, resp, sconn); err != nil {
		s.cfg.Logf("enroll from %s: write response: %v", p.Remote, err)
	}
}

// enrollCall routes an enroll request. Only enroll methods are accepted, and the method must
// match the channel type; anything else is forbidden / bad_request and never reaches the service.
func (s *Server) enrollCall(ctx context.Context, typ string, p auth.Principal, key ssh.PublicKey, req *api.Request) *api.Response {
	if !api.IsEnrollMethod(req.Method) {
		return api.NewError(req.ID, api.Errorf(api.CodeForbidden, "forbidden"))
	}
	var out any
	var err error
	switch {
	case typ == transport.ChanJoin && req.Method == api.MethodEnrollJoin:
		var ep api.EnrollJoinParams
		if e := api.DecodeParams(req.Params, &ep); e != nil {
			return api.NewError(req.ID, e)
		}
		out, err = s.cfg.Service.Join(ctx, p, service.SourceEnroll, ep.Token, ep.Name, key)
	case typ == transport.ChanRegister && req.Method == api.MethodEnrollRegister:
		var ep api.EnrollRegisterParams
		if e := api.DecodeParams(req.Params, &ep); e != nil {
			return api.NewError(req.ID, e)
		}
		out, err = s.cfg.Service.Register(ctx, p, service.SourceEnroll, ep.Code, ep.Name, key)
	default:
		return api.NewError(req.ID, api.Errorf(api.CodeBadRequest, "method does not match channel"))
	}
	if err != nil {
		return api.NewError(req.ID, service.ToAPIError(err))
	}
	resp, rerr := api.NewResult(req.ID, out)
	if rerr != nil {
		return api.NewError(req.ID, api.Internal(rerr))
	}
	return resp
}

// ipLimiter is a per-source-IP token bucket: burst = rate tokens, refilled at rate per minute.
type ipLimiter struct {
	rate float64 // tokens per minute, also the burst
	now  func() time.Time

	max int // hard cap on len(m)

	mu sync.Mutex
	m  map[string]*bucket

	// global is an extra bucket shared by all sources (nil: disabled). Per-source buckets can be
	// evicted by an attacker rotating addresses (D-22); this one cannot.
	global     *bucket
	globalRate float64
}

// setGlobal enables the all-sources limit of perMin connections per minute (burst = perMin).
func (l *ipLimiter) setGlobal(perMin int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.globalRate = float64(perMin)
	l.global = &bucket{tokens: l.globalRate, last: l.now()}
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newIPLimiter(perMin int, now func() time.Time) *ipLimiter {
	return &ipLimiter{rate: float64(perMin), now: now, max: maxBuckets, m: map[string]*bucket{}}
}

// maxBuckets is the hard cap of the table. At the cap, fully refilled (idle) buckets are dropped
// first; if none is idle the least recently used bucket is evicted, so the table never grows
// past the cap whatever the source addresses.
const maxBuckets = 4096

// allow reports whether a connection from remote (host:port) may proceed. IPv6 sources share a
// bucket per /64.
func (l *ipLimiter) allow(remote string) bool {
	ip := ipKey(remote)
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.m[ip]
	if b == nil {
		if len(l.m) >= l.max {
			l.prune(now)
		}
		if len(l.m) >= l.max {
			l.evictOldest()
		}
		b = &bucket{tokens: l.rate, last: now}
		l.m[ip] = b
	}
	b.tokens += now.Sub(b.last).Minutes() * l.rate
	if b.tokens > l.rate {
		b.tokens = l.rate
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	if g := l.global; g != nil {
		g.tokens += now.Sub(g.last).Minutes() * l.globalRate
		if g.tokens > l.globalRate {
			g.tokens = l.globalRate
		}
		g.last = now
		if g.tokens < 1 {
			return false // the per-source token is kept: this source was not the abuser
		}
		g.tokens--
	}
	b.tokens--
	return true
}

// prune drops buckets that have refilled completely. Caller holds l.mu.
func (l *ipLimiter) prune(now time.Time) {
	for k, b := range l.m {
		if b.tokens+now.Sub(b.last).Minutes()*l.rate >= l.rate {
			delete(l.m, k)
		}
	}
}

// evictOldest drops the least recently used bucket. Caller holds l.mu.
func (l *ipLimiter) evictOldest() {
	var oldest string
	var at time.Time
	for k, b := range l.m {
		if oldest == "" || b.last.Before(at) {
			oldest, at = k, b.last
		}
	}
	delete(l.m, oldest)
}
