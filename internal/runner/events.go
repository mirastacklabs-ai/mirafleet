package runner

// Usage events (docs/M7.md section 4.5). Every credential use is counted per session, credential,
// host, status and 60 s window, kept in a bounded in-memory table and sent in batches on
// events@mirafleet. An event holds no URL, path, header or value. When the table is full new keys
// are dropped and counted; the next batch reports the count (central writes events.dropped).

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// Event sender tuning (variables so tests can shorten them).
var (
	eventWindow     = 60 * time.Second
	eventFlushEvery = 10 * time.Second
	eventMaxKeys    = 1024
)

type evKey struct {
	session, cred, host, status string
	windowMs                    int64
}

// eventAgg is the bounded aggregate table.
type eventAgg struct {
	mu      sync.Mutex
	now     func() time.Time
	window  time.Duration
	maxKeys int
	pending map[evKey]int64
	dropped int64
}

func newEventAgg() *eventAgg {
	return &eventAgg{now: time.Now, window: eventWindow, maxKeys: eventMaxKeys, pending: map[evKey]int64{}}
}

func (a *eventAgg) windowStart(t time.Time) int64 {
	w := a.window.Milliseconds()
	return t.UnixMilli() / w * w
}

// record counts one use. status is a transport.Event* value.
func (a *eventAgg) record(session, cred, host, status string) {
	host = clip(host, transport.MaxEventFieldLen)
	if host == "" {
		host = "unknown"
	}
	k := evKey{session: session, cred: cred, host: host, status: status, windowMs: a.windowStart(a.now())}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.pending[k]; !ok && len(a.pending) >= a.maxKeys {
		a.dropped++
		return
	}
	a.pending[k]++
}

func (a *eventAgg) hasPending() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.pending) > 0 || a.dropped > 0
}

// take removes and returns the batches to send: windows that have ended (all of them when all is
// set) and the dropped count, which rides on the first batch.
func (a *eventAgg) take(all bool) []*transport.EventBatch {
	a.mu.Lock()
	cur := a.windowStart(a.now())
	var keys []evKey
	for k := range a.pending {
		if all || k.windowMs < cur {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		x, y := keys[i], keys[j]
		if x.windowMs != y.windowMs {
			return x.windowMs < y.windowMs
		}
		if x.session != y.session {
			return x.session < y.session
		}
		if x.cred != y.cred {
			return x.cred < y.cred
		}
		if x.host != y.host {
			return x.host < y.host
		}
		return x.status < y.status
	})
	var batches []*transport.EventBatch
	b := &transport.EventBatch{}
	size := 0
	flush := func() {
		if len(b.Events) > 0 {
			batches = append(batches, b)
		}
		b, size = &transport.EventBatch{}, 0
	}
	for _, k := range keys {
		ev := transport.Event{Type: transport.EventCredentialUse, Session: k.session, Credential: k.cred,
			Host: k.host, Status: k.status, Count: a.pending[k], WindowMs: k.windowMs, WindowS: int(a.window / time.Second)}
		if ev.WindowS < 1 {
			ev.WindowS = 1
		}
		enc, _ := json.Marshal(ev)
		if len(b.Events) >= transport.MaxBatchEvents || size+len(enc)+1 > transport.MaxBatchBytes-256 {
			flush()
		}
		b.Events = append(b.Events, ev)
		size += len(enc) + 1
		delete(a.pending, k)
	}
	flush()
	if a.dropped > 0 {
		if len(batches) == 0 {
			batches = append(batches, &transport.EventBatch{})
		}
		batches[0].Dropped, a.dropped = a.dropped, 0
	}
	a.mu.Unlock()
	return batches
}

// requeue puts unsent batches back (sums into the table; what no longer fits is counted dropped).
func (a *eventAgg) requeue(batches []*transport.EventBatch) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, b := range batches {
		a.dropped += b.Dropped
		for _, ev := range b.Events {
			k := evKey{ev.Session, ev.Credential, ev.Host, ev.Status, ev.WindowMs}
			if _, ok := a.pending[k]; !ok && len(a.pending) >= a.maxKeys {
				a.dropped += ev.Count
				continue
			}
			a.pending[k] += ev.Count
		}
	}
}

// runEvents sends the aggregates over one connection until ctx ends. A central that refuses the
// channel (an older central) is logged once and left alone for this connection.
func (r *Runner) runEvents(ctx context.Context, client *ssh.Client) {
	if r.dev == nil {
		return
	}
	agg := r.dev.events
	var ch ssh.Channel
	defer func() {
		if ch != nil {
			ch.Close()
		}
	}()
	t := time.NewTicker(eventFlushEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if !agg.hasPending() {
			continue
		}
		batches := agg.take(false)
		if len(batches) == 0 {
			continue
		}
		if ch == nil {
			c, reqs, err := client.OpenChannel(transport.ChanEvents, nil)
			if err != nil {
				agg.requeue(batches)
				if _, refused := err.(*ssh.OpenChannelError); refused {
					r.cfg.Logf("central refused %s: usage events are not sent on this connection", transport.ChanEvents)
					return
				}
				continue
			}
			go ssh.DiscardRequests(reqs)
			ch = c
		}
		for i, b := range batches {
			if err := writeBounded(ch, func() error { return transport.WriteEventBatch(ch, b) }); err != nil {
				agg.requeue(batches[i:])
				ch.Close()
				ch = nil
				break
			}
		}
	}
}
