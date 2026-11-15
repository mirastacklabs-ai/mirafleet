package runner

// Host-mode glue (docs/M7.md sections 3 and 4): the optional executor capabilities the runner
// dispatches the M7 ops to, the per-session credential wiring (broker, ssh-agent, CA bundle, ssh
// config, tool environment) and the dispatch of fs, job_*, output_read and creds_sync.
//
// The executor is reached only through small optional interfaces, so the container executors are
// untouched and hostx can implement them independently. Credential values never leave the
// holder: a session gets a broker and an agent that use them, and an environment that names only
// files, sockets and the per-session proxy.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/audit"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/broker"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/sshagent"
)

// LabelCredentials is the spec label that carries the credential names a workspace_open asked
// for: a comma-separated list; absent means every held credential, empty means none. Central
// sets it; the runner checks every name is held and removes the label before the executor sees it.
const LabelCredentials = "mirafleet.credentials"

// SessionHooks is implemented by an executor that has a per-session state directory. The runner
// puts the agent socket, CA bundle and ssh config in SessionDir, and hands the executor the tool
// environment through SetSessionEnv; the executor adds it to every child it starts for the
// session (foreground bash and background jobs), over anything the agent sets. A nil or empty
// env clears it.
type SessionHooks interface {
	SessionDir(id workspace.ID) (string, error)
	SetSessionEnv(id workspace.ID, env map[string]string)
}

// FSExecutor runs one file-tool op. For a write, body is the raw content. An image result sets
// FSResult.Length and returns those bytes as data.
type FSExecutor interface {
	FS(ctx context.Context, id workspace.ID, op transport.FSOp, body []byte) (res transport.FSResult, data []byte, err error)
}

// JobExecutor runs background jobs and pages saved output. JobStatus with an empty job lists the
// session's jobs. Busy is the number of running jobs (heartbeat busy).
type JobExecutor interface {
	JobStart(ctx context.Context, id workspace.ID, op transport.JobOp) (transport.JobResult, error)
	JobStatus(ctx context.Context, id workspace.ID, job string) ([]transport.JobResult, error)
	JobTail(ctx context.Context, id workspace.ID, job, filter string) (transport.JobResult, error)
	JobCancel(ctx context.Context, id workspace.ID, job string) (transport.JobResult, error)
	OutputRead(ctx context.Context, id workspace.ID, op transport.OutputOp) (transport.OutputResult, error)
	Busy() int
}

// DestroyDeleter is implemented by an executor that can also delete the session directory
// (workspace_close {delete: true}).
type DestroyDeleter interface {
	DestroyDelete(ctx context.Context, id workspace.ID) error
}

// BashExecutor is implemented by the host executor: a foreground bash command run in the
// session's persisted cwd, with the full output kept on the runner under the result's Ref
// (docs/M7.md section 3.6). The runner uses it for `bash -lc <command>` execs with no stdin, env
// or cwd; everything else goes through Exec. The timeout is passed separately because the
// executor reports a timeout itself.
type BashExecutor interface {
	Bash(ctx context.Context, id workspace.ID, command string, timeout time.Duration, requestID string) (transport.WorkspaceExec, error)
}

// HostModer marks an executor as host mode (heartbeat feature "host").
type HostModer interface{ HostMode() bool }

// devCapable reports whether the M7 capabilities of ex are live: an executor that implements
// HostModer (the fake does for tests) must say yes.
func devCapable(ex workspace.Executor) bool {
	if h, ok := ex.(HostModer); ok {
		return h.HostMode()
	}
	return true
}

// ---- per-session wiring ----

const (
	caFile        = "ca.pem"
	sshConfigFile = "ssh_config"
	knownHosts    = "known_hosts"
)

// systemBundle reads the VM's CA bundle (a variable so tests inject one).
var systemBundle = broker.SystemBundle

// devMgr owns the credential holder, the event table and the live sessions.
type devMgr struct {
	r      *Runner
	hooks  SessionHooks
	holder *credHolder
	events *eventAgg
	now    func() time.Time

	mu       sync.Mutex
	sessions map[workspace.ID]*devSession
}

func newDevMgr(r *Runner, hooks SessionHooks) *devMgr {
	return &devMgr{r: r, hooks: hooks, holder: newCredHolder(), events: newEventAgg(), now: time.Now,
		sessions: map[workspace.ID]*devSession{}}
}

func (m *devMgr) logf(format string, a ...any) { m.r.cfg.Logf(format, a...) }

