//go:build !linux

// Package initproc is the PID 1 of a workspace container (linux only).
package initproc

import "errors"

// Run fails outside linux.
func Run() error { return errors.New("workspace init runs only on linux") }
