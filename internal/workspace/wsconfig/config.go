// Package wsconfig parses and validates the runner's workspace.yaml (docs/M3.md section 1.11).
// It is the workspace-mode security boundary on the runner: every rule fails closed, and there
// is deliberately no field for privileged containers, unconfined profiles or host mounts.
//
// Parse checks everything that can be checked without touching the disk. Load additionally
// checks file ownership and modes (containerd root, OpenSandbox API key).
package wsconfig

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
)

// Executor names.
const (
	ExecContainerd  = "containerd"
	ExecOpenSandbox = "opensandbox"
)

// Hard ceilings. Config values above these are refused.
const (
	MaxConcurrent  = 1024
	MaxExecMax     = 24 * time.Hour
	DefaultExecMax = time.Hour

	// MaxSecureRuntimeLen matches the heartbeat's name limit (transport.Heartbeat.Validate).
	MaxSecureRuntimeLen = 64
	// Heartbeat image limits (mirror transport.MaxHeartbeatImages / MaxHeartbeatImageBytes); images
	// beyond them are not advertised to central (see HeartbeatImagesDropped).
	heartbeatImages     = 32
	heartbeatImageBytes = 256

	maxCPUs     = 1024
	maxMemory   = int64(4) << 40 // 4 TiB
	maxPids     = 4 << 20
	maxTmpfs    = int64(1) << 40
	maxDisk     = int64(64) << 40
	maxFile     = int64(64) << 30
	maxMaxAge   = 365 * 24 * time.Hour
	maxEnvAllow = 256
	maxImages   = 1024

	userNSStride = 65536
	minUIDBase   = 65536
)

// Bytes is a size such as 8GiB, 512MiB or a plain byte count.
type Bytes int64

var sizeRE = regexp.MustCompile(`^([0-9]+)\s*(B|KiB|MiB|GiB|TiB)?$`)

// UnmarshalYAML parses "1GiB", "512MiB", "4096". Only binary units are accepted.
func (b *Bytes) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return errors.New("size must be a scalar")
	}
	m := sizeRE.FindStringSubmatch(strings.TrimSpace(n.Value))
	if m == nil {
		return fmt.Errorf("invalid size %q (use e.g. 512MiB, 8GiB)", n.Value)
	}
	v, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid size %q", n.Value)
	}
	mult := map[string]int64{"": 1, "B": 1, "KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40}[m[2]]
	if v > math.MaxInt64/mult {
		return fmt.Errorf("size %q overflows", n.Value)
	}
	*b = Bytes(v * mult)
	return nil
}

// Limits are the runner's caps; a fleet request above any of them is refused.
type Limits struct {
	CPUs    float64       `yaml:"cpus"`
	Memory  Bytes         `yaml:"memory"`
	Pids    int64         `yaml:"pids"`
	Tmpfs   Bytes         `yaml:"tmpfs"`
	Disk    Bytes         `yaml:"disk"`
	MaxFile Bytes         `yaml:"max_file"`
	ExecMax time.Duration `yaml:"exec_max"` // hard cap on one exec; <= 24h, default 1h
}

// Containerd configures the containerd executor.
type Containerd struct {
	Address       string `yaml:"address"`
	Namespace     string `yaml:"namespace"`
	Snapshotter   string `yaml:"snapshotter"`
	Runtime       string `yaml:"runtime"`
	SecureRuntime string `yaml:"secure_runtime"`
	Root          string `yaml:"root"`
	UserNS        string `yaml:"userns"`   // required (default) | off
	UIDBase       int64  `yaml:"uid_base"` // default 1000000
	AppArmor      string `yaml:"apparmor"` // auto (default) | required | off
}

// OpenSandbox configures the OpenSandbox executor. It is reached over mTLS on loopback only;
// the TLS front (Front) may be mirafleet's own or a third-party proxy, with identical rules.
type OpenSandbox struct {
	Server        string `yaml:"server"`
	Front         string `yaml:"front"` // builtin (default) | external
	CAFile        string `yaml:"ca_file"`
	PinSHA256     string `yaml:"pin_sha256"`
	ClientCert    string `yaml:"client_cert"`
	ClientKey     string `yaml:"client_key"`
	APIKeyFile    string `yaml:"api_key_file"`
	SecureRuntime string `yaml:"secure_runtime"`
}

