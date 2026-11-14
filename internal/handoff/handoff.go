// Package handoff stores the harness session id that a hook hands to the MCP server, keyed by
// (parent PID, project dir). It is harness-neutral and takes the state directory as a parameter.
//
// TODO(M5): on windows the 0700/0600 modes are no-ops; rely on the parent dir's owner-only DACL
// from internal/keys. No Windows ACL code here.
package handoff

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
)

const (
	subdir     = "sessions"
	maxFile    = 4096
	maxAge     = 7 * 24 * time.Hour
	maxScanned = 1000
)

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

func (k Key) clean() string { return filepath.Clean(k.Project) }

func (k Key) name() string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%s", k.PID, k.clean())))
	return hex.EncodeToString(sum[:])[:32] + ".json"
}

// sessionsDir returns <dir>/sessions. With create it makes it 0700; either way a symlink or
// non-directory is refused.
func sessionsDir(dir string, create bool) (string, error) {
	d := filepath.Join(dir, subdir)
	fi, err := os.Lstat(d)
	if errors.Is(err, os.ErrNotExist) && create {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return "", err
		}
		fi, err = os.Lstat(d)
	}
	if err != nil {
		return "", err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return "", fmt.Errorf("handoff: %s is not a plain directory", d)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(d, 0o700); err != nil {
			return "", err
		}
	}
	return d, nil
}

// Write stores session for k atomically (0600) and prunes files older than 7 days.
func Write(dir string, k Key, session string, now time.Time) error {
	if e := api.ValidateSession(session); e != nil {
		return e
	}
	d, err := sessionsDir(dir, true)
	if err != nil {
		return err
	}
	b, err := json.Marshal(file{V: 1, Session: session, Project: k.clean(), PID: k.PID, Written: now.UTC()})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(d, ".tmp-*") // 0600
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename
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
	if err := os.Chtimes(tmp, now, now); err != nil { // age is measured against the caller's clock
		return err
	}
	if err := os.Rename(tmp, filepath.Join(d, k.name())); err != nil {
		return err
	}
	prune(d, now)
	return nil
}

// prune removes files older than maxAge, scanning at most maxScanned entries. Best effort.
func prune(d string, now time.Time) {
	ents, err := os.ReadDir(d)
	if err != nil {
		return
	}
	if len(ents) > maxScanned {
		ents = ents[:maxScanned]
	}
	for _, e := range ents {
		if !e.Type().IsRegular() {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(fi.ModTime()) > maxAge {
			_ = os.Remove(filepath.Join(d, e.Name()))
		}
	}
}

// Read returns the session stored for k. A missing, mismatched or invalid file is not found
// (ok=false, nil error); an unsafe directory, oversize or unparsable file is an error.
func Read(dir string, k Key) (string, bool, error) {
	d, err := sessionsDir(dir, false)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	f, err := os.Open(filepath.Join(d, k.name()))
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer f.Close()
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

// RemoveIf deletes the handoff for k only when it stores session, so a SessionEnd for an old
// session cannot remove a newer session's handoff. A missing or different-session file is left
// alone (removed=false, nil error). It reads, compares, then removes: a Write by another process
// in that small window can still be removed. The window is microseconds and the cost is a lost
// reattach (the MCP server falls back to a per-process session), so no file locking is used.
func RemoveIf(dir string, k Key, session string) (removed bool, err error) {
	s, ok, err := Read(dir, k) // same symlink/dir checks as every other entry point
	if err != nil || !ok || s != session {
		return false, err
	}
	d, err := sessionsDir(dir, false)
	if err != nil {
		return false, err
	}
	if err := os.Remove(filepath.Join(d, k.name())); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// Remove deletes the handoff for k; a missing file is not an error.
func Remove(dir string, k Key) error {
	d, err := sessionsDir(dir, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(d, k.name())); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
