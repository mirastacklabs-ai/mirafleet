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
	"syscall"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/mcpserver"
	"github.com/mirastacklabs-ai/mirafleet/internal/opconf"
	"github.com/mirastacklabs-ai/mirafleet/internal/orch"
)

// Cross-platform (linux, darwin, windows). This file is the only place the MCP server reads its
// environment (MIRAFLEET_CONFIG_DIR / MIRAFLEET_STATE_DIR via opconf, home, parent pid); orch and
// mcpserver get everything passed in.

// mcpShutdownGrace bounds orch.Close after the server stops.
const mcpShutdownGrace = 5 * time.Second

type serveOpts struct {
	context, fleet, session, projectDir string
}

// parseServeFlags parses the serve flags. -project-dir defaults to cwd and is made absolute.
// On -h it prints the flag defaults to errw (stderr, never stdout) and returns flag.ErrHelp.
func parseServeFlags(args []string, cwd string, errw io.Writer) (serveOpts, error) {
	var o serveOpts
	fs := flag.NewFlagSet("mcp serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.context, "context", "", "context name")
	fs.StringVar(&o.fleet, "fleet", "", "default fleet")
	fs.StringVar(&o.session, "session", "", "session id")
	fs.StringVar(&o.projectDir, "project-dir", "", "project directory")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fs.SetOutput(errw)
			fmt.Fprintln(errw, "usage: mirafleet mcp [serve] [flags]")
			fs.PrintDefaults()
			return o, flag.ErrHelp
		}
		return o, fmt.Errorf("mcp serve: %w", err)
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("mcp serve: unexpected argument %q", fs.Arg(0))
	}
	if o.projectDir == "" {
		o.projectDir = cwd
	}
	if !filepath.IsAbs(o.projectDir) {
		if cwd == "" {
			return o, errors.New("mcp serve: cannot resolve -project-dir (no working directory)")
		}
		o.projectDir = filepath.Join(cwd, o.projectDir)
	}
	o.projectDir = filepath.Clean(o.projectDir)
	return o, nil
}

// mcpServe is the production entry: signals, TTY hint, real stdio.
func mcpServe(args []string, stdin io.Reader, stdout io.Writer) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	if f, ok := stdin.(*os.File); ok {
		if st, err := f.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 {
			fmt.Fprintln(os.Stderr, "mirafleet mcp speaks MCP on stdin/stdout; configure it in your harness "+
				"(e.g. `claude mcp add mirafleet -- mirafleet mcp`). Press Ctrl-C to exit.")
		}
	}
	return runServe(ctx, stop, args, stdin, stdout, os.Stderr)
}

// runServe wires orch to the MCP server and runs it until ctx ends or stdin closes. stop releases
// the signal handling as soon as the server returns, so a second signal during the shutdown grace
// force-quits.
func runServe(ctx context.Context, stop func(), args []string, in io.Reader, out, errw io.Writer) error {
	cwd, _ := os.Getwd()
	opts, err := parseServeFlags(args, cwd, errw)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	logf := func(format string, a ...any) { fmt.Fprintf(errw, "mirafleet mcp: "+format+"\n", a...) }
	cfgDir, cfgErr := opconf.Dir()
	if cfgErr != nil {
		cfgDir = "" // orch reports "not set up" to the model; the server still starts
		logf("config dir unavailable: %v", cfgErr)
	}
	stateDir, stateErr := opconf.StateDir()
	if stateErr != nil {
		stateDir = ""
	}
	home, _ := os.UserHomeDir()
	o, err := orch.New(orch.Config{
		ConfigDir:  cfgDir,
		ConfigErr:  cfgErr,
		StateDir:   stateDir,
		StateErr:   stateErr,
		ProjectDir: opts.projectDir,
		Home:       home,
		Context:    opts.context,
		Fleet:      opts.fleet,
		Session:    opts.session,
		PPID:       os.Getppid(),
		Logf:       logf,
	})
	if err != nil {
		return err
	}
	serveErr := mcpserver.Serve(ctx, in, out, mcpserver.Config{
		Name:    "mirafleet",
		Version: version,
		Tools:   append(mcpserver.ListTools(o), mcpserver.WorkspaceTools(o)...),
		Logf:    logf,
	})
	stop()
	// Cancel in-flight work and close connections; never workspaces. Bounded.
	done := make(chan struct{})
	go func() { _ = o.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(mcpShutdownGrace):
		logf("shutdown timed out after %s", mcpShutdownGrace)
	}
	if serveErr != nil {
		return fmt.Errorf("mcp server stopped: %w", serveErr)
	}
	return nil
}
