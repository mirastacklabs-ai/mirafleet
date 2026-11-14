package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
		return mcpAPI(args[1:], stdin, stdout)
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
//
// Workspace methods (docs/M3.md §1.9, decision 5): workspace.exec gets its requested timeout_s
// plus the margin, capped at the server's 24 h ceiling; without timeout_s the fleet default
// applies, which this client cannot see, so the cap is the ceiling (the server enforces the real
// deadline). workspace.open gets the 5 min open timeout plus the margin; file calls 100 s.
func mcpCallLimit(method string, params json.RawMessage) time.Duration {
	switch method {
	case api.MethodActionRun:
		var p struct {
			TimeoutS int `json:"timeout_s"`
		}
		_ = json.Unmarshal(params, &p) // unparsable: the server rejects it; use the default
		return api.ActionTimeout(p.TimeoutS) + mcpRunMargin
	case api.MethodWorkspaceExec:
		var p struct {
			TimeoutS int `json:"timeout_s"`
		}
		_ = json.Unmarshal(params, &p)
		ceil := api.HardWorkspaceExecCeiling
		if p.TimeoutS > 0 && time.Duration(p.TimeoutS) < ceil/time.Second {
			ceil = time.Duration(p.TimeoutS) * time.Second
		}
		return ceil + mcpRunMargin
	case api.MethodWorkspaceOpen:
		return mcpWorkspaceOpenLimit
	case api.MethodWorkspaceReadFile, api.MethodWorkspaceWriteFile:
		return mcpWorkspaceFileLimit
	}
	return mcpCallTimeout
}

// Client-side limits for workspace calls (server timeouts plus mcpRunMargin).
const (
	mcpWorkspaceOpenLimit = 5*time.Minute + mcpRunMargin
	mcpWorkspaceFileLimit = 100 * time.Second
)

// mcpAPIArgs is the parsed command line of `mcp api`.
type mcpAPIArgs struct {
	method string
	params json.RawMessage
	in     string // body file for write_file / exec stdin; "-" = stdin
	out    string // file for the read_file body
}

func parseMCPAPIArgs(args []string) (*mcpAPIArgs, error) {
	const usage = "usage: mirafleet mcp api METHOD [JSON] [-in FILE] [-out FILE]"
	fs := flag.NewFlagSet("mcp api", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	in := fs.String("in", "", "body for workspace.write_file / workspace.exec stdin (- = stdin)")
	out := fs.String("out", "", "file for the workspace.read_file body")
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, fmt.Errorf("%s: %v", usage, err)
		}
		args = fs.Args()
		if len(args) == 0 {
			break
		}
		pos, args = append(pos, args[0]), args[1:]
	}
	if len(pos) < 1 || len(pos) > 2 {
		return nil, errors.New(usage)
	}
	a := &mcpAPIArgs{method: pos[0], in: *in, out: *out}
	if len(pos) == 2 {
		if !json.Valid([]byte(pos[1])) {
			return nil, errors.New("params are not valid JSON")
		}
		a.params = json.RawMessage(pos[1])
	}
	switch {
	case a.in != "" && a.method != api.MethodWorkspaceWriteFile && a.method != api.MethodWorkspaceExec:
		return nil, fmt.Errorf("-in only applies to %s and %s", api.MethodWorkspaceWriteFile, api.MethodWorkspaceExec)
	case a.out == "-":
		return nil, errors.New("-out - is not supported: the body is binary and stdout carries the JSON result; give a file path")
	case a.out != "" && a.method != api.MethodWorkspaceReadFile:
		return nil, fmt.Errorf("-out only applies to %s", api.MethodWorkspaceReadFile)
	case a.method == api.MethodWorkspaceReadFile && a.out == "":
		return nil, fmt.Errorf("%s needs -out FILE for the file body", api.MethodWorkspaceReadFile)
	case a.method == api.MethodWorkspaceWriteFile && a.in == "":
		return nil, fmt.Errorf("%s needs -in FILE for the file body (an empty file writes an empty file)", api.MethodWorkspaceWriteFile)
	}
	return a, nil
}

