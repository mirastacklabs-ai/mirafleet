package main

import (
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/keys"
	"github.com/mirastacklabs-ai/mirafleet/internal/opconf"
	"github.com/mirastacklabs-ai/mirafleet/internal/orch"
)

// Cross-platform (linux, darwin, windows): no central or runner dependencies.

const (
	maxSecretBytes = 4096
	mcpCallTimeout = orch.CallTimeout
	// mcpRunMargin is added to an action.run timeout for dial, queueing and reply latency.
	mcpRunMargin = orch.RunMargin
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
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return mcpServe(args, stdin, stdout)
	}
	switch args[0] {
	case "serve":
		return mcpServe(args[1:], stdin, stdout)
	case "context":
		return mcpContext(args[1:], stdout)
	case "login":
		return mcpLogin(args[1:], stdin, stdout)
	case "api":
		return mcpAPI(args[1:], stdin, stdout)
	case "hook":
		return mcpHookMain(args[1:], stdin, stdout)
	}
	return fmt.Errorf("unknown mcp command %q (want serve, context, login, api or hook)", args[0])
}

// loadOperatorKey loads the operator key through internal/keys, whose per-platform permission
// check (mode bits on unix, owner-only DACL on Windows) is the security boundary and fails closed.
func loadOperatorKey(dir string) (ssh.Signer, error) {
	return keys.LoadSigner(opconf.KeyPath(dir))
}

// writeAtomic0600 writes dir/name atomically with a private temp file (explicit owner-only DACL on
// Windows), fsync, rename and a directory fsync. See keys.WriteAtomic0600.
func writeAtomic0600(dir, name string, data []byte) error {
	return keys.WriteAtomic0600(dir, name, data)
}
