package main

// Workspace-mode wiring of the runner commands (docs/M3.md §1.12): executor construction for
// `runner run -workspace`, the node PKI (`tls-init`), the TLS front (`tls-front`) and the local
// `workspace list|prune` admin commands. This file is the only place (with ctrd's own package)
// that links containerd, and only on linux.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/keys"
	"github.com/mirastacklabs-ai/mirafleet/internal/runner"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/ctrd"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/osb"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/wsconfig"
)

const defaultWorkspaceFile = "/etc/mirafleet/workspace.yaml"

// newExecutor builds the executor named by cfg. It is a seam for tests (a fake executor); the
// default talks to a real containerd or OpenSandbox. owner is the ownership name: it tags every
// workspace the executor creates and bounds what List returns, so it must be unique across
// fleets (docs/M3.md C-16): "<fleet>/<runner>".
var newExecutor = func(cfg *wsconfig.Config, owner string) (workspace.Executor, error) {
	switch cfg.Executor {
	case wsconfig.ExecContainerd:
		return ctrd.New(cfg, ctrd.Options{Runner: owner})
	case wsconfig.ExecOpenSandbox:
		return osb.NewExecutor(cfg, owner)
	}
	return nil, fmt.Errorf("unknown executor %q", cfg.Executor)
}

// ownerName is the executor ownership name of a joined runner.
func ownerName(c *runner.FileConfig) string { return c.Fleet + "/" + c.Name }

// loadWorkspace loads workspace.yaml (with the on-disk checks) and builds the executor.
func loadWorkspace(path string, rc *runner.FileConfig) (*wsconfig.Config, workspace.Executor, error) {
	wc, err := wsconfig.Load(path)
	if err != nil {
		return nil, nil, err
	}
	ex, err := newExecutor(wc, ownerName(rc))
	if err != nil {
		return nil, nil, fmt.Errorf("start %s executor: %w", wc.Executor, err)
	}
	return wc, ex, nil
}

// ---- runner tls-init ----

