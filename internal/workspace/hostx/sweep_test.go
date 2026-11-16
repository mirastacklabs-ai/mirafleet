//go:build linux

package hostx

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const day = 24 * time.Hour

func TestSweepAndWarning(t *testing.T) {
	busyJob := map[string]bool{}
	m, clk, base := newMgr(t, func(c *SessionsConfig) { c.Busy = func(id string) bool { return busyJob[id] } })
	wd := filepath.Join(base, "checkout")
	os.MkdirAll(wd, 0o755)
	os.WriteFile(filepath.Join(wd, "keep"), []byte("x"), 0o644)

	open := func(id, workdir string) {
		s, err := m.Open(id, workdir)
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
	}
	open("old", "")
	open("warn", "")
	open("fresh", "")
	open("job", "")
	open("shared", wd)
	live, _ := m.Open("live", "")
	defer live.Close()
	// The sessions were last used at t0; age them by moving the clock, refreshing some.
	clk.t = clk.t.Add(10 * day)
	open("fresh", "")
	clk.t = clk.t.Add(18 * day) // old: 28 d unused; fresh: 18 d
	open("warn", "")            // refreshed now
	clk.t = clk.t.Add(1 * day)  // old 29 d, warn 1 d, job 29 d, live 29 d but held
	busyJob["job"] = true

	rep := m.Sweep()
	if len(rep.Removed) != 0 {
		t.Fatalf("nothing is 30 d old yet: %+v", rep.Removed)
	}
	warned := map[string]bool{}
	for _, w := range rep.Warned {
		warned[w.ID] = true
	}
	if !warned["old"] || warned["job"] || warned["live"] || warned["fresh"] || warned["warn"] || warned["shared"] {
		t.Fatalf("warned: %v", warned)
	}

	clk.t = clk.t.Add(2 * day) // old/job/live/shared 31 d, fresh 21 d
	rep = m.Sweep()
	removed := map[string]bool{}
	for _, r := range rep.Removed {
		removed[r.ID] = true
	}
	if !removed["old"] || !removed["shared"] || len(removed) != 2 {
		t.Fatalf("removed: %v errs %v", removed, rep.Errors)
	}
	if _, err := os.Stat(filepath.Join(m.cfg.sessionsDir(), "old")); err == nil {
		t.Fatal("old dir remains")
	}
	for _, id := range []string{"job", "live", "fresh", "warn"} {
		if _, err := os.Stat(filepath.Join(m.cfg.sessionsDir(), id)); err != nil {
			t.Fatalf("%s must remain: %v", id, err)
		}
	}
	if _, err := os.Stat(filepath.Join(wd, "keep")); err != nil {
		t.Fatal("sweep touched a workdir")
	}

	// Reattach inside the warning window surfaces it once; use clears it.
	busyJob["job"] = false
	clk.t = clk.t.Add(day) // job unused 32 d: now eligible, but open it first
	s, err := m.Open("fresh", "")
	if err != nil {
		t.Fatal(err)
	}
	if s.Warning != "" {
		t.Fatalf("fresh warned: %q", s.Warning)
	}
	s.Close()
	clk.t = clk.t.Add(8 * day)  // fresh now 8 d unused
	clk.t = clk.t.Add(20 * day) // 28 d: inside the window
	s, _ = m.Open("fresh", "")
	if s.Warning == "" {
		t.Fatal("no warning inside the window")
	}
	s.Close()
}

func TestSweepSkipsLiveHolderProcess(t *testing.T) {
	m, clk, _ := newMgr(t, nil)
	s, _ := m.Open("x", "")
	s.Close()
	// Pretend another runner process (alive) still holds it.
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skip("no sleep")
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	mt, _ := m.load("x")
	mt.HeldPID = cmd.Process.Pid
	m.save("x", mt)
	clk.t = clk.t.Add(60 * day)
	if rep := m.Sweep(); len(rep.Removed) != 0 {
		t.Fatalf("removed a held dir: %+v", rep.Removed)
	}
	if err := m.Delete("x"); err != ErrBusy {
		t.Fatalf("%v", err)
	}
	cmd.Process.Kill()
	cmd.Wait()
	if rep := m.Sweep(); len(rep.Removed) != 1 {
		t.Fatalf("dead holder should not block: %+v", rep)
	}
}

