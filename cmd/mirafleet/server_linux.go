package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/actions"
	"github.com/mirastacklabs-ai/mirafleet/internal/audit"
	"github.com/mirastacklabs-ai/mirafleet/internal/keys"
	"github.com/mirastacklabs-ai/mirafleet/internal/runner"
)

// heartbeatEvery is the heartbeat interval runners use; a runner silent for 3 intervals is
// offline.
const heartbeatEvery = 15 * time.Second

const (
	defaultRunnerData  = "/var/lib/mirafleet/runner"
	defaultActionsFile = "/etc/mirafleet/actions.yaml"
)

func runnerMain(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "join":
			return runnerJoin(args[1:], os.Stdin, os.Stdout)
		case "run":
			return runnerRun(args[1:])
		case "tls-init":
			return runnerTLSInit(args[1:], os.Stdout)
		case "tls-front":
			return runnerTLSFront(args[1:])
		case "workspace":
			return runnerWorkspace(args[1:], os.Stdout, time.Now())
		case "host":
			return runnerHost(args[1:], os.Stdout)
		}
	}
	return errors.New("usage: mirafleet runner join ADDR -name NAME [-data DIR] -token -  |  run [-data DIR] [-actions FILE | -workspace FILE]  |  tls-init  |  tls-front  |  workspace list|prune  |  host prune [-older-than D] [-yes]")
}

func runnerJoin(args []string, stdin io.Reader, stdout io.Writer) error {
	addr, args := splitAddr(args)
	fs := flag.NewFlagSet("runner join", flag.ContinueOnError)
	data := fs.String("data", defaultRunnerData, "data directory")
	name := fs.String("name", "", "runner name")
	tok := fs.String("token", "", "join token; must be - (read from stdin)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if addr == "" {
		addr = fs.Arg(0)
	}
	if addr == "" || *name == "" {
		return errors.New("usage: mirafleet runner join ADDR -name NAME [-data DIR] -token -")
	}
	if *tok != "-" {
		return errors.New("-token must be - (the token is read from stdin, never from argv or env)")
	}
	syscall.Umask(0o077)
	dataGiven := false
	var err error
	if *data, dataGiven, err = runnerDataFlag(fs, *data); err != nil { // XDG default for an ordinary user
		return err
	}
	// Refuse before consuming stdin or touching the network.
	if _, err := os.Stat(runner.ConfigPath(*data)); err == nil {
		return runner.ErrAlreadyJoined
	}
	token, err := readSecret(stdin, "join token")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), mcpCallTimeout)
	defer cancel()
	res, err := runner.Join(ctx, *data, addr, *name, token)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "joined fleet %q as runner %q: status %s\n", res.Fleet, res.Runner, res.Status)
	if res.Status != "active" {
		fmt.Fprintln(stdout, "an admin must approve this runner on central before it can run actions")
	}
	if !dataGiven && os.Geteuid() != 0 {
		fmt.Fprintf(stdout, "next: put workspace.yaml in %s, then start it with `systemctl --user enable --now mirafleet-runner` "+
			"(unit: packaging/systemd/user/mirafleet-runner.service; once, as an admin: `loginctl enable-linger $USER`)\n", *data)
		return nil
	}
	fmt.Fprintf(stdout, "next: start it with `mirafleet runner run -data %s` (under systemd; the unit ships with the M6 packages)\n", *data)
	return nil
}

func runnerRun(args []string) error {
	fs := flag.NewFlagSet("runner run", flag.ContinueOnError)
	data := fs.String("data", defaultRunnerData, "data directory")
	actFile := fs.String("actions", defaultActionsFile, "actions.yaml path (actions mode, the default)")
	wsFile := fs.String("workspace", "", "workspace.yaml path (workspace mode; exclusive with -actions)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	if given["actions"] && given["workspace"] {
		return errors.New("runner run: -actions and -workspace are mutually exclusive (one runner, one mode)")
	}
	if given["workspace"] && *wsFile == "" {
		return errors.New("runner run: -workspace needs a file")
	}
	var err error
	if *data, _, err = runnerDataFlag(fs, *data); err != nil { // XDG default for an ordinary user
		return err
	}
	cfg, err := runner.LoadConfig(*data)
	if err != nil {
		return err
	}
	signer, err := keys.LoadSigner(runner.KeyPath(*data))
	if err != nil {
		return fmt.Errorf("load runner key: %w", err)
	}
	rcfg := runner.Config{
		Central: cfg.Central, Name: cfg.Name, Signer: signer, KnownHosts: runner.KnownHostsPath(*data),
		Version: version, Logf: logf, HeartbeatEvery: heartbeatEvery,
	}
	if given["workspace"] {
		wc, ex, err := loadWorkspace(*wsFile, cfg)
		if err != nil {
			return err
		}
		defer ex.Close()
		rcfg.Workspace, rcfg.WorkspaceConfig = ex, wc
		rcfg.EventSpool = filepath.Join(*data, "events.spool") // usage events survive a restart or crash
		if rcfg.CorporateCA, err = loadCorporateCA(wc); err != nil {
			return err
		}
		logf("runner %q (fleet %s) -> %s, workspace mode: %s executor, %d slot(s), owner %s",
			cfg.Name, cfg.Fleet, cfg.Central, wc.Executor, wc.Concurrent, ownerName(cfg))
	} else {
		acts, err := actions.Load(*actFile)
		if err != nil {
			return err
		}
		rcfg.Actions = acts
		logf("runner %q (fleet %s) -> %s, actions: %s", cfg.Name, cfg.Fleet, cfg.Central, strings.Join(acts.Names(), ","))
		// Central does not report the fleet mode back (the heartbeat ack has no such field), so
		// this can only be a hint (docs/M3.md D-11).
		// Only when neither flag was given: an explicit -actions means the operator chose this mode.
		if !given["actions"] {
			logf("note: started without -workspace, so this runner serves actions only; if fleet %q is a workspace-mode fleet it will receive no workspace work: restart with `runner run -workspace FILE`", cfg.Fleet)
		}
	}
	if err := rcfg.Validate(); err != nil {
		return err
	}
	al, err := audit.Open(filepath.Join(*data, "audit.jsonl"))
	if err != nil {
		return err
	}
	defer al.Close()
	rcfg.Audit = al
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runner.New(rcfg).Run(ctx); err != nil && ctx.Err() == nil {
		return err // a configuration error; cancellation is the normal way out
	}
	return nil
}
