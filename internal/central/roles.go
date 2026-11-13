package central

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/central/registry"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// allowedChannels is the per-role allow-list of channel types a peer may open toward central.
// Anything not listed is rejected. Runners open none: central opens exec@ channels toward them
// over their own connection.
var allowedChannels = map[auth.Kind]map[string]bool{
	auth.KindRunner:   {},
	auth.KindOperator: {transport.ChanAPI: true},
	auth.KindEnroll:   {transport.ChanJoin: true, transport.ChanRegister: true},
}

// channelAllowed reports whether a peer of kind k may open a channel of type typ.
func channelAllowed(k auth.Kind, typ string) bool { return allowedChannels[k][typ] }

// dispatch hands an authenticated connection to its role. It returns when the connection is
// done; the caller closes it.
func (s *Server) dispatch(sconn *ssh.ServerConn, p auth.Principal, accepted time.Time, chans <-chan ssh.NewChannel, reqs <-chan *ssh.Request) {
	switch p.Kind {
	case auth.KindRunner:
		s.serveRunner(sconn, p, chans, reqs)
	case auth.KindOperator:
		s.serveOperator(sconn, p, chans, reqs)
	case auth.KindEnroll:
		s.serveEnroll(sconn, p, accepted, chans, reqs)
	default:
		s.cfg.Logf("connection from %s: unknown principal kind %q; closing", sconn.RemoteAddr(), p.Kind)
	}
}

// rejectChannels rejects every channel the peer opens (used for runners, whose allow-list is
// empty; operators and enroll peers have their own channel loops).
func rejectChannels(k auth.Kind, chans <-chan ssh.NewChannel) {
	for nch := range chans {
		if !channelAllowed(k, nch.ChannelType()) {
			nch.Reject(ssh.UnknownChannelType, "channel type not allowed")
			continue
		}
		nch.Reject(ssh.Prohibited, "not supported")
	}
}

// registryStatus maps a stored runner status to a registry status. Revoked and unknown statuses
// have no live status: such a runner is refused.
func registryStatus(st store.Status) (registry.Status, bool) {
	switch st {
	case store.StatusActive:
		return registry.StatusActive, true
	case store.StatusPending:
		return registry.StatusPending, true
	case store.StatusDisabled:
		return registry.StatusDisabled, true
	}
	return "", false
}

// serveRunner is the runner role: heartbeats register a session keyed by the runner's store id.
// Pending runners may heartbeat; the registry refuses exec for them.
//
// Store touches (last_seen_at/info) never delay a heartbeat reply: they are handed to a
// per-connection worker that keeps only the newest pending value (coalescing), and the
// connect/disconnect writes of a flapping runner are throttled (see claimEdge).
func (s *Server) serveRunner(sconn *ssh.ServerConn, p auth.Principal, chans <-chan ssh.NewChannel, reqs <-chan *ssh.Request) {
	reg := s.cfg.Registry
	staleAfter := reg.StaleAfter()
	// Runners never open channels toward central.
	go rejectChannels(auth.KindRunner, chans)
	// A connection that never heartbeats is never registered; close it after the stale window.
	var sess *registry.Session
	var lastHB transport.Heartbeat
	var lastTouch time.Time
	var registered atomic.Bool
	var tc *toucher
	guard := time.AfterFunc(staleAfter, func() {
		if !registered.Load() {
			s.cfg.Logf("runner %q (id %d) silent for >%s; closing connection", p.Name, p.ID, staleAfter)
			sconn.Close()
		}
	})
	defer guard.Stop()
	defer func() {
		if sess != nil {
			reg.Unregister(sess)
			tc.stop() // no touch may race the final one
			if s.claimEdge(p.ID, s.cfg.Now()) {
				s.touchRunner(p.ID, lastHB) // D-02: last_seen/info on disconnect
			}
		}
	}()
	for r := range reqs {
		if r.Type != transport.ReqHeartbeat {
			r.Reply(false, nil)
			continue
		}
		var hb transport.Heartbeat
		if err := json.Unmarshal(r.Payload, &hb); err != nil {
			r.Reply(false, nil)
			continue
		}
		now := s.cfg.Now()
		if sess == nil {
			// Register on first heartbeat: by then the runner has installed its channel handler.
			var err error
			sess, err = s.register(sconn, p, hb)
			if err != nil {
				s.cfg.Logf("runner %q (id %d) refused: %v", p.Name, p.ID, err)
				r.Reply(false, nil)
				sconn.Close()
				continue
			}
			registered.Store(true)
			tc = s.startToucher(p.ID)
			lastHB = hb
			if s.claimEdge(p.ID, now) {
				lastTouch = now
				tc.put(hb) // D-02: last_seen/info on register
			} else {
				// Throttled: the next heartbeat after the edge gap does the write instead.
				lastTouch = now.Add(s.edgeGap() - s.cfg.TouchInterval)
			}
		} else {
			sess.Heartbeat(hb)
			lastHB = hb
			if now.Sub(lastTouch) >= s.cfg.TouchInterval {
				lastTouch = now
				tc.put(hb)
			}
		}
		r.Reply(true, nil)
	}
}

