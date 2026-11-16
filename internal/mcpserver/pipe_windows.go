//go:build windows

package mcpserver

import (
	"errors"
	"syscall"
)

// Windows error codes for a write to a pipe whose reader has gone.
const (
	errorBrokenPipe syscall.Errno = 109 // ERROR_BROKEN_PIPE
	errorNoData     syscall.Errno = 232 // ERROR_NO_DATA ("the pipe is being closed")
)

func isBrokenPipe(err error) bool {
	return errors.Is(err, syscall.EPIPE) || errors.Is(err, errorBrokenPipe) || errors.Is(err, errorNoData)
}
