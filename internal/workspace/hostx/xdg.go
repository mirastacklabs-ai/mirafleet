//go:build linux

// Package hostx runs a dev workspace directly on the runner host as the runner's own user.
// This file group holds the path defaults, the session directories, the file tools and the
// cleanup sweep.
package hostx

import (
	"errors"
	"os"
	"path/filepath"
)

// Paths are the runner user's per-user directories (XDG base directory spec).
type Paths struct {
	Config string // $XDG_CONFIG_HOME/mirafleet (default ~/.config/mirafleet): workspace.yaml, runner key
	State  string // $XDG_STATE_HOME/mirafleet (default ~/.local/state/mirafleet): session metadata
	Data   string // $XDG_DATA_HOME/mirafleet (default ~/.local/share/mirafleet): session dirs
}

// DefaultPaths resolves the directories from the process environment.
func DefaultPaths() (Paths, error) {
	home, _ := os.UserHomeDir()
	return PathsFrom(os.Getenv, home)
}

// PathsFrom resolves the directories from getenv and the home directory. As the spec says, an
// XDG variable that is empty or not absolute is ignored.
func PathsFrom(getenv func(string) string, home string) (Paths, error) {
	pick := func(env, fallback string) (string, error) {
		if v := getenv(env); v != "" && filepath.IsAbs(v) {
			return filepath.Join(filepath.Clean(v), "mirafleet"), nil
		}
		if home == "" || !filepath.IsAbs(home) {
			return "", errors.New("hostx: no absolute home directory and " + env + " is not set")
		}
		return filepath.Join(home, fallback, "mirafleet"), nil
	}
	var p Paths
	var err error
	if p.Config, err = pick("XDG_CONFIG_HOME", ".config"); err != nil {
		return Paths{}, err
	}
	if p.State, err = pick("XDG_STATE_HOME", filepath.Join(".local", "state")); err != nil {
		return Paths{}, err
	}
	if p.Data, err = pick("XDG_DATA_HOME", filepath.Join(".local", "share")); err != nil {
		return Paths{}, err
	}
	return p, nil
}

// SessionsRoot is the default parent of the per-session directories.
func (p Paths) SessionsRoot() string { return filepath.Join(p.Data, "work") }

// Ensure creates the three directories with mode 0700 (and tightens existing ones).
func (p Paths) Ensure() error {
	for _, d := range []string{p.Config, p.State, p.Data} {
		if err := ensureDir(d); err != nil {
			return err
		}
	}
	return nil
}

// ensureDir creates dir (and missing parents; the last element is forced to 0700).
func ensureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return errors.New("hostx: " + dir + " is not a directory")
	}
	if fi.Mode().Perm() != 0o700 {
		return os.Chmod(dir, 0o700)
	}
	return nil
}
