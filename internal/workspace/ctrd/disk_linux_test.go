//go:build linux

package ctrd

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMeasure(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a"), make([]byte, 100<<10), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "d"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(dir, "a"), filepath.Join(dir, "d", "hardlink")); err != nil {
		t.Fatal(err)
	}
	// A huge directory outside, reachable only through a symlink: never followed.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "big"), make([]byte, 5<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "out")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	used, partial, err := measure(context.Background(), root.FS())
	if err != nil || partial {
		t.Fatalf("measure: %d %v %v", used, partial, err)
	}
	if used < 100<<10 || used > 100<<10+64<<10 {
		t.Errorf("used = %d, want about 100 KiB (hardlink once, symlink not followed)", used)
	}
	// A sparse file counts its allocated blocks, not its length.
	f, err := os.Create(filepath.Join(dir, "sparse"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(1 << 30); err != nil {
		t.Fatal(err)
	}
	f.Close()
	used2, _, _ := measure(context.Background(), root.FS())
	if used2-used > 64<<10 {
		t.Errorf("sparse file counted as %d bytes", used2-used)
	}
	// An ended context gives a partial (lower-bound) answer.
	for i := 0; i < 600; i++ {
		_ = os.WriteFile(filepath.Join(dir, "d", "f"+string(rune('a'+i%26))+string(rune('a'+i/26))), nil, 0o600)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, partial, _ := measure(ctx, root.FS()); !partial {
		t.Error("cancelled walk not reported partial")
	}
}

func TestDiskEntrySnapshot(t *testing.T) {
	for _, c := range []struct {
		used, quota int64
		partial     bool
		over, kill  bool
	}{
		{0, 100, false, false, false},
		{99, 100, false, false, false},
		{100, 100, false, true, false},
		{199, 100, false, true, false},
		{200, 100, false, true, true},
		{1, 100, true, true, false},
	} {
		en := diskEntry{used: c.used, quota: c.quota, partial: c.partial}
		s := en.snapshot()
		if s.Over != c.over || s.Kill != c.kill {
			t.Errorf("%+v -> %+v", c, s)
		}
	}
}

func TestStaleAfter(t *testing.T) {
	if staleAfter(time.Second, false) != time.Second || staleAfter(time.Second, true) <= time.Second {
		t.Error("partial walks must be cached longer")
	}
}
