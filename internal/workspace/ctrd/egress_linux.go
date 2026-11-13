//go:build linux

package ctrd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"

	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/egress"
)

const (
	// labelEgress holds the workspace's egress rules (comma separated) so a restarted runner
	// can rebuild the proxy. Container label values are limited to a few KiB in total.
	labelEgress = "mirafleet.egress"

	egressListen   = "127.0.0.1:3128" // inside the container's own network namespace
	egressProxyURL = "http://" + egressListen
	maxEgressLabel = 2048
)

// egressMgr owns the proxies of the workspaces on this runner. The zero value is usable.
type egressMgr struct {
	mu      sync.Mutex
	limits  *egress.Limits
	proxies map[workspace.ID]*egress.Proxy
}

func (m *egressMgr) sharedLimits() *egress.Limits {
	if m.limits == nil {
		m.limits = egress.NewLimits(0, 0)
	}
	return m.limits
}

// EgressEnv returns the proxy environment for execs in workspace id, or nil when the workspace
// has no egress listener (network=none, or not attached). NO_PROXY is empty on purpose:
// the container has no other route.
func (e *Executor) EgressEnv(id workspace.ID) map[string]string {
	e.egress.mu.Lock()
	defer e.egress.mu.Unlock()
	if _, ok := e.egress.proxies[id]; !ok {
		return nil
	}
	return map[string]string{
		"HTTP_PROXY": egressProxyURL, "HTTPS_PROXY": egressProxyURL,
		"http_proxy": egressProxyURL, "https_proxy": egressProxyURL,
		"NO_PROXY": "", "no_proxy": "",
	}
}

// egressRules converts the spec's rules to proxy rule strings and validates them against the
// runner's policy. CIDR and IP-literal rules need egress_cidr_rules.
func (e *Executor) egressRules(allow []workspace.EgressRule) ([]string, error) {
	rules := make([]string, 0, len(allow))
	for _, r := range allow {
		h := r.Host
		if h == "" {
			return nil, refuse("egress rule with empty host")
		}
		if r.Port < 0 || r.Port > 65535 {
			return nil, refuse("egress rule %q: bad port %d", h, r.Port)
		}
		isAddr := strings.Contains(h, "/")
		if !isAddr {
			_, err := netip.ParseAddr(strings.Trim(h, "[]"))
			isAddr = err == nil
		}
		if isAddr && !e.cfg.EgressCIDRRules {
			return nil, refuse("egress rule %q: IP and CIDR rules are not enabled on this runner (egress_cidr_rules)", h)
		}
		s := h
		if strings.Contains(h, ":") && !strings.HasPrefix(h, "[") {
			s = "[" + h + "]"
		}
		if r.Port != 0 {
			s += ":" + strconv.Itoa(r.Port)
		} else if strings.Contains(h, "/") {
			return nil, refuse("egress rule %q: a CIDR rule needs a port", h)
		}
		rules = append(rules, s)
	}
	if _, err := egress.NewAllowlist(rules); err != nil {
		return nil, refuse("%v", err)
	}
	if len(strings.Join(rules, ",")) > maxEgressLabel {
		return nil, refuse("egress allowlist too large")
	}
	return rules, nil
}

// startEgress opens the listener inside the task's network namespace and serves the proxy.
func (e *Executor) startEgress(id workspace.ID, pid uint32, rules []string) error {
	al, err := egress.NewAllowlist(rules)
	if err != nil {
		return err
	}
	e.egress.mu.Lock()
	defer e.egress.mu.Unlock()
	if _, ok := e.egress.proxies[id]; ok {
		return nil
	}
	l, err := egress.ListenInNetns(fmt.Sprintf("/proc/%d/ns/net", pid), egressListen)
	if err != nil {
		return fmt.Errorf("%w: egress listener: %v", workspace.ErrUnavailable, err)
	}
	p := egress.New(egress.Config{
		Allow: al, Workspace: string(id), Limits: e.egress.sharedLimits(),
		Logger: slog.Default().With("component", "egress"),
	})
	go func() { _ = p.Serve(l) }()
	if e.egress.proxies == nil {
		e.egress.proxies = map[workspace.ID]*egress.Proxy{}
	}
	e.egress.proxies[id] = p
	return nil
}

// stopEgress closes the workspace's listener and tunnels. Safe to call when there is none.
func (e *Executor) stopEgress(id workspace.ID) {
	e.egress.mu.Lock()
	p := e.egress.proxies[id]
	delete(e.egress.proxies, id)
	e.egress.mu.Unlock()
	if p != nil {
		_ = p.Close()
	}
}

// closeEgress stops every proxy (executor Close).
func (e *Executor) closeEgress() {
	e.egress.mu.Lock()
	ps := e.egress.proxies
	e.egress.proxies = nil
	e.egress.mu.Unlock()
	for _, p := range ps {
		_ = p.Close()
	}
}

// reattachEgress re-opens listeners for running egress workspaces after a runner restart. A
// workspace whose listener cannot be restored simply has no network (fail closed).
func (e *Executor) reattachEgress(ctx context.Context) {
	ctx = namespaces.WithNamespace(ctx, e.ns)
	cs, err := e.client.Containers(ctx, `labels."`+labelNetwork+`"==`+string(workspace.NetEgress))
	if err != nil {
		slog.Warn("egress re-attach: list containers", "err", err)
		return
	}
	for _, c := range cs {
		if err := e.reattachOne(ctx, c); err != nil {
			slog.Warn("egress re-attach failed; workspace has no network", "container", c.ID(), "err", err)
		}
	}
}

func (e *Executor) reattachOne(ctx context.Context, c containerd.Container) error {
	info, err := c.Info(ctx, containerd.WithoutRefreshedMetadata)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return err
	}
	id := workspace.ID(info.Labels[labelWorkspace])
	if !workspace.ValidID(id) {
		return nil
	}
	t, err := c.Task(ctx, nil)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return err
	}
	st, err := t.Status(ctx)
	if err != nil {
		return err
	}
	if st.Status != containerd.Running {
		return nil
	}
	var rules []string
	if s := info.Labels[labelEgress]; s != "" {
		rules = strings.Split(s, ",")
	}
	// Re-check against today's policy: a rule the runner no longer permits is a failure.
	if !e.cfg.EgressCIDRRules {
		for _, r := range rules {
			if strings.Contains(r, "/") || isIPRule(r) {
				return errors.New("stored rules include IP/CIDR entries but egress_cidr_rules is off")
			}
		}
	}
	return e.startEgress(id, t.Pid(), rules)
}

func isIPRule(r string) bool {
	h := r
	if strings.HasPrefix(r, "[") {
		if i := strings.IndexByte(r, ']'); i > 0 {
			h = r[1:i]
		}
	} else if host, _, err := net.SplitHostPort(r); err == nil {
		h = host
	}
	_, err := netip.ParseAddr(h)
	return err == nil
}
