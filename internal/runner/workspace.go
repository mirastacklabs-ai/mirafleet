package runner

// Workspace mode: the runner serves workspace@mirafleet channels through a workspace.Executor.
// Central is not trusted: every request is re-validated here against the runner's own
// workspace.yaml.
//
// Channel flow (one op per channel):
//  1. decode the request (strict); malformed -> channel rejected (ConnectionFailed);
//  2. re-validate it; if valid, admit it: a full slot table (create) or a saturated workspace
//     or runner (exec) rejects the channel BEFORE accepting it with ResourceShortage, which central
//     reports as ErrBusy. After Accept, a busy answer is never a channel reject;
//  3. accept, audit "start", read exactly StdinLen/Length body bytes (bounded by a timer),
//     run the op, audit the outcome, write one WorkspaceResult frame, then (read only) the body,
//     then CloseWrite.
// Central closing the channel (or the connection dropping) cancels the executor context, which
// kills an in-flight exec.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/audit"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/egress"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/wsconfig"
)

const (
	maxExecPerWorkspace = 4
	wsBodyTimeout       = 60 * time.Second
	wsCreateTimeout     = 5 * time.Minute
	wsFileTimeout       = 60 * time.Second
	wsDestroyTimeout    = 60 * time.Second
	wsListTimeout       = 10 * time.Second
	wsRefuseMax         = 16                                // refused channels whose bodies are being drained, at once
	wsResultMax         = transport.MaxWorkspaceResultBytes // shared with central's registry
	wsOutputBudget      = 1<<20 - 16<<10                    // JSON-encoded stdout+stderr together
	auditArgvMax        = 4 << 10
	wsMsgMax            = 1 << 10
	wsCancelPoll        = 250 * time.Millisecond
	defaultFileMode     = fs.FileMode(0o644)
)

// wsEntry tracks one live (or being created) workspace.
type wsEntry struct {
	sem       chan struct{} // concurrent execs, cap maxExecPerWorkspace
	refs      int           // creates in flight
	confirmed bool          // the executor has it
	doomed    bool          // destroyed while a create was in flight; createDone destroys once it settles
	confGen   uint64        // wsState.gen when confirmed was last set (0: seeded from the executor)

	// destroying is set while a normal-path destroy runs the executor call (F-26). New ops on the
	// entry are refused like on a doomed one, a concurrent destroy waits on destroyDone and
	// answers the same result (no second executor call), prune keeps the entry, and the slot is
	// freed exactly once, by the destroy that set the mark, after the executor returns.
	destroying  bool
	destroyDone chan struct{} // closed when that destroy settles
	destroyOut  outcome       // its answer, valid once destroyDone is closed
}

type wsState struct {
	ex       workspace.Executor
	cfg      *wsconfig.Config
	inflight chan struct{} // all in-flight ops, a coarse bound on goroutines and buffers
	refusing chan struct{} // refused channels draining their body (no buffering), pre-accept bound

	mu     sync.Mutex
	live   map[workspace.ID]*wsEntry
	seeded bool
	gen    uint64 // bumped on every confirm; lets a List prune only entries confirmed before it began
	tomb   tombstones
	now    func() time.Time // injectable clock (tombstone expiry)

	gone  func(workspace.ID)   // called when a workspace was destroyed (M7: tears its credential wiring down)
	adopt func([]workspace.ID) // called once with the workspaces found at start (M7: wires their credentials)
}

// goneWS tells the runner a workspace no longer exists.
func (s *wsState) goneWS(id workspace.ID) {
	if s.gone != nil {
		s.gone(id)
	}
}

const (
	tombMax = 256
	tombTTL = 10 * time.Minute
)

// tombstones remembers ids that were destroyed while absent (a destroy found no entry and the
// executor said not found). A create for such an id is refused, which closes the race where
// central's stray destroy reaches the runner before the create is admitted. Bounded by count
// (oldest dropped first) and by age. The caller holds wsState.mu.
type tombstones struct {
	at    map[workspace.ID]time.Time
	order []workspace.ID // oldest first
}

func (t *tombstones) expire(now time.Time) {
	n := 0
	for n < len(t.order) && now.Sub(t.at[t.order[n]]) >= tombTTL {
		delete(t.at, t.order[n])
		n++
	}
	t.order = t.order[n:]
}

func (t *tombstones) add(id workspace.ID, now time.Time) {
	t.expire(now)
	if t.at == nil {
		t.at = map[workspace.ID]time.Time{}
	}
	if _, ok := t.at[id]; ok {
		for i, o := range t.order {
			if o == id {
				t.order = append(t.order[:i], t.order[i+1:]...)
				break
			}
		}
	} else if len(t.order) >= tombMax {
		delete(t.at, t.order[0])
		t.order = t.order[1:]
	}
	t.at[id] = now
	t.order = append(t.order, id)
}

func (t *tombstones) has(id workspace.ID, now time.Time) bool {
	t.expire(now)
	_, ok := t.at[id]
	return ok
}

func newWSState(ex workspace.Executor, cfg *wsconfig.Config) *wsState {
	return &wsState{ex: ex, cfg: cfg, now: time.Now, live: map[workspace.ID]*wsEntry{},
		inflight: make(chan struct{}, cfg.Concurrent*(maxExecPerWorkspace+1)+8),
		refusing: make(chan struct{}, wsRefuseMax)}
}

