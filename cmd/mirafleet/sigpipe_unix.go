//go:build !windows

package main

import (
	"os"
	"os/signal"
	"syscall"
)

// ignoreSIGPIPE makes a write to a closed stdout return EPIPE instead of killing the process (Go
// exits with SIGPIPE on a broken pipe at fd 1 or 2 unless the program has called Notify for it).
// The MCP server can still be answering a call when the harness closes our stdout, and shutdown
// must stay orderly (o.Close runs). signal.Notify is used rather than signal.Ignore on purpose:
// Ignore sets SIG_IGN, which child processes inherit across exec, while a handled signal is
// reset to the default in children. The channel is drained so the runtime never blocks on it.
func ignoreSIGPIPE() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGPIPE)
	go func() {
		for range ch {
		}
	}()
}
