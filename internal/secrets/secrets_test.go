package secrets

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func newKey(t *testing.T) (Key, string) {
	t.Helper()
	k, s, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k, s
}

func TestSealOpenRoundTrip(t *testing.T) {
	k, _ := newKey(t)
	for _, pt := range [][]byte{[]byte("decoy-secret-value"), {}, bytes.Repeat([]byte{0}, 5000)} {
		s, err := Seal(k, pt, []byte("id1:v1"))
		if err != nil {
			t.Fatal(err)
		}
		if len(pt) > 0 && bytes.Contains(s, pt[:min(len(pt), 8)]) && len(pt) < 100 {
			t.Fatal("ciphertext contains plaintext")
		}
		got, err := Open(k, s, []byte("id1:v1"))
		if err != nil || !bytes.Equal(got, pt) {
			t.Fatalf("open: %q %v", got, err)
		}
	}
}

func TestRandomNonce(t *testing.T) {
	k, _ := newKey(t)
	a, _ := Seal(k, []byte("x"), nil)
	b, _ := Seal(k, []byte("x"), nil)
	if bytes.Equal(a, b) {
		t.Fatal("two seals identical")
	}
}

func TestWrongKey(t *testing.T) {
	k1, _ := newKey(t)
	k2, _ := newKey(t)
	s, _ := Seal(k1, []byte("v"), nil)
	if _, err := Open(k2, s, nil); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("want ErrWrongKey, got %v", err)
	}
}

func TestTamperAndTruncate(t *testing.T) {
	k, _ := newKey(t)
	s, _ := Seal(k, []byte("value"), []byte("a"))
	for i := range s {
		c := bytes.Clone(s)
		c[i] ^= 1
		if _, err := Open(k, c, []byte("a")); err == nil {
			t.Fatalf("flip at byte %d accepted", i)
		}
	}
	for n := 0; n < len(s); n++ {
		if _, err := Open(k, s[:n], []byte("a")); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("truncate to %d: %v", n, err)
		}
	}
	if _, err := Open(k, s, []byte("b")); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("aad mismatch: %v", err)
	}
}

func TestParseKey(t *testing.T) {
	_, s := newKey(t)
	if _, err := ParseKey(s + "\n"); err != nil {
		t.Fatal(err)
	}
	bad := []string{
		base64.StdEncoding.EncodeToString(make([]byte, 32)),
		base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("ab"), 16)),
		base64.StdEncoding.EncodeToString([]byte("short")),
		base64.StdEncoding.EncodeToString(make([]byte, 64)),
		"not base64 !!",
	}
	for _, b := range bad {
		if _, err := ParseKey(b); !errors.Is(err, ErrWeakKey) {
			t.Errorf("%q: want ErrWeakKey, got %v", b, err)
		}
	}
	if _, err := ParseKey("  "); !errors.Is(err, ErrNoKey) {
		t.Fatalf("empty: %v", err)
	}
}

func TestKeyFromEnv(t *testing.T) {
	t.Setenv(EnvKey, "")
	if _, err := KeyFromEnv(); !errors.Is(err, ErrNoKey) || !strings.Contains(err.Error(), "keygen") {
		t.Fatalf("no key: %v", err)
	}
	k, s := newKey(t)
	t.Setenv(EnvKey, s)
	got, err := KeyFromEnv()
	if err != nil || !got.VerifyCheck(k.Check()) {
		t.Fatalf("env key: %v", err)
	}
}

func TestZeroKeyRefused(t *testing.T) {
	var z Key
	if _, err := Seal(z, []byte("x"), nil); !errors.Is(err, ErrNoKey) {
		t.Fatalf("seal: %v", err)
	}
	if z.VerifyCheck(z.Check()) {
		t.Fatal("zero key verified")
	}
}

func TestCheck(t *testing.T) {
	k1, _ := newKey(t)
	k2, _ := newKey(t)
	if !k1.VerifyCheck(k1.Check()) || k1.VerifyCheck(k2.Check()) || k1.VerifyCheck(nil) {
		t.Fatal("key check wrong")
	}
}

func TestKeyNotLeaked(t *testing.T) {
	k, s := newKey(t)
	for _, out := range []string{k.String(), k.GoString()} {
		if strings.Contains(out, s) {
			t.Fatal("key leaked")
		}
	}
}

func TestRekey(t *testing.T) {
	k1, _ := newKey(t)
	k2, _ := newKey(t)
	s, _ := Seal(k1, []byte("decoy"), []byte("c1:v1"))
	r, err := Rekey(k1, k2, s, []byte("c1:v1"), []byte("c1:v2"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(k1, r, []byte("c1:v2")); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("old key opens rekeyed: %v", err)
	}
	if got, err := Open(k2, r, []byte("c1:v2")); err != nil || string(got) != "decoy" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := Rekey(k2, k1, s, nil, nil); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("rekey with wrong old key: %v", err)
	}
}