// Config is the parsed workspace.yaml.
type Config struct {
	Executor   string        `yaml:"executor"`
	Concurrent int           `yaml:"concurrent"`
	MaxAge     time.Duration `yaml:"max_age"`
	Images     []string      `yaml:"images"`
	Limits     Limits        `yaml:"limits"`
	Network    []string      `yaml:"network"`
	EnvAllow   []string      `yaml:"env_allow"`
	// EgressCIDRRules lets central ask for CIDR or IP-literal egress rules (default false: only
	// hostname rules are accepted from central).
	EgressCIDRRules bool `yaml:"egress_cidr_rules"`

	Containerd  *Containerd  `yaml:"containerd"`
	OpenSandbox *OpenSandbox `yaml:"opensandbox"`
}

var envNameRE = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)
var pinRE = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// Parse decodes and validates b without touching the filesystem. Unknown fields are errors.
func Parse(b []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("workspace config is empty")
		}
		return nil, fmt.Errorf("workspace config: %w", err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("workspace config: multiple YAML documents")
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("workspace config: %w", err)
	}
	return &c, nil
}

// Load reads path, parses it, then checks file ownership and modes on disk.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("workspace config: %w", err)
	}
	c, err := Parse(b)
	if err != nil {
		return nil, err
	}
	if err := c.CheckFiles(); err != nil {
		return nil, fmt.Errorf("workspace config: %w", err)
	}
	return c, nil
}

// AcceptsGuarded reports whether the selected executor has a secure runtime (gVisor/Kata)
// configured, so the runner may serve fleets that require one.
func (c *Config) AcceptsGuarded() bool {
	switch c.Executor {
	case ExecContainerd:
		return c.Containerd != nil && c.Containerd.SecureRuntime != ""
	case ExecOpenSandbox:
		return c.OpenSandbox != nil && c.OpenSandbox.SecureRuntime != ""
	}
	return false
}

// HeartbeatImagesDropped returns the allowlist patterns that cannot be advertised in a heartbeat
// (more than 32 entries, or longer than 256 bytes). They still apply on the runner; central just
// does not see them. Callers should warn.
func (c *Config) HeartbeatImagesDropped() []string {
	var out []string
	n := 0
	for _, im := range c.Images {
		if n >= heartbeatImages || len(im) > heartbeatImageBytes {
			out = append(out, im)
			continue
		}
		n++
	}
	return out
}

// AllowsNetwork reports whether mode ("none" or "egress") is permitted.
func (c *Config) AllowsNetwork(mode string) bool {
	for _, m := range c.Network {
		if m == mode {
			return true
		}
	}
	return false
}

// AllowsImage reports whether ref matches the image allowlist.
func (c *Config) AllowsImage(ref string) bool { return workspace.MatchImage(c.Images, ref) }

// AllowsEnv reports whether name is on the env allowlist (it is never on the denylist).
func (c *Config) AllowsEnv(name string) bool {
	for _, e := range c.EnvAllow {
		if e == name {
			return !workspace.EnvDenied(name)
		}
	}
	return false
}

func (c *Config) validate() error {
	switch c.Executor {
	case ExecContainerd, ExecOpenSandbox:
	default:
		return fmt.Errorf("executor %q: must be %q or %q", c.Executor, ExecContainerd, ExecOpenSandbox)
	}
	if c.Concurrent < 1 || c.Concurrent > MaxConcurrent {
		return fmt.Errorf("concurrent %d: must be 1..%d", c.Concurrent, MaxConcurrent)
	}
	if c.MaxAge < 0 || c.MaxAge > maxMaxAge {
		return fmt.Errorf("max_age %s: must be 0..%s", c.MaxAge, maxMaxAge)
	}
	if err := c.validateImages(); err != nil {
		return err
	}
	if err := c.validateLimits(); err != nil {
		return err
	}
	if len(c.Network) == 0 {
		return errors.New("network: list at least one of none, egress")
	}
	seen := map[string]bool{}
	for _, m := range c.Network {
		if m != "none" && m != "egress" {
			return fmt.Errorf("network %q: must be none or egress", m)
		}
		if seen[m] {
			return fmt.Errorf("network %q listed twice", m)
		}
		seen[m] = true
	}
	if len(c.EnvAllow) > maxEnvAllow {
		return fmt.Errorf("env_allow: more than %d names", maxEnvAllow)
	}
	for _, e := range c.EnvAllow {
		if !envNameRE.MatchString(e) {
			return fmt.Errorf("env_allow %q: invalid variable name", e)
		}
		if workspace.EnvDenied(e) {
			return fmt.Errorf("env_allow %q: name is on the hard denylist and cannot be allowed", e)
		}
	}
	switch c.Executor {
	case ExecContainerd:
		if c.OpenSandbox != nil {
			return errors.New("opensandbox block present but executor is containerd")
		}
		if c.Containerd == nil {
			return errors.New("containerd block required for executor containerd")
		}
		return c.Containerd.validate()
	default:
		if c.Containerd != nil {
			return errors.New("containerd block present but executor is opensandbox")
		}
		if c.OpenSandbox == nil {
			return errors.New("opensandbox block required for executor opensandbox")
		}
		return c.OpenSandbox.validate()
	}
}

