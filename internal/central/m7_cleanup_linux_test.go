//go:build linux

package central

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/hostx"
)

func init() { m7Cleanup = m7CleanupSweep }

// m7CleanupSweep drives the host executor's session manager (the real cleanup policy, docs/M7.md
// 3.4) on its own injected clock: a directory unused for 27 days is warned about when it is
// reattached, one unused for 30 days is deleted by the sweep, and close alone never deletes.
func m7CleanupSweep(t *testing.T, clk *m4Clock) {
	root, state := t.TempDir(), t.TempDir()
	mgr, err := hostx.NewManager(hostx.SessionsConfig{Root: root, StateDir: state, Now: clk.now})
	must(t, err)
	mk := func(id string) string {
		sd, err := mgr.Open(id, "")
		must(t, err)
		must(t, os.WriteFile(filepath.Join(sd.Dir, "f"), []byte("x"), 0o600))
		must(t, sd.Close())
		return sd.Dir
	}
	keep, drop := mk("keep-me"), mk("drop-me")

	clk.advance(24 * 24 * time.Hour)
	if r := mgr.Sweep(); len(r.Removed) != 0 || len(r.Warned) != 0 {
		t.Fatalf("sweep at 24 days: %+v", r)
	}
	clk.advance(4 * 24 * time.Hour) // 28 days: inside the 3-day warning window
	if r := mgr.Sweep(); len(r.Removed) != 0 || len(r.Warned) != 2 {
		t.Fatalf("sweep at 28 days: removed %d warned %d", len(r.Removed), len(r.Warned))
	}
	// Reattaching warns, keeps the files and resets the clock for that session only.
	sd, err := mgr.Open("keep-me", "")
	must(t, err)
	if sd.Warning == "" {
		t.Error("reattaching a directory 2 days from deletion gave no warning")
	}
	if _, err := os.Stat(filepath.Join(sd.Dir, "f")); err != nil {
		t.Errorf("reattach lost the files: %v", err)
	}
	must(t, sd.Close())
	clk.advance(3 * 24 * time.Hour) // 31 days since drop-me was used, 3 since keep-me
	r := mgr.Sweep()
	if len(r.Removed) != 1 || len(r.Errors) != 0 {
		t.Fatalf("sweep at 31 days: %+v", r)
	}
	if _, err := os.Stat(drop); !os.IsNotExist(err) {
		t.Errorf("the unused directory survived the sweep: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("the recently used directory was deleted: %v", err)
	}
}
