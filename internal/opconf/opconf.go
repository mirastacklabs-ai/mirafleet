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
	"strings"

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
	return platformConfigDir()
}

// EnvStateDir overrides the state directory (tests). It must be an absolute path.
const EnvStateDir = "MIRAFLEET_STATE_DIR"

// StateDir returns the per-user state directory (session handoff files): $MIRAFLEET_STATE_DIR,
// else linux os.UserCacheDir()/mirafleet, darwin ~/Library/Caches/mirafleet, windows
// %LocalAppData%\mirafleet\state. It does not create it.
func StateDir() (string, error) {
	if d := os.Getenv(EnvStateDir); d != "" {
		if !filepath.IsAbs(d) {
			return "", fmt.Errorf("%s must be an absolute path", EnvStateDir)
		}
		return filepath.Clean(d), nil
	}
	return platformStateDir()
}

// windowsConfigDir is the Windows config dir computed from env: %LocalAppData%\mirafleet, never
// Roaming (it would copy the key to a profile server). Pure so it is testable on any OS.
func windowsConfigDir(getenv func(string) string) (string, error) {
	base := getenv("LocalAppData")
	if base == "" {
		return "", errors.New("%LocalAppData% is not set")
	}
	if !isWinAbs(base) {
		return "", errors.New("%LocalAppData% must be an absolute path")
	}
	return strings.TrimRight(base, `\/`) + `\mirafleet`, nil
}

// windowsStateDir is %LocalAppData%\mirafleet\state.
func windowsStateDir(getenv func(string) string) (string, error) {
	d, err := windowsConfigDir(getenv)
	if err != nil {
		return "", err
	}
	return d + `\state`, nil
}

// isWinAbs accepts drive-letter (C:\ or C:/) and UNC (\\host) paths.
func isWinAbs(p string) bool {
	if len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/') &&
		(p[0] >= 'A' && p[0] <= 'Z' || p[0] >= 'a' && p[0] <= 'z') {
		return true
	}
	return strings.HasPrefix(p, `\\`)
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
	// DefaultFleet is used when neither a tool argument, a flag nor .mirafleet.yaml names a fleet.
	DefaultFleet string `yaml:"default_fleet,omitempty"`
}

// Output cap bounds and default (bytes).
const (
	MinOutputCap     = 4096
	MaxOutputCap     = 262144
	DefaultOutputCap = 30720
)

// Contexts is the contents of contexts.yaml.
type Contexts struct {
	Current  string    `yaml:"current,omitempty"`
	Contexts []Context `yaml:"contexts"`
	// OutputCapBytes caps tool output; 0 (absent) means DefaultOutputCap.
	OutputCapBytes int `yaml:"output_cap_bytes,omitempty"`
}

// OutputCap returns the effective output cap in bytes.
func (c *Contexts) OutputCap() int {
	if c.OutputCapBytes == 0 {
		return DefaultOutputCap
	}
	return c.OutputCapBytes
}

// Find returns the named context.
func (c *Contexts) Find(name string) (Context, bool) {
	for _, x := range c.Contexts {
		if x.Name == name {
			return x, true
		}
	}
	return Context{}, false
}

// Names lists context names in file order.
func (c *Contexts) Names() []string {
	out := make([]string, 0, len(c.Contexts))
	for _, x := range c.Contexts {
		out = append(out, x.Name)
	}
	return out
}

// Use sets the current context; it must exist.
func (c *Contexts) Use(name string) error {
	if _, ok := c.Find(name); !ok {
		return fmt.Errorf("no such context %q", name)
	}
	c.Current = name
	return nil
}

// Remove deletes a context, clearing Current if it pointed at it.
func (c *Contexts) Remove(name string) error {
	for i, x := range c.Contexts {
		if x.Name == name {
			c.Contexts = append(c.Contexts[:i:i], c.Contexts[i+1:]...)
			if c.Current == name {
				c.Current = ""
			}
			return nil
		}
	}
	return fmt.Errorf("no such context %q", name)
}

// UseContext loads contexts.yaml from dir, sets the current context and saves atomically.
func UseContext(dir, name string) error {
	c, err := LoadContexts(dir)
	if err != nil {
		return err
	}
	if err := c.Use(name); err != nil {
		return err
	}
	return SaveContexts(dir, c)
}

// RemoveContext loads contexts.yaml from dir, removes the context and saves atomically.
func RemoveContext(dir, name string) error {
	c, err := LoadContexts(dir)
	if err != nil {
		return err
	}
	if err := c.Remove(name); err != nil {
		return err
	}
	return SaveContexts(dir, c)
}

// nameRE is the context and fleet name grammar. It matches store.FleetNameRE; opconf cannot import
// store (SQLite) on the orchestrator side.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// errJump is returned when a contexts file carries a jump key.
var errJump = errors.New("SSH jump hosts are not supported yet (remove the \"jump\" key)")

// rejectJump walks the YAML tree and fails on any mapping key named "jump".
func rejectJump(n *yaml.Node) error {
	if n == nil {
		return nil
	}
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == "jump" {
				return errJump
			}
		}
	}
	for _, ch := range n.Content {
		if err := rejectJump(ch); err != nil {
			return err
		}
	}
	return nil
}

// Validate checks names, addresses, default fleets, the output cap and that Current, if set,
// exists.
func (c *Contexts) Validate() error {
	if c.OutputCapBytes != 0 && (c.OutputCapBytes < MinOutputCap || c.OutputCapBytes > MaxOutputCap) {
		return fmt.Errorf("output_cap_bytes %d out of range %d..%d", c.OutputCapBytes, MinOutputCap, MaxOutputCap)
	}
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
		if x.DefaultFleet != "" && !nameRE.MatchString(x.DefaultFleet) {
			return fmt.Errorf("context %q: invalid default_fleet %q", x.Name, x.DefaultFleet)
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
	var root yaml.Node
	if err := yaml.Unmarshal(b, &root); err == nil {
		if err := rejectJump(&root); err != nil {
			return nil, fmt.Errorf("%s: %w", ContextsPath(dir), err)
		}
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