func (c *Config) validateImages() error {
	if len(c.Images) == 0 {
		return errors.New("images: allowlist must not be empty")
	}
	if len(c.Images) > maxImages {
		return fmt.Errorf("images: more than %d entries", maxImages)
	}
	for _, p := range c.Images {
		if err := validImagePattern(p); err != nil {
			return fmt.Errorf("images %q: %w", p, err)
		}
	}
	return nil
}

// validImagePattern accepts exact references, "repo:*" and "repo@sha256:*" only, with
// MatchImage semantics. It probes MatchImage so the grammar stays in one place.
func validImagePattern(p string) error {
	switch {
	case strings.HasSuffix(p, "@sha256:*"):
		probe := strings.TrimSuffix(p, "*") + strings.Repeat("0", 64)
		if !workspace.MatchImage([]string{p}, probe) {
			return errors.New("invalid pattern")
		}
	case strings.HasSuffix(p, ":*"):
		probe := strings.TrimSuffix(p, "*") + "x"
		if !workspace.MatchImage([]string{p}, probe) {
			return errors.New("invalid pattern")
		}
	case strings.Contains(p, "*"):
		return errors.New("wildcards are only allowed as ':*' or '@sha256:*' suffix")
	default:
		if !workspace.MatchImage([]string{p}, p) {
			return errors.New("invalid image reference")
		}
	}
	return nil
}

func (c *Config) validateLimits() error {
	l := &c.Limits
	if !(l.CPUs > 0) || l.CPUs > maxCPUs || math.IsNaN(l.CPUs) {
		return fmt.Errorf("limits.cpus %v: must be >0 and <= %d", l.CPUs, maxCPUs)
	}
	for _, x := range []struct {
		name string
		v    int64
		max  int64
	}{
		{"memory", int64(l.Memory), maxMemory},
		{"pids", l.Pids, maxPids},
		{"tmpfs", int64(l.Tmpfs), maxTmpfs},
		{"disk", int64(l.Disk), maxDisk},
		{"max_file", int64(l.MaxFile), maxFile},
	} {
		if x.v <= 0 || x.v > x.max {
			return fmt.Errorf("limits.%s %d: must be >0 and <= %d", x.name, x.v, x.max)
		}
	}
	if l.ExecMax == 0 {
		l.ExecMax = DefaultExecMax
	}
	if l.ExecMax < 0 || l.ExecMax > MaxExecMax {
		return fmt.Errorf("limits.exec_max %s: must be >0 and <= %s", l.ExecMax, MaxExecMax)
	}
	return nil
}