// checkNames refuses a workspace_open that asks for a credential this runner does not hold.
func (m *devMgr) checkNames(names []string) error {
	for _, n := range names {
		if !m.holder.has(n) {
			return fmt.Errorf("credential %q is not held by this runner", n)
		}
	}
	return nil
}

// open wires (or re-wires, on a reattach) the session. restrict says names narrows the set.
func (m *devMgr) open(id workspace.ID, restrict bool, names []string) error {
	dir, err := m.hooks.SessionDir(id)
	if err != nil {
		return err
	}
	m.mu.Lock()
	s := m.sessions[id]
	if s == nil {
		s = &devSession{m: m, id: id, dir: dir}
		m.sessions[id] = s
	}
	m.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.restrict, s.names = restrict, map[string]bool{}
	for _, n := range names {
		s.names[n] = true
	}
	if err := s.refresh(); err != nil {
		s.teardown()
		m.mu.Lock()
		delete(m.sessions, id)
		m.mu.Unlock()
		return err
	}
	return nil
}

// close tears the session's wiring down (the directory stays).
func (m *devMgr) close(id workspace.ID) {
	m.mu.Lock()
	s := m.sessions[id]
	delete(m.sessions, id)
	m.mu.Unlock()
	if s == nil {
		return
	}
	s.mu.Lock()
	s.teardown()
	s.mu.Unlock()
}

func (m *devMgr) closeAll() {
	m.mu.Lock()
	ids := make([]workspace.ID, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.close(id)
	}
}

// refreshAll re-evaluates every session after a creds_sync.
func (m *devMgr) refreshAll() {
	m.mu.Lock()
	ss := make([]*devSession, 0, len(m.sessions))
	for _, s := range m.sessions {
		ss = append(ss, s)
	}
	m.mu.Unlock()
	for _, s := range ss {
		s.mu.Lock()
		if err := s.refresh(); err != nil {
			m.logf("workspace %s: credential refresh failed: %v", s.id, err)
		}
		s.mu.Unlock()
	}
}

// tick runs on every heartbeat: a credential that expired between syncs stops being offered.
func (m *devMgr) tick() {
	sig := m.holder.signature(m.now())
	m.mu.Lock()
	var stale []*devSession
	for _, s := range m.sessions {
		if s.lastSig() != sig {
			stale = append(stale, s)
		}
	}
	m.mu.Unlock()
	for _, s := range stale {
		s.mu.Lock()
		if err := s.refresh(); err != nil {
			m.logf("workspace %s: credential refresh failed: %v", s.id, err)
		}
		s.mu.Unlock()
	}
}

// devSession is one workspace's credential wiring. mu guards everything below.
type devSession struct {
	m   *devMgr
	id  workspace.ID
	dir string

	mu       sync.Mutex
	restrict bool
	names    map[string]bool
	sig      string // holder signature at the last refresh (read by tick under sigMu)
	sigMu    sync.Mutex

	brk      *broker.Broker
	brkLn    net.Listener
	brkHosts string // sorted CA host set; a change rebuilds the broker
	prov     *broker.MapProvider

	ag    *sshagent.Server
	idp   *identProvider
	envOK map[string]string
}

func (s *devSession) lastSig() string {
	s.sigMu.Lock()
	defer s.sigMu.Unlock()
	return s.sig
}

