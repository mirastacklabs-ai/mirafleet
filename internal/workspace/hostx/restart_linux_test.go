//go:build linux

package hostx

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
)

func newExecutorAt(t *testing.T, base string) *Executor {
	t.Helper()
	m, err := NewManager(SessionsConfig{Root: filepath.Join(base, "work"), StateDir: filepath.Join(base, "state"), DeleteAfter: -1})
	if err != nil {
		t.Fatal(err)
	}
	ex, err := NewExecutor(ExecutorConfig{Manager: m, Engine: newEngine(t, Config{}), StateDir: filepath.Join(base, "state")})
	if err != nil {
		t.Fatal(err)
	}
	return ex
}

func listIDs(t *testing.T, ex *Executor) map[workspace.ID]bool {
	t.Helper()
	infos, err := ex.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m := map[workspace.ID]bool{}
	for _, in := range infos {
		m[in.ID] = true
	}
	return m
}

// An executor started on the state of a previous runner process lists the workspaces that were
// still open, and not the ones that were closed (S11 follow-up 5).
func TestExecutorReadoptsOpenSessionsAfterRestart(t *testing.T) {
	base := t.TempDir()
	ctx := context.Background()
	ex1 := newExecutorAt(t, base)
	keep, closed := workspace.NewID(), workspace.NewID()
	for _, id := range []workspace.ID{keep, closed} {
		if err := ex1.Create(ctx, workspace.Spec{ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(base, "work", Slug(string(keep)), "f.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ex1.Destroy(ctx, closed); err != nil {
		t.Fatal(err)
	}
	if err := ex1.Close(); err != nil { // runner shutdown: keep stays open
		t.Fatal(err)
	}

	ex2 := newExecutorAt(t, base)
	t.Cleanup(func() { ex2.Close() })
	got := listIDs(t, ex2)
	if !got[keep] || got[closed] || len(got) != 1 {
		t.Fatalf("list after restart = %v, want only %s", got, keep)
	}
	rc, _, err := ex2.ReadFile(ctx, keep, "/workspace/f.txt", 0, 0)
	if err != nil {
		t.Fatalf("re-adopted session is not usable: %v", err)
	}
	rc.Close()

	// A crash (no Close at all) is re-adopted too.
	ex3 := newExecutorAt(t, base)
	t.Cleanup(func() { ex3.Close() })
	if got := listIDs(t, ex3); !got[keep] {
		t.Fatalf("list after a crash restart = %v", got)
	}
}

// A project-file workdir NAME resolves to a shared directory under the sessions root; sessions
// naming the same workdir share it, and a name that could leave the root is refused (follow-up 7).
func TestWorkdirNameResolvesUnderRoot(t *testing.T) {
	base := t.TempDir()
	ex := newExecutorAt(t, base)
	t.Cleanup(func() { ex.Close() })
	ctx := context.Background()
	a, b := workspace.NewID(), workspace.NewID()
	for _, id := range []workspace.ID{a, b} {
		if err := ex.Create(ctx, workspace.Spec{ID: id, Labels: map[string]string{LabelWorkdir: "proj-1"}}); err != nil {
			t.Fatalf("create with workdir name: %v", err)
		}
	}
	dir := filepath.Join(base, "work", "proj-1")
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("workdir %s: %v %v", dir, fi, err)
	}
	if _, err := ex.WriteFile(ctx, a, "/workspace/shared.txt", strings.NewReader("hi"), 0, true, 0o600); err != nil {
		t.Fatal(err)
	}
	if rc, _, err := ex.ReadFile(ctx, b, "/workspace/shared.txt", 0, 0); err != nil {
		t.Fatalf("second session does not see the shared dir: %v", err)
	} else {
		rc.Close()
	}
	for _, bad := range []string{"../escape", "a/b", ".hidden", "..", "A", "x y"} {
		err := ex.Create(ctx, workspace.Spec{ID: workspace.NewID(), Labels: map[string]string{LabelWorkdir: bad}})
		if err == nil {
			t.Errorf("workdir %q was accepted", bad)
		}
	}
	// Closing a workdir session keeps the directory.
	if err := ex.Destroy(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "shared.txt")); err != nil {
		t.Fatalf("workdir content lost on close: %v", err)
	}
}