func (k *Containerd) validate() error {
	if k.Address == "" {
		k.Address = "/run/containerd/containerd.sock"
	}
	if !filepath.IsAbs(k.Address) {
		return fmt.Errorf("containerd.address %q: must be an absolute path", k.Address)
	}
	switch k.Namespace {
	case "":
		return errors.New("containerd.namespace: required (use \"mirafleet\")")
	case "k8s.io", "moby":
		return fmt.Errorf("containerd.namespace %q is refused: it belongs to another runtime", k.Namespace)
	}
	if !nsRE.MatchString(k.Namespace) {
		return fmt.Errorf("containerd.namespace %q: invalid", k.Namespace)
	}
	if k.Snapshotter == "" {
		k.Snapshotter = "overlayfs"
	}
	if k.Runtime == "" {
		k.Runtime = "io.containerd.runc.v2"
	}
	if !tokRE.MatchString(k.Snapshotter) {
		return fmt.Errorf("containerd.snapshotter %q: invalid", k.Snapshotter)
	}
	if !tokRE.MatchString(k.Runtime) {
		return fmt.Errorf("containerd.runtime %q: invalid", k.Runtime)
	}
	if k.SecureRuntime != "" && (!tokRE.MatchString(k.SecureRuntime) || len(k.SecureRuntime) > MaxSecureRuntimeLen) {
		return fmt.Errorf("containerd.secure_runtime %q: invalid (at most %d characters)", k.SecureRuntime, MaxSecureRuntimeLen)
	}
	if !filepath.IsAbs(k.Root) || filepath.Clean(k.Root) != k.Root || k.Root == "/" {
		return fmt.Errorf("containerd.root %q: must be a clean absolute path other than /", k.Root)
	}
	switch k.UserNS {
	case "":
		k.UserNS = "required"
	case "required", "off":
	default:
		return fmt.Errorf("containerd.userns %q: must be required or off", k.UserNS)
	}
	switch k.AppArmor {
	case "":
		k.AppArmor = "auto"
	case "auto", "required", "off":
	default:
		return fmt.Errorf("containerd.apparmor %q: must be auto, required or off", k.AppArmor)
	}
	if k.UIDBase == 0 {
		k.UIDBase = 1000000
	}
	// Slots use uid_base + slot*65536, slot < MaxConcurrent; stay out of real users and in uint32.
	if k.UIDBase < minUIDBase || k.UIDBase+int64(MaxConcurrent+1)*userNSStride > math.MaxUint32 {
		return fmt.Errorf("containerd.uid_base %d: must be >= %d and leave room for %d slots", k.UIDBase, minUIDBase, MaxConcurrent)
	}
	return nil
}