// seed loads the executor's existing workspaces once (runner restart keeps them). s.mu is held.
func (s *wsState) seed(ctx context.Context) error {
	if s.seeded {
		return nil
	}
	sctx, cancel := context.WithTimeout(ctx, wsListTimeout)
	defer cancel()
	infos, err := s.ex.List(sctx)
	if err != nil {
		return err
	}
	var adopted []workspace.ID
	for _, in := range infos {
		if workspace.ValidID(in.ID) {
			s.live[in.ID] = &wsEntry{sem: make(chan struct{}, maxExecPerWorkspace), confirmed: true}
			adopted = append(adopted, in.ID)
		}
	}
	s.seeded = true
	if s.adopt != nil {
		s.adopt(adopted) // M7: wire credentials for the sessions that survived a runner restart
	}
	return nil
}

type admission int

const (
	admitOK admission = iota
	admitBusy
	admitNotFound
	admitUnavailable
)

// admit takes the slots an op needs. The returned release must be called when the op ends.
func (s *wsState) admit(ctx context.Context, op *wsOp) (release func(), a admission) {
	select {
	case s.inflight <- struct{}{}:
	default:
		return nil, admitBusy
	}
	relInflight := func() { <-s.inflight }
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.seed(ctx); err != nil {
		relInflight()
		return nil, admitUnavailable
	}
	switch op.req.Op {
	case transport.OpCreate:
		e := s.live[op.id]
		if e != nil && (e.doomed || e.destroying) {
			relInflight()
			return nil, admitBusy
		}
		if e == nil && s.tomb.has(op.id, s.now()) {
			// A destroy for this id already ran: answered not_found, nothing was created.
			relInflight()
			return nil, admitNotFound
		}
		if e == nil {
			if len(s.live) >= s.cfg.Concurrent {
				relInflight()
				return nil, admitBusy
			}
			e = &wsEntry{sem: make(chan struct{}, maxExecPerWorkspace)}
			s.live[op.id] = e
		}
		e.refs++
		return func() { relInflight() }, admitOK
	case transport.OpExec, transport.OpFS, transport.OpJobStart, transport.OpJobStatus,
		transport.OpJobTail, transport.OpJobCancel, transport.OpOutputRead:
		e := s.live[op.id]
		if e == nil || e.doomed || e.destroying {
			relInflight()
			return nil, admitNotFound
		}
		select {
		case e.sem <- struct{}{}:
			return func() { <-e.sem; relInflight() }, admitOK
		default:
			relInflight()
			return nil, admitBusy
		}
	case transport.OpRead, transport.OpWrite:
		if e := s.live[op.id]; e == nil || e.doomed || e.destroying {
			relInflight()
			return nil, admitNotFound
		}
	}
	return relInflight, admitOK
}

// createDone settles a create: on failure the slot is freed unless the workspace exists.
func (s *wsState) createDone(id workspace.ID, ok bool, logf func(string, ...any)) {
	s.mu.Lock()
	e := s.live[id]
	if e == nil {
		s.mu.Unlock()
		return
	}
	e.refs--
	if e.doomed && e.refs <= 0 {
		// A destroy arrived while the create was in flight: it is ours to run now. The entry stays
		// (slot counted, ops see not_found, creates see busy) until the destroy finishes.
		s.mu.Unlock()
		dctx, cancel := context.WithTimeout(context.Background(), wsDestroyTimeout)
		err := s.ex.Destroy(dctx, id)
		cancel()
		failed := err != nil && !errors.Is(err, workspace.ErrNotFound)
		if failed {
			logf("workspace %s: deferred destroy after in-flight create failed: %v", id, err)
		} else {
			s.goneWS(id)
		}
		s.mu.Lock()
		if cur := s.live[id]; cur == e {
			if failed {
				// The container may still exist: keep the slot held and let a later destroy
				// (reconcile orphan path) take the normal path. If the create had failed and nothing
				// exists, the next OpList prunes the entry (the executor won't list it).
				e.doomed = false
				s.confirm(e)
			} else {
				delete(s.live, id)
			}
		}
		s.mu.Unlock()
		return
	}
	defer s.mu.Unlock()
	if ok {
		s.confirm(e)
	} else if e.refs <= 0 && !e.confirmed {
		delete(s.live, id)
	}
}

// confirm marks e as held by the executor. s.mu is held.
func (s *wsState) confirm(e *wsEntry) {
	s.gen++
	e.confirmed, e.confGen = true, s.gen
}

// prune forgets entries that were confirmed before a List began (gen <= startGen), have no create
// in flight and no pending destroy, yet are absent from the executor's listing: the container is
// gone (a failed create whose deferred destroy also failed, or removed out of band). A create
// that finished during the List has a newer generation and is kept.
func (s *wsState) prune(startGen uint64, listed map[workspace.ID]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, e := range s.live {
		if e.confirmed && e.refs == 0 && !e.doomed && !e.destroying && e.confGen <= startGen && !listed[id] {
			delete(s.live, id)
		}
	}
}

// cleanupFailedCreate destroys what a failed or cancelled Create may have left behind (the
// Executor contract says Create cleans up, this is the belt to those braces). It runs before the
// slot entry is dropped, with its own bounded context since ctx may be the reason Create failed.
// A workspace that already existed (confirmed, or another create in flight) is left alone.
func (s *wsState) cleanupFailedCreate(ctx context.Context, id workspace.ID) {
	s.mu.Lock()
	e := s.live[id]
	skip := e == nil || e.confirmed || e.refs > 1
	s.mu.Unlock()
	if skip {
		return
	}
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), wsDestroyTimeout)
	defer cancel()
	s.ex.Destroy(dctx, id) // best effort
}

