package keys

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func newLine(t *testing.T, comment string) string {
	t.Helper()
	l, err := Generate(t.TempDir(), "k", comment)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestParseAuthorizedSkipsJunkWithoutLockout(t *testing.T) {
	good := newLine(t, "r1")
	data := "# header\n\n" + good + "\n   \n# trailing comment\n"
	ok, errs := ParseAuthorized([]byte(data))
	if len(ok) != 1 || ok[0].Name != "r1" || len(errs) != 0 {
		t.Fatalf("%+v %v", ok, errs)
	}
}

func TestParseAuthorizedBadLinesReportedWithNumber(t *testing.T) {
	good := newLine(t, "r1")
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ek, _ := ssh.NewPublicKey(&ec.PublicKey)
	data := strings.Join([]string{
		good,                               // 1 ok
		"garbage line",                     // 2 bad
		`command="id" ` + newLine(t, "r2"), // 3 options
		PubLine(ek, "ecdsa"),               // 4 wrong type
		newLine(t, "Bad Name"),             // 5 invalid name
		newLine(t, ""),                     // 6 no name
		newLine(t, "r3"),                   // 7 ok
	}, "\n")
	ok, errs := ParseAuthorized([]byte(data))
	if len(ok) != 2 || ok[0].Name != "r1" || ok[1].Name != "r3" || ok[1].Line != 7 {
		t.Fatalf("%+v", ok)
	}
	if len(errs) != 5 {
		t.Fatalf("%v", errs)
	}
	for i, ln := range []string{"line 2", "line 3", "line 4", "line 5", "line 6"} {
		if !strings.HasPrefix(errs[i].Error(), ln+":") {
			t.Errorf("err %d = %v, want prefix %s", i, errs[i], ln)
		}
	}
}

func TestParseAuthorizedDuplicateNameRefused(t *testing.T) {
	a, b, c := newLine(t, "dup"), newLine(t, "dup"), newLine(t, "solo")
	ok, errs := ParseAuthorized([]byte(a + "\n" + b + "\n" + c + "\n" + a + "\n"))
	if len(ok) != 1 || ok[0].Name != "solo" {
		t.Fatalf("dup name must be refused for every key: %+v", ok)
	}
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "duplicate runner name") {
		t.Fatalf("%v", errs)
	}
	// The same key listed twice under one name is harmless.
	ok, errs = ParseAuthorized([]byte(c + "\n" + c + "\n"))
	if len(ok) != 2 || len(errs) != 0 {
		t.Fatalf("%+v %v", ok, errs)
	}
}

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

func TestGenerateExclusiveAndLoadSignerMode(t *testing.T) {
	dir := t.TempDir()
	if _, err := Generate(dir, "k", "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(dir, "k", "k"); !errors.Is(err, os.ErrExist) {
		t.Fatalf("second Generate must fail with ErrExist, got %v", err)
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

func TestEnsureDirTightensExisting(t *testing.T) {
	d := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDir(d); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(d); st.Mode().Perm() != 0o700 {
		t.Fatalf("mode %v", st.Mode())
	}
	n := filepath.Join(d, "new", "sub")
	if err := EnsureDir(n); err != nil {
		t.Fatal(err)
	}
}
