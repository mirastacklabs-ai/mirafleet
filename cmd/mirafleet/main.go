// mirafleet: central | runner | central exec. See docs/ARCHITECTURE.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/mirastacklabs-ai/mirafleet/internal/actions"
	"github.com/mirastacklabs-ai/mirafleet/internal/audit"
	"github.com/mirastacklabs-ai/mirafleet/internal/central"
	"github.com/mirastacklabs-ai/mirafleet/internal/keys"
	"github.com/mirastacklabs-ai/mirafleet/internal/runner"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "central":
		if err = requireServerOS("central"); err != nil {
			break
		}
		if len(os.Args) > 2 && os.Args[2] == "exec" {
			err = centralExec(os.Args[3:])
		} else {
			err = centralRun(os.Args[2:])
		}
	case "runner":
		if err = requireServerOS("runner"); err != nil {
			break
		}
		err = runnerRun(os.Args[2:])
	case "version":
		fmt.Println(version)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "mirafleet:", err)
		os.Exit(1)
	}
}

// requireServerOS refuses to start central or runner outside linux (see platform_*.go).
func requireServerOS(what string) error {
	if !serverSupported {
		return fmt.Errorf("mirafleet %s is supported on linux only (current: %s)", what, runtime.GOOS)
	}
	return nil
}

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  mirafleet central [-data DIR] [-listen ADDR] [-init]
  mirafleet central exec [-data DIR] <runner> <action> [k=v...]
  mirafleet runner  [-data DIR] [-central ADDR] [-name NAME] [-actions FILE] [-labels k=v,...] [-init]
  mirafleet version
`)
	os.Exit(2)
}

func centralRun(args []string) error {
	fs := flag.NewFlagSet("central", flag.ExitOnError)
	data := fs.String("data", "/var/lib/mirafleet/central", "data directory")
	listen := fs.String("listen", "0.0.0.0:2222", "SSH listen address")
	init := fs.Bool("init", false, "generate the host key and an empty authorized_runners file, then exit")
	fs.Parse(args)

	hostKey := filepath.Join(*data, "host_ed25519")
	authFile := filepath.Join(*data, "authorized_runners")
	if *init {
		pub, err := keys.Generate(*data, "host_ed25519", "")
		if err != nil {
			return err
		}
		// Exclusive create: never clobber an existing authorized_runners.
		if err := keys.WriteNew(authFile, nil, 0o600); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		fmt.Printf("host key written to %s\nadd runner keys (authorized_keys format, comment = runner name) to %s\n", hostKey, authFile)
		fmt.Printf("runner known_hosts line:\n%s %s\n", *listen, pub)
		return nil
	}
	if err := keys.EnsureDir(*data); err != nil { // 0700, tightened if it already existed
		return err
	}
	signer, err := keys.LoadSigner(hostKey)
	if err != nil {
		return fmt.Errorf("load host key (run with -init first): %w", err)
	}
	al, err := audit.Open(filepath.Join(*data, "audit.jsonl"))
	if err != nil {
		return err
	}
	defer al.Close()
	srv := central.New(central.Config{Listen: *listen, HostKey: signer, AuthorizedRunnersFile: authFile, Audit: al, Logf: logf})
	addr, err := srv.Start()
	if err != nil {
		return err
	}
	defer srv.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := srv.ServeAdmin(ctx, adminSock(*data)); err != nil {
		return err
	}
	logf("central listening on %s, admin socket %s", addr, adminSock(*data))
	<-ctx.Done()
	return nil
}

func adminSock(data string) string { return filepath.Join(data, "admin.sock") }

// centralExec talks to a running central over its local admin unix socket (see central/admin.go).
func centralExec(args []string) error {
	fs := flag.NewFlagSet("central exec", flag.ExitOnError)
	data := fs.String("data", "/var/lib/mirafleet/central", "central data directory")
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
	res, err := central.AdminExec(adminSock(*data), fs.Arg(0), transport.ExecRequest{Action: fs.Arg(1), Params: params})
	if res != nil {
		fmt.Fprint(os.Stdout, res.Stdout)
		fmt.Fprint(os.Stderr, res.Stderr)
	}
	if err != nil {
		return err
	}
	switch {
	case res.Refused:
		return fmt.Errorf("%s", res.Message)
	case res.TimedOut:
		return fmt.Errorf("action timed out")
	case res.ExitCode != 0:
		os.Exit(res.ExitCode)
	}
	return nil
}

func runnerRun(args []string) error {
	fs := flag.NewFlagSet("runner", flag.ExitOnError)
	data := fs.String("data", "/var/lib/mirafleet/runner", "data directory")
	centralAddr := fs.String("central", "127.0.0.1:2222", "central address host:port")
	hn, _ := os.Hostname()
	name := fs.String("name", hn, "runner name (key comment used by -init)")
	actFile := fs.String("actions", "/etc/mirafleet/actions.yaml", "actions.yaml path")
	labels := fs.String("labels", "", "labels, k=v,k=v")
	init := fs.Bool("init", false, "generate the runner key, print its authorized_runners line, then exit")
	fs.Parse(args)

	keyPath := filepath.Join(*data, "runner_ed25519")
	if *init {
		pub, err := keys.Generate(*data, "runner_ed25519", *name)
		if err != nil {
			return err
		}
		fmt.Printf("key written to %s\nadd this line to central's authorized_runners:\n%s\n", keyPath, pub)
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
		Actions: acts, Labels: lm, Version: version, Audit: al, Logf: logf,
	}).Run(ctx)
	return nil
}

func logf(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }
