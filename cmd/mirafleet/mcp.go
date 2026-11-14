package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/keys"
	"github.com/mirastacklabs-ai/mirafleet/internal/opconf"
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

// loadOperatorKey loads the operator key through internal/keys, whose per-platform permission
// check (mode bits on unix, owner-only DACL on Windows) is the security boundary and fails closed.
func loadOperatorKey(dir string) (ssh.Signer, error) {
	return keys.LoadSigner(opconf.KeyPath(dir))
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