func (s *wsState) forget(id workspace.ID) {
	s.mu.Lock()
	delete(s.live, id)
	s.mu.Unlock()
}

// opErr is a re-validation failure with the result code to answer with.
type opErr struct {
	code string
	msg  string
}

func (e *opErr) Error() string { return e.msg }

func refuse(format string, a ...any) *opErr {
	return &opErr{transport.CodeRefused, fmt.Sprintf(format, a...)}
}

// wsOp is a request after local re-validation.
type wsOp struct {
	req     *transport.WorkspaceRequest
	id      workspace.ID
	spec    workspace.Spec
	cwd     string
	path    string
	env     map[string]string
	timeout time.Duration
	mode    fs.FileMode
	limit   int64 // read: bytes wanted
	bodyLen int64 // raw request body bytes (exec stdin or write data)

	credRestrict bool     // create: the open named credentials (LabelCredentials)
	credNames    []string // create: those names
}

// prepare re-validates req against the runner's config. It never trusts central.
func (s *wsState) prepare(req *transport.WorkspaceRequest) (*wsOp, *opErr) {
	op := &wsOp{req: req, id: workspace.ID(req.ID)}
	switch req.Op {
	case transport.OpCreate, transport.OpExec, transport.OpRead, transport.OpWrite, transport.OpDestroy,
		transport.OpFS, transport.OpJobStart, transport.OpJobStatus, transport.OpJobTail,
		transport.OpJobCancel, transport.OpOutputRead:
		if !workspace.ValidID(op.id) {
			return nil, refuse("invalid workspace id")
		}
	case transport.OpList, transport.OpCredsSync:
	default:
		return nil, refuse("unknown op")
	}
	cfg := s.cfg
	switch req.Op {
	case transport.OpCreate:
		sp := req.Spec
		if sp == nil {
			return nil, refuse("spec required")
		}
		if sp.Image == "" || !cfg.AllowsImage(sp.Image) {
			return nil, refuse("image not allowed on this runner")
		}
		pull := workspace.PullPolicy(sp.Pull)
		if !pull.Valid() {
			return nil, refuse("bad pull policy")
		}
		for _, c := range []struct {
			what     string
			v, limit int64
		}{
			{"cpu", sp.CPUMilli, int64(cfg.Limits.CPUs * 1000)},
			{"memory", sp.MemoryBytes, int64(cfg.Limits.Memory)},
			{"pids", sp.Pids, cfg.Limits.Pids},
			{"tmpfs", sp.TmpfsBytes, int64(cfg.Limits.Tmpfs)},
			{"disk", sp.DiskBytes, int64(cfg.Limits.Disk)},
		} {
			if c.v < 0 || c.v > c.limit {
				return nil, refuse("%s limit above this runner's cap", c.what)
			}
		}
		mode := sp.NetworkMode
		if mode == "" {
			mode = "none"
		}
		if !cfg.AllowsNetwork(mode) {
			return nil, refuse("network mode %q not allowed on this runner", mode)
		}
		if mode != "egress" && len(sp.Allow) > 0 {
			return nil, refuse("egress rules need network mode egress")
		}
		if sp.RequireSecureRuntime && !cfg.AcceptsGuarded() {
			return nil, refuse("this runner has no secure runtime")
		}
		// Compare in milliseconds before converting: the Duration multiply would wrap.
		if sp.MaxAgeMs < 0 || sp.MaxAgeMs > transport.MaxWorkspaceAgeMs ||
			(cfg.MaxAge > 0 && sp.MaxAgeMs > cfg.MaxAge.Milliseconds()) {
			return nil, refuse("max age above this runner's cap")
		}
		maxAge := time.Duration(sp.MaxAgeMs) * time.Millisecond
		if cfg.MaxAge > 0 && maxAge == 0 {
			maxAge = cfg.MaxAge
		}
		labels := make(map[string]string, len(sp.Labels))
		for k, v := range sp.Labels {
			if k == LabelCredentials {
				labels[k] = v // reserved label central sets; removed below
				continue
			}
			if k == LabelWorkdir { // reserved label for the host executor, which reads it
				if h, ok := s.ex.(HostModer); !ok || !h.HostMode() {
					return nil, refuse("workdir needs a host runner")
				}
				labels[k] = v
				continue
			}
			if k == LabelSession { // reserved label for the host executor, which reads it
				if h, ok := s.ex.(HostModer); !ok || !h.HostMode() {
					return nil, refuse("session label needs a host runner")
				}
				labels[k] = v
				continue
			}
			if strings.HasPrefix(strings.ToLower(k), "mirafleet.") {
				return nil, refuse("label names starting with mirafleet. are reserved")
			}
			labels[k] = v
		}
		restrict, names, lerr := credNamesFromLabels(labels)
		if lerr != nil {
			return nil, refuse("%v", lerr)
		}
		op.credRestrict, op.credNames = restrict, names
		allow := make([]workspace.EgressRule, 0, len(sp.Allow))
		for _, a := range sp.Allow {
			allow = append(allow, workspace.EgressRule{Host: a.Host, Port: a.Port})
		}
		if err := checkEgressRules(sp.Allow, cfg.EgressCIDRRules); err != nil {
			return nil, refuse("%v", err)
		}
		op.spec = workspace.Spec{ID: op.id, Image: sp.Image, Pull: pull,
			Limits: workspace.Limits{CPUMilli: sp.CPUMilli, MemoryBytes: sp.MemoryBytes, Pids: sp.Pids,
				TmpfsBytes: sp.TmpfsBytes, DiskBytes: sp.DiskBytes},
			Network:              workspace.Network{Mode: workspace.NetworkMode(mode), Allow: allow},
			RequireSecureRuntime: sp.RequireSecureRuntime, Labels: labels, MaxAge: maxAge}
	case transport.OpExec:
		if err := workspace.ValidArgv(req.Argv); err != nil {
			return nil, refuse("%v", err)
		}
		cwd, err := workspace.CleanPath(req.Cwd)
		if err != nil {
			return nil, refuse("cwd: %v", err)
		}
		op.cwd = cwd
		env, err := workspace.FilterEnv(req.Env, cfg.EnvAllow)
		if err != nil {
			return nil, refuse("%v", err)
		}
		op.env = env
		if req.StdinLen < 0 || req.StdinLen > transport.MaxBodyBytes {
			return nil, &opErr{transport.CodeTooLarge, "stdin too large"}
		}
		op.bodyLen = req.StdinLen
		max := cfg.Limits.ExecMax
		if max <= 0 {
			max = wsconfig.DefaultExecMax
		}
		op.timeout = max
		if req.TimeoutMs > 0 {
			if t := time.Duration(req.TimeoutMs) * time.Millisecond; t < max {
				op.timeout = t
			}
		}
	case transport.OpRead, transport.OpWrite:
		p, err := workspace.CleanPath(req.Path)
		if err != nil {
			return nil, refuse("path: %v", err)
		}
		if p == workspace.Root {
			return nil, refuse("path is the workspace root")
		}
		op.path = p
		if req.Offset < 0 {
			return nil, refuse("negative offset")
		}
		if req.Op == transport.OpRead {
			if req.Limit < 0 || req.Limit > transport.MaxBodyBytes {
				return nil, &opErr{transport.CodeTooLarge, "limit too large"}
			}
			op.limit = req.Limit
			if op.limit == 0 {
				op.limit = transport.MaxBodyBytes
			}
			break
		}
		if req.Mode < 0 || req.Mode > 0o777 {
			return nil, refuse("file mode must be within 0o777")
		}
		op.mode = fs.FileMode(req.Mode)
		if op.mode == 0 {
			op.mode = defaultFileMode
		}
		if req.Length < 0 || req.Length > transport.MaxBodyBytes {
			return nil, &opErr{transport.CodeTooLarge, "file chunk too large"}
		}
		if mf := int64(cfg.Limits.MaxFile); req.Offset > mf || req.Length > mf-req.Offset {
			return nil, &opErr{transport.CodeTooLarge, "file would exceed this runner's max_file"}
		}
		op.bodyLen = req.Length
	}
	return op, nil
}

