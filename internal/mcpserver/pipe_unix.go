//go:build !windows

package mcpserver

import (
	"errors"
	"syscall"
)

// isBrokenPipe reports a write to a closed pipe (EPIPE; SIGPIPE is ignored by cmd/mirafleet).
func isBrokenPipe(err error) bool { return errors.Is(err, syscall.EPIPE) }
