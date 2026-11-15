package central

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/central/registry"
	"github.com/mirastacklabs-ai/mirafleet/internal/protover"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// allowedChannels is the per-role allow-list of channel types a peer may open toward central.
// Anything not listed is rejected. Runners open only events@ (host-mode usage events); central
// opens exec@ and workspace@ channels toward them over their own connection.
var allowedChannels = map[auth.Kind]map[string]bool{
	auth.KindRunner:   {transport.ChanEvents: true},
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
	// Runners open only events@; everything else is rejected. Tracked by the wait group (this
	// function is itself tracked, so Add cannot race Close's Wait).
	s.wg.Add(1)
	go func() { defer s.wg.Done(); s.runnerChannels(p, chans) }()
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
			// D-02: last_seen/info on disconnect. Never throttled (only connect edges are), and
			// written by the touch worker after its pending write, in the background: a hung
			// store (each write is bounded by storeTimeout) never delays teardown.
			s.markEdge(p.ID, s.cfg.Now())
			tc.finish(lastHB)
		}
	}()
	warnedActions := false // log the dropped invalid actions list once per connection
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
		if sess == nil {
			// Version skew (docs/M6.md 2.2), checked on the first heartbeat: a missing proto is
			// level 0. Tell the runner why, audit it, and close.
			if verr := protover.Check("runner", p.Name, s.cfg.Proto, hb.Proto); verr != nil {
				s.refuseRunner(sconn, p, r, verr)
				return
			}
		}
		if err := hb.Validate(); err != nil && len(hb.Actions) > 0 {
			// F-16: an invalid Actions list is informational and may be version skew; drop it and
			// keep the connection if everything else is valid.
			stripped := hb
			stripped.Actions = nil
			if stripped.Validate() == nil {
				if !warnedActions {
					warnedActions = true
					s.cfg.Logf("runner %q (id %d) sent an invalid actions list, ignoring it: %v", p.Name, p.ID, err)
				}
				hb = stripped
			}
		}
		if err := hb.Validate(); err != nil {
			// Out-of-range workspace fields: a misbehaving or hostile runner. Refuse and drop it.
			s.cfg.Logf("runner %q (id %d) sent an invalid heartbeat: %v", p.Name, p.ID, err)
			r.Reply(false, nil)
			sconn.Close()
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
			if hb.Executor != "" {
				s.reconcileAsync(p.ID)
			}
			edge := s.claimEdge(p.ID, now)
			if hasFeature(hb, transport.FeatureCreds) {
				if edge {
					s.deliverAsync(p.ID)
				} else {
					// H-06: a flapping runner costs one decrypt+send per edge gap; the next
					// heartbeat's retry delivers the set.
					s.cfg.Service.MarkDeliveryStale(p.ID)
				}
			}
			lastHB = hb
			if edge {
				lastTouch = now
				tc.put(hb) // D-02: last_seen/info on register
			} else {
				// Throttled: the next heartbeat after the edge gap does the write instead.
				lastTouch = now.Add(s.edgeGap() - s.cfg.TouchInterval)
			}
		} else {
			sess.Heartbeat(hb)
			if hasFeature(hb, transport.FeatureCreds) {
				s.cfg.Service.ReportCredentialStatus(p.ID, hb.Credentials)
				if s.cfg.Service.ClaimDeliveryRetry(p.ID) { // last creds_sync failed: retry
					s.retryDeliverAsync(p.ID)
				}
			}
			if hb.Busy > 0 { // running background shells keep their workspaces from idling out
				bctx, bcancel := context.WithTimeout(s.ctx, storeTimeout)
				s.cfg.Service.TouchBusy(bctx, p.ID, hb.Busy)
				bcancel()
			}
			lastHB = hb
			if now.Sub(lastTouch) >= s.cfg.TouchInterval {
				lastTouch = now
				tc.put(hb)
			}
		}
		r.Reply(true, nil)
	}
}

// refuseTimeout bounds how long central waits for the runner to acknowledge refused@mirafleet.
const refuseTimeout = 5 * time.Second

// refuseRunner sends the refusal message, audits it, answers the pending heartbeat with false and
// closes the connection. The message goes first and is acknowledged (bounded by refuseTimeout),
// so the runner has it before it sees the failed heartbeat. Runs on the request loop of the
// connection: serveRunner returns right after, and its deferred cleanup runs.
func (s *Server) refuseRunner(sconn *ssh.ServerConn, p auth.Principal, r *ssh.Request, verr error) {
	msg := verr.Error()
	s.cfg.Logf("runner %q (id %d) refused: %s", p.Name, p.ID, msg)
	if s.claimRefusal(p.ID, s.cfg.Now()) {
		actx, cancel := context.WithTimeout(context.Background(), storeTimeout)
		s.cfg.Service.RunnerRefusedVersion(actx, p.ID, msg)
		cancel()
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		sconn.SendRequest(transport.ReqRefused, true, []byte(msg))
	}()
	select {
	case <-done:
	case <-time.After(refuseTimeout):
	}
	r.Reply(false, nil)
	sconn.Close()
}

// reconcileAsync compares the runner's workspaces with the store in the background (M3.md 1.6). It
// runs outside the registry lock and the heartbeat path, tracked by the wait group so Close drains
// it before the store closes. Called from serveRunner, itself tracked, so Add cannot race Wait.
func (s *Server) reconcileAsync(id int64) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.cfg.Service.ReconcileRunner(s.ctx, id)
	}()
}

func hasFeature(hb transport.Heartbeat, f string) bool {
	for _, x := range hb.Features {
		if x == f {
			return true
		}
	}
	return false
}

