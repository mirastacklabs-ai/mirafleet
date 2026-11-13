// Package keys holds Ed25519 key helpers: generation, loading and known-hosts-style pinning.
package keys

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/crypto/ssh"
)

// EnsureDir creates dir with mode 0700 and tightens it to 0700 if it already exists. On Windows
// os.Chmod(0700) is effectively a no-op (it only toggles the read-only attribute); owner-only
// DACLs arrive with M5, and until then the Windows key loader fails closed.
func EnsureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// WriteNew creates path exclusively (it must not exist) with the given mode.
func WriteNew(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Generate creates an Ed25519 key pair at <dir>/<name> (private, 0600, OpenSSH PEM) and
// <dir>/<name>.pub (authorized_keys line ending in comment). It returns the public line.
// It refuses to overwrite an existing private key (O_EXCL, no check-then-write race).
func Generate(dir, name, comment string) (pubLine string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	blk, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return "", err
	}
	sp, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", err
	}
	if err := EnsureDir(dir); err != nil {
		return "", err
	}
	pubLine = PubLine(sp, comment)
	if err := WriteNew(filepath.Join(dir, name), pem.EncodeToMemory(blk), 0o600); err != nil {
		return "", err
	}
	if err := WriteNew(filepath.Join(dir, name+".pub"), []byte(pubLine+"\n"), 0o644); err != nil {
		return "", err
	}
	return pubLine, nil
}

// PubLine formats a public key as an authorized_keys line (no trailing newline).
func PubLine(k ssh.PublicKey, comment string) string {
	l := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k)))
	if comment != "" {
		l += " " + comment
	}
	return l
}

// LoadSigner reads an OpenSSH private key file. It refuses keys that are accessible to
// group or others (any 0o077 mode bit).
func LoadSigner(path string) (ssh.Signer, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if err := checkPrivateKeyPerm(path, st); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	return ssh.ParsePrivateKey(b)
}

// RunnerNameRE is the allowed shape of a runner name (the key comment).
var RunnerNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// Authorized is one accepted line of an authorized-runners file.
type Authorized struct {
	Key  ssh.PublicKey
	Name string
	Line int
}

// ParseAuthorized parses an authorized_keys-format file body line by line. Blank and '#' lines
// are skipped. Bad lines (unparsable, options present, not ssh-ed25519, invalid name) are
// reported in errs with their line number and do not affect good lines. A name mapped to more
// than one distinct key is refused entirely (all its lines dropped, one error reported): two
// hosts must never be able to answer to the same name.
func ParseAuthorized(data []byte) (ok []Authorized, errs []error) {
	var all []Authorized
	for i, raw := range strings.Split(string(data), "\n") {
		n := i + 1
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, comment, opts, rest, err := ssh.ParseAuthorizedKey([]byte(line))
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("line %d: %w", n, err))
		case len(opts) > 0:
			errs = append(errs, fmt.Errorf("line %d: key options are not allowed", n))
		case len(bytes.TrimSpace(rest)) > 0:
			errs = append(errs, fmt.Errorf("line %d: trailing data after key", n))
		case k.Type() != ssh.KeyAlgoED25519:
			errs = append(errs, fmt.Errorf("line %d: key type %s not allowed (only ssh-ed25519)", n, k.Type()))
		case !RunnerNameRE.MatchString(comment):
			errs = append(errs, fmt.Errorf("line %d: invalid runner name %q (must match %s)", n, comment, RunnerNameRE))
		default:
			all = append(all, Authorized{k, comment, n})
		}
	}
	byName := map[string]map[string]bool{}
	for _, a := range all {
		if byName[a.Name] == nil {
			byName[a.Name] = map[string]bool{}
		}
		byName[a.Name][string(a.Key.Marshal())] = true
	}
	reported := map[string]bool{}
	for _, a := range all {
		if len(byName[a.Name]) > 1 {
			if !reported[a.Name] {
				reported[a.Name] = true
				errs = append(errs, fmt.Errorf("line %d: duplicate runner name %q is used by %d different keys; refusing that name for all of them", a.Line, a.Name, len(byName[a.Name])))
			}
			continue
		}
		ok = append(ok, a)
	}
	return ok, errs
}

// PinnedHostKeys returns an ssh.HostKeyCallback that accepts only keys listed in a
// known-hosts-style file. Each line is "<host>[,<host>...] <keytype> <base64> [comment]"
// where host is the dialed address (host:port) or "*". Hashed entries are not supported.
// The file is read on every connection attempt.
func PinnedHostKeys(path string) ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read known hosts: %w", err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			hosts, rest, ok := strings.Cut(line, " ")
			if !ok {
				continue
			}
			match := false
			for _, h := range strings.Split(hosts, ",") {
				if h == "*" || h == hostname || h == remote.String() {
					match = true
				}
			}
			if !match {
				continue
			}
			pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(rest))
			if err == nil && bytes.Equal(pk.Marshal(), key.Marshal()) {
				return nil
			}
		}
		return fmt.Errorf("host key for %s (%s) not pinned in %s", hostname, ssh.FingerprintSHA256(key), path)
	}
}
