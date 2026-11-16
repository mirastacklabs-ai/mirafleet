//go:build !windows

package main

import (
	"os/signal"
	"syscall"
)

// ignoreSIGPIPE makes a write to a closed stdout return EPIPE instead of killing the process (Go
// exits with SIGPIPE on a broken pipe at fd 1 or 2). The MCP server can still be answering a
// call when the harness closes our stdout; shutdown must stay orderly (o.Close runs).
func ignoreSIGPIPE() { signal.Ignore(syscall.SIGPIPE) }