// deliverAsync sends a connected host runner its full credential set (creds_sync) in the
// background: the runner answers on the same connection that is still waiting for the first
// heartbeat reply. Tracked like reconcileAsync. Failures are logged without values.
func (s *Server) deliverAsync(id int64) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := s.cfg.Service.RunnerConnected(s.ctx, id); err != nil {
			s.cfg.Logf("credential delivery to runner %d: %v", id, err)
		}
	}()
}

// retryDeliverAsync re-sends the credential set of a runner whose last delivery failed. The
// caller holds the claim from Service.ClaimDeliveryRetry. Tracked like deliverAsync.
func (s *Server) retryDeliverAsync(id int64) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := s.cfg.Service.RetryDelivery(s.ctx, id); err != nil {
			s.cfg.Logf("credential delivery retry to runner %d: %v", id, err)
		}
	}()
}

// runnerChannels serves the channels a runner opens toward central: events@ only.
func (s *Server) runnerChannels(p auth.Principal, chans <-chan ssh.NewChannel) {
	for nch := range chans {
		if !channelAllowed(auth.KindRunner, nch.ChannelType()) {
			nch.Reject(ssh.UnknownChannelType, "channel type not allowed")
			continue
		}
		ch, reqs, err := nch.Accept()
		if err != nil {
			continue
		}
		go ssh.DiscardRequests(reqs)
		s.wg.Add(1) // the caller's counter keeps the total above zero while chans is open
		go func() {
			defer s.wg.Done()
			defer ch.Close()
			s.readEvents(p, ch)
		}()
	}
}

// readEvents ingests the length-prefixed EventBatch frames of one events@ channel until it ends
// or a frame is invalid (the channel is then closed).
func (s *Server) readEvents(p auth.Principal, r io.Reader) {
	for {
		b, err := transport.ReadEventBatch(r)
		if err != nil {
			if err != io.EOF && s.ctx.Err() == nil {
				s.cfg.Logf("runner %q (id %d): events channel: %v", p.Name, p.ID, err)
			}
			return
		}
		ctx, cancel := context.WithTimeout(s.ctx, storeTimeout)
		err = s.cfg.Service.IngestRunnerEvents(ctx, p.ID, b)
		cancel()
		if err != nil {
			s.cfg.Logf("runner %q (id %d): events not recorded: %v", p.Name, p.ID, err)
		}
	}
}

// toucher writes a runner's last_seen/info to the store off the heartbeat path. It holds at most
// one pending heartbeat: a newer one replaces an unwritten older one. put has a single caller.
type toucher struct {
	ch    chan transport.Heartbeat
	final transport.Heartbeat // written by finish before ch is closed
}

func (s *Server) startToucher(id int64) *toucher {
	t := &toucher{ch: make(chan transport.Heartbeat, 1)}
	// Tracked by the wait group so Server.Close drains it (each write is bounded by storeTimeout)
	// before the caller closes the store. startToucher runs inside serveRunner, itself tracked,
	// so the counter is never zero here and Add cannot race Close's Wait.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for hb := range t.ch {
			s.touchRunner(id, hb)
		}
		s.touchRunner(id, t.final) // the closed channel orders this after finish's write
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

// finish queues the final (disconnect) write with hb after any pending one and returns at once;
// the worker performs both in the background, in order, then exits. Single caller, last use.
func (t *toucher) finish(hb transport.Heartbeat) { t.final = hb; close(t.ch) }

// edgeGap is the minimum spacing of connect edge writes per runner.
func (s *Server) edgeGap() time.Duration { return s.cfg.TouchInterval / 10 }

// claimEdge reports whether a connect store write for runner id may happen now, and records it
// if so. A runner flapping faster than edgeGap costs the store at most one connect write per
// gap; periodic heartbeat writes are unaffected and still refresh last_seen. Disconnect writes
// are never throttled (markEdge).
func (s *Server) claimEdge(id int64, now time.Time) bool {
	s.edgeMu.Lock()
	defer s.edgeMu.Unlock()
	if last, ok := s.edgeAt[id]; ok && now.Sub(last) < s.edgeGap() {
		return false
	}
	s.setEdgeLocked(id, now)
	return true
}

// refusalGap is the minimum spacing of runner.refused_version audit rows per runner: a runner
// that retries every minute would otherwise write a row per attempt, indefinitely.
const refusalGap = 10 * time.Minute

// claimRefusal reports whether a runner.refused_version audit row for runner id may be written
// now, and records it if so (same pattern as claimEdge). The refusal itself is never throttled.
func (s *Server) claimRefusal(id int64, now time.Time) bool {
	s.edgeMu.Lock()
	defer s.edgeMu.Unlock()
	if last, ok := s.refusedAt[id]; ok && now.Sub(last) < refusalGap {
		return false
	}
	if s.refusedAt == nil {
		s.refusedAt = map[int64]time.Time{}
	}
	s.refusedAt[id] = now
	return true
}

// markEdge records an unthrottled edge (the disconnect) so a quick reconnect's connect write is
// throttled against it.
func (s *Server) markEdge(id int64, now time.Time) {
	s.edgeMu.Lock()
	defer s.edgeMu.Unlock()
	s.setEdgeLocked(id, now)
}

func (s *Server) setEdgeLocked(id int64, now time.Time) {
	if s.edgeAt == nil {
		s.edgeAt = map[int64]time.Time{}
	}
	s.edgeAt[id] = now
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
	replacing := s.cfg.Registry.Get(r.ID) != nil
	sess, err := s.cfg.Registry.Register(r.ID, r.Name, r.Fleet, sconn, st, hb)
	if err != nil {
		return nil, err
	}
	if replacing {
		s.cfg.Service.RunnerReplaced(ctx, r.ID)
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
