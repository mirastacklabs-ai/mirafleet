package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/apiclient"
	"github.com/mirastacklabs-ai/mirafleet/internal/keys"
	"github.com/mirastacklabs-ai/mirafleet/internal/opconf"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// Cross-platform (linux, darwin, windows): no central or runner dependencies.

func mcpLogin(args []string, stdin io.Reader, stdout io.Writer) error {
	addr, args := splitAddr(args)
	fs := flag.NewFlagSet("mcp login", flag.ContinueOnError)
	name := fs.String("name", "", "operator and context name")
	code := fs.String("code", "", "registration code; must be - (read from stdin)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if addr == "" {
		addr = fs.Arg(0)
	}
	if addr == "" || *name == "" {
		return errors.New("usage: mirafleet mcp login ADDR -name NAME -code -")
	}
	if *code != "-" {
		return errors.New("-code must be - (the code is read from stdin, never from argv)")
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return fmt.Errorf("central address %q: %w", addr, err)
	}
	if !keys.RunnerNameRE.MatchString(*name) {
		return fmt.Errorf("invalid name %q (lowercase letters, digits, . _ -; max 63)", *name)
	}
	secret, err := readSecret(stdin, "registration code")
	if err != nil {
		return err
	}
	dir, err := opconf.EnsureDir()
	if err != nil {
		return err
	}
	if _, err := os.Stat(opconf.KeyPath(dir)); errors.Is(err, os.ErrNotExist) {
		if _, err := keys.Generate(dir, opconf.KeyName, *name); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	signer, err := loadOperatorKey(dir)
	if err != nil {
		return fmt.Errorf("load operator key: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), mcpCallTimeout)
	defer cancel()
	res, hk, err := apiclient.Register(ctx, addr, secret, *name, signer)
	if err != nil {
		return err
	}
	if err := recordLogin(dir, *name, addr, hk); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "registered operator %q (%s); context %q -> %s is now current\nconfig: %s\n",
		res.Operator, res.Status, *name, addr, dir)
	return loginSkew(ctx, dir, addr, signer)
}

// loginSkew says hello to the central just registered with and returns its version-skew refusal
// as the error, which the CLI prints and turns into a non-zero exit. Any other
// hello failure is ignored: registration succeeded, and `mcp` reports connection problems itself.
func loginSkew(ctx context.Context, dir, addr string, signer ssh.Signer) error {
	cl, err := apiclient.Dial(ctx, addr, transport.UserOperator, signer, keys.PinnedHostKeys(opconf.KnownHostsPath(dir)))
	if err != nil {
		return nil
	}
	defer cl.Close()
	_, err = cl.Hello(ctx, "mcp")
	var ae *api.Error
	if errors.As(err, &ae) && ae.Code == api.CodeUnsupportedVersion {
		return errors.New(ae.Message)
	}
	return nil
}

// recordLogin pins the central host key and makes name the current context for addr. One lock
// covers both the known_hosts pin and the contexts.yaml change, so two concurrent logins cannot
// drop each other's pin or context. UpdateContexts' lock is not re-entrant: nothing in the
// callback may call another opconf function that takes it.
func recordLogin(dir, name, addr string, hk ssh.PublicKey) error {
	pinned := false
	err := opconf.UpdateContexts(dir, func(cs *opconf.Contexts) error {
		if err := pinHost(dir, addr, hk); err != nil {
			return fmt.Errorf("registered, but writing known_hosts failed: %w", err)
		}
		pinned = true
		found := false
		for i := range cs.Contexts {
			if cs.Contexts[i].Name == name {
				cs.Contexts[i].Addr, found = addr, true
			}
		}
		if !found {
			cs.Contexts = append(cs.Contexts, opconf.Context{Name: name, Addr: addr})
		}
		cs.Current = name
		return nil
	})
	if err != nil && pinned {
		return fmt.Errorf("registered, but saving the context failed: %w", err)
	}
	return err
}

// pinHost records the verified central key for addr in the operator known_hosts, replacing any
// older line for the same address and keeping the rest.
func pinHost(dir, addr string, hk ssh.PublicKey) error {
	path := opconf.KnownHostsPath(dir)
	var out []string
	if b, err := os.ReadFile(path); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if l = strings.TrimSpace(l); l == "" {
				continue
			}
			if h, _, _ := strings.Cut(l, " "); h == addr {
				continue
			}
			out = append(out, l)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	out = append(out, apiclient.KnownHostsLine(addr, hk))
	return writeAtomic0600(dir, filepath.Base(path), []byte(strings.Join(out, "\n")+"\n"))
}
