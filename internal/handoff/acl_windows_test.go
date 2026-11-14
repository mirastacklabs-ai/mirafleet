//go:build windows

package handoff

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/keys"
)

// The directories Write creates carry the protected owner-only DACL: keys.EnsureDir on an
// existing directory re-verifies owner, protection and ACEs, and fails otherwise.
func TestCreatedDirsPassKeysDACLCheck(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := Write(dir, k1, "s", time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{dir, filepath.Join(dir, subdir)} {
		if err := keys.EnsureDir(d); err != nil {
			t.Fatalf("%s: %v", d, err)
		}
	}
}
