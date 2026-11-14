package service

import (
	"context"
	"crypto/rand"
	"math/big"
	"sort"
	"strconv"
	"strings"

	"github.com/mirastacklabs-ai/mirafleet/internal/central/registry"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
)

// maxPlacementTries bounds how many candidate runners one open attempts (M3.md 1.4).
const maxPlacementTries = 3

// candidate is a runner that may take a new workspace.
type candidate struct {
	info   registry.SessionInfo
	free   int
	runner store.Runner
}

// placementReq is what a workspace needs from a runner.
type placementReq struct {
	fleet   string
	cfg     store.WorkspaceConfig
	guarded bool
	labels  map[string]string
	image   string
}

// candidates returns the runners that can take the request, best first: most free slots, with a
// crypto/rand tie-break. Runners with no free slot are dropped. Labels are matched against the
// runner's trusted (token-derived) labels in the store, never the heartbeat's.
func (s *Service) candidates(ctx context.Context, req placementReq) ([]candidate, error) {
	runners, err := s.st.ListRunners(ctx, req.fleet, false)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]store.Runner, len(runners))
	for _, r := range runners {
		if r.Status == store.StatusActive {
			byID[r.ID] = r
		}
	}
	var cs []candidate
	var ids []int64
	for _, si := range s.live.Snapshot() {
		r, ok := byID[si.ID]
		if !ok || si.Fleet != req.fleet || !si.Online || si.Status != registry.StatusActive {
			continue
		}
		if si.Slots <= 0 || si.Executor != string(req.cfg.Executor) {
			continue
		}
		if req.guarded && si.SecureRuntime == "" {
			continue
		}
		if !labelsSubset(req.labels, r.Labels) || !workspace.MatchImage(si.Images, req.image) {
			continue
		}
		cs = append(cs, candidate{info: si, runner: r})
		ids = append(ids, si.ID)
	}
	if len(cs) == 0 {
		return nil, nil
	}
	live, err := s.st.CountLiveWorkspaces(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := cs[:0]
	for _, c := range cs {
		c.free = c.info.Slots - live[c.info.ID]
		if c.free > 0 {
			out = append(out, c)
		}
	}
	shuffle(out)
	sort.SliceStable(out, func(i, j int) bool { return out[i].free > out[j].free })
	return out, nil
}

func labelsSubset(want, have map[string]string) bool {
	for k, v := range want {
		if hv, ok := have[k]; !ok || hv != v {
			return false
		}
	}
	return true
}

// shuffle is a Fisher-Yates shuffle driven by crypto/rand (the tie-break of 1.4).
func shuffle(cs []candidate) {
	for i := len(cs) - 1; i > 0; i-- {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			return // keep the current order; placement stays correct
		}
		j := int(n.Int64())
		cs[i], cs[j] = cs[j], cs[i]
	}
}

// runnerOnline reports whether runner id has a live, non-stale session.
func (s *Service) runnerOnline(id int64) bool {
	for _, si := range s.live.Snapshot() {
		if si.ID == id {
			return si.Online
		}
	}
	return false
}

// buildSpec turns the fleet config into the runner-side spec.
func buildSpec(cfg store.WorkspaceConfig, image string, labels map[string]string, guarded bool) transport.WorkspaceSpec {
	const mib = 1 << 20
	// Backstop for rows written before the store bounded max_ttl: the runner refuses more.
	maxAge := min(cfg.MaxTTL.Milliseconds(), transport.MaxWorkspaceAgeMs)
	sp := transport.WorkspaceSpec{
		Image:                image,
		Pull:                 string(cfg.Pull),
		CPUMilli:             int64(cfg.CPUMilli),
		MemoryBytes:          int64(cfg.MemoryMB) * mib,
		Pids:                 int64(cfg.Pids),
		TmpfsBytes:           int64(cfg.TmpfsMB) * mib,
		DiskBytes:            int64(cfg.DiskMB) * mib,
		NetworkMode:          string(cfg.Network),
		RequireSecureRuntime: guarded,
		MaxAgeMs:             maxAge,
	}
	if len(labels) > 0 {
		sp.Labels = labels
	}
	if cfg.Network == store.NetworkEgress {
		for _, e := range cfg.Egress {
			sp.Allow = append(sp.Allow, egressRule(e))
		}
	}
	return sp
}

// egressRule splits a validated allowlist entry ("host[:port]", "*.d[:port]", "cidr:port",
// "[v6/len]:port") into host and port; the port defaults to 443. Wildcards and CIDRs travel in
// the host field as written.
func egressRule(e string) transport.EgressRule {
	host, port := e, "443"
	if strings.HasPrefix(e, "[") {
		if end := strings.Index(e, "]"); end > 0 {
			host = e[1:end]
			if rest := strings.TrimPrefix(e[end+1:], ":"); rest != "" {
				port = rest
			}
		}
	} else if i := strings.LastIndexByte(e, ':'); i >= 0 {
		host, port = e[:i], e[i+1:]
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		p = 443
	}
	return transport.EgressRule{Host: host, Port: p}
}
