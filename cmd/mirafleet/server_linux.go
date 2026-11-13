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

const defaultRunnerData = "/var/lib/mirafleet/runner"

func runnerMain(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "join":
			return runnerJoin(args[1:], os.Stdin, os.Stdout)
		case "run":
			return runnerRun(args[1:])
		}
	}
	return errors.New("usage: mirafleet runner join ADDR -name NAME [-data DIR] -token -  |  mirafleet runner run [-data DIR] [-actions FILE]")
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
	fmt.Fprintf(stdout, "next: start it with `mirafleet runner run -data %s` (under systemd; the unit ships with the M6 packages)\n", *data)
	return nil
}

func runnerRun(args []string) error {
	fs := flag.NewFlagSet("runner run", flag.ContinueOnError)
	data := fs.String("data", defaultRunnerData, "data directory")
	actFile := fs.String("actions", "/etc/mirafleet/actions.yaml", "actions.yaml path")
	if err := fs.Parse(args); err != nil {
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
	acts, err := actions.Load(*actFile)
	if err != nil {
		return err
	}
	al, err := audit.Open(filepath.Join(*data, "audit.jsonl"))
	if err != nil {
		return err
	}
	defer al.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logf("runner %q (fleet %s) -> %s, actions: %s", cfg.Name, cfg.Fleet, cfg.Central, strings.Join(acts.Names(), ","))
	runner.New(runner.Config{
		Central: cfg.Central, Name: cfg.Name, Signer: signer, KnownHosts: runner.KnownHostsPath(*data),
		Actions: acts, Version: version, Audit: al, Logf: logf, HeartbeatEvery: heartbeatEvery,
	}).Run(ctx)
	return nil
}
