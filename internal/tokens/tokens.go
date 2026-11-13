// Package tokens holds the formats of join tokens (mfj1.) and operator registration codes
// (mfr1.), shared by central and clients. Pure Go, cross-platform, no I/O.
//
// Join token:  mfj1.<id: 16 lowercase hex>.<secret: 43 b64url>.<host-key hash: 43 b64url>
// Reg. code:   mfr1.<26 base32>.<host-key hash: 43 b64url>
//
// The host-key hash pins central on first contact (no TOFU). b64url is unpadded base64url; every
// value must be canonical (it re-encodes to the same string).
package tokens

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	JoinPrefix = "mfj1."
	CodePrefix = "mfr1."

	idHexLen    = 16
	b64Len      = 43 // 32 bytes, unpadded base64url
	CodeLen     = 26 // base32 chars of the HMAC: 130 bits
	StepSeconds = 60
	codeDomain  = "mirafleet-register-v1"
)

// ErrFormat is returned for anything that is not exactly a well-formed token or code. It never
// echoes the input.
var ErrFormat = errors.New("tokens: malformed")

var (
	b64 = base64.RawURLEncoding
	// b32 is RFC 4648 base32, UPPERCASE, no padding (alphabet A-Z 2-7). Lowercase is rejected.
	b32 = base32.StdEncoding.WithPadding(base32.NoPadding)
)

// HostKeyHash returns the SHA-256 of the key's wire encoding as unpadded base64url (43 chars),
// the same digest ssh.FingerprintSHA256 prints after "SHA256:" (that one uses std base64).
func HostKeyHash(k ssh.PublicKey) string {
	sum := sha256.Sum256(k.Marshal())
	return b64.EncodeToString(sum[:])
}

// NewJoinToken makes a token for the given central host key. secretHash is SHA-256 of the 32-byte
// secret; central stores only that and looks the token up by id.
func NewJoinToken(hostKey ssh.PublicKey) (tok, id string, secretHash []byte, err error) {
	var raw [8 + 32]byte
	if _, err = rand.Read(raw[:]); err != nil {
		return "", "", nil, err
	}
	id = hex.EncodeToString(raw[:8])
	secret := raw[8:]
	h := sha256.Sum256(secret)
	tok = JoinPrefix + id + "." + b64.EncodeToString(secret) + "." + HostKeyHash(hostKey)
	return tok, id, h[:], nil
}

// ParseJoinToken strictly parses a join token.
func ParseJoinToken(s string) (id string, secret []byte, hostKeyHash string, err error) {
	rest, ok := strings.CutPrefix(s, JoinPrefix)
	if !ok {
		return "", nil, "", ErrFormat
	}
	parts := strings.Split(rest, ".")
	if len(parts) != 3 || len(parts[0]) != idHexLen || !lowerHex(parts[0]) {
		return "", nil, "", ErrFormat
	}
	secret, ok = decodeB64(parts[1])
	if !ok {
		return "", nil, "", ErrFormat
	}
	if _, ok = decodeB64(parts[2]); !ok {
		return "", nil, "", ErrFormat
	}
	return parts[0], secret, parts[2], nil
}

// Step returns the registration-code time step: unix seconds / 60.
func Step(now time.Time) int64 { return now.Unix() / StepSeconds }

// SecondsLeft returns whole seconds until the current step ends (1..60).
func SecondsLeft(now time.Time) int {
	return StepSeconds - int(now.Unix()%StepSeconds)
}

// codeBody is the 26-char base32 (uppercase) prefix of
// HMAC-SHA256(secret, "mirafleet-register-v1" || big-endian uint64 step).
func codeBody(secret []byte, step int64) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(codeDomain))
	var s [8]byte
	binary.BigEndian.PutUint64(s[:], uint64(step))
	m.Write(s[:])
	return b32.EncodeToString(m.Sum(nil))[:CodeLen]
}

// ErrSecretLen is returned for a registration secret that is not exactly 32 bytes.
var ErrSecretLen = errors.New("tokens: registration secret must be 32 bytes")

// Code returns the full registration code for a step: mfr1.<26 base32>.<host-key hash>. The
// secret must be exactly 32 bytes (ErrSecretLen otherwise).
func Code(secret []byte, step int64, hostKey ssh.PublicKey) (string, error) {
	if len(secret) != 32 {
		return "", ErrSecretLen
	}
	return CodePrefix + codeBody(secret, step) + "." + HostKeyHash(hostKey), nil
}

// ParseCode strictly parses a registration code. code is the 26-char body (what the server
// checks with VerifyCode); hostKeyHash is the pin for the client.
func ParseCode(s string) (code, hostKeyHash string, err error) {
	rest, ok := strings.CutPrefix(s, CodePrefix)
	if !ok {
		return "", "", ErrFormat
	}
	parts := strings.Split(rest, ".")
	if len(parts) != 2 || len(parts[0]) != CodeLen || !upperB32(parts[0]) {
		return "", "", ErrFormat
	}
	if _, ok := decodeB64(parts[1]); !ok {
		return "", "", ErrFormat
	}
	return parts[0], parts[1], nil
}

// VerifyCode reports whether presented (the body returned by ParseCode) is the code for the
// CURRENT step of now. Previous and future steps are refused. The compare is constant-time.
// Verification has no side effects, so the same code verifies any number of times in its step.
// A secret that is not exactly 32 bytes never verifies.
func VerifyCode(secret []byte, now time.Time, presented string) bool {
	if len(secret) != 32 {
		return false
	}
	want := codeBody(secret, Step(now))
	return len(presented) == len(want) && subtle.ConstantTimeCompare([]byte(presented), []byte(want)) == 1
}

func decodeB64(s string) ([]byte, bool) {
	if len(s) != b64Len {
		return nil, false
	}
	b, err := b64.DecodeString(s)
	if err != nil || len(b) != 32 || b64.EncodeToString(b) != s {
		return nil, false
	}
	return b, true
}

func lowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func upperB32(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'A' && c <= 'Z' || c >= '2' && c <= '7') {
			return false
		}
	}
	return true
}
