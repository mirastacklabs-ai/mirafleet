package auth

import "testing"

// TestParams are cheap Argon2id parameters for tests that hash many passwords.
var TestParams = Params{Time: 1, Memory: 8, Threads: 1}

// SetParamsForTest makes new hashes use p and returns a function that restores the previous
// parameters. It panics outside a test binary, so production code cannot weaken the cost.
// Call it from TestMain or at the top of a test that does not run in parallel with others that
// hash.
func SetParamsForTest(p Params) (restore func()) {
	if !testing.Testing() {
		panic("auth: SetParamsForTest is for tests only")
	}
	old := params
	params = p
	return func() { params = old }
}
