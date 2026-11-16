// Package handoff stores the harness session id that a hook hands to the MCP server, keyed by
// (parent PID, project dir). It is harness-neutral and takes the state directory as a parameter.
//
// Every operation runs inside an os.Root opened on <dir>/sessions, so a path component swapped for
// a symlink after the checks cannot make an operation act outside the directory (F-01).
//
// Directories are created with keys.EnsureDir: 0700 on unix, a protected owner-only DACL on
// Windows (where file modes are no-ops, so private files rely on that directory DACL).
package handoff

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/keys"
)

const (
	subdir     = "sessions"
	maxFile    = 4096
	maxAge     = 7 * 24 * time.Hour
	maxScanned = 1000
)

// HarnessPIDEnv names the variable a launcher sets to the harness PID when it sits between the
// harness and this process (the Claude Code plugin starts mirafleet through a Node launcher, so
// os.Getppid() would be the launcher, which differs for the hook and the MCP server).
const HarnessPIDEnv = "MIRAFLEET_HARNESS_PID"

// HarnessPID returns the PID that keys handoffs: HarnessPIDEnv when it holds a positive integer,
// else the parent PID.
func HarnessPID() int {
	if n, err := strconv.Atoi(os.Getenv(HarnessPIDEnv)); err == nil && n > 0 {
		return n
	}
	return os.Getppid()
}

// Key identifies a handoff: the parent (harness) PID and the project directory.
type Key struct {
	PID     int
	Project string
}

type file struct {
	V       int       `json:"v"`
	Session string    `json:"session"`
	Project string    `json:"project"`
	PID     int       `json:"pid"`
	Written time.Time `json:"written"`
}

// clean returns the project path with symlinks resolved (falling back to the cleaned path when it
// cannot be resolved), so a logical $PWD and the physical path give the same key on the hook and
// the server side.
func (k Key) clean() string {
	p := filepath.Clean(k.Project)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return filepath.Clean(r)
	}
	return p
}

func (k Key) name() string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%s", k.PID, k.clean())))
	return hex.EncodeToString(sum[:])[:32] + ".json"
}

// checkPlainDir refuses a symlink, a non-directory and (unix) a directory owned by someone else.
func checkPlainDir(path string, fi os.FileInfo) error {
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("handoff: %s is not a plain directory", path)
	}
	if !ownedByUs(fi) {
		return fmt.Errorf("handoff: %s is not owned by the current user", path)
	}
	return nil
}

// openSessions opens <dir>/sessions as an os.Root. The state dir and sessions are Lstat-checked
// (plain directory, ours); with create they are made 0700. A missing directory is an error that
// satisfies errors.Is(err, os.ErrNotExist). The caller must Close the root.
func openSessions(dir string, create bool) (*os.Root, error) {
	if create {
		if err := keys.EnsureDir(dir); err != nil {
			return nil, err
		}
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if err := checkPlainDir(dir, fi); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	// The handle must be the directory that was checked, not one swapped in between.
	if got, err := root.Stat("."); err != nil || !os.SameFile(fi, got) {
		return nil, fmt.Errorf("handoff: %s changed while opening", dir)
	}
	d := filepath.Join(dir, subdir)
	fi, err = root.Lstat(subdir)
	if errors.Is(err, os.ErrNotExist) && create {
		if err = keys.EnsureDir(d); err != nil {
			return nil, err
		}
		fi, err = root.Lstat(subdir)
	}
	if err != nil {
		return nil, err
	}
	if err := checkPlainDir(d, fi); err != nil {
		return nil, err
	}
	sr, err := root.OpenRoot(subdir)
	if err != nil {
		return nil, err
	}
	if got, err := sr.Stat("."); err != nil || !os.SameFile(fi, got) {
		sr.Close()
		return nil, fmt.Errorf("handoff: %s changed while opening", d)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		// Tighten on the verified handle, not by name.
		f, err := sr.Open(".")
		if err == nil {
			err = f.Chmod(0o700)
			f.Close()
		}
		if err != nil {
			sr.Close()
			return nil, err
		}
	}
	return sr, nil
}

// Write stores session for k atomically (0600) and prunes files older than 7 days.
func Write(dir string, k Key, session string, now time.Time) error {
	if e := api.ValidateSession(session); e != nil {
		return e
	}
	sr, err := openSessions(dir, true)
	if err != nil {
		return err
	}
	defer sr.Close()
	b, err := json.Marshal(file{V: 1, Session: session, Project: k.clean(), PID: k.PID, Written: now.UTC()})
	if err != nil {
		return err
	}
	var r [8]byte
	if _, err := rand.Read(r[:]); err != nil {
		return err
	}
	tmp := ".tmp-" + hex.EncodeToString(r[:])
	f, err := sr.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer sr.Remove(tmp) // fails harmlessly after a successful rename
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := sr.Chtimes(tmp, now, now); err != nil { // age is measured against the caller's clock
		return err
	}
	if err := sr.Rename(tmp, k.name()); err != nil {
		return err
	}
	prune(sr, now)
	return nil
}

// prune removes files older than maxAge, scanning at most maxScanned entries (read in batches of
// that size, never the whole directory). Best effort.
func prune(sr *os.Root, now time.Time) {
	d, err := sr.Open(".")
	if err != nil {
		return
	}
	defer d.Close()
	ents, _ := d.ReadDir(maxScanned)
	for _, e := range ents {
		if !e.Type().IsRegular() {
			continue
		}
		fi, err := sr.Lstat(e.Name())
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if now.Sub(fi.ModTime()) > maxAge {
			_ = sr.Remove(e.Name())
		}
	}
}

// readIn reads and validates the handoff for k inside sr.
func readIn(sr *os.Root, k Key) (string, bool, error) {
	f, err := sr.OpenFile(k.name(), openFlags, 0)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", false, err
	}
	if !fi.Mode().IsRegular() { // a FIFO or device planted in the directory
		return "", false, fmt.Errorf("handoff: %s is not a regular file", k.name())
	}
	b, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil {
		return "", false, err
	}
	if len(b) > maxFile {
		return "", false, fmt.Errorf("handoff: file larger than %d bytes", maxFile)
	}
	var h file
	if err := json.Unmarshal(b, &h); err != nil {
		return "", false, fmt.Errorf("handoff: %w", err)
	}
	if h.V != 1 || h.PID != k.PID || h.Project != k.clean() || api.ValidateSession(h.Session) != nil {
		return "", false, nil
	}
	return h.Session, true, nil
}

// Read returns the session stored for k. A missing, mismatched or invalid file is not found
// (ok=false, nil error); an unsafe directory, a non-regular, oversize or unparsable file is an
// error.
func Read(dir string, k Key) (string, bool, error) {
	sr, err := openSessions(dir, false)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer sr.Close()
	return readIn(sr, k)
}

// RemoveIf deletes the handoff for k only when it stores session, so a SessionEnd for an old
// session cannot remove a newer session's handoff. A missing or different-session file is left
// alone (removed=false, nil error). It reads, compares, then removes: a Write by another process
// in that small window can still be removed. The window is microseconds and the cost is a lost
// reattach (the MCP server falls back to a per-process session), so no file locking is used.
func RemoveIf(dir string, k Key, session string) (removed bool, err error) {
	sr, err := openSessions(dir, false)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer sr.Close()
	s, ok, err := readIn(sr, k)
	if err != nil || !ok || s != session {
		return false, err
	}
	if err := sr.Remove(k.name()); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// Remove deletes the handoff for k; a missing file is not an error.
func Remove(dir string, k Key) error {
	sr, err := openSessions(dir, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer sr.Close()
	if err := sr.Remove(k.name()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