// checkEgressRules parses every rule exactly as the egress proxy will (central is not trusted) and
// refuses CIDR and IP-literal rules unless this runner's workspace.yaml opts in.
func checkEgressRules(rules []transport.EgressRule, allowCIDR bool) error {
	strs := make([]string, 0, len(rules))
	for _, a := range rules {
		host := a.Host
		if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
			host = "[" + host + "]"
		}
		strs = append(strs, host+":"+strconv.Itoa(a.Port))
		bare := strings.Trim(a.Host, "[]")
		if _, err := netip.ParseAddr(bare); err == nil || strings.Contains(bare, "/") {
			if !allowCIDR {
				return errors.New("CIDR and IP-literal egress rules are not allowed on this runner")
			}
		}
	}
	if _, err := egress.NewAllowlist(strs); err != nil {
		return err
	}
	return nil
}

// ---- audit ----

func clip(s string, max int) string {
	s = strings.ToValidUTF8(s, "�")
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

func newRequestID() string {
	var b [8]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// auditParams describes the request for the log: argv (clipped), cwd, env NAMES, paths and sizes.
// Env values and file content never appear.
func auditParams(req *transport.WorkspaceRequest) map[string]string {
	p := map[string]string{}
	switch req.Op {
	case transport.OpCreate:
		if sp := req.Spec; sp != nil {
			p["image"] = clip(sp.Image, 256)
			p["network"] = clip(sp.NetworkMode, 16)
		}
	case transport.OpExec:
		b, _ := json.Marshal(req.Argv)
		p["argv"] = clip(string(b), auditArgvMax)
		p["cwd"] = clip(req.Cwd, 512)
		names := make([]string, 0, len(req.Env))
		for k := range req.Env {
			names = append(names, clip(k, 64))
		}
		sort.Strings(names)
		p["env_names"] = strings.Join(names, ",")
		p["stdin_bytes"] = fmt.Sprint(req.StdinLen)
		p["timeout_ms"] = fmt.Sprint(req.TimeoutMs)
	case transport.OpRead:
		p["path"] = clip(req.Path, 512)
		p["offset"] = fmt.Sprint(req.Offset)
		p["limit"] = fmt.Sprint(req.Limit)
	case transport.OpWrite:
		p["path"] = clip(req.Path, 512)
		p["offset"] = fmt.Sprint(req.Offset)
		p["bytes"] = fmt.Sprint(req.Length)
	default:
		if isDevOp(req.Op) {
			return devAuditParams(req)
		}
	}
	return p
}

// ---- serving ----

// outcome is what an op produced.
type outcome struct {
	res    transport.WorkspaceResult
	data   []byte // read: raw bytes sent after the result frame
	audit  string // audit outcome: ok | failed | refused | timeout | error | cancelled
	exit   int
	params map[string]string // extra audit params (bytes, sha256)
}

func fail(code, outc string, err error) outcome {
	return outcome{res: transport.WorkspaceResult{Code: code, Message: clip(err.Error(), wsMsgMax)}, audit: outc}
}

// failFor maps an executor error to a result code.
func failFor(ctx context.Context, err error) outcome {
	switch {
	case errors.Is(err, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded:
		return fail(transport.CodeTimeout, "timeout", err)
	case errors.Is(err, context.Canceled) || ctx.Err() == context.Canceled:
		return fail(transport.CodeInternal, "cancelled", err)
	case errors.Is(err, workspace.ErrNotFound):
		return fail(transport.CodeNotFound, "error", err)
	case errors.Is(err, workspace.ErrRefused):
		return fail(transport.CodeRefused, "refused", err)
	case errors.Is(err, workspace.ErrBusy):
		return fail(transport.CodeBusy, "error", err)
	case errors.Is(err, workspace.ErrTooLarge):
		return fail(transport.CodeTooLarge, "error", err)
	case errors.Is(err, workspace.ErrQuota):
		return fail(transport.CodeQuota, "error", err)
	case errors.Is(err, workspace.ErrUnavailable):
		return fail(transport.CodeUnavailable, "error", err)
	case errors.Is(err, workspace.ErrUnsupported):
		return fail(transport.CodeUnsupported, "error", err)
	}
	return fail(transport.CodeInternal, "error", err)
}

func (r *Runner) serveWorkspace(ctx context.Context, nch ssh.NewChannel) {
	s := r.ws
	if s == nil {
		nch.Reject(ssh.Prohibited, "workspace mode is not enabled on this runner")
		return
	}
	req, err := transport.DecodeWorkspaceRequest(nch.ExtraData())
	if err != nil {
		r.cfg.Audit.Log(audit.Event{Action: "workspace", Outcome: "refused", ExitCode: -1, Message: clip(err.Error(), wsMsgMax)})
		nch.Reject(ssh.ConnectionFailed, "malformed workspace request")
		return
	}
	rid := newRequestID()
	action := "workspace." + req.Op
	logEv := func(outc string, exit int, dur time.Duration, msg string, extra map[string]string) {
		p := auditParams(req)
		for k, v := range extra {
			p[k] = v
		}
		r.cfg.Audit.Log(audit.Event{Action: action, Params: p, Outcome: outc, ExitCode: exit,
			DurationMS: dur.Milliseconds(), Message: clip(msg, wsMsgMax), Subject: req.ID, RequestID: rid})
	}

	op, perr := s.prepare(req)
	var release func()
	var notFound, unavailable bool
	if perr == nil {
		rel, a := s.admit(ctx, op)
		switch a {
		case admitBusy:
			msg := "runner busy: no free workspace slot or exec capacity"
			logEv("refused", -1, 0, msg, nil)
			nch.Reject(ssh.ResourceShortage, msg)
			return
		case admitNotFound:
			notFound = true
		case admitUnavailable:
			unavailable = true
		default:
			release = rel
		}
	}
	if release != nil {
		defer release()
	} else {
		// A refused request still has a body to drain; bound how many drain at once so a flood of
		// refused channels cannot pile up goroutines (the body is discarded, never buffered).
		select {
		case s.refusing <- struct{}{}:
			defer func() { <-s.refusing }()
		default:
			msg := "runner busy: too many refused requests in flight"
			logEv("refused", -1, 0, msg, nil)
			nch.Reject(ssh.ResourceShortage, msg)
			return
		}
	}
	ch, reqs, err := nch.Accept()
	if err != nil {
		if release != nil && req.Op == transport.OpCreate {
			s.createDone(op.id, false, r.cfg.Logf)
		}
		return
	}
	go ssh.DiscardRequests(reqs)
	defer ch.Close()
	ectx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Central closing the channel cancels ectx. A closed channel refuses further writes, which a
	// no-reply channel request surfaces; plain reads cannot tell it from central's CloseWrite.
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		t := time.NewTicker(wsCancelPoll)
		defer t.Stop()
		for {
			select {
			case <-watchDone:
				return
			case <-ectx.Done():
				return
			case <-t.C:
				if _, err := ch.SendRequest("keepalive@mirafleet", false, nil); err != nil {
					cancel()
					return
				}
			}
		}
	}()

	start := time.Now()
	logEv("start", 0, 0, "", nil)

	var body []byte
	bodyLen := req.StdinLen
	if req.Op == transport.OpWrite {
		bodyLen = req.Length
	}
	var out outcome
	var sum string
	if bodyLen > 0 { // read (and so drain) the body even when the request is refused
		var berr error
		body, berr = readBody(ectx, ch, bodyLen, release == nil)
		if berr != nil {
			out = fail(transport.CodeBadRequest, "refused", fmt.Errorf("body: %v", berr))
			out.params = map[string]string{"bytes": fmt.Sprint(len(body))}
		}
	}
	if req.Op == transport.OpWrite && len(body) > 0 {
		h := sha256.Sum256(body)
		sum = hex.EncodeToString(h[:])
	}
	switch {
	case out.res.Code != "": // body failure
	case perr != nil:
		out = outcome{res: transport.WorkspaceResult{Code: perr.code, Message: clip(perr.msg, wsMsgMax)}, audit: "refused"}
	case notFound:
		out = fail(transport.CodeNotFound, "error", workspace.ErrNotFound)
	case unavailable:
		out = fail(transport.CodeUnavailable, "error", workspace.ErrUnavailable)
	default:
		out = r.runWS(ectx, op, body)
	}
	if req.Op == transport.OpCreate && release != nil {
		s.createDone(op.id, out.res.Code == transport.CodeOK, r.cfg.Logf)
	}
	dur := time.Since(start)
	out.res.DurationMs = dur.Milliseconds()
	extra := out.params
	if sum != "" {
		if extra == nil {
			extra = map[string]string{}
		}
		extra["sha256"] = sum
		extra["bytes"] = fmt.Sprint(len(body))
	}
	if out.audit == "" {
		out.audit = "ok"
	}
	logEv(out.audit, out.exit, dur, out.res.Message, extra) // record the outcome before telling central
	if ectx.Err() != nil && out.audit == "cancelled" {
		return
	}
	// A central that never reads must not pin the slot: the writes are bounded and close the channel.
	werr := writeBounded(ch, func() error {
		if err := transport.WriteResult(ch, &out.res, wsResultMax); err != nil {
			return err
		}
		if out.res.Code == transport.CodeOK && (req.Op == transport.OpRead || (req.Op == transport.OpFS && len(out.data) > 0)) {
			if _, err := ch.Write(out.data); err != nil {
				return err
			}
		}
		return nil
	})
	if werr != nil {
		return
	}
	ch.CloseWrite()
}

// wsWriteTimeout bounds the result frame and read body writes (a variable so tests can shorten it).
var wsWriteTimeout = 60 * time.Second

var errWriteTimeout = errors.New("workspace channel write timed out")

// writeBounded runs fn (writes to ch) and closes ch if it takes longer than wsWriteTimeout, which
// unblocks the write.
func writeBounded(ch io.Closer, fn func() error) error {
	t := time.AfterFunc(wsWriteTimeout, func() { ch.Close() })
	err := fn()
	if !t.Stop() && err == nil { // the timer fired just as the write finished
		err = errWriteTimeout
	}
	return err
}

// readBody reads exactly n body bytes plus central's CloseWrite, bounded by wsBodyTimeout and
// ctx (either closes the channel, which unblocks the read). With discard the bytes are checked
// and dropped instead of buffered (refused requests).
func readBody(ctx context.Context, ch ssh.Channel, n int64, discard bool) ([]byte, error) {
	bctx, cancel := context.WithTimeout(ctx, wsBodyTimeout)
	defer cancel()
	stop := context.AfterFunc(bctx, func() { ch.Close() })
	var buf bytes.Buffer
	var dst io.Writer = &buf
	if discard {
		dst = io.Discard
	}
	err := transport.CopyBody(dst, ch, n)
	if !stop() && err == nil { // the timer fired just as the read finished
		err = bctx.Err()
	}
	return buf.Bytes(), err
}

func (r *Runner) runWS(ctx context.Context, op *wsOp, body []byte) outcome {
	s := r.ws
	req := op.req
	switch req.Op {
	case transport.OpCreate:
		cctx, cancel := context.WithTimeout(ctx, wsCreateTimeout)
		defer cancel()
		if op.credRestrict && len(op.credNames) > 0 {
			if r.dev == nil {
				return fail(transport.CodeRefused, "refused", errors.New("this runner holds no credentials"))
			}
			if err := r.dev.checkNames(op.credNames); err != nil {
				return fail(transport.CodeRefused, "refused", err)
			}
		}
		if err := s.ex.Create(cctx, op.spec); err != nil {
			s.cleanupFailedCreate(ctx, op.id)
			return failFor(cctx, err)
		}
		if fo := r.devOpen(op); fo != nil {
			s.cleanupFailedCreate(ctx, op.id)
			return *fo
		}
		return outcome{res: transport.WorkspaceResult{Code: transport.CodeOK}}
	case transport.OpDestroy:
		s.mu.Lock()
		e := s.live[op.id]
		if e != nil && e.destroying {
			// A destroy of this entry is already running the executor call: wait for it and give
			// the same answer, so there is no second executor call and no early slot release.
			done := e.destroyDone
			s.mu.Unlock()
			select {
			case <-done:
				s.mu.Lock()
				out := e.destroyOut
				s.mu.Unlock()
				return out
			case <-ctx.Done():
				return failFor(ctx, ctx.Err())
			}
		}
		if e != nil && (e.doomed || (e.refs > 0 && !e.confirmed)) {
			// A create is still in flight, so the container may not exist yet: createDone owns
			// the destroy and runs it once the create settles. Nothing is lost or left untracked.
			// An already doomed entry (deferred destroy possibly running) answers ok too, so the
			// slot is never freed while a destroy is still running.
			e.doomed = true
			s.mu.Unlock()
			return outcome{res: transport.WorkspaceResult{Code: transport.CodeOK}}
		}
		absent := e == nil
		// The mark is settled in a defer that defaults to failure, so a panic can never leave the
		// entry stuck destroying: waiters get a failure and the slot is kept.
		settled := outcome{res: transport.WorkspaceResult{Code: transport.CodeInternal, Message: "destroy did not complete"}}
		if e != nil {
			e.destroying, e.destroyDone = true, make(chan struct{})
			defer func() {
				s.mu.Lock()
				e.destroying, e.destroyOut = false, settled
				close(e.destroyDone)
				s.mu.Unlock()
			}()
		}
		s.mu.Unlock()
		// Detached from the first caller's cancellation: every waiter shares this destroy.
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), wsDestroyTimeout)
		defer cancel()
		derr := func() (err error) {
			defer func() {
				if p := recover(); p != nil {
					err = fmt.Errorf("executor destroy panicked: %v", p)
				}
			}()
			return r.destroyEx(dctx, op)
		}()
		if derr != nil && !errors.Is(derr, workspace.ErrNotFound) {
			out := failFor(dctx, derr)
			settled = out // the container may still exist: keep the slot, release the mark
			return out
		}
		notFound := derr != nil
		s.goneWS(op.id)
		// The lock was dropped around the executor call, so a create may have been admitted since.
		// Only the entry this destroy saw is deleted; a newer create's entry is doomed instead (its
		// container may now be about to exist), and createDone destroys it once it settles.
		s.mu.Lock()
		cur := s.live[op.id]
		switch {
		case cur == nil:
			if absent && notFound {
				s.tomb.add(op.id, s.now())
			}
		case cur == e && e.refs <= 0:
			delete(s.live, op.id) // the one place a normally destroyed slot is freed
		case cur.refs > 0:
			cur.doomed = true
		}
		ok := outcome{res: transport.WorkspaceResult{Code: transport.CodeOK}}
		settled = ok
		s.mu.Unlock()
		return ok
	case transport.OpList:
		lctx, cancel := context.WithTimeout(ctx, wsListTimeout)
		defer cancel()
		s.mu.Lock()
		startGen := s.gen
		s.mu.Unlock()
		infos, err := s.ex.List(lctx)
		if err != nil {
			return failFor(lctx, err)
		}
		listed := make(map[workspace.ID]bool, len(infos))
		for _, in := range infos {
			listed[in.ID] = true
		}
		s.prune(startGen, listed)
		if len(infos) > transport.MaxListEntries {
			infos = infos[:transport.MaxListEntries]
		}
		list := make([]transport.WorkspaceInfo, 0, len(infos))
		for _, in := range infos {
			list = append(list, transport.WorkspaceInfo{ID: string(in.ID), Image: clip(in.Image, transport.MaxImageBytes),
				State: clip(string(in.State), 32), CreatedMs: in.Created.UnixMilli(), DiskBytes: in.DiskBytes})
		}
		return outcome{res: transport.WorkspaceResult{Code: transport.CodeOK, List: list}}
	case transport.OpExec:
		return r.runExec(ctx, op, body)
	case transport.OpFS, transport.OpJobStart, transport.OpJobStatus, transport.OpJobTail,
		transport.OpJobCancel, transport.OpOutputRead, transport.OpCredsSync:
		return r.runDev(ctx, op, body)
	case transport.OpRead:
		fctx, cancel := context.WithTimeout(ctx, wsFileTimeout)
		defer cancel()
		rc, info, err := s.ex.ReadFile(fctx, op.id, op.path, req.Offset, op.limit)
		if err != nil {
			return failFor(fctx, err)
		}
		defer rc.Close()
		data, err := io.ReadAll(io.LimitReader(rc, op.limit))
		if err != nil {
			return failFor(fctx, err)
		}
		f := &transport.WorkspaceFile{Size: info.Size, Mode: int64(info.Mode.Perm()), Length: int64(len(data)),
			EOF: req.Offset+int64(len(data)) >= info.Size}
		return outcome{res: transport.WorkspaceResult{Code: transport.CodeOK, File: f}, data: data,
			params: map[string]string{"bytes": fmt.Sprint(len(data))}}
	case transport.OpWrite:
		fctx, cancel := context.WithTimeout(ctx, wsFileTimeout)
		defer cancel()
		info, err := s.ex.WriteFile(fctx, op.id, op.path, bytes.NewReader(body), req.Offset, req.Truncate, op.mode)
		if err != nil {
			return failFor(fctx, err)
		}
		return outcome{res: transport.WorkspaceResult{Code: transport.CodeOK,
			File: &transport.WorkspaceFile{Size: info.Size, Mode: int64(info.Mode.Perm())}}}
	}
	return fail(transport.CodeBadRequest, "refused", errors.New("unknown op"))
}

