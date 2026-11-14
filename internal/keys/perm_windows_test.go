//go:build windows

package keys

import (
	"path/filepath"
	"testing"
)

func TestLoadSignerAcceptsGeneratedKeyOnWindows(t *testing.T) {
	dir := t.TempDir()
	if _, err := Generate(dir, "k", "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSigner(filepath.Join(dir, "k")); err != nil {
		t.Fatal(err)
	}
}