var (
	nsRE  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)
	tokRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.+-]{0,127}$`)
)

func (o *OpenSandbox) validate() error {
	u, err := url.Parse(o.Server)
	if err != nil || o.Server == "" {
		return fmt.Errorf("opensandbox.server %q: invalid URL", o.Server)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("opensandbox.server %q: scheme must be https (plain http is refused)", o.Server)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("opensandbox.server %q: no credentials, query or fragment allowed", o.Server)
	}
	if u.Path != "" && u.Path != "/" || u.RawPath != "" {
		return fmt.Errorf("opensandbox.server %q: no path allowed (use scheme://host:port)", o.Server)
	}
	// The port is explicit (no implied 443): the server is a local loopback service on a
	// site-chosen port, so a missing port is almost certainly a typo for the front's port.
	port := u.Port()
	if n, err := strconv.Atoi(port); port == "" || err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("opensandbox.server %q: an explicit port in 1..65535 is required (use scheme://host:port)", o.Server)
	}
	switch u.Hostname() {
	case "127.0.0.1", "::1", "localhost":
	default:
		return fmt.Errorf("opensandbox.server host %q: must be loopback (127.0.0.1, ::1 or localhost)", u.Hostname())
	}
	switch o.Front {
	case "":
		o.Front = "builtin"
	case "builtin", "external":
	default:
		return fmt.Errorf("opensandbox.front %q: must be builtin or external", o.Front)
	}
	if o.CAFile == "" {
		return errors.New("opensandbox.ca_file: required (pin-only trust is not supported; pin_sha256 is an optional extra leaf check)")
	}
	if o.PinSHA256 != "" && !pinRE.MatchString(o.PinSHA256) {
		return errors.New("opensandbox.pin_sha256: must be 64 hex characters")
	}
	for _, f := range []struct{ name, v string }{
		{"ca_file", o.CAFile}, {"client_cert", o.ClientCert},
		{"client_key", o.ClientKey}, {"api_key_file", o.APIKeyFile},
	} {
		if f.v == "" {
			return fmt.Errorf("opensandbox.%s: required (mTLS client cert, key and API key file are all mandatory)", f.name)
		}
		if !filepath.IsAbs(f.v) {
			return fmt.Errorf("opensandbox.%s %q: must be an absolute path", f.name, f.v)
		}
	}
	if o.SecureRuntime != "" && (!tokRE.MatchString(o.SecureRuntime) || len(o.SecureRuntime) > MaxSecureRuntimeLen) {
		return fmt.Errorf("opensandbox.secure_runtime %q: invalid (at most %d characters)", o.SecureRuntime, MaxSecureRuntimeLen)
	}
	return nil
}

// ---- on-disk checks (Load only) ----

// FileStat is the subset of stat the checks need.
type FileStat struct {
	Mode  os.FileMode
	UID   uint32
	IsDir bool
}

// statFn and getuid are swapped by tests.
var (
	statFn = osStat
	getuid = os.Getuid
)

func osStat(path string) (FileStat, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return FileStat{}, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return FileStat{}, errors.New("is a symlink")
	}
	// Read Uid reflectively so this builds on platforms without syscall.Stat_t; there the
	// field is missing and the check fails closed.
	sys := reflect.ValueOf(fi.Sys())
	if sys.Kind() == reflect.Pointer {
		sys = sys.Elem()
	}
	if !sys.IsValid() || sys.Kind() != reflect.Struct {
		return FileStat{}, errors.New("cannot determine file owner on this platform")
	}
	f := sys.FieldByName("Uid")
	if !f.IsValid() || !f.CanUint() {
		return FileStat{}, errors.New("cannot determine file owner on this platform")
	}
	return FileStat{Mode: fi.Mode(), UID: uint32(f.Uint()), IsDir: fi.IsDir()}, nil
}

// CheckFiles verifies ownership and modes: the containerd root must be a directory owned by
// root with mode 0700; the OpenSandbox API key file and mTLS client key must be regular files
// with mode 0600 or 0400 owned by the runner's uid.
func (c *Config) CheckFiles() error {
	switch c.Executor {
	case ExecContainerd:
		st, err := statFn(c.Containerd.Root)
		if err != nil {
			return fmt.Errorf("containerd.root %q: %w", c.Containerd.Root, err)
		}
		if !st.IsDir {
			return fmt.Errorf("containerd.root %q: not a directory", c.Containerd.Root)
		}
		if st.UID != 0 {
			return fmt.Errorf("containerd.root %q: must be owned by root (uid 0), is uid %d", c.Containerd.Root, st.UID)
		}
		if st.Mode.Perm() != 0o700 || st.Mode&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			return fmt.Errorf("containerd.root %q: mode must be 0700, is %04o", c.Containerd.Root, st.Mode.Perm())
		}
	case ExecOpenSandbox:
		if err := checkSecretFile("api_key_file", c.OpenSandbox.APIKeyFile); err != nil {
			return err
		}
		return checkSecretFile("client_key", c.OpenSandbox.ClientKey)
	}
	return nil
}

// checkSecretFile requires a regular file, mode 0600 or 0400, owned by the runner's uid.
func checkSecretFile(name, p string) error {
	st, err := statFn(p)
	if err != nil {
		return fmt.Errorf("opensandbox.%s %q: %w", name, p, err)
	}
	if st.IsDir || !st.Mode.IsRegular() {
		return fmt.Errorf("opensandbox.%s %q: not a regular file", name, p)
	}
	if err := checkParentDir(name, p); err != nil {
		return err
	}
	if m := st.Mode.Perm(); m != 0o600 && m != 0o400 {
		return fmt.Errorf("opensandbox.%s %q: mode must be 0600 or 0400, is %04o", name, p, m)
	}
	if uid := getuid(); uid < 0 || st.UID != uint32(uid) {
		return fmt.Errorf("opensandbox.%s %q: must be owned by the runner user (uid %d), is uid %d", name, p, uid, st.UID)
	}
	return nil
}

// checkParentDir requires the directory holding a secret file to be owned by root or the
// runner's uid and not group- or world-writable: otherwise another local user could swap the
// file between Load and the later read. It fails closed when ownership cannot be determined.
func checkParentDir(name, p string) error {
	dir := filepath.Dir(p)
	st, err := statFn(dir)
	if err != nil {
		return fmt.Errorf("opensandbox.%s %q: parent directory %q: %w", name, p, dir, err)
	}
	if !st.IsDir {
		return fmt.Errorf("opensandbox.%s %q: parent %q is not a directory", name, p, dir)
	}
	if uid := getuid(); st.UID != 0 && (uid < 0 || st.UID != uint32(uid)) {
		return fmt.Errorf("opensandbox.%s %q: parent directory %q must be owned by root or the runner user (uid %d), is uid %d", name, p, dir, uid, st.UID)
	}
	if st.Mode.Perm()&0o022 != 0 {
		return fmt.Errorf("opensandbox.%s %q: parent directory %q must not be group- or world-writable (mode %04o)", name, p, dir, st.Mode.Perm())
	}
	return nil
}
