// Package keys holds Ed25519 key helpers: generation, loading and known-hosts-style pinning.
package keys

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/crypto/ssh"
)

// Platform hooks, set by acl_windows.go. On Windows os.Chmod/os.OpenFile modes do not set a DACL,
// so directories and private files are created with a protected owner-only DACL instead.
var (
	mkdirPrivate func(dir string) error
	createExcl   func(path string, mode os.FileMode) (*os.File, error)
)

// EnsureDir creates dir with mode 0700 and tightens it to 0700 if it already exists. On Windows
// it creates the missing directories with a protected owner-only DACL (acl_windows.go).
func EnsureDir(dir string) error {
	if mkdirPrivate != nil {
		return mkdirPrivate(dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// ACE policy shared by all platforms so it can be unit-tested anywhere; only Windows feeds it.
const (
	aceTypeAllow = 0 // ACCESS_ALLOWED_ACE_TYPE; every other value (deny, audit, callback...) is refused
	sidSystem    = "S-1-5-18"
	sidAdmins    = "S-1-5-32-544"
)

type aceEntry struct {
	Type      byte
	Inherited bool
	SID       string // set for allow ACEs only
}

type aclInfo struct {
	Owner     string
	Protected bool // SE_DACL_PROTECTED: no inheritance from the parent
	HasDACL   bool // a NULL DACL grants everyone full access
	ACEs      []aceEntry
}

// aclPolicy fails closed unless the owner is user, the DACL is present and protected, and every
// ACE is an explicit allow for user, SYSTEM or BUILTIN\Administrators. Deny ACEs are refused too:
// a deny on a foreign SID would be harmless, but accepting some denies means reasoning about
// ordering and group membership; a key we created never has one, so refusing costs nothing.
func aclPolicy(a aclInfo, user string) error {
	if user == "" || a.Owner == "" { // an unconvertible SID must never match another empty one
		return fmt.Errorf("empty owner or user SID")
	}
	if a.Owner != user {
		return fmt.Errorf("owner %s is not the current user", a.Owner)
	}
	if !a.HasDACL {
		return fmt.Errorf("no DACL (grants everyone access)")
	}
	if !a.Protected {
		return fmt.Errorf("DACL is not protected from inheritance")
	}
	for _, e := range a.ACEs {
		switch {
		case e.Type != aceTypeAllow:
			return fmt.Errorf("unsupported ACE type %d", e.Type)
		case e.Inherited:
			return fmt.Errorf("inherited ACE for %s", e.SID)
		case e.SID != user && e.SID != sidSystem && e.SID != sidAdmins:
			return fmt.Errorf("access granted to %s", e.SID)
		}
	}
	return nil
}

// WriteNew creates path exclusively (it must not exist) with the given mode. On Windows a mode
// without group/other bits yields a protected owner-only DACL from creation.
func WriteNew(path string, data []byte, mode os.FileMode) error {
	var f *os.File
	var err error
	if createExcl != nil {
		f, err = createExcl(path, mode)
	} else {
		f, err = os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	}
	if err != nil {
		return err
	}
	if err := writeSync(f, data); err != nil {
		f.Close()
		os.Remove(path) // created by us (O_EXCL): never leave a partial file
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return err
	}
	return nil
}

func writeSync(f interface {
	Write([]byte) (int, error)
	Sync() error
}, data []byte) error {
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Sync()
}

// WriteAtomic0600 writes dir/name atomically: a fresh private temp file (0600; on Windows a
// protected owner-only DACL set at creation, not inherited from dir), fsync, rename over name,
// fsync of dir. The temp file is removed on failure.
func WriteAtomic0600(dir, name string, data []byte) error {
	var r [8]byte
	if _, err := rand.Read(r[:]); err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".tmp-"+hex.EncodeToString(r[:]))
	if err := WriteNew(tmp, data, 0o600); err != nil {
		return err
	}
	defer os.Remove(tmp) // no-op after a successful rename
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return err
	}
	return SyncDir(dir)
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

// ErrHostKeyMismatch is wrapped by the error PinnedHostKeys returns for a key that is not pinned.
var ErrHostKeyMismatch = errors.New("host key is not pinned")

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
		return fmt.Errorf("%w: host key for %s (%s) not pinned in %s", ErrHostKeyMismatch, hostname, ssh.FingerprintSHA256(key), path)
	}
}