func runnerTLSInit(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("runner tls-init", flag.ContinueOnError)
	dir := fs.String("dir", "", "directory for the node CA, front and client certificates (created 0700; must be empty)")
	days := fs.Int("days", 365, "certificate validity in days (1..3650)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" || fs.NArg() > 0 {
		return errors.New("usage: mirafleet runner tls-init -dir DIR [-days N]")
	}
	if ents, err := os.ReadDir(*dir); err == nil {
		if len(ents) > 0 {
			return fmt.Errorf("%s is not empty: tls-init never overwrites; use a fresh directory", *dir)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	syscall.Umask(0o077)
	if err := keys.EnsureDir(*dir); err != nil {
		return err
	}
	p, err := osb.InitPKI(*dir, *days, time.Now())
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "node CA certificate: %s\nfront certificate:   %s (key %s)\nrunner client cert:  %s (key %s)\n"+
		"valid until:         %s\nThe CA private key was discarded; rotate by running tls-init into a new directory.\n\n"+
		"workspace.yaml:\n"+
		"opensandbox:\n  server: https://127.0.0.1:8443\n  ca_file: %s\n  pin_sha256: %s\n  client_cert: %s\n  client_key: %s\n\n"+
		"start the front next to OpenSandbox: mirafleet runner tls-front -dir %s\n",
		p.CAFile, p.FrontCert, p.FrontKey, p.ClientCert, p.ClientKey, p.NotAfter.UTC().Format(time.RFC3339),
		p.CAFile, p.FrontPinSHA, p.ClientCert, p.ClientKey, p.Dir)
	return nil
}

// ---- runner tls-front ----

func runnerTLSFront(args []string) error {
	f, err := newTLSFront(args)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return f.Run(ctx)
}

// newTLSFront parses the tls-front flags and builds the front (every rule is checked here).
func newTLSFront(args []string) (*osb.Front, error) {
	fs := flag.NewFlagSet("runner tls-front", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:8443", "TLS listen address (must be loopback)")
	upstream := fs.String("upstream", "127.0.0.1:8080", "OpenSandbox server address (loopback, plain http)")
	dir := fs.String("dir", "", "directory written by tls-init (uses ca.pem, front.pem, front-key.pem)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if *dir == "" || fs.NArg() > 0 {
		return nil, errors.New("usage: mirafleet runner tls-front [-listen 127.0.0.1:8443] [-upstream 127.0.0.1:8080] -dir DIR")
	}
	return osb.NewFront(osb.FrontConfig{Listen: *listen, Upstream: *upstream,
		CertFile: filepath.Join(*dir, osb.PKIFrontCertFile), KeyFile: filepath.Join(*dir, osb.PKIFrontKeyFile),
		CAFile: filepath.Join(*dir, osb.PKICAFile), Logf: logf})
}

// ---- runner workspace list|prune ----

// runnerWorkspace is the local admin command on the runner host: it talks to the executor
// directly (no central), so it works after a runner revoke, when central can no longer reach
// the host. Stop `runner run` first when pruning with -all.
func runnerWorkspace(args []string, out io.Writer, now time.Time) error {
	if len(args) == 0 || (args[0] != "list" && args[0] != "prune") {
		return errors.New("usage: mirafleet runner workspace list|prune [-data DIR] [-workspace FILE] [-all] [-yes]")
	}
	sub := args[0]
	fs := flag.NewFlagSet("runner workspace "+sub, flag.ContinueOnError)
	data := fs.String("data", defaultRunnerData, "runner data directory")
	file := fs.String("workspace", defaultWorkspaceFile, "workspace.yaml path")
	all, yes := new(bool), new(bool)
	if sub == "prune" {
		all = fs.Bool("all", false, "destroy every workspace of this runner, not only those older than max_age")
		yes = fs.Bool("yes", false, "confirm: without it prune only lists what it would destroy")
	}
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("runner workspace %s takes no arguments", sub)
	}
	rc, err := runner.LoadConfig(*data)
	if err != nil {
		return err
	}
	wc, ex, err := loadWorkspace(*file, rc)
	if err != nil {
		return err
	}
	defer ex.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	infos, err := ex.List(ctx)
	if err != nil {
		return fmt.Errorf("list workspaces: %w", err)
	}
	if sub == "list" {
		return printWorkspaces(out, infos, now)
	}

	victims := infos
	if !*all {
		if wc.MaxAge <= 0 {
			return errors.New("max_age is not set in workspace.yaml: use -all to prune everything")
		}
		victims = nil
		for _, i := range infos {
			if now.Sub(i.Created) > wc.MaxAge {
				victims = append(victims, i)
			}
		}
	}
	if len(victims) == 0 {
		fmt.Fprintln(out, "nothing to prune")
		return nil
	}
	if err := printWorkspaces(out, victims, now); err != nil {
		return err
	}
	if !*yes {
		fmt.Fprintf(out, "%d workspace(s) would be destroyed; re-run with -yes to do it\n", len(victims))
		return nil
	}
	var failed int
	for _, i := range victims {
		dctx, dcancel := context.WithTimeout(ctx, 2*time.Minute)
		err := ex.Destroy(dctx, i.ID)
		dcancel()
		if err != nil {
			failed++
			fmt.Fprintf(out, "destroy %s: %v\n", i.ID, err)
			continue
		}
		fmt.Fprintf(out, "destroyed %s\n", i.ID)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d workspace(s) could not be destroyed", failed, len(victims))
	}
	return nil
}

func printWorkspaces(out io.Writer, infos []workspace.Info, now time.Time) error {
	tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATE\tAGE\tDISK\tIMAGE")
	for _, i := range infos {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", i.ID, i.State, now.Sub(i.Created).Round(time.Second), i.DiskBytes, i.Image)
	}
	return tw.Flush()
}
