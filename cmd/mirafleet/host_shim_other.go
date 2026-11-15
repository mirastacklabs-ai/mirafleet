//go:build !linux

package main

import "errors"

// hostShimMain fails closed: host mode runs on linux only.
func hostShimMain(args []string) error { return errors.New("host-shim is supported on linux only") }
