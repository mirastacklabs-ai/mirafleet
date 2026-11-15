package main

// The host executor of `runner run -workspace` (docs/M7.md section 3): tool calls run as the
// runner's own unprivileged user on the VM. Built from workspace.yaml `executor: host`.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/hostx"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/wsconfig"
)

// hostHarden is the start-up hardening (dumpable 0, no core files, refuse root and capabilities,
// own state dirs); a seam so tests can run as root in a container.
var hostHarden = hostx.Harden

// hostExecutor is the hostx executor plus the cleanup sweeper that runs until Close.
type hostExecutor struct {
	*hostx.Executor
	stop context.CancelFunc
	done chan struct{}
}

func (h *hostExecutor) Close() error {
	h.stop()
	<-h.done
	return h.Executor.Close()
}

// newHostExecutor builds the host executor from cfg: XDG state/data directories of the runner
// user, the sessions root (host.root), the clean-environment engine and a daily cleanup sweep.
func newHostExecutor(cfg *wsconfig.Config) (workspace.Executor, error) {
	paths, err := hostx.DefaultPaths()
	if err != nil {
		return nil, err
	}
	if err := paths.Ensure(); err != nil {
		return nil, err
	}
	if err := hostHarden(paths.State); err != nil {
		return nil, err
	}
	hc := cfg.Host
	if hc == nil {
		hc = &wsconfig.Host{}
	}
	root := paths.SessionsRoot()
	if hc.Root != "" {
		if root, err = expandHome(hc.Root); err != nil {
			return nil, err
		}
	}
	del := hc.DeleteAfter()
	if del == 0 {
		del = -1 // 0 = never
	}
	mgr, err := hostx.NewManager(hostx.SessionsConfig{Root: root, StateDir: paths.State, DeleteAfter: del})
	if err != nil {
		return nil, err
	}
	eng, err := hostx.New(hostx.Config{EnvAllow: cfg.EnvAllow, JobMax: hc.JobMax})
	if err != nil {
		return nil, err
	}
	ex, err := hostx.NewExecutor(hostx.ExecutorConfig{Manager: mgr, Engine: eng, StateDir: paths.State})
	if err != nil {
		return nil, err
	}
	ctx, stop := context.WithCancel(context.Background())
	h := &hostExecutor{Executor: ex, stop: stop, done: make(chan struct{})}
	go func() {
		defer close(h.done)
		mgr.RunSweeper(ctx, 0, func(r hostx.SweepReport) {
			logf("host sweep: removed %d, %d near expiry, %d error(s)", len(r.Removed), len(r.Warned), len(r.Errors))
		})
	}()
	return h, nil
}

// expandHome resolves a leading "~/" against the runner user's home directory.
func expandHome(p string) (string, error) {
	rest, ok := strings.CutPrefix(p, "~/")
	if !ok {
		return filepath.Clean(p), nil
	}
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return "", fmt.Errorf("host.root %q: no absolute home directory", p)
	}
	return filepath.Join(home, rest), nil
}

// loadCorporateCA reads host.corporate_ca_file (empty = none) for the credential broker.
func loadCorporateCA(wc *wsconfig.Config) ([]byte, error) {
	if wc == nil || wc.Host == nil || wc.Host.CorporateCAFile == "" {
		return nil, nil
	}
	pem, err := readCorporateCA(wc.Host.CorporateCAFile)
	if err != nil {
		return nil, fmt.Errorf("host.corporate_ca_file: %w", err)
	}
	return pem, nil
}
