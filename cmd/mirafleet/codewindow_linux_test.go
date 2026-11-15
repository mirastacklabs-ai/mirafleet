//go:build linux

package main

import (
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/tokens"
)

// waitCodeWindow sleeps into the next code step when fewer than 15 s of the
// current one remain, so a registration code fetched right after stays valid
// for the call that uses it (tokens.VerifyCode accepts only the current step).
func waitCodeWindow() {
	if left := tokens.SecondsLeft(time.Now()); left < 15 {
		time.Sleep(time.Duration(left)*time.Second + 100*time.Millisecond)
	}
}
