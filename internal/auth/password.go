package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Params are Argon2id cost parameters. Memory is in KiB.
type Params struct {
	Time    uint32
	Memory  uint32
	Threads uint8
}

// defaultParams is the production cost (docs/M4.md §2.4): t=3, m=64 MiB, p=2. Unexported so no
// other package can weaken it; tests use SetParamsForTest.
var defaultParams = Params{Time: 3, Memory: 64 * 1024, Threads: 2}

// Bounds enforced when parsing a stored hash, so a corrupt or planted row cannot make
// verification allocate gigabytes or run for minutes.
const (
	maxMemoryKiB = 1 << 18 // 256 MiB
	maxTime      = 6
	maxThreads   = 16
	saltLen      = 16
	keyLen       = 32
	minSaltLen   = 8
	maxSaltLen   = 64
	minKeyLen    = 16
	maxKeyLen    = 64
)

// Password length rules (docs/M4.md §2.4).
const (
	MinPasswordBytes = 12
	MaxPasswordBytes = 256
)

// Password rule failures.
var (
	ErrPasswordLength   = errors.New("auth: password must be 12 to 256 bytes")
	ErrPasswordUTF8     = errors.New("auth: password must be valid UTF-8")
	ErrPasswordUsername = errors.New("auth: password must differ from the username")
	ErrPasswordRepeat   = errors.New("auth: password must not be one repeated character")
)

// params is the cost used for new hashes and for NeedsRehash. Tests lower it; production
// code never changes it.
var params = defaultParams

// ValidatePassword applies the password rules: 12 to 256 bytes of valid UTF-8, not equal to
// the username, not all one character. No composition rules (NIST SP 800-63B).
func ValidatePassword(password, username string) error {
	if len(password) < MinPasswordBytes || len(password) > MaxPasswordBytes {
		return ErrPasswordLength
	}
	if !utf8.ValidString(password) {
		return ErrPasswordUTF8
	}
	if strings.EqualFold(password, username) {
		return ErrPasswordUsername
	}
	first, _ := utf8.DecodeRuneInString(password)
	same := true
	for _, r := range password {
		if r != first {
			same = false
			break
		}
	}
	if same {
		return ErrPasswordRepeat
	}
	return nil
}

// HashPassword returns a PHC string ($argon2id$v=19$m=..,t=..,p=..$salt$hash) using the
// current parameters and a fresh random salt. It does not apply the password rules.
func HashPassword(password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, params.Time, params.Memory, params.Threads, keyLen)
	return encodePHC(params, salt, key), nil
}

// VerifyPassword reports whether password matches the PHC string. The key comparison is
// constant-time. A malformed or out-of-bounds hash never matches.
func VerifyPassword(password, encoded string) bool {
	p, salt, key, err := parsePHC(encoded)
	if err != nil {
		DummyVerify(password) // timing parity with a real verification
		return false
	}
	got := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, uint32(len(key)))
	return subtle.ConstantTimeCompare(got, key) == 1
}

// NeedsRehash reports whether encoded was made with parameters other than the current ones
// (or cannot be parsed), so a successful login should store a fresh hash.
func NeedsRehash(encoded string) bool {
	p, salt, key, err := parsePHC(encoded)
	return err != nil || p != params || len(key) != keyLen || len(salt) != saltLen
}

// dummySalt is fixed: the dummy exists only to cost the same as a real verification.
var dummySalt = []byte("mirafleet-dummy!")

// DummyVerify burns one Argon2id hash at the current cost and always returns false. Login
// calls it for an unknown username so timing does not reveal whether the user exists.
func DummyVerify(password string) bool {
	got := argon2.IDKey([]byte(password), dummySalt, params.Time, params.Memory, params.Threads, keyLen)
	_ = subtle.ConstantTimeCompare(got, make([]byte, keyLen))
	return false
}

func encodePHC(p Params, salt, key []byte) string {
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Time, p.Threads, b64.EncodeToString(salt), b64.EncodeToString(key))
}

var errPHC = errors.New("auth: malformed or unsupported password hash")

// parsePHC parses and bounds-checks a PHC string. It never panics.
func parsePHC(s string) (Params, []byte, []byte, error) {
	var zero Params
	// encoding/base64 silently skips \r and \n; refuse them so the encoding is canonical.
	if strings.ContainsAny(s, "\r\n") {
		return zero, nil, nil, errPHC
	}
	f := strings.Split(s, "$")
	if len(f) != 6 || f[0] != "" || f[1] != "argon2id" || f[2] != "v="+strconv.Itoa(argon2.Version) {
		return zero, nil, nil, errPHC
	}
	kv := strings.Split(f[3], ",")
	if len(kv) != 3 {
		return zero, nil, nil, errPHC
	}
	m, ok1 := numField(kv[0], "m=")
	t, ok2 := numField(kv[1], "t=")
	p, ok3 := numField(kv[2], "p=")
	if !ok1 || !ok2 || !ok3 ||
		t < 1 || t > maxTime || p < 1 || p > maxThreads || m > maxMemoryKiB || m < 8*p {
		return zero, nil, nil, errPHC
	}
	b64 := base64.RawStdEncoding.Strict()
	salt, err := b64.DecodeString(f[4])
	if err != nil || len(salt) < minSaltLen || len(salt) > maxSaltLen {
		return zero, nil, nil, errPHC
	}
	key, err := b64.DecodeString(f[5])
	if err != nil || len(key) < minKeyLen || len(key) > maxKeyLen {
		return zero, nil, nil, errPHC
	}
	return Params{Time: uint32(t), Memory: uint32(m), Threads: uint8(p)}, salt, key, nil
}

// numField parses "<prefix><canonical decimal>" (digits only, no sign, no leading zero).
func numField(s, prefix string) (uint64, bool) {
	if !strings.HasPrefix(s, prefix) {
		return 0, false
	}
	d := s[len(prefix):]
	if d == "" || len(d) > 10 || (len(d) > 1 && d[0] == '0') {
		return 0, false
	}
	for i := 0; i < len(d); i++ {
		if d[i] < '0' || d[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseUint(d, 10, 64)
	return n, err == nil
}
