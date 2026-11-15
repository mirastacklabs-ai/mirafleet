//go:build linux

package main

import "github.com/mirastacklabs-ai/mirafleet/internal/workspace/hostx"

// hostShimMain is the hidden `mirafleet host-shim job ...` entry point: the tiny supervisor of one
// background shell (docs/M7.md section 3.5). The runner starts it in its own session; it runs the
// command in its own process group, writes status and logs into the session's jobs dir and exits
// when the command does, so jobs outlive runner restarts. All logic lives in hostx.RunShim.
func hostShimMain(args []string) error { return hostx.RunShim(args) }
