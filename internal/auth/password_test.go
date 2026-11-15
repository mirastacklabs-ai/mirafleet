package auth

import (
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/argon2"
)

// setParamsForTest lowers the Argon2 cost for the test's duration. Tests using it must not
// run in parallel.
func setParamsForTest(t *testing.T, p Params) {
	t.Helper()
	old := params
	params = p
	t.Cleanup(func() { params = old })
}

var fastParams = Params{Time: 1, Memory: 64, Threads: 1}

func TestDefaultParams(t *testing.T) {
	if defaultParams != (Params{Time: 3, Memory: 64 * 1024, Threads: 2}) {
		t.Fatalf("defaultParams = %+v", defaultParams)
	}
	if params != defaultParams {
		t.Fatalf("active params = %+v, want defaults", params)
	}
	got := encodePHC(defaultParams, make([]byte, saltLen), make([]byte, keyLen))
	if !strings.HasPrefix(got, "$argon2id$v=19$m=65536,t=3,p=2$") {
		t.Fatalf("PHC = %q", got)
	}
}

func TestHashVerify(t *testing.T) {
	setParamsForTest(t, fastParams)
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=64,t=1,p=1$") {
		t.Fatalf("hash = %q", h)
	}
	if !VerifyPassword("correct horse battery", h) {
		t.Fatal("right password rejected")
	}
	if VerifyPassword("correct horse batterz", h) || VerifyPassword("", h) {
		t.Fatal("wrong password accepted")
	}
	h2, _ := HashPassword("correct horse battery")
	if h == h2 {
		t.Fatal("salt not random")
	}
	if NeedsRehash(h) {
		t.Fatal("fresh hash needs rehash")
	}
}

func TestNeedsRehash(t *testing.T) {
	setParamsForTest(t, fastParams)
	h, _ := HashPassword("correct horse battery")
	params = Params{Time: 2, Memory: 64, Threads: 1}
	if !NeedsRehash(h) {
		t.Fatal("changed params should need rehash")
	}
	if !VerifyPassword("correct horse battery", h) {
		t.Fatal("old-param hash must still verify")
	}
	if !NeedsRehash("garbage") || !NeedsRehash("") {
		t.Fatal("unparseable should need rehash")
	}
}

func TestPHCStrictness(t *testing.T) {
	setParamsForTest(t, fastParams)
	h, _ := HashPassword("correct horse battery")
	f := strings.Split(h, "$")
	// A newline inside the salt or key would be skipped by the base64 decoder; refuse it.
	for _, i := range []int{4, 5} {
		g := append([]string(nil), f...)
		g[i] = g[i][:4] + "\n" + g[i][4:]
		bad := strings.Join(g, "$")
		if _, _, _, err := parsePHC(bad); err == nil {
			t.Errorf("newline in field %d accepted", i)
		}
		if VerifyPassword("correct horse battery", bad) {
			t.Errorf("newline in field %d verified", i)
		}
	}
	// A valid hash with a non-16-byte salt parses, verifies and is flagged for rehash.
	salt := make([]byte, 12)
	key := argon2.IDKey([]byte("correct horse battery"), salt, fastParams.Time, fastParams.Memory, fastParams.Threads, keyLen)
	odd := encodePHC(fastParams, salt, key)
	if !VerifyPassword("correct horse battery", odd) || !NeedsRehash(odd) {
		t.Error("12-byte salt should verify and need rehash")
	}
}

func TestDummyVerify(t *testing.T) {
	setParamsForTest(t, fastParams)
	if DummyVerify("anything at all") || DummyVerify("") {
		t.Fatal("dummy must never verify")
	}
}

func TestParseBounds(t *testing.T) {
	const tail = "$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	ok := []string{
		"$argon2id$v=19$m=65536,t=3,p=2" + tail,
		"$argon2id$v=19$m=262144,t=6,p=16" + tail,
	}
	for _, s := range ok {
		if _, _, _, err := parsePHC(s); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
	bad := []string{
		"$argon2id$v=19$m=262145,t=3,p=2" + tail, // > 256 MiB
		"$argon2id$v=19$m=65536,t=7,p=2" + tail,
		"$argon2id$v=19$m=65536,t=3,p=17" + tail,
		"$argon2id$v=19$m=65536,t=0,p=2" + tail,
		"$argon2id$v=19$m=65536,t=3,p=0" + tail,
		"$argon2id$v=19$m=4,t=3,p=2" + tail,
		"$argon2id$v=19$m=99999999999999,t=3,p=2" + tail,
		"$argon2id$v=19$m=065536,t=3,p=2" + tail,
		"$argon2id$v=19$m=+65536,t=3,p=2" + tail,
		"$argon2id$v=18$m=65536,t=3,p=2" + tail,
		"$argon2i$v=19$m=65536,t=3,p=2" + tail,
		"$argon2id$v=19$t=3,m=65536,p=2" + tail,
		"$argon2id$v=19$m=65536,t=3,p=2$AAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"$argon2id$v=19$m=65536,t=3,p=2$AAAAAAAAAAAAAAAAAAAAAA$AAAA",
		"$argon2id$v=19$m=65536,t=3,p=2$!!!!$!!!!",
		"argon2id$v=19$m=65536,t=3,p=2" + tail,
		"",
	}
	for _, s := range bad {
		if _, _, _, err := parsePHC(s); err == nil {
			t.Errorf("%q accepted", s)
		}
		if VerifyPassword("x", s) {
			t.Errorf("%q verified", s)
		}
	}
}

func TestValidatePassword(t *testing.T) {
	long := strings.Repeat("ab", 128)
	cases := []struct {
		pw, user string
		want     error
	}{
		{"abcdefghijkl", "admin", nil},
		{"correct horse battery staple", "admin", nil},
		{long, "admin", nil},
		{long + "a", "admin", ErrPasswordLength},
		{"abcdefghijk", "admin", ErrPasswordLength},
		{"", "admin", ErrPasswordLength},
		{"abcdefghijk\xff", "admin", ErrPasswordUTF8},
		{"administrator", "administrator", ErrPasswordUsername},
		{"Administrator", "administrator", ErrPasswordUsername},
		{"aaaaaaaaaaaa", "admin", ErrPasswordRepeat},
		{strings.Repeat("\xc3\xa9", 6), "admin", ErrPasswordRepeat}, // 12 bytes, one rune
		{"p\xc3\xa4ssw\xc3\xb6rd-ok", "admin", nil},
	}
	for _, c := range cases {
		if err := ValidatePassword(c.pw, c.user); !errors.Is(err, c.want) {
			t.Errorf("%q: got %v, want %v", c.pw, err, c.want)
		}
	}
}

func FuzzParsePHC(f *testing.F) {
	f.Add("$argon2id$v=19$m=65536,t=3,p=2$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	f.Add("$argon2id$v=19$m=1048577,t=11,p=17$AAAA$AAAA")
	f.Add("$$$$$")
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		p, salt, key, err := parsePHC(s)
		if err != nil {
			return
		}
		if p.Memory > maxMemoryKiB || p.Time < 1 || p.Time > maxTime || p.Threads < 1 || p.Threads > maxThreads {
			t.Fatalf("unbounded params accepted: %+v from %q", p, s)
		}
		if len(salt) < minSaltLen || len(key) < minKeyLen {
			t.Fatalf("short salt/key accepted from %q", s)
		}
		if len(salt) > maxSaltLen || len(key) > maxKeyLen {
			t.Fatalf("oversized salt/key accepted from %q", s)
		}
		if strings.ContainsAny(s, "\r\n") {
			t.Fatalf("newline accepted in %q", s)
		}
	})
}
