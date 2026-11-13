//go:build unix

package keys

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSignerRefusesLooseModes(t *testing.T) {
	dir := t.TempDir()
	if _, err := Generate(dir, "k", "k"); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "k")
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	if _, err := LoadSigner(p); err != nil {
		t.Fatal(err)
	}
	for _, m := range []os.FileMode{0o640, 0o604, 0o644, 0o660} {
		os.Chmod(p, m)
		if _, err := LoadSigner(p); err == nil {
			t.Errorf("mode %04o accepted", m)
		}
	}
}