func TestSweepOff(t *testing.T) {
	m, clk, _ := newMgr(t, func(c *SessionsConfig) { c.DeleteAfter = -1 })
	s, _ := m.Open("a", "")
	s.Close()
	clk.t = clk.t.Add(400 * day)
	if rep := m.Sweep(); len(rep.Removed)+len(rep.Warned) != 0 {
		t.Fatalf("%+v", rep)
	}
}

func TestPrune(t *testing.T) {
	m, clk, base := newMgr(t, nil)
	wd := filepath.Join(base, "co")
	os.MkdirAll(wd, 0o755)
	for _, id := range []string{"a", "b"} {
		s, _ := m.Open(id, "")
		write(t, s, "f", "12345")
		s.Close()
	}
	w, _ := m.Open("w", wd)
	w.Close()
	clk.t = clk.t.Add(5 * day)
	b, _ := m.Open("b", "")
	defer b.Close()
	clk.t = clk.t.Add(5 * day)
	c, _ := m.Open("c", "")
	c.Close()

	rep := m.Prune(PruneOptions{OlderThan: 7 * day})
	if len(rep.Candidates) != 1 || rep.Candidates[0].ID != "a" || rep.Candidates[0].Size != 5 || len(rep.Removed) != 0 {
		t.Fatalf("dry run: %+v", rep)
	}
	if _, err := os.Stat(filepath.Join(m.cfg.sessionsDir(), "a", "f")); err != nil {
		t.Fatal("dry run deleted")
	}
	rep = m.Prune(PruneOptions{OlderThan: 7 * day, Yes: true})
	if len(rep.Removed) != 1 {
		t.Fatalf("%+v", rep)
	}
	if _, err := os.Stat(filepath.Join(m.cfg.sessionsDir(), "a")); err == nil {
		t.Fatal("a remains")
	}
	// b is held (live): matched by age 0 but skipped.
	rep = m.Prune(PruneOptions{OlderThan: time.Nanosecond, Yes: true})
	if len(rep.Skipped) != 1 || rep.Skipped[0].ID != "b" {
		t.Fatalf("%+v", rep)
	}
	if _, err := os.Stat(filepath.Join(m.cfg.sessionsDir(), "b", "f")); err != nil {
		t.Fatal("live dir pruned")
	}
	if _, err := os.Stat(wd); err != nil {
		t.Fatal("workdir pruned")
	}
	if got := mustList(t, m); len(got) != 3 {
		t.Fatalf("expect b, c and w left: %+v", got)
	}
}

// Records live in StateDir/meta, and a delete or prune also removes the executor's
// state directory of the session (saved output, job logs).
func TestRemoveDropsStateDirAndMetaIsApart(t *testing.T) {
	m, clk, base := newMgr(t, nil)
	for _, id := range []string{"x", "x.json"} { // "x.json" must not collide with x's record
		s, err := m.Open(id, "")
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
		if err := os.MkdirAll(filepath.Join(base, "state", "sessions", Slug(id), "out"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if got := mustList(t, m); len(got) != 2 {
		t.Fatalf("x and x.json must be two sessions: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(base, "state", "meta", "x.json")); err != nil {
		t.Fatalf("record not in meta/: %v", err)
	}
	if err := m.Delete("x.json"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, "state", "sessions", "x.json")); err == nil {
		t.Fatal("delete left the state dir")
	}
	clk.t = clk.t.Add(10 * day)
	rep := m.Prune(PruneOptions{OlderThan: 7 * day, Yes: true})
	if len(rep.Removed) != 1 || len(rep.Errors) != 0 {
		t.Fatalf("%+v", rep)
	}
	if _, err := os.Stat(filepath.Join(base, "state", "sessions", "x")); err == nil {
		t.Fatal("prune left the state dir")
	}
}