// selected returns the usable credentials this session may use.
func (s *devSession) selected(now time.Time) []*credEntry {
	var out []*credEntry
	for _, e := range s.m.holder.entries() {
		if !e.usable(now) || (s.restrict && !s.names[e.meta.Name]) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// refresh brings the broker, agent, files and environment in line with the holder. s.mu is held.
func (s *devSession) refresh() error {
	now := s.m.now()
	s.sigMu.Lock()
	s.sig = s.m.holder.signature(now)
	s.sigMu.Unlock()
	sel := s.selected(now)
	var web, keys []*credEntry
	for _, e := range sel {
		if e.isSSH() {
			keys = append(keys, e)
		} else {
			web = append(web, e)
		}
	}
	env := map[string]string{}
	if err := s.refreshBroker(web, env); err != nil {
		return err
	}
	if err := s.refreshAgent(keys, env); err != nil {
		return err
	}
	s.envOK = env
	s.m.hooks.SetSessionEnv(s.id, env)
	return nil
}

func (s *devSession) refreshBroker(web []*credEntry, env map[string]string) error {
	caPath := filepath.Join(s.dir, caFile)
	if len(web) == 0 {
		s.closeBroker()
		os.Remove(caPath)
		return nil
	}
	creds := make([]broker.Credential, len(web))
	for i, e := range web {
		creds[i] = e.bc
	}
	hosts := broker.NewMapProvider(creds...).Hosts()
	sort.Strings(hosts)
	key := strings.Join(compact(hosts), ",")
	if s.brk != nil && key == s.brkHosts {
		s.prov.Replace(creds) // same host set: the CA stays, only the values change
	} else {
		// The session CA's NameConstraints are fixed at creation: a new host set needs a new broker,
		// a new CA and a new bundle.
		sys, err := systemBundle()
		if err != nil {
			return errors.New("no system CA bundle found on this VM")
		}
		prov := broker.NewMapProvider(creds...)
		sid := string(s.id)
		b, err := broker.New(broker.Config{Provider: prov, CorporateCA: s.m.r.cfg.CorporateCA,
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
			OnUse:  func(u broker.Use) { s.m.onUse(sid, u) }})
		if err != nil {
			return fmt.Errorf("broker: %v", err)
		}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			b.Close()
			return fmt.Errorf("broker listen: %v", err)
		}
		if err := writeSessionFile(caPath, b.Bundle(sys)); err != nil {
			b.Close()
			ln.Close()
			return err
		}
		go b.Serve(ln)
		s.closeBroker()
		s.brk, s.brkLn, s.brkHosts, s.prov = b, ln, key, prov
	}
	addr := s.brkLn.Addr().String()
	port := addr[strings.LastIndexByte(addr, ':')+1:]
	proxy := s.brk.ProxyURL(addr)
	for _, k := range []string{"HTTPS_PROXY", "HTTP_PROXY", "https_proxy", "http_proxy"} {
		env[k] = proxy
	}
	env["NO_PROXY"], env["no_proxy"] = "localhost,127.0.0.1,::1", "localhost,127.0.0.1,::1"
	for _, k := range []string{"SSL_CERT_FILE", "GIT_SSL_CAINFO", "CURL_CA_BUNDLE", "REQUESTS_CA_BUNDLE", "PIP_CERT", "NODE_EXTRA_CA_CERTS"} {
		env[k] = caPath
	}
	pw := s.brk.Password()
	env["JAVA_TOOL_OPTIONS"] = strings.Join([]string{
		"-Dhttp.proxyHost=127.0.0.1", "-Dhttp.proxyPort=" + port,
		"-Dhttps.proxyHost=127.0.0.1", "-Dhttps.proxyPort=" + port,
		"-Dhttp.proxyUser=mirafleet", "-Dhttp.proxyPassword=" + pw,
		"-Dhttps.proxyUser=mirafleet", "-Dhttps.proxyPassword=" + pw,
		"-Dhttp.nonProxyHosts=localhost|127.0.0.1",
		"-Djdk.http.auth.tunneling.disabledSchemes=",
	}, " ")
	return nil
}

func (s *devSession) closeBroker() {
	if s.brk != nil {
		s.brk.Close()
		s.brkLn.Close()
	}
	s.brk, s.brkLn, s.brkHosts, s.prov = nil, nil, "", nil
}

func compact(sorted []string) []string {
	out := sorted[:0:0]
	for i, h := range sorted {
		if i == 0 || h != sorted[i-1] {
			out = append(out, h)
		}
	}
	return out
}

func (s *devSession) refreshAgent(keys []*credEntry, env map[string]string) error {
	if len(keys) == 0 {
		s.closeAgent()
		for _, p := range s.agentFiles() {
			os.Remove(p)
		}
		return nil
	}
	ids := make([]sshagent.Identity, len(keys))
	for i, e := range keys {
		id := *e.identity
		name, host := e.meta.Name, e.meta.Hosts[0]
		id.Signer = &useSigner{Signer: id.Signer, on: func(err error) {
			st := transport.EventOK
			if err != nil {
				st = transport.EventError
			}
			s.m.record(string(s.id), name, host, st)
		}}
		ids[i] = id
	}
	if s.idp == nil {
		s.idp = &identProvider{}
	}
	s.idp.set(ids)
	if s.ag == nil {
		ag, err := sshagent.Listen(s.dir, s.idp, os.Getuid())
		if err != nil {
			return fmt.Errorf("ssh-agent: %v", err)
		}
		s.ag = ag
	}
	cfg, err := s.writeSSHFiles(keys)
	if err != nil {
		return err
	}
	env["SSH_AUTH_SOCK"] = s.ag.Path()
	env["GIT_SSH_COMMAND"] = "ssh -F " + shQuote(cfg)
	return nil
}

func (s *devSession) closeAgent() {
	if s.ag != nil {
		s.ag.Close()
		s.ag = nil
	}
	s.idp = nil
}

func (s *devSession) agentFiles() []string {
	files := []string{filepath.Join(s.dir, sshConfigFile), filepath.Join(s.dir, knownHosts)}
	pubs, _ := filepath.Glob(filepath.Join(s.dir, "id_*.pub"))
	return append(files, pubs...)
}

// writeSSHFiles writes the pinned known_hosts, the public halves of the keys (so IdentitiesOnly
// selects the agent's key) and the ssh config. None of it is secret.
func (s *devSession) writeSSHFiles(keys []*credEntry) (cfgPath string, err error) {
	var cfg, kh strings.Builder
	for _, p := range s.agentFiles()[2:] {
		os.Remove(p) // stale pub files of removed keys
	}
	khPath := filepath.Join(s.dir, knownHosts)
	for i, e := range keys {
		pubPath := filepath.Join(s.dir, "id_"+strconv.Itoa(i)+".pub")
		if err := writeSessionFile(pubPath, ssh.MarshalAuthorizedKey(e.identity.Signer.PublicKey())); err != nil {
			return "", err
		}
		for _, l := range e.meta.KnownHosts {
			kh.WriteString(l + "\n")
		}
		user := e.identity.User
		for _, h := range e.meta.Hosts {
			name, port, hasPort := strings.Cut(h, ":")
			fmt.Fprintf(&cfg, "Host %s\n", name)
			if hasPort {
				fmt.Fprintf(&cfg, "    Port %s\n", port)
			}
			fmt.Fprintf(&cfg, "    User %s\n    IdentityAgent %s\n    IdentityFile %s\n    IdentitiesOnly yes\n",
				user, sshArg(s.ag.Path()), sshArg(pubPath))
			fmt.Fprintf(&cfg, "    UserKnownHostsFile %s\n    GlobalKnownHostsFile /dev/null\n    StrictHostKeyChecking yes\n    PasswordAuthentication no\n",
				sshArg(khPath))
		}
	}
	if err := writeSessionFile(khPath, []byte(kh.String())); err != nil {
		return "", err
	}
	cfgPath = filepath.Join(s.dir, sshConfigFile)
	return cfgPath, writeSessionFile(cfgPath, []byte(cfg.String()))
}

// teardown closes the proxy and the agent socket and removes the files; the directory stays.
func (s *devSession) teardown() {
	s.closeBroker()
	s.closeAgent()
	os.Remove(filepath.Join(s.dir, caFile))
	for _, p := range s.agentFiles() {
		os.Remove(p)
	}
	s.m.hooks.SetSessionEnv(s.id, nil)
}

// writeAtomic writes data to path with mode 0600 through a temporary file in the same directory.
func writeSessionFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(data)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		os.Remove(tmp)
		return errors.Join(werr, cerr)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// sshArg quotes a path for an ssh_config value.
func sshArg(s string) string {
	if strings.ContainsAny(s, " \t\"") {
		return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	return s
}

// identProvider is the agent's atomically replaceable identity list.
type identProvider struct {
	mu  sync.RWMutex
	ids []sshagent.Identity
}

func (p *identProvider) set(ids []sshagent.Identity) {
	p.mu.Lock()
	p.ids = ids
	p.mu.Unlock()
}

func (p *identProvider) Identities() []sshagent.Identity {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]sshagent.Identity(nil), p.ids...)
}

