//go:build !linux

// Package hostx is the host-mode workspace executor. It exists on linux
// only; this stub fails closed everywhere else.
package hostx

import "errors"

// ErrUnsupported is returned by every hostx entry point outside linux.
var ErrUnsupported = errors.New("hostx: host mode runs on linux only")

// RunShim fails outside linux.
func RunShim(args []string) error { return ErrUnsupported }
