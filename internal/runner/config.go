package runner

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/mirastacklabs-ai/mirafleet/internal/keys"
)

// File names inside the runner data directory.
const (
	ConfigName     = "runner.yaml"
	KeyName        = "runner_ed25519"
	KnownHostsName = "known_hosts"
)

// ErrNotJoined means the data directory has no runner.yaml.
var ErrNotJoined = errors.New("runner is not joined: run `mirafleet runner join` first")

// ErrAlreadyJoined means the data directory already holds a runner.yaml.
var ErrAlreadyJoined = errors.New("this data directory has already joined a central (runner.yaml exists); use a fresh -data directory")

// FileConfig is the contents of <data>/runner.yaml, written by `runner join`.
type FileConfig struct {
	Central string `yaml:"central"` // host:port
	Name    string `yaml:"name"`
	Fleet   string `yaml:"fleet"`
}

// Validate checks the fields.
func (c *FileConfig) Validate() error {
	if _, _, err := net.SplitHostPort(c.Central); err != nil {
		return fmt.Errorf("central %q: %w", c.Central, err)
	}
	if !keys.RunnerNameRE.MatchString(c.Name) {
		return fmt.Errorf("invalid runner name %q", c.Name)
	}
	if c.Fleet == "" {
		return errors.New("fleet required")
	}
	return nil
}

// ConfigPath, KeyPath and KnownHostsPath locate the runner's files in dir.
func ConfigPath(dir string) string     { return filepath.Join(dir, ConfigName) }
func KeyPath(dir string) string        { return filepath.Join(dir, KeyName) }
func KnownHostsPath(dir string) string { return filepath.Join(dir, KnownHostsName) }

// LoadConfig reads and validates <dir>/runner.yaml. Unknown fields are an error. A missing file
// is ErrNotJoined.
func LoadConfig(dir string) (*FileConfig, error) {
	b, err := os.ReadFile(ConfigPath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotJoined
	}
	if err != nil {
		return nil, err
	}
	var c FileConfig
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse %s: %w", ConfigPath(dir), err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", ConfigPath(dir), err)
	}
	return &c, nil
}

// SaveConfig validates and writes <dir>/runner.yaml (0600) atomically.
func SaveConfig(dir string, c *FileConfig) error {
	if err := c.Validate(); err != nil {
		return err
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return writeAtomic(dir, ConfigName, b)
}

// writeAtomic writes dir/name (0600) via a temp file in dir, fsync and rename.
func writeAtomic(dir, name string, data []byte) error {
	f, err := os.CreateTemp(dir, ".tmp-*") // 0600
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
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
	return os.Rename(tmp, filepath.Join(dir, name))
}
