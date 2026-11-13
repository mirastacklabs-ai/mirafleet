package tokens

import (
	"bytes"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func fixedKey(t *testing.T, seedByte byte) ssh.PublicKey {
	t.Helper()
	priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seedByte}, 32))
	pk, err := ssh.NewPublicKey(priv.Public())
	if err != nil {
		t.Fatal(err)
	}
	return pk
}

func mustCode(t *testing.T, secret []byte, step int64, k ssh.PublicKey) string {
	t.Helper()
	c, err := Code(secret, step, k)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSecretLengthEnforced(t *testing.T) {
	k := fixedKey(t, 1)
	now := time.Unix(1_800_000_030, 0)
	good := bytes.Repeat([]byte{7}, 32)
	body, _, err := ParseCode(mustCode(t, good, Step(now), k))
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{0, 31, 33} {
		s := bytes.Repeat([]byte{7}, n)
		if _, err := Code(s, Step(now), k); !errors.Is(err, ErrSecretLen) {
			t.Errorf("Code len %d: %v", n, err)
		}
		// Even the body computed from the full secret's prefix/extension must not verify.
		if VerifyCode(s, now, body) {
			t.Errorf("VerifyCode accepted len %d", n)
		}
	}
	if _, err := Code(nil, 1, k); !errors.Is(err, ErrSecretLen) {
		t.Errorf("Code nil: %v", err)
	}
	if VerifyCode(nil, now, body) {
		t.Error("VerifyCode accepted nil")
	}
}

func TestHostKeyHash(t *testing.T) {
	k := fixedKey(t, 1)
	h := HostKeyHash(k)
	if len(h) != 43 {
		t.Fatalf("len %d", len(h))
	}
	if "SHA256:"+strings.NewReplacer("-", "+", "_", "/").Replace(h) != ssh.FingerprintSHA256(k) {
		t.Fatalf("%s vs %s", h, ssh.FingerprintSHA256(k))
	}
	if h == HostKeyHash(fixedKey(t, 2)) {
		t.Fatal("collision")
	}
}

func TestJoinTokenRoundTrip(t *testing.T) {
	k := fixedKey(t, 1)
	tok, id, sh, err := NewJoinToken(k)
	if err != nil {
		t.Fatal(err)
	}
	gid, secret, hk, err := ParseJoinToken(tok)
	if err != nil {
		t.Fatal(err)
	}
	if gid != id || hk != HostKeyHash(k) || len(secret) != 32 || len(id) != 16 {
		t.Fatal("mismatch")
	}
	if sum := sha256.Sum256(secret); !bytes.Equal(sum[:], sh) {
		t.Fatal("secret hash mismatch")
	}
	tok2, id2, _, _ := NewJoinToken(k)
	if tok2 == tok || id2 == id {
		t.Fatal("not random")
	}
}

func TestJoinTokenStrict(t *testing.T) {
	k := fixedKey(t, 1)
	tok, _, _, _ := NewJoinToken(k)
	parts := strings.Split(tok, ".")
	hk := parts[3]
	sec := parts[2]
	id := parts[1]
	bad := []string{
		"",
		tok + ".",
		tok + "x",
		tok[:len(tok)-1],
		"mfj2." + strings.TrimPrefix(tok, "mfj1."),
		"MFJ1." + strings.TrimPrefix(tok, "mfj1."),
		" " + tok,
		tok + "\n",
		"mfj1." + strings.ToUpper(id) + "." + sec + "." + hk,
		"mfj1." + id[:15] + "." + sec + "." + hk,
		"mfj1." + id[:15] + "g." + sec + "." + hk,
		"mfj1." + id + "." + sec[:42] + "." + hk,
		"mfj1." + id + "." + sec + "+" + "." + hk,
		"mfj1." + id + "." + strings.Repeat("A", 42) + "B." + hk, // non-canonical trailing bits
		"mfj1." + id + "." + sec + "." + hk[:42],
		"mfj1." + id + "." + sec + "." + strings.Repeat("=", 43),
		"mfj1." + id + "." + sec,
		"mfj1." + id + "." + sec + "." + hk + "." + hk,
		"mfr1." + id + "." + sec + "." + hk,
	}
	for _, s := range bad {
		if _, _, _, err := ParseJoinToken(s); err == nil {
			t.Errorf("accepted %q", s)
		}
	}
}

// Independent computation of the spec formula.
func refBody(secret []byte, step int64) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte("mirafleet-register-v1"))
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(step))
	m.Write(b[:])
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(m.Sum(nil))[:26]
}

func TestCodeKnownAnswer(t *testing.T) {
	secret := bytes.Repeat([]byte{0x42}, 32)
	k := fixedKey(t, 1)
	cases := []struct {
		step int64
		body string
	}{
		{0, kat0},
		{29000000, kat1},
	}
	for _, c := range cases {
		if got := refBody(secret, c.step); got != c.body {
			t.Fatalf("ref step %d: %s want %s", c.step, got, c.body)
		}
		want := "mfr1." + c.body + "." + HostKeyHash(k)
		if got := mustCode(t, secret, c.step, k); got != want {
			t.Fatalf("step %d: %s want %s", c.step, got, want)
		}
	}
	if katHash != HostKeyHash(k) {
		t.Fatalf("host key hash vector: %s", HostKeyHash(k))
	}
}

