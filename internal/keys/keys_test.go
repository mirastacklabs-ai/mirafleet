package keys

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRunnerNameRE(t *testing.T) {
	for _, n := range []string{"a", "web-01.prod_x", "0abc"} {
		if !RunnerNameRE.MatchString(n) {
			t.Errorf("%q should be valid", n)
		}
	}
	for _, n := range []string{"", "-a", ".a", "A", "a b", "a/b", strings.Repeat("a", 64)} {
		if RunnerNameRE.MatchString(n) {
			t.Errorf("%q should be invalid", n)
		}
	}
}

func TestGenerateExclusive(t *testing.T) {
	dir := t.TempDir()
	if _, err := Generate(dir, "k", "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(dir, "k", "k"); !errors.Is(err, os.ErrExist) {
		t.Fatalf("second Generate must fail with ErrExist, got %v", err)
	}
}

func TestEnsureDirTightensExisting(t *testing.T) {
	d := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDir(d); err != nil {
		t.Fatal(err)
	}
	// Windows reports synthetic mode bits; protection there is the DACL,
	// covered by TestEnsureDirTightensExistingDir in acl_windows_test.go.
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(d); st.Mode().Perm() != 0o700 {
			t.Fatalf("mode %v", st.Mode())
		}
	}
	n := filepath.Join(d, "new", "sub")
	if err := EnsureDir(n); err != nil {
		t.Fatal(err)
	}
}
