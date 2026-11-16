// Package secrets encrypts credential values at rest on central: AES-256-GCM
// with a random nonce per value, a versioned envelope that names the key, and a key-check value
// central stores to detect a missing or wrong key at start-up.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

// EnvKey is the environment variable holding the base64 32-byte key.
const EnvKey = "MIRAFLEET_SECRETS_KEY"

// KeySize is the AES-256 key length in bytes.
const KeySize = 32

const (
	envelopeV1 = 1
	keyIDLen   = 4
	nonceLen   = 12
	headerLen  = 1 + keyIDLen + nonceLen
	tagLen     = 16
)

var (
	// ErrNoKey means the key environment variable is unset or empty.
	ErrNoKey = errors.New("secrets: " + EnvKey + " is not set (generate one with `mirafleet central keygen`)")
	// ErrWeakKey means the key is the wrong length or obviously low-entropy.
	ErrWeakKey = errors.New("secrets: key must be 32 random bytes, base64 encoded")
	// ErrWrongKey means the data was sealed under a different key.
	ErrWrongKey = errors.New("secrets: wrong key")
	// ErrCorrupt means the envelope is truncated, malformed or was tampered with.
	ErrCorrupt = errors.New("secrets: ciphertext is corrupt or was tampered with")
)

// Key is a validated 32-byte secrets key. The zero value is invalid.
type Key struct {
	b  [KeySize]byte
	ok bool
}

// ParseKey decodes a base64 key (standard or URL alphabet, padded or not) and refuses keys that
// are not exactly 32 bytes or that have fewer than 8 distinct byte values.
func ParseKey(s string) (Key, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Key{}, ErrNoKey
	}
	var raw []byte
	var err error
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if raw, err = enc.DecodeString(s); err == nil {
			break
		}
	}
	if err != nil || len(raw) != KeySize {
		return Key{}, ErrWeakKey
	}
	seen := map[byte]struct{}{}
	for _, c := range raw {
		seen[c] = struct{}{}
	}
	if len(seen) < 8 {
		return Key{}, ErrWeakKey
	}
	var k Key
	copy(k.b[:], raw)
	k.ok = true
	return k, nil
}

// KeyFromEnv reads and parses MIRAFLEET_SECRETS_KEY from the process environment.
func KeyFromEnv() (Key, error) { return ParseKey(os.Getenv(EnvKey)) }

// GenerateKey returns a fresh random key and its base64 form (for `central keygen`).
func GenerateKey() (Key, string, error) {
	var k Key
	if _, err := rand.Read(k.b[:]); err != nil {
		return Key{}, "", err
	}
	k.ok = true
	return k, base64.StdEncoding.EncodeToString(k.b[:]), nil
}

func (k Key) mac(label string) []byte {
	h := hmac.New(sha256.New, k.b[:])
	h.Write([]byte(label))
	return h.Sum(nil)
}

func (k Key) id() []byte { return k.mac("mirafleet secrets key id v1")[:keyIDLen] }

// Check returns the key-check value central stores: an HMAC of a constant under the key. It
// reveals nothing usable about the key.
func (k Key) Check() []byte { return k.mac("mirafleet secrets key check v1") }

// VerifyCheck reports whether stored matches this key's check value.
func (k Key) VerifyCheck(stored []byte) bool {
	return k.ok && subtle.ConstantTimeCompare(k.Check(), stored) == 1
}

// String never reveals the key.
func (k Key) String() string { return "secrets.Key{redacted}" }

// GoString never reveals the key.
func (k Key) GoString() string { return k.String() }

func (k Key) gcm() (cipher.AEAD, error) {
	if !k.ok {
		return nil, ErrNoKey
	}
	blk, err := aes.NewCipher(k.b[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(blk)
}

// Seal encrypts plaintext (which may be empty). aad is authenticated but not stored; callers pass
// the credential id and version so a ciphertext cannot be moved between rows.
// Envelope: version(1) | key id(4) | nonce(12) | ciphertext+tag.
func Seal(k Key, plaintext, aad []byte) ([]byte, error) {
	g, err := k.gcm()
	if err != nil {
		return nil, err
	}
	out := make([]byte, headerLen, headerLen+len(plaintext)+tagLen)
	out[0] = envelopeV1
	copy(out[1:], k.id())
	if _, err := rand.Read(out[1+keyIDLen : headerLen]); err != nil {
		return nil, err
	}
	return g.Seal(out, out[1+keyIDLen:headerLen], plaintext, fullAAD(out[:1+keyIDLen], aad)), nil
}

func fullAAD(head, aad []byte) []byte {
	return append(append([]byte{}, head...), aad...)
}

// Open decrypts a sealed envelope. It returns ErrWrongKey when the envelope names another key,
// and ErrCorrupt for truncated, malformed, tampered data or a mismatched aad.
func Open(k Key, sealed, aad []byte) ([]byte, error) {
	g, err := k.gcm()
	if err != nil {
		return nil, err
	}
	if len(sealed) < headerLen+tagLen {
		return nil, ErrCorrupt
	}
	if sealed[0] != envelopeV1 {
		return nil, fmt.Errorf("%w: unknown envelope version %d", ErrCorrupt, sealed[0])
	}
	if subtle.ConstantTimeCompare(sealed[1:1+keyIDLen], k.id()) != 1 {
		return nil, ErrWrongKey
	}
	pt, err := g.Open(nil, sealed[1+keyIDLen:headerLen], sealed[headerLen:], fullAAD(sealed[:1+keyIDLen], aad))
	if err != nil {
		return nil, ErrCorrupt
	}
	return pt, nil
}

// Rekey re-encrypts one sealed value from oldKey to newKey. oldAAD and newAAD differ when the
// caller also bumps the credential version.
func Rekey(oldKey, newKey Key, sealed, oldAAD, newAAD []byte) ([]byte, error) {
	pt, err := Open(oldKey, sealed, oldAAD)
	if err != nil {
		return nil, err
	}
	defer clear(pt)
	return Seal(newKey, pt, newAAD)
}