// runBash runs a foreground `bash -lc` on a host executor: the executor owns the timeout and the
// cwd, keeps the full output under Ref and fills the byte counts (the transport result has no
// cwd field; the next command starts in it).
func (r *Runner) runBash(ctx context.Context, bx BashExecutor, op *wsOp, command string) outcome {
	we, err := bx.Bash(ctx, op.id, command, op.timeout, newRequestID())
	if ctx.Err() != nil { // cancelled by central
		return fail(transport.CodeInternal, "cancelled", ctx.Err())
	}
	if err != nil {
		return failFor(ctx, err)
	}
	o := outcome{res: transport.WorkspaceResult{Code: transport.CodeOK, Exec: &we}, exit: we.ExitCode, audit: "ok"}
	switch {
	case we.TimedOut:
		o.audit = "timeout"
		o.res.Message = "timed out; process killed"
	case we.ExitCode != 0:
		o.audit = "failed"
	}
	return o
}

func (r *Runner) runExec(ctx context.Context, op *wsOp, body []byte) outcome {
	if bx, ok := r.ws.ex.(BashExecutor); ok && len(body) == 0 && len(op.env) == 0 && (op.cwd == "" || op.cwd == workspace.Root) {
		if a := op.req.Argv; len(a) == 3 && a[0] == "bash" && a[1] == "-lc" {
			return r.runBash(ctx, bx, op, a[2])
		}
	}
	xctx, cancel := context.WithTimeout(ctx, op.timeout)
	defer cancel()
	outC, errC := workspace.NewCapture(wsOutputBudget/2), workspace.NewCapture(wsOutputBudget/2)
	er := workspace.ExecRequest{Argv: op.req.Argv, Cwd: op.cwd, Env: op.env, Stdout: outC, Stderr: errC}
	if len(body) > 0 {
		er.Stdin = bytes.NewReader(body)
	}
	res, err := r.ws.ex.Exec(xctx, op.id, er)
	timedOut := res.TimedOut || xctx.Err() == context.DeadlineExceeded
	if err != nil && !(timedOut && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled))) {
		return failFor(xctx, err)
	}
	if ctx.Err() != nil { // cancelled by central, not by the timeout
		return fail(transport.CodeInternal, "cancelled", ctx.Err())
	}
	so, st := outC.Result()
	se, st2 := errC.Result()
	o := outcome{res: transport.WorkspaceResult{Code: transport.CodeOK, Exec: &transport.WorkspaceExec{
		ExitCode: res.ExitCode, Stdout: so, Stderr: se, Truncated: st || st2, TimedOut: timedOut,
		DurationMs: res.Duration.Milliseconds()}}, exit: res.ExitCode, audit: "ok"}
	switch {
	case timedOut:
		o.audit = "timeout"
		o.res.Message = "timed out; process killed"
	case res.ExitCode != 0:
		o.audit = "failed"
	}
	return o
}