// useSigner reports each signature to the usage log; it never sees more than the agent does.
type useSigner struct {
	ssh.Signer
	on func(err error)
}

func (u *useSigner) Sign(r io.Reader, data []byte) (*ssh.Signature, error) {
	sig, err := u.Signer.Sign(r, data)
	u.on(err)
	return sig, err
}

func (u *useSigner) SignWithAlgorithm(r io.Reader, data []byte, algo string) (*ssh.Signature, error) {
	var sig *ssh.Signature
	var err error
	if as, ok := u.Signer.(ssh.AlgorithmSigner); ok {
		sig, err = as.SignWithAlgorithm(r, data, algo)
	} else {
		sig, err = u.Signer.Sign(r, data)
	}
	u.on(err)
	return sig, err
}

// useStatus maps an upstream HTTP status to an event status. Only a rejected credential and a
// failing upstream are reported as such; an ordinary 404 means the credential was accepted.
func useStatus(code int) string {
	switch {
	case code == 401 || code == 403:
		return transport.EventDenied
	case code <= 0 || code >= 500:
		return transport.EventError
	}
	return transport.EventOK
}

// onUse records a broker use: runner-local audit row plus the aggregated event.
func (m *devMgr) onUse(session string, u broker.Use) {
	m.record(session, u.Credential, u.Host, useStatus(u.Status))
}

