// Package registry holds the live runner sessions on central, keyed by runner id (the DB id from
// M2 S2 onwards). It owns the Exec path over a runner's own SSH connection, the heartbeat
// staleness watch, kicks and tombstones (M2 §1.6).
//
// Locking: the registry lock guards only the maps. It is never held across network I/O
// (OpenChannel, Close, reads); a session's own mutex guards its mutable fields.
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// Status mirrors the runner lifecycle status that matters to live sessions.
type Status string

const (
	StatusActive   Status = "active"
	StatusPending  Status = "pending"  // may heartbeat, never exec
	StatusDisabled Status = "disabled" // refuse new exec, let in-flight finish
)

// Valid reports whether st is a known status. Anything else is rejected and never executes.
func (st Status) Valid() bool {
	return st == StatusActive || st == StatusPending || st == StatusDisabled
}

var (
	ErrInvalidStatus = errors.New("invalid runner status")
	ErrTombstoned    = errors.New("runner is tombstoned")
	ErrPending       = errors.New("runner is pending approval")
	ErrDisabled      = errors.New("runner is disabled")
)

// KickError is the cancellation cause of a kicked session.
type KickError struct{ Reason string }

func (e *KickError) Error() string { return "runner session kicked: " + e.Reason }

// Conn is the part of *ssh.ServerConn the registry uses.
type Conn interface {
	OpenChannel(name string, data []byte) (ssh.Channel, <-chan *ssh.Request, error)
	Close() error
}

// SessionInfo is a Snapshot entry.
type SessionInfo struct {
	ID            int64
	Name          string
	Fleet         string
	Status        Status
	Online        bool // registered, not kicked, heartbeat not stale
	LastHeartbeat time.Time
	Since         time.Time
	Info          transport.Heartbeat
}

// Live is what the service layer needs from the registry.
type Live interface {
	Exec(ctx context.Context, id int64, req transport.ExecRequest) (*transport.ExecResult, error)
	Kick(id int64, reason string)
	Tombstone(id int64)
	ClearTombstone(id int64)
	SetStatus(id int64, st Status) error
	Snapshot() []SessionInfo
}

// Options configures a Registry.
type Options struct {
	// StaleAfter: a session with no heartbeat for this long is offline and its connection is
	// closed. Required (M1 default: 3 x heartbeat interval).
	StaleAfter time.Duration
	Logf       func(format string, args ...any)
}

type Registry struct {
	opt Options

	mu         sync.Mutex
	sessions   map[int64]*Session
	tombstones map[int64]struct{}
	statuses   map[int64]Status // desired status per id, recorded by SetStatus even when offline
}

var _ Live = (*Registry)(nil)

// New returns a Registry. opt.StaleAfter must be positive.
func New(opt Options) (*Registry, error) {
	if opt.StaleAfter <= 0 {
		return nil, errors.New("registry: Options.StaleAfter must be > 0")
	}
	if opt.Logf == nil {
		opt.Logf = func(string, ...any) {}
	}
	return &Registry{opt: opt, sessions: map[int64]*Session{},
		tombstones: map[int64]struct{}{}, statuses: map[int64]Status{}}, nil
}

// StaleAfter is the heartbeat silence after which a session is offline.
func (r *Registry) StaleAfter() time.Duration { return r.opt.StaleAfter }

// Session is one runner's live connection.
type Session struct {
	ID    int64
	Name  string
	Fleet string
	since time.Time

	conn     Conn
	ctx      context.Context
	cancel   context.CancelCauseFunc
	stop     chan struct{} // closes the watch
	stopOnce sync.Once

	mu       sync.Mutex
	status   Status
	hb       transport.Heartbeat
	lastSeen time.Time
}

func (s *Session) Conn() Conn { return s.conn }

// Context is cancelled when the session is kicked or unregistered.
func (s *Session) Context() context.Context { return s.ctx }

// Heartbeat records a heartbeat.
func (s *Session) Heartbeat(hb transport.Heartbeat) {
	s.mu.Lock()
	s.hb, s.lastSeen = hb, time.Now()
	s.mu.Unlock()
}

func (s *Session) lastActivity() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastSeen.IsZero() {
		return s.since
	}
	return s.lastSeen
}

func (r *Registry) stale(s *Session) bool { return time.Since(s.lastActivity()) > r.opt.StaleAfter }

// Register adds a session; it is called on the runner's first heartbeat (by then the runner has
// installed its channel handler). The tombstone check and the insert happen under one lock, so
// a revoke cannot slip between authentication and registration. An existing session for the
// same id is replaced and its connection closed (the new connection wins). A status recorded by
// SetStatus for id overrides the caller-supplied one, under the same lock. An unknown status is
// rejected.
func (r *Registry) Register(id int64, name, fleet string, conn Conn, status Status, hb transport.Heartbeat) (*Session, error) {
	if !status.Valid() {
		return nil, ErrInvalidStatus
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	now := time.Now()
	s := &Session{ID: id, Name: name, Fleet: fleet, since: now, conn: conn, ctx: ctx, cancel: cancel,
		stop: make(chan struct{}), status: status, hb: hb, lastSeen: now}
	r.mu.Lock()
	if _, dead := r.tombstones[id]; dead {
		r.mu.Unlock()
		cancel(ErrTombstoned)
		return nil, ErrTombstoned
	}
	if rec, ok := r.statuses[id]; ok {
		s.status = rec // not yet shared, so no s.mu needed
	}
	old := r.sessions[id]
	r.sessions[id] = s
	r.mu.Unlock()
	if old != nil {
		r.end(old, errors.New("replaced by a newer connection"))
		old.conn.Close()
	}
	go r.watch(s)
	return s, nil
}

// Unregister removes s if it is still the current session for its id.
func (r *Registry) Unregister(s *Session) {
	r.mu.Lock()
	cur := r.sessions[s.ID] == s
	if cur {
		delete(r.sessions, s.ID)
	}
	r.mu.Unlock()
	r.end(s, errors.New("unregistered"))
	if cur {
		r.opt.Logf("runner %q offline", s.Name)
	}
}

func (r *Registry) end(s *Session, cause error) {
	s.cancel(cause)
	s.stopOnce.Do(func() { close(s.stop) })
}

// watch closes the connection of a runner that stopped heartbeating (dead peer, half-open TCP).
func (r *Registry) watch(s *Session) {
	t := time.NewTicker(max(r.opt.StaleAfter/6, time.Millisecond))
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			if r.stale(s) {
				r.opt.Logf("runner %q silent for >%s; closing connection", s.Name, r.opt.StaleAfter)
				s.conn.Close()
				return
			}
		}
	}
}

