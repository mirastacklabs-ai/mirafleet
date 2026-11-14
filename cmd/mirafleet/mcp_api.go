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
	"os"
	"path/filepath"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/apiclient"
	"github.com/mirastacklabs-ai/mirafleet/internal/opconf"
	"github.com/mirastacklabs-ai/mirafleet/internal/orch"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// Cross-platform (linux, darwin, windows): no central or runner dependencies.

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

// writeReadFileBody checks the read_file result against the body and writes the body to out.
// An undecodable result (E-09) is reported as such, separately from a length mismatch.
func writeReadFileBody(out string, res json.RawMessage, rbody []byte) error {
	var rr api.WorkspaceReadFileResult
	if err := json.Unmarshal(res, &rr); err != nil {
		return fmt.Errorf("bad result: %v: not writing %s", err, out)
	}
	if rr.Length != len(rbody) {
		return fmt.Errorf("read_file body is %d bytes but the result announces %d: not writing %s", len(rbody), rr.Length, out)
	}
	// 0600 and atomic: a partial file is never visible under the final name.
	if err := writeAtomic0600(filepath.Dir(out), filepath.Base(out), rbody); err != nil {
		return fmt.Errorf("write %s: %w", out, err)
	}
	return nil
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
	ctx, cancel := context.WithTimeout(context.Background(), orch.CallLimit(args[0], params))
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
		if err := writeReadFileBody(pa.out, res, rbody); err != nil {
			return err
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