func TestStepAndSecondsLeft(t *testing.T) {
	t0 := time.Unix(120, 0)
	if Step(t0) != 2 || SecondsLeft(t0) != 60 {
		t.Fatal(Step(t0), SecondsLeft(t0))
	}
	t1 := time.Unix(179, 999_000_000)
	if Step(t1) != 2 || SecondsLeft(t1) != 1 {
		t.Fatal(Step(t1), SecondsLeft(t1))
	}
	if Step(time.Unix(180, 0)) != 3 {
		t.Fatal("step boundary")
	}
}

func TestVerifyCodeCurrentStepOnly(t *testing.T) {
	secret := bytes.Repeat([]byte{7}, 32)
	k := fixedKey(t, 1)
	now := time.Unix(1_800_000_030, 0)
	step := Step(now)
	body, hk, err := ParseCode(mustCode(t, secret, step, k))
	if err != nil || hk != HostKeyHash(k) {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ { // not single-use
		if !VerifyCode(secret, now, body) {
			t.Fatalf("verification %d refused", i)
		}
	}
	// Whole current window accepted, boundaries exclusive of neighbours.
	if !VerifyCode(secret, time.Unix(step*60, 0), body) || !VerifyCode(secret, time.Unix(step*60+59, 0), body) {
		t.Fatal("window edges")
	}
	if VerifyCode(secret, time.Unix((step+1)*60, 0), body) {
		t.Fatal("expired code accepted in next step")
	}
	if VerifyCode(secret, time.Unix(step*60-1, 0), body) {
		t.Fatal("code accepted in previous step")
	}
	// A previous-step code is refused now.
	prev, _, _ := ParseCode(mustCode(t, secret, step-1, k))
	if VerifyCode(secret, now, prev) {
		t.Fatal("previous step accepted")
	}
	next, _, _ := ParseCode(mustCode(t, secret, step+1, k))
	if VerifyCode(secret, now, next) {
		t.Fatal("next step accepted")
	}
	// Wrong secret, tamper, wrong length.
	if VerifyCode(bytes.Repeat([]byte{8}, 32), now, body) {
		t.Fatal("wrong secret")
	}
	tam := []byte(body)
	tam[0] ^= 1
	if VerifyCode(secret, now, string(tam)) || VerifyCode(secret, now, "") || VerifyCode(secret, now, body[:25]) || VerifyCode(secret, now, body+"A") {
		t.Fatal("tampered accepted")
	}
}

func TestParseCodeStrict(t *testing.T) {
	secret := bytes.Repeat([]byte{7}, 32)
	k := fixedKey(t, 1)
	c := mustCode(t, secret, 5, k)
	if _, _, err := ParseCode(c); err != nil {
		t.Fatal(err)
	}
	body := c[5:31]
	hk := HostKeyHash(k)
	bad := []string{
		"", c + ".", c + "x", " " + c, c + "\n",
		"MFR1." + body + "." + hk,
		"mfr2." + body + "." + hk,
		"mfr1." + strings.ToLower(body) + "." + hk,
		"mfr1." + body[:25] + "." + hk,
		"mfr1." + body + "A." + hk,
		"mfr1." + body[:25] + "1." + hk, // '1' not in base32 alphabet
		"mfr1." + body[:25] + "=" + "." + hk,
		"mfr1." + body,
		"mfr1." + body + "." + hk[:42],
		"mfr1." + body + "." + hk + ".x",
		"mfj1." + body + "." + hk,
	}
	for _, s := range bad {
		if _, _, err := ParseCode(s); err == nil {
			t.Errorf("accepted %q", s)
		}
	}
}

func FuzzParseJoinToken(f *testing.F) {
	tok, _, _, _ := NewJoinToken(func() ssh.PublicKey {
		pub, _, _ := ed25519.GenerateKey(bytes.NewReader(bytes.Repeat([]byte{3}, 64)))
		k, _ := ssh.NewPublicKey(pub)
		return k
	}())
	f.Add(tok)
	f.Add("mfj1...")
	f.Fuzz(func(t *testing.T, s string) {
		id, secret, hk, err := ParseJoinToken(s)
		if err != nil {
			return
		}
		if len(id) != 16 || len(secret) != 32 || len(hk) != 43 {
			t.Fatal("bad shape")
		}
		if "mfj1."+id+"."+b64.EncodeToString(secret)+"."+hk != s {
			t.Fatal("non-canonical accepted")
		}
	})
}

func FuzzParseCode(f *testing.F) {
	f.Add("mfr1.ABCDEFGHIJKLMNOPQRSTUVWXYZ.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	f.Add("mfr1..")
	f.Fuzz(func(t *testing.T, s string) {
		c, hk, err := ParseCode(s)
		if err != nil {
			return
		}
		if len(c) != 26 || len(hk) != 43 || "mfr1."+c+"."+hk != s {
			t.Fatal("bad shape")
		}
	})
}
