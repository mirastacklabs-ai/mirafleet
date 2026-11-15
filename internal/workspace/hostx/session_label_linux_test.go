//go:build linux

package hostx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
)

func sessSpec(id workspace.ID, sess string) workspace.Spec {
	return workspace.Spec{ID: id, Labels: map[string]string{LabelSession: sess}}
}

// Reopening a session (a new workspace id, same session label) reattaches its directory, files
// and state dir; a different session gets its own; a second live open of the same session is
// refused; a restart re-adopts it under its workspace id.
func TestSessionLabelReattach(t *testing.T) {
	base := t.TempDir()
	ctx := context.Background()
	ex := newExecutorAt(t, base)
	t.Cleanup(func() { ex.Close() })

	a1, a2, b := workspace.NewID(), workspace.NewID(), workspace.NewID()
	if err := ex.Create(ctx, sessSpec(a1, "sess-a")); err != nil {
		t.Fatal(err)
	}
	if _, err := ex.WriteFile(ctx, a1, "/workspace/f.txt", strings.NewReader("hi"), 0, true, 0o600); err != nil {
		t.Fatal(err)
	}
	dir1, err := ex.SessionDir(a1)
	if err != nil {
		t.Fatal(err)
	}
	// A concurrent open of the same session is refused, not shared.
	if err := ex.Create(ctx, sessSpec(a2, "sess-a")); !errors.Is(err, workspace.ErrRefused) {
		t.Fatalf("second live open of one session: %v", err)
	}
	if err := ex.Destroy(ctx, a1); err != nil {
		t.Fatal(err)
	}
	if err := ex.Create(ctx, sessSpec(a2, "sess-a")); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	dir2, _ := ex.SessionDir(a2)
	if dir1 != dir2 {
		t.Fatalf("state dir differs: %s vs %s", dir1, dir2)
	}
	rc, _, err := ex.ReadFile(ctx, a2, "/workspace/f.txt", 0, 0)
	if err != nil {
		t.Fatalf("reopened session lost its files: %v", err)
	}
	rc.Close()
	if _, err := os.Stat(filepath.Join(base, "work", "sessions", Slug("sess-a"), "f.txt")); err != nil {
		t.Fatal(err)
	}

	if err := ex.Create(ctx, sessSpec(b, "sess-b")); err != nil {
		t.Fatal(err)
	}
	if rc, _, err := ex.ReadFile(ctx, b, "/workspace/f.txt", 0, 0); err == nil {
		rc.Close()
		t.Fatal("a different session sees the first session's files")
	}
	if dirB, _ := ex.SessionDir(b); dirB == dir1 {
		t.Fatal("different sessions share a state dir")
	}

	// Restart: re-adopted under the workspace id it was opened as.
	ex.Close()
	ex2 := newExecutorAt(t, base)
	t.Cleanup(func() { ex2.Close() })
	got := listIDs(t, ex2)
	if !got[a2] || !got[b] || got[a1] || len(got) != 2 {
		t.Fatalf("after restart: %v", got)
	}
	rc, _, err = ex2.ReadFile(ctx, a2, "/workspace/f.txt", 0, 0)
	if err != nil {
		t.Fatalf("re-adopted session: %v", err)
	}
	rc.Close()
}

// A workdir name equal to a session slug is a different directory.
func TestWorkdirNameDoesNotShareSessionDir(t *testing.T) {
	base := t.TempDir()
	ctx := context.Background()
	ex := newExecutorAt(t, base)
	t.Cleanup(func() { ex.Close() })
	s, w := workspace.NewID(), workspace.NewID()
	if err := ex.Create(ctx, sessSpec(s, "shared")); err != nil {
		t.Fatal(err)
	}
	if err := ex.Create(ctx, workspace.Spec{ID: w, Labels: map[string]string{LabelWorkdir: "shared"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := ex.WriteFile(ctx, s, "/workspace/only-session.txt", strings.NewReader("x"), 0, true, 0o600); err != nil {
		t.Fatal(err)
	}
	if rc, _, err := ex.ReadFile(ctx, w, "/workspace/only-session.txt", 0, 0); err == nil {
		rc.Close()
		t.Fatal("workdir and session share a directory")
	}
}