// Get returns the live (registered, not kicked, not stale) session for id, or nil.
func (r *Registry) Get(id int64) *Session {
	r.mu.Lock()
	s := r.sessions[id]
	r.mu.Unlock()
	if s == nil || s.ctx.Err() != nil || r.stale(s) {
		return nil
	}
	return s
}

// Kick cancels the session context and closes its connection. The registry lock is not held
// while closing.
func (r *Registry) Kick(id int64, reason string) {
	r.mu.Lock()
	s := r.sessions[id]
	r.mu.Unlock()
	if s == nil {
		return
	}
	s.cancel(&KickError{Reason: reason})
	s.conn.Close()
}

// Tombstone refuses future registrations of id until ClearTombstone. It does not kick.
func (r *Registry) Tombstone(id int64) {
	r.mu.Lock()
	r.tombstones[id] = struct{}{}
	r.mu.Unlock()
}

func (r *Registry) ClearTombstone(id int64) {
	r.mu.Lock()
	delete(r.tombstones, id)
	r.mu.Unlock()
}

// SetStatus records the desired status for id (kept even when the runner is offline, and applied
// by the next Register) and updates the live session if there is one. An unknown status is
// rejected and changes nothing.
func (r *Registry) SetStatus(id int64, st Status) error {
	if !st.Valid() {
		return ErrInvalidStatus
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statuses[id] = st
	if s := r.sessions[id]; s != nil {
		s.mu.Lock()
		s.status = st
		s.mu.Unlock()
	}
	return nil
}

// Snapshot lists registered sessions sorted by id.
func (r *Registry) Snapshot() []SessionInfo {
	r.mu.Lock()
	list := make([]*Session, 0, len(r.sessions))
	for _, s := range r.sessions {
		list = append(list, s)
	}
	r.mu.Unlock()
	out := make([]SessionInfo, 0, len(list))
	for _, s := range list {
		s.mu.Lock()
		out = append(out, SessionInfo{ID: s.ID, Name: s.Name, Fleet: s.Fleet, Status: s.status,
			LastHeartbeat: s.lastSeen, Since: s.since, Info: s.hb})
		s.mu.Unlock()
		out[len(out)-1].Online = s.ctx.Err() == nil && !r.stale(s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Exec asks a runner to run an action over a channel opened on the runner's own connection.
// A guard refusal comes back as a result with Refused set, not as an error. The exec context
// derives from both ctx and the session context, so a kick cancels it.
func (r *Registry) Exec(ctx context.Context, id int64, req transport.ExecRequest) (*transport.ExecResult, error) {
	sess := r.Get(id)
	if sess == nil {
		return nil, fmt.Errorf("runner %d is not online", id)
	}
	sess.mu.Lock()
	st := sess.status
	sess.mu.Unlock()
	switch st { // deny by default: only an active session execs
	case StatusActive:
	case StatusPending:
		return nil, ErrPending
	case StatusDisabled:
		return nil, ErrDisabled
	default:
		return nil, fmt.Errorf("%w: %q", ErrInvalidStatus, string(st))
	}
	ectx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopLink := context.AfterFunc(sess.ctx, cancel)
	defer stopLink()
	res, err := execOn(ectx, sess.conn, req)
	if err != nil && ctx.Err() == nil && sess.ctx.Err() != nil {
		return nil, context.Cause(sess.ctx)
	}
	return res, err
}

func execOn(ctx context.Context, conn Conn, req transport.ExecRequest) (*transport.ExecResult, error) {
	payload, _ := json.Marshal(req)
	// OpenChannel itself cannot be cancelled: run it aside and give up on ctx. If it opens late,
	// the orphan channel is closed.
	type opened struct {
		ch   ssh.Channel
		reqs <-chan *ssh.Request
		err  error
	}
	done := make(chan opened, 1)
	go func() {
		ch, reqs, err := conn.OpenChannel(transport.ChanExec, payload)
		done <- opened{ch, reqs, err}
	}()
	var o opened
	select {
	case o = <-done:
	case <-ctx.Done():
		go func() {
			if late := <-done; late.err == nil {
				late.ch.Close()
				go ssh.DiscardRequests(late.reqs)
			}
		}()
		return nil, ctx.Err()
	}
	if o.err != nil {
		return nil, fmt.Errorf("open exec channel: %w", o.err)
	}
	ch := o.ch
	go ssh.DiscardRequests(o.reqs)
	defer ch.Close()
	stop := context.AfterFunc(ctx, func() { ch.Close() })
	defer stop()
	data, err := io.ReadAll(io.LimitReader(ch, transport.MaxResultBytes+1))
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	if len(data) > transport.MaxResultBytes {
		return nil, fmt.Errorf("result too large: runner sent more than %d bytes", transport.MaxResultBytes)
	}
	var res transport.ExecResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("runner sent no valid result (%d bytes)", len(data))
	}
	return &res, nil
}
