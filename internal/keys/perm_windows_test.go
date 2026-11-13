//go:build windows

package keys

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadSignerFailsClosedOnWindows(t *testing.T) {
	dir := t.TempDir()
	if _, err := Generate(dir, "k", "k"); err != nil {
		t.Fatal(err)
	}
	_, err := LoadSigner(filepath.Join(dir, "k"))
	if err == nil || !strings.Contains(err.Error(), "cannot be verified on windows yet (M5)") {
		t.Fatalf("got %v", err)
	}
}
