package main

import "runtime/debug"

// init falls back to the module version stamped by `go install ...@vX.Y.Z` when the release
// build did not set -X main.version (goreleaser does). Source builds report "(devel)", which
// is ignored so the version stays "dev".
func init() {
	if version != "dev" {
		return
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		version = resolveVersion(version, bi.Main.Version)
	}
}

// resolveVersion returns the linked version when set, else the module version when it is a
// real one, else the linked value unchanged.
func resolveVersion(linked, module string) string {
	if linked != "dev" || module == "" || module == "(devel)" {
		return linked
	}
	return module
}
