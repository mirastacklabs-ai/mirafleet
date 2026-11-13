package apiclient

import (
	"bufio"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/keys"
	"github.com/mirastacklabs-ai/mirafleet/internal/tokens"
)

// ErrHostKeyMismatch is returned when the server's host key is not the expected one.
var ErrHostKeyMismatch = errors.New("host key does not match the pinned key")

// PinnedHash accepts only a host key whose tokens.HostKeyHash equals hash (the hash embedded in
// a join token or registration code). A malformed hash fails closed.
func PinnedHash(hash string) ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		if len(hash) != 43 {
			return errors.New("invalid pinned host key hash")
		}
		got := tokens.HostKeyHash(key)
		if subtle.ConstantTimeCompare([]byte(got), []byte(hash)) != 1 {
			return fmt.Errorf("%w (server presented %s)", ErrHostKeyMismatch, ssh.FingerprintSHA256(key))
		}
		return nil
	}
}

// KnownHosts accepts only keys pinned in a known_hosts-style file (see keys.PinnedHostKeys).
func KnownHosts(path string) ssh.HostKeyCallback { return keys.PinnedHostKeys(path) }

// Fingerprint accepts only the key whose fingerprint equals fp, given as "SHA256:<base64>"
// (ssh style) or the bare unpadded base64url hash used in tokens.
func Fingerprint(fp string) ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		got := ssh.FingerprintSHA256(key)
		want := fp
		if !strings.HasPrefix(want, "SHA256:") {
			want = "SHA256:" + strings.NewReplacer("-", "+", "_", "/").Replace(want)
		}
		if len(want) != len(got) || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			return fmt.Errorf("%w (server presented %s)", ErrHostKeyMismatch, got)
		}
		return nil
	}
}

// KnownHostsLine formats a known_hosts line for addr and key, for callers recording a verified
// key (e.g. after Join or Register).
func KnownHostsLine(addr string, key ssh.PublicKey) string {
	return addr + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
}

// maxAnswerBytes bounds how much of stdin Prompt reads.
const maxAnswerBytes = 64

// ErrNoTTY is returned by Prompt when stdin is not an interactive terminal.
var ErrNoTTY = errors.New("host key unknown and stdin is not a terminal; refusing to trust it (pin it with a token, code or fingerprint)")

// IsTTY reports whether f is an interactive terminal: a character device that is not the null
// device. Portable (no ioctl), so it is a conservative heuristic, not proof: other character
// devices (e.g. /dev/zero) pass it. The read cap in Prompt, not this check, bounds such misuse.
func IsTTY(f *os.File) bool {
	if f == nil {
		return false
	}
	st, err := f.Stat()
	if err != nil || st.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	if null, err := os.Stat(os.DevNull); err == nil && os.SameFile(st, null) {
		return false
	}
	return true
}

// Prompt asks the user on out to confirm the presented host key, reading the answer from in. It
// refuses (ErrNoTTY) without asking when in is not a terminal, so piped or scripted use can never
// be tricked into trust-on-first-use. Only the exact answer "yes" accepts.
func Prompt(in *os.File, out io.Writer) ssh.HostKeyCallback {
	return func(hostname string, _ net.Addr, key ssh.PublicKey) error {
		return prompt(IsTTY(in), in, out, hostname, key)
	}
}

func prompt(tty bool, in io.Reader, out io.Writer, hostname string, key ssh.PublicKey) error {
	if !tty {
		return ErrNoTTY
	}
	fmt.Fprintf(out, "The authenticity of host %s cannot be established.\n%s key fingerprint is %s.\nTrust it and continue (type yes)? ",
		hostname, key.Type(), ssh.FingerprintSHA256(key))
	// Cap the read: an endless char device (/dev/zero, /dev/urandom) as stdin must neither hang
	// nor exhaust memory. Anything over the cap has no newline-terminated "yes" and is refused.
	line, err := bufio.NewReader(io.LimitReader(in, maxAnswerBytes)).ReadString('\n')
	if err != nil && line == "" {
		return errors.New("host key not confirmed")
	}
	if strings.TrimSpace(line) != "yes" {
		return errors.New("host key not confirmed")
	}
	return nil
}
