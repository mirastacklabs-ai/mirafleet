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
		return "", errors.New("%LocalAppData% must be a local absolute path (drive letter; UNC paths are refused)")
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

// isWinAbs accepts drive-letter (C:\ or C:/) paths only. UNC paths (\\host\share) are refused:
// the operator key would live on a network share, where the local owner-only DACL check does not
// apply (F-10).
func isWinAbs(p string) bool {
	return len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/') &&
		(p[0] >= 'A' && p[0] <= 'Z' || p[0] >= 'a' && p[0] <= 'z')
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
	return UpdateContexts(dir, func(c *Contexts) error { return c.Use(name) })
}

// RemoveContext loads contexts.yaml from dir, removes the context and saves atomically.
func RemoveContext(dir, name string) error {
	return UpdateContexts(dir, func(c *Contexts) error { return c.Remove(name) })
}

// LockName is the advisory lock file next to contexts.yaml. It stays on disk (empty); the lock is
// held on an open file, so a leftover file never blocks anyone.
const LockName = ".contexts.lock"

// UpdateContexts is a locked load-modify-save of contexts.yaml (F-11): it takes an exclusive
// advisory lock (flock; LockFileEx on Windows) on LockName, loads the file (a missing one is
// empty), runs fn, and saves only if fn returns nil. Two concurrent updates (two `mcp login`
// runs, or login racing `mcp context use`) therefore cannot lose each other's change. The lock
// is advisory: it protects only against other callers of UpdateContexts / SaveContexts. A plain
// LoadContexts followed by SaveContexts has no such protection and can overwrite a concurrent
// change; use UpdateContexts for any read-modify-write. The lock is not re-entrant: fn must not
// call any opconf function that takes it (SaveContexts, UseContext, RemoveContext, UpdateContexts).
func UpdateContexts(dir string, fn func(*Contexts) error) error {
	if err := keys.EnsureDir(dir); err != nil {
		return err
	}
	lk, err := lockFile(filepath.Join(dir, LockName))
	if err != nil {
		return fmt.Errorf("lock %s: %w", ContextsPath(dir), err)
	}
	defer lk.Close()
	c, err := LoadContexts(dir)
	if err != nil {
		return err
	}
	if err := fn(c); err != nil {
		return err
	}
	return saveContexts(dir, c)
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
	if err := decodeSingle(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", ContextsPath(dir), err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", ContextsPath(dir), err)
	}
	return &c, nil
}

// errMultiDoc is returned when a YAML file carries more than one document (F-07).
var errMultiDoc = errors.New("more than one YAML document (remove the extra \"---\" section)")

// decodeSingle strictly decodes exactly one YAML document into out: unknown fields are an error,
// an empty input leaves out untouched, and a second document is refused rather than ignored.
func decodeSingle(b []byte, out any) error {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	var extra yaml.Node
	switch err := dec.Decode(&extra); {
	case err == nil:
		return errMultiDoc
	case !errors.Is(err, io.EOF):
		return err
	}
	return nil
}

// SaveContexts validates and writes contexts.yaml (0600; on Windows an explicit owner-only DACL)
// via a private temp file, fsync, rename and a directory fsync, under the contexts lock. It
// creates dir (0700) if needed. For a read-modify-write use UpdateContexts.
func SaveContexts(dir string, c *Contexts) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if err := keys.EnsureDir(dir); err != nil {
		return err
	}
	lk, err := lockFile(filepath.Join(dir, LockName))
	if err != nil {
		return fmt.Errorf("lock %s: %w", ContextsPath(dir), err)
	}
	defer lk.Close()
	return saveContexts(dir, c)
}

// saveContexts is SaveContexts for a caller that already holds the lock and has made dir.
func saveContexts(dir string, c *Contexts) error {
	if err := c.Validate(); err != nil {
		return err
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return keys.WriteAtomic0600(dir, ContextsName, b)
}
