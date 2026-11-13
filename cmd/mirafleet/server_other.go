//go:build !linux

package main

import "errors"

// Central and runner are linux-only. These stubs keep the other platforms free of the server
// dependencies (SQLite); main also refuses through requireServerOS, so this is defence in depth.
var errServerOnly = errors.New("central and runner are supported on linux only")

func centralInit([]string) error { return errServerOnly }
func centralRun([]string) error  { return errServerOnly }
func runnerMain([]string) error  { return errServerOnly }