// toucher writes a runner's last_seen/info to the store off the heartbeat path. It holds at most
// one pending heartbeat: a newer one replaces an unwritten older one. put has a single caller.
type toucher struct {
	ch   chan transport.Heartbeat
	done chan struct{}
}

func (s *Server) startToucher(id int64) *toucher {
	t := &toucher{ch: make(chan transport.Heartbeat, 1), done: make(chan struct{})}
	go func() {
		defer close(t.done)
		for hb := range t.ch {
			s.touchRunner(id, hb)
		}
	}()
	return t
}

func (t *toucher) put(hb transport.Heartbeat) {
	select {
	case t.ch <- hb:
		return
	default:
	}
	select { // full: drop the stale pending value, keep the newest
	case <-t.ch:
	default:
	}
	select {
	case t.ch <- hb:
	default:
	}
}

// stop lets the worker finish its pending write and waits for it.
func (t *toucher) stop() { close(t.ch); <-t.done }

// edgeGap is the minimum spacing of connect/disconnect writes per runner.
func (s *Server) edgeGap() time.Duration { return s.cfg.TouchInterval / 10 }

// claimEdge reports whether a connect/disconnect store write for runner id may happen now, and
// records it if so. A runner flapping faster than edgeGap costs the store at most one edge write
// per gap; periodic heartbeat writes are unaffected and still refresh last_seen.
func (s *Server) claimEdge(id int64, now time.Time) bool {
	s.edgeMu.Lock()
	defer s.edgeMu.Unlock()
	if last, ok := s.edgeAt[id]; ok && now.Sub(last) < s.edgeGap() {
		return false
	}
	if s.edgeAt == nil {
		s.edgeAt = map[int64]time.Time{}
	}
	s.edgeAt[id] = now
	return true
}

// register reloads the runner from the store by id (its CURRENT status, not the one captured at
// authentication) and registers the session. The registry re-checks the tombstone and any
// status recorded by a concurrent lifecycle change under its own lock.
func (s *Server) register(sconn *ssh.ServerConn, p auth.Principal, hb transport.Heartbeat) (*registry.Session, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := s.cfg.Store.GetRunnerByID(ctx, p.ID)
	if err != nil {
		return nil, fmt.Errorf("load runner: %w", err)
	}
	st, ok := registryStatus(r.Status)
	if !ok {
		return nil, fmt.Errorf("runner status %q", r.Status)
	}
	sess, err := s.cfg.Registry.Register(r.ID, r.Name, r.Fleet, sconn, st, hb)
	if err != nil {
		return nil, err
	}
	s.cfg.Logf("runner %s (id %d, %s) online (%s/%s)", r.Ref(), r.ID, r.Status, hb.OS, hb.Arch)
	return sess, nil
}

// storeTimeout bounds the best-effort last_seen writes.
const storeTimeout = 5 * time.Second

// touchRunner records last_seen_at and info for a runner (D-02). Failures are logged only.
func (s *Server) touchRunner(id int64, hb transport.Heartbeat) {
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	info := map[string]string{"hostname": clip(hb.Hostname), "os": clip(hb.OS), "arch": clip(hb.Arch), "version": clip(hb.Version)}
	if err := s.cfg.Store.TouchRunner(ctx, id, s.cfg.Now(), info); err != nil {
		s.cfg.Logf("touch runner %d: %v", id, err)
	}
}

// maxInfoValue bounds each heartbeat-supplied info value stored for a runner, so an oversized
// hostname can never push the info blob over the store's limit and block last_seen updates.
const maxInfoValue = 256

// clip returns s cut to at most maxInfoValue bytes on a rune boundary, invalid UTF-8 dropped.
func clip(s string) string {
	s = strings.ToValidUTF8(s, "")
	if len(s) <= maxInfoValue {
		return s
	}
	n := maxInfoValue
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