func (m *devMgr) record(session, cred, host, status string) {
	m.events.record(session, cred, host, status)
	outc := "ok"
	if status != transport.EventOK {
		outc = status
	}
	m.r.cfg.Audit.Log(audit.Event{Action: "credential.use", Outcome: outc, ExitCode: -1, Subject: session,
		Params: map[string]string{"credential": cred, "host": clip(host, 253), "status": status}})
}

// ---- spec label ----

// credNamesFromLabels removes LabelCredentials from labels and returns its names.
func credNamesFromLabels(labels map[string]string) (restrict bool, names []string, err error) {
	v, ok := labels[LabelCredentials]
	if !ok {
		return false, nil, nil
	}
	delete(labels, LabelCredentials)
	if v == "" {
		return true, nil, nil
	}
	seen := map[string]bool{}
	for _, n := range strings.Split(v, ",") {
		if !transport.ValidCredName(n) || seen[n] || len(seen) >= transport.MaxSyncCreds {
			return false, nil, errors.New("invalid credentials list")
		}
		seen[n] = true
		names = append(names, n)
	}
	return true, names, nil
}

// ---- heartbeat ----

// fillDevHeartbeat adds the M7 fields: features, credentials (never values) and busy jobs.
func (r *Runner) fillDevHeartbeat(hb *transport.Heartbeat) {
	ex := r.ws.ex
	if !devCapable(ex) {
		return
	}
	if _, ok := ex.(FSExecutor); ok {
		hb.Features = append(hb.Features, transport.FeatureFS)
	}
	if j, ok := ex.(JobExecutor); ok {
		hb.Features = append(hb.Features, transport.FeatureJobs)
		hb.Busy = min(max(j.Busy(), 0), transport.MaxHeartbeatBusy)
	}
	if r.dev != nil {
		hb.Features = append(hb.Features, transport.FeatureCreds, transport.FeatureEvents)
		hb.Credentials = r.dev.holder.heartbeat(r.dev.now())
	}
	if h, ok := ex.(HostModer); ok && h.HostMode() {
		hb.Features = append(hb.Features, transport.FeatureHost)
	}
}

// ---- dispatch ----

func devAuditParams(req *transport.WorkspaceRequest) map[string]string {
	p := map[string]string{}
	switch req.Op {
	case transport.OpFS:
		if f := req.FS; f != nil {
			p["op"] = f.Op
			if f.Path != "" {
				p["path"] = clip(f.Path, 512)
			}
			if f.Op == transport.FSWrite {
				p["bytes"] = fmt.Sprint(f.Length)
				p["sha256"] = f.SHA256
			}
		}
	case transport.OpJobStart, transport.OpJobStatus, transport.OpJobTail, transport.OpJobCancel:
		if j := req.Job; j != nil {
			if j.Job != "" {
				p["job"] = j.Job
			}
			if j.Command != "" {
				p["command"] = clip(j.Command, auditArgvMax)
			}
		}
	case transport.OpOutputRead:
		if o := req.Out; o != nil {
			p["ref"], p["job"] = o.Ref, o.Job
			p["offset"], p["limit"] = fmt.Sprint(o.Offset), fmt.Sprint(o.Limit)
		}
	}
	return p
}

func isDevOp(op string) bool {
	switch op {
	case transport.OpFS, transport.OpJobStart, transport.OpJobStatus, transport.OpJobTail,
		transport.OpJobCancel, transport.OpOutputRead, transport.OpCredsSync:
		return true
	}
	return false
}

