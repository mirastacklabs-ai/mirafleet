// Package opconf locates and reads the operator's local configuration: the key pair, known_hosts
// and a minimal contexts file (central address + name) used from M5. Cross-platform.
package opconf

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"gopkg.in/yaml.v3"

	"github.com/mirastacklabs-ai/mirafleet/internal/keys"
)

// EnvDir overrides the config directory (tests, unusual setups). It must be an absolute path.
const EnvDir = "MIRAFLEET_CONFIG_DIR"

// File names inside the directory.
const (
	KeyName        = "operator_ed25519"
	KnownHostsName = "known_hosts"
	ContextsName   = "contexts.yaml"
)

// Dir returns the operator config directory: $MIRAFLEET_CONFIG_DIR, else
// os.UserConfigDir()/mirafleet. It does not create it.
func Dir() (string, error) {
	if d := os.Getenv(EnvDir); d != "" {
		if !filepath.IsAbs(d) {
			return "", fmt.Errorf("%s must be an absolute path", EnvDir)
		}
		return filepath.Clean(d), nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user config dir: %w", err)
	}
	return filepath.Join(base, "mirafleet"), nil
}

// EnsureDir returns Dir after creating it with mode 0700 (tightened if it already exists).
func EnsureDir() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	if err := keys.EnsureDir(d); err != nil {
		return "", err
	}
	return d, nil
}

// KeyPath is the operator's private key (the public key is KeyPath + ".pub").
func KeyPath(dir string) string { return filepath.Join(dir, KeyName) }

// KnownHostsPath is the pinned central host keys file.
func KnownHostsPath(dir string) string { return filepath.Join(dir, KnownHostsName) }

// ContextsPath is the contexts file.
func ContextsPath(dir string) string { return filepath.Join(dir, ContextsName) }

// Context names one central the operator talks to.
type Context struct {
	Name string `yaml:"name"`
	Addr string `yaml:"addr"` // host:port
}

// Contexts is the contents of contexts.yaml.
type Contexts struct {
	Current  string    `yaml:"current,omitempty"`
	Contexts []Context `yaml:"contexts"`
}

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// Validate checks names and addresses and that Current, if set, exists.
func (c *Contexts) Validate() error {
	seen := map[string]bool{}
	for _, x := range c.Contexts {
		if !nameRE.MatchString(x.Name) {
			return fmt.Errorf("invalid context name %q", x.Name)
		}
		if seen[x.Name] {
			return fmt.Errorf("duplicate context %q", x.Name)
		}
		seen[x.Name] = true
		if x.Addr == "" {
			return fmt.Errorf("context %q: addr required", x.Name)
		}
	}
	if c.Current != "" && !seen[c.Current] {
		return fmt.Errorf("current context %q not defined", c.Current)
	}
	return nil
}

// LoadContexts reads contexts.yaml from dir. A missing file is an empty set. Unknown fields are
// an error.
func LoadContexts(dir string) (*Contexts, error) {
	b, err := os.ReadFile(ContextsPath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return &Contexts{}, nil
	}
	if err != nil {
		return nil, err
	}
	var c Contexts
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse %s: %w", ContextsPath(dir), err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", ContextsPath(dir), err)
	}
	return &c, nil
}

// SaveContexts validates and writes contexts.yaml (0600) via a temp file, fsync and rename. It
// creates dir (0700) if needed.
func SaveContexts(dir string, c *Contexts) error {
	if err := c.Validate(); err != nil {
		return err
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	if err := keys.EnsureDir(dir); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".contexts-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
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
	return os.Rename(tmp, ContextsPath(dir))
}
