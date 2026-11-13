package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/actions"
	"github.com/mirastacklabs-ai/mirafleet/internal/audit"
	"github.com/mirastacklabs-ai/mirafleet/internal/central"
	"github.com/mirastacklabs-ai/mirafleet/internal/central/registry"
	"github.com/mirastacklabs-ai/mirafleet/internal/keys"
	"github.com/mirastacklabs-ai/mirafleet/internal/policy"
	"github.com/mirastacklabs-ai/mirafleet/internal/runner"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/store/sqlite"
)

// heartbeatEvery is the heartbeat interval runners use; a runner silent for 3 intervals is
// offline.
const heartbeatEvery = 15 * time.Second

func adminSock(data string) string { return filepath.Join(data, "admin.sock") }

func centralRun(args []string) error {
	fs := flag.NewFlagSet("central", flag.ExitOnError)
	data := fs.String("data", "/var/lib/mirafleet/central", "data directory")
	listen := fs.String("listen", "0.0.0.0:2222", "SSH listen address")
	init := fs.Bool("init", false, "generate the host key and create the database, then exit")
	fs.Parse(args)

	syscall.Umask(0o077)                          // the database, -wal and -shm are created 0600
	if err := keys.EnsureDir(*data); err != nil { // 0700, tightened if it already exists
		return err
	}
	hostKey := filepath.Join(*data, "host_ed25519")
	dbPath := filepath.Join(*data, "mirafleet.db")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *init {
		// TODO(S10a): `central init` also creates the operator registration secret.
		pub, err := keys.Generate(*data, "host_ed25519", "")
		if err != nil {
			return err
		}
		db, err := sqlite.Open(ctx, dbPath)
		if err != nil {
			return err
		}
		if err := db.Close(); err != nil {
			return err
		}
		fmt.Printf("host key written to %s, database at %s\nrunner known_hosts line:\n%s %s\n", hostKey, dbPath, *listen, pub)
		return nil
	}

	signer, err := keys.LoadSigner(hostKey)
	if err != nil {
		return fmt.Errorf("load host key (run with -init first): %w", err)
	}
	db, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	reg, err := registry.New(registry.Options{StaleAfter: 3 * heartbeatEvery, Logf: logf})
	if err != nil {
		return err
	}
	ops := central.NewOperatorConns()
	svc, err := service.New(service.Config{Store: db, Policy: policy.TablePolicy{}, Live: reg,
		HostKey: signer.PublicKey(), Now: time.Now, OperatorConns: ops, Logf: logf})
	if err != nil {
		return err
	}
	srv, err := central.New(central.Config{Listen: *listen, HostKey: signer, Store: db, Service: svc,
		Registry: reg, Operators: ops, Logf: logf})
	if err != nil {
		return err
	}
	addr, err := srv.Start()
	if err != nil {
		return err
	}
	defer srv.Close()
	if err := srv.ServeAdmin(ctx, adminSock(*data)); err != nil {
		return err
	}
	logf("central listening on %s, admin socket %s", addr, adminSock(*data))
	<-ctx.Done()
	return nil
}

// centralExec talks to a running central over its local admin unix socket (see central/admin.go).
func centralExec(args []string) error {
	fs := flag.NewFlagSet("central exec", flag.ExitOnError)
	data := fs.String("data", "/var/lib/mirafleet/central", "central data directory")
	confirm := fs.String("confirm", "", "fleet name, required for guarded fleets")
	fs.Parse(args)
	if fs.NArg() < 2 {
		usage()
	}
	params := map[string]string{}
	for _, kv := range fs.Args()[2:] {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("parameter %q is not k=v", kv)
		}
		params[k] = v
	}
	res, err := central.AdminExec(adminSock(*data), central.AdminRequest{Runner: fs.Arg(0), Action: fs.Arg(1), Params: params, Confirm: *confirm})
	if err != nil {
		return err
	}
	fmt.Fprint(os.Stdout, res.Stdout)
	fmt.Fprint(os.Stderr, res.Stderr)
	if res.ExitCode != 0 {
		os.Exit(res.ExitCode)
	}
	return nil
}

func runnerRun(args []string) error {
	fs := flag.NewFlagSet("runner", flag.ExitOnError)
	data := fs.String("data", "/var/lib/mirafleet/runner", "data directory")
	centralAddr := fs.String("central", "127.0.0.1:2222", "central address host:port")
	hn, _ := os.Hostname()
	name := fs.String("name", hn, "runner name (log label and key comment used by -init)")
	actFile := fs.String("actions", "/etc/mirafleet/actions.yaml", "actions.yaml path")
	labels := fs.String("labels", "", "labels, k=v,k=v")
	init := fs.Bool("init", false, "generate the runner key and print its public line, then exit")
	fs.Parse(args)

	keyPath := filepath.Join(*data, "runner_ed25519")
	if *init {
		// TODO(S10b): replaced by `runner join`, which enrols with a join token.
		pub, err := keys.Generate(*data, "runner_ed25519", *name)
		if err != nil {
			return err
		}
		fmt.Printf("key written to %s\npublic key:\n%s\n", keyPath, pub)
		fmt.Printf("put central's host key line in %s (format: \"%s ssh-ed25519 AAAA...\")\n", filepath.Join(*data, "known_hosts"), *centralAddr)
		return nil
	}
	signer, err := keys.LoadSigner(keyPath)
	if err != nil {
		return fmt.Errorf("load runner key (run with -init first): %w", err)
	}
	acts, err := actions.Load(*actFile)
	if err != nil {
		return err
	}
	lm := map[string]string{}
	for _, kv := range strings.Split(*labels, ",") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			lm[k] = v
		}
	}
	if _, _, err := net.SplitHostPort(*centralAddr); err != nil {
		return err
	}
	al, err := audit.Open(filepath.Join(*data, "audit.jsonl"))
	if err != nil {
		return err
	}
	defer al.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logf("runner %q -> %s, actions: %s", *name, *centralAddr, strings.Join(acts.Names(), ","))
	runner.New(runner.Config{
		Central: *centralAddr, Name: *name, Signer: signer, KnownHosts: filepath.Join(*data, "known_hosts"),
		Actions: acts, Labels: lm, Version: version, Audit: al, Logf: logf, HeartbeatEvery: heartbeatEvery,
	}).Run(ctx)
	return nil
}