// runDev executes fs, job_*, output_read and creds_sync.
func (r *Runner) runDev(ctx context.Context, op *wsOp, body []byte) outcome {
	req := op.req
	unsupported := func() outcome { return fail(transport.CodeUnsupported, "error", workspace.ErrUnsupported) }
	ex := r.ws.ex
	if !devCapable(ex) {
		return unsupported()
	}
	switch req.Op {
	case transport.OpCredsSync:
		defer clear(body) // the request body held the secrets; wipe our copy
		if r.dev == nil {
			return unsupported()
		}
		sync, err := transport.DecodeCredsSync(body)
		if err != nil {
			return fail(transport.CodeBadRequest, "refused", err)
		}
		applied, gen, n := r.dev.holder.apply(sync)
		if applied {
			r.dev.refreshAll()
			select {
			case r.hbKick <- struct{}{}: // let central see the new status without waiting
			default:
			}
		}
		o := outcome{res: transport.WorkspaceResult{Code: transport.CodeOK, Creds: &transport.CredsSyncResult{Generation: gen, Count: n}},
			params: map[string]string{"generation": fmt.Sprint(gen), "count": fmt.Sprint(n)}}
		if !applied {
			o.params["stale"] = "true"
		}
		return o
	case transport.OpFS:
		fx, ok := ex.(FSExecutor)
		if !ok {
			return unsupported()
		}
		fctx, cancel := context.WithTimeout(ctx, wsFileTimeout)
		defer cancel()
		res, data, err := fx.FS(fctx, op.id, *req.FS, body)
		if err != nil {
			return failFor(fctx, err)
		}
		if res.Length != int64(len(data)) {
			return fail(transport.CodeInternal, "error", errors.New("fs result body length mismatch"))
		}
		return outcome{res: transport.WorkspaceResult{Code: transport.CodeOK, FS: &res}, data: data}
	}
	jx, ok := ex.(JobExecutor)
	if !ok {
		return unsupported()
	}
	jctx, cancel := context.WithTimeout(ctx, wsFileTimeout)
	defer cancel()
	ok2 := func(res transport.WorkspaceResult) outcome {
		res.Code = transport.CodeOK
		return outcome{res: res}
	}
	switch req.Op {
	case transport.OpJobStart:
		j, err := jx.JobStart(jctx, op.id, *req.Job)
		if err != nil {
			return failFor(jctx, err)
		}
		return ok2(transport.WorkspaceResult{Job: &j})
	case transport.OpJobStatus:
		js, err := jx.JobStatus(jctx, op.id, req.Job.Job)
		if err != nil {
			return failFor(jctx, err)
		}
		if req.Job.Job != "" {
			if len(js) == 0 {
				return fail(transport.CodeNotFound, "error", workspace.ErrNotFound)
			}
			return ok2(transport.WorkspaceResult{Job: &js[0]})
		}
		if len(js) > transport.MaxJobList {
			js = js[:transport.MaxJobList]
		}
		return ok2(transport.WorkspaceResult{Jobs: js})
	case transport.OpJobTail:
		j, err := jx.JobTail(jctx, op.id, req.Job.Job, req.Job.Filter)
		if err != nil {
			return failFor(jctx, err)
		}
		return ok2(transport.WorkspaceResult{Job: &j})
	case transport.OpJobCancel:
		j, err := jx.JobCancel(jctx, op.id, req.Job.Job)
		if err != nil {
			return failFor(jctx, err)
		}
		return ok2(transport.WorkspaceResult{Job: &j})
	case transport.OpOutputRead:
		o, err := jx.OutputRead(jctx, op.id, *req.Out)
		if err != nil {
			return failFor(jctx, err)
		}
		return ok2(transport.WorkspaceResult{Output: &o})
	}
	return fail(transport.CodeBadRequest, "refused", errors.New("unknown op"))
}

// devOpen wires a freshly created (or reattached) workspace. It returns a failure outcome or nil.
func (r *Runner) devOpen(op *wsOp) *outcome {
	if r.dev == nil {
		return nil
	}
	if err := r.dev.open(op.id, op.credRestrict, op.credNames); err != nil {
		o := fail(transport.CodeInternal, "error", err)
		return &o
	}
	return nil
}

// destroyEx destroys the workspace, deleting its directory too when asked.
func (r *Runner) destroyEx(ctx context.Context, op *wsOp) error {
	if op.req.Delete {
		dd, ok := r.ws.ex.(DestroyDeleter)
		if !ok {
			return fmt.Errorf("%w: this executor cannot delete a session directory", workspace.ErrUnsupported)
		}
		return dd.DestroyDelete(ctx, op.id)
	}
	return r.ws.ex.Destroy(ctx, op.id)
}
