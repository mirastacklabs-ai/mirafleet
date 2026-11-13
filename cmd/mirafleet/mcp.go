package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/apiclient"
	"github.com/mirastacklabs-ai/mirafleet/internal/keys"
	"github.com/mirastacklabs-ai/mirafleet/internal/opconf"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// Cross-platform (linux, darwin, windows): no central or runner dependencies.

const (
	maxSecretBytes = 4096
	mcpCallTimeout = 2 * time.Minute
	// mcpRunMargin is added to an action.run timeout for dial, queueing and reply latency.
	mcpRunMargin = 30 * time.Second
)

// readSecret reads a token or code from r (stdin), trimmed. It is never echoed or logged.
func readSecret(r io.Reader, what string) (string, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxSecretBytes+1))
	if err != nil {
		return "", fmt.Errorf("read %s from stdin: %w", what, err)
	}
	if len(b) > maxSecretBytes {
		return "", fmt.Errorf("%s on stdin is too long", what)
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", fmt.Errorf("no %s on stdin", what)
	}
	return s, nil
}

// splitAddr takes the leading positional ADDR (if any) off args so flags may follow it.
func splitAddr(args []string) (addr string, rest []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

func mcpMain(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 {
		fmt.Fprintln(stdout, "mirafleet mcp: the MCP stdio server arrives in M5.\n"+
			"available now: mirafleet mcp login ADDR -name NAME -code -   |   mirafleet mcp api METHOD [JSON]")
		return nil
	}
	switch args[0] {
	case "login":
		return mcpLogin(args[1:], stdin, stdout)
	case "api":
		return mcpAPI(args[1:], stdout)
	}
	return fmt.Errorf("unknown mcp command %q (want login or api)", args[0])
}

// errWindowsKey: the key loader fails closed on Windows until owner-only DACLs land (M5).
var errWindowsKey = errors.New("operator key storage is not supported on windows yet: private key permissions cannot be verified (owner-only ACLs arrive in M5); use linux or macOS")

func loadOperatorKey(dir string) (ssh.Signer, error) {
	s, err := keys.LoadSigner(opconf.KeyPath(dir))
	if err != nil && runtime.GOOS == "windows" {
		return nil, errWindowsKey
	}
	return s, err
}

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
	if runtime.GOOS == "windows" {
		return errWindowsKey
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
	if err := pinHost(dir, addr, hk); err != nil {
		return fmt.Errorf("registered, but writing known_hosts failed: %w", err)
	}
	cs, err := opconf.LoadContexts(dir)
	if err != nil {
		return err
	}
	found := false
	for i := range cs.Contexts {
		if cs.Contexts[i].Name == *name {
			cs.Contexts[i].Addr, found = addr, true
		}
	}
	if !found {
		cs.Contexts = append(cs.Contexts, opconf.Context{Name: *name, Addr: addr})
	}
	cs.Current = *name
	if err := opconf.SaveContexts(dir, cs); err != nil {
		return fmt.Errorf("registered, but saving the context failed: %w", err)
	}
	fmt.Fprintf(stdout, "registered operator %q (%s); context %q -> %s is now current\nconfig: %s\n",
		res.Operator, res.Status, *name, addr, dir)
	return nil
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

func writeAtomic0600(dir, name string, data []byte) error {
	f, err := os.CreateTemp(dir, ".tmp-*") // 0600
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, name))
}

// mcpCallLimit is the client-side limit for one `mcp api` call: action.run gets its requested
// timeout_s (server default when omitted, server max at most) plus mcpRunMargin; every other
// method keeps mcpCallTimeout.
func mcpCallLimit(method string, params json.RawMessage) time.Duration {
	if method != api.MethodActionRun {
		return mcpCallTimeout
	}
	var p struct {
		TimeoutS int `json:"timeout_s"`
	}
	_ = json.Unmarshal(params, &p) // unparsable: the server rejects it; use the default
	return api.ActionTimeout(p.TimeoutS) + mcpRunMargin
}

func mcpAPI(args []string, stdout io.Writer) error {
	if len(args) < 1 || len(args) > 2 {
		return errors.New("usage: mirafleet mcp api METHOD [JSON]")
	}
	var params json.RawMessage
	if len(args) == 2 {
		if !json.Valid([]byte(args[1])) {
			return errors.New("params are not valid JSON")
		}
		params = json.RawMessage(args[1])
	}
	dir, err := opconf.Dir()
	if err != nil {
		return err
	}
	cs, err := opconf.LoadContexts(dir)
	if err != nil {
		return err
	}
	var addr string
	for _, c := range cs.Contexts {
		if c.Name == cs.Current {
			addr = c.Addr
		}
	}
	if addr == "" {
		return errors.New("no current context: run `mirafleet mcp login ADDR -name NAME -code -` first")
	}
	signer, err := loadOperatorKey(dir)
	if err != nil {
		return fmt.Errorf("load operator key: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), mcpCallLimit(args[0], params))
	defer cancel()
	cl, err := apiclient.Dial(ctx, addr, transport.UserOperator, signer, apiclient.KnownHosts(opconf.KnownHostsPath(dir)))
	if err != nil {
		return err
	}
	defer cl.Close()
	var p any
	if params != nil {
		p = params
	}
	var res json.RawMessage
	if err := cl.Call(ctx, args[0], p, &res); err != nil {
		var ae *api.Error
		if errors.As(err, &ae) {
			return fmt.Errorf("%s: %s", ae.Code, ae.Message)
		}
		return err
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, res, "", "  "); err != nil {
		buf.Reset()
		buf.Write(res)
	}
	fmt.Fprintln(stdout, buf.String())
	return nil
}
