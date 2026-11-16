//go:build windows

package main

// ignoreSIGPIPE is a no-op: Windows has no SIGPIPE; a write to a closed pipe just errors.
func ignoreSIGPIPE() {}