// ---- heartbeat ----

// fillHeartbeat adds the workspace fields, bounded as Heartbeat.Validate requires.
func (r *Runner) fillHeartbeat(hb *transport.Heartbeat) {
	if r.ws == nil {
		return
	}
	cfg := r.ws.cfg
	hb.Slots = cfg.Concurrent
	hb.Executor = r.ws.ex.Name()
	switch {
	case cfg.Containerd != nil:
		hb.SecureRuntime = cfg.Containerd.SecureRuntime
	case cfg.OpenSandbox != nil:
		hb.SecureRuntime = cfg.OpenSandbox.SecureRuntime
	}
	for _, im := range cfg.Images {
		if len(hb.Images) >= transport.MaxHeartbeatImages {
			break
		}
		if len(im) <= transport.MaxHeartbeatImageBytes {
			hb.Images = append(hb.Images, im)
		}
	}
	r.fillDevHeartbeat(hb)
}

// diskSampleEvery is the period of the disk sampler (a var so tests can shorten it).
var diskSampleEvery = 60 * time.Second

// runDiskSampler samples workspace disk usage every diskSampleEvery until ctx is done and
// destroys workspaces at 2x their quota. It does nothing when the executor has no
// DiskUsager. Run waits for it, so nothing outlives the runner.
func (r *Runner) runDiskSampler(ctx context.Context) {
	if r.ws == nil {
		return
	}
	if _, ok := r.ws.ex.(workspace.DiskUsager); !ok {
		return
	}
	t := time.NewTicker(diskSampleEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.sampleDisks(ctx)
		}
	}
}

