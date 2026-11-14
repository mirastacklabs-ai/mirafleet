package orch

import (
	"errors"
	"testing"

	"github.com/mirastacklabs-ai/mirafleet/internal/opconf"
)

// A failed directory resolution by the caller must never fall back to opconf (which would read
// the environment again) and must surface as ErrNotSetUp.
func TestConfigErrNoFallback(t *testing.T) {
	dir := oneCtx(t)
	state := t.TempDir()
	t.Setenv(opconf.EnvDir, dir) // a perfectly good directory a fallback would find
	t.Setenv(opconf.EnvStateDir, state)

	o, err := New(Config{ConfigErr: errors.New("boom"), StateErr: errors.New("boom"), ProjectDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	if o.cfg.ConfigDir != "" || o.cfg.StateDir != "" {
		t.Errorf("fell back to opconf: config=%q state=%q", o.cfg.ConfigDir, o.cfg.StateDir)
	}
	if _, err := o.ContextNames(); !errors.Is(err, ErrNotSetUp) {
		t.Errorf("ContextNames err = %v, want ErrNotSetUp", err)
	}
	if _, err := (&pool{o: o}).connect(); !errors.Is(err, ErrNotSetUp) {
		t.Errorf("connect err = %v, want ErrNotSetUp", err)
	}

	// Without the errors, the documented fallback still works.
	o2, _ := New(Config{ProjectDir: t.TempDir()})
	defer o2.Close()
	if o2.cfg.ConfigDir != dir || o2.cfg.StateDir != state {
		t.Errorf("fallback: config=%q state=%q", o2.cfg.ConfigDir, o2.cfg.StateDir)
	}
}

// An empty directory is never joined into a path, so no relative operator_key is possible.
func TestEmptyDirNeverUsed(t *testing.T) {
	o := &Orch{} // ConfigDir "" and cfgErr nil: the shape a careless caller could produce
	if !o.dirUnavailable() {
		t.Fatal("empty ConfigDir reported available")
	}
	if _, err := o.loadContexts(); !errors.Is(err, ErrNotSetUp) {
		t.Errorf("loadContexts err = %v, want ErrNotSetUp", err)
	}
}