// checkOutPath fails early (before any network call) when the -out file could not be written: its
// directory must exist and the path itself must not be a directory.
func checkOutPath(out string) error {
	dir := filepath.Dir(out)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return fmt.Errorf("-out %s: directory %s does not exist", out, dir)
	}
	if fi, err := os.Lstat(out); err == nil && fi.IsDir() {
		return fmt.Errorf("-out %s is a directory", out)
	}
	return nil
}

// readBodyFile reads the request body from path ("-" is stdin), refusing more than
// api.MaxFileChunk bytes.
func readBodyFile(path string, stdin io.Reader) ([]byte, error) {
	var r io.Reader = stdin
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		r = f
	} else if stdin == nil {
		return nil, errors.New("no stdin")
	}
	b, err := io.ReadAll(io.LimitReader(r, api.MaxFileChunk+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if len(b) > api.MaxFileChunk {
		return nil, fmt.Errorf("body is larger than %d bytes: send files in chunks of at most that size (use offset)", api.MaxFileChunk)
	}
	return b, nil
}

// withBodyParams returns params with the fields the body implies filled in when the caller left
// them out: write_file gets length and sha256, exec gets stdin:true. Fields the caller set are
// kept (the server then refuses a mismatch).
func withBodyParams(method string, params json.RawMessage, body []byte) (json.RawMessage, error) {
	m := map[string]json.RawMessage{}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &m); err != nil {
			return nil, fmt.Errorf("params must be a JSON object: %w", err)
		}
	}
	set := func(k string, v any) {
		if _, ok := m[k]; !ok {
			m[k], _ = json.Marshal(v)
		}
	}
	switch method {
	case api.MethodWorkspaceWriteFile:
		sum := sha256.Sum256(body)
		set("length", len(body))
		set("sha256", hex.EncodeToString(sum[:]))
	case api.MethodWorkspaceExec:
		set("stdin", true)
	}
	return json.Marshal(m)
}

func mcpAPI(args []string, stdin io.Reader, stdout io.Writer) error {
	pa, err := parseMCPAPIArgs(args)
	if err != nil {
		return err
	}
	if pa.out != "" {
		if err := checkOutPath(pa.out); err != nil {
			return err
		}
	}
	args = []string{pa.method}
	params := pa.params
	var body []byte
	if pa.in != "" {
		if body, err = readBodyFile(pa.in, stdin); err != nil {
			return err
		}
		if params, err = withBodyParams(pa.method, params, body); err != nil {
			return err
		}
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
	var rbody []byte
	switch {
	case pa.in != "":
		err = cl.CallBody(ctx, args[0], p, body, &res)
	case pa.out != "":
		rbody, err = cl.CallReadBody(ctx, args[0], p, &res)
	default:
		err = cl.Call(ctx, args[0], p, &res)
	}
	if err != nil {
		var ae *api.Error
		if errors.As(err, &ae) {
			return fmt.Errorf("%s: %s", ae.Code, ae.Message)
		}
		return err
	}
	if pa.out != "" {
		var rr api.WorkspaceReadFileResult
		if err := json.Unmarshal(res, &rr); err != nil || rr.Length != len(rbody) {
			return fmt.Errorf("read_file body is %d bytes but the result announces %d: not writing %s", len(rbody), rr.Length, pa.out)
		}
		// 0600 and atomic: a partial file is never visible under the final name.
		if err := writeAtomic0600(filepath.Dir(pa.out), filepath.Base(pa.out), rbody); err != nil {
			return fmt.Errorf("write %s: %w", pa.out, err)
		}
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, res, "", "  "); err != nil {
		buf.Reset()
		buf.Write(res)
	}
	fmt.Fprintln(stdout, buf.String())
	return nil
}