// sampleDisks runs one sampling pass. Central learns of a destroyed workspace through its next
// reconcile or list (the row goes lost).
func (r *Runner) sampleDisks(ctx context.Context) {
	s := r.ws
	du, ok := s.ex.(workspace.DiskUsager)
	if !ok {
		return
	}
	s.mu.Lock()
	if err := s.seed(ctx); err != nil { // runner restart: pick up the executor's workspaces
		s.mu.Unlock()
		return
	}
	ids := make([]workspace.ID, 0, len(s.live))
	for id, e := range s.live {
		if e.confirmed {
			ids = append(ids, id)
		}
	}
	s.mu.Unlock()
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		uctx, cancel := context.WithTimeout(ctx, wsFileTimeout)
		u, err := du.Usage(uctx, id)
		cancel()
		if err != nil {
			if errors.Is(err, workspace.ErrNotFound) {
				s.forget(id)
			}
			continue
		}
		if !u.Kill {
			continue
		}
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), wsDestroyTimeout)
		derr := s.ex.Destroy(dctx, id)
		cancel()
		msg := fmt.Sprintf("disk usage %d bytes is at least twice the quota %d: workspace destroyed", u.Used, u.Quota)
		outc := "destroyed"
		if derr != nil && !errors.Is(derr, workspace.ErrNotFound) {
			outc, msg = "failed", msg+": "+derr.Error()
		} else {
			s.forget(id)
			s.goneWS(id)
		}
		r.cfg.Audit.Log(audit.Event{Action: "workspace.disk_kill", Outcome: outc, ExitCode: -1,
			Message: clip(msg, wsMsgMax), Subject: string(id), RequestID: newRequestID()})
	}
}
