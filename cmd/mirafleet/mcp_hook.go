package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/mirastacklabs-ai/mirafleet/integrations/claude-code/hook"
)

// Cross-platform: the hook runs on the orchestrator side only.

// pluginVersion is the version in integrations/claude-code/.claude-plugin/plugin.json. The binary's
// own `version` is set at link time ("dev" in tests), so a test keeps this constant and the
// manifest in lockstep; bump both together.
const pluginVersion = "0.5.0"

// mcpHook runs `mirafleet mcp hook <harness> <event>`. Only claude-code has hooks.
func mcpHook(args []string, stdin io.Reader, stdout, stderr io.Writer, env hook.Env) (int, error) {
	if len(args) != 2 {
		return 0, fmt.Errorf("usage: mirafleet mcp hook claude-code <session-start|session-end|pre-tool-use>")
	}
	if args[0] != "claude-code" {
		return 0, fmt.Errorf("unknown hook harness %q (want claude-code)", args[0])
	}
	return hook.Run(context.Background(), args[1], stdin, stdout, stderr, env), nil
}

func mcpHookMain(args []string, stdin io.Reader, stdout io.Writer) error {
	code, err := mcpHook(args, stdin, stdout, os.Stderr, hook.DefaultEnv())
	if err != nil {
		return err
	}
	if code != 0 {
		os.Exit(code)
	}
	return nil
}
