package wsconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const base = `
executor: containerd
concurrent: 4
max_age: 168h
images: ["docker.io/library/golang@sha256:*", "ghcr.io/acme/dev:*", "alpine:3.20"]
limits: {cpus: 4, memory: 8GiB, pids: 2048, tmpfs: 1GiB, disk: 20GiB, max_file: 64MiB, exec_max: 2h}
network: [none, egress]
env_allow: [LANG, CI, GOFLAGS]
containerd:
  namespace: mirafleet
  root: /var/lib/mirafleet/ws
  secure_runtime: io.containerd.runsc.v1
`

const osBase = `
executor: opensandbox
concurrent: 2
images: ["alpine:3.20"]
limits: {cpus: 2, memory: 4GiB, pids: 512, tmpfs: 256MiB, disk: 5GiB, max_file: 16MiB}
network: [none]
opensandbox:
  server: "https://127.0.0.1:8443"
  ca_file: /etc/mirafleet/ca.pem
  client_cert: /etc/mirafleet/c.pem
  client_key: /etc/mirafleet/c.key
  api_key_file: /etc/mirafleet/opensandbox.key
  secure_runtime: gvisor
`

func TestParseValid(t *testing.T) {
	c, err := Parse([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	if c.Limits.Memory != 8<<30 || c.Limits.ExecMax != 2*time.Hour || c.MaxAge != 168*time.Hour {
		t.Fatalf("limits: %+v", c.Limits)
	}
	k := c.Containerd
	if k.UserNS != "required" || k.AppArmor != "auto" || k.Snapshotter != "overlayfs" || k.UIDBase != 1000000 {
		t.Fatalf("defaults: %+v", k)
	}
	if !c.AcceptsGuarded() || !c.AllowsNetwork("egress") || c.AllowsNetwork("host") {
		t.Fatal("accessors")
	}
	if !c.AllowsImage("alpine:3.20") || c.AllowsImage("alpine:3.19") || !c.AllowsImage("ghcr.io/acme/dev:x") {
		t.Fatal("images")
	}
	if !c.AllowsEnv("CI") || c.AllowsEnv("HOME") {
		t.Fatal("env")
	}
	o, err := Parse([]byte(osBase))
	if err != nil {
		t.Fatal(err)
	}
	if o.OpenSandbox.Front != "builtin" || o.Limits.ExecMax != DefaultExecMax || !o.AcceptsGuarded() {
		t.Fatalf("os: %+v %+v", o.OpenSandbox, o.Limits)
	}
}

func TestAcceptsGuardedFalse(t *testing.T) {
	c, err := Parse([]byte(strings.Replace(base, "  secure_runtime: io.containerd.runsc.v1\n", "", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if c.AcceptsGuarded() {
		t.Fatal("no secure runtime must not accept guarded")
	}
}

func rep(src, old, new string) string {
	if !strings.Contains(src, old) {
		panic("test bug: " + old)
	}
	return strings.Replace(src, old, new, 1)
}

func TestParseRejects(t *testing.T) {
	cases := []struct{ name, yaml, want string }{
		{"empty", "", "empty"},
		{"unknown field", base + "privileged: true\n", "privileged"},
		{"unknown nested", rep(base, "namespace: mirafleet", "namespace: mirafleet\n  unconfined: true"), "unconfined"},
		{"two docs", base + "---\nexecutor: containerd\n", "multiple"},
		{"bad executor", rep(base, "executor: containerd", "executor: docker"), "executor"},
		{"no executor", rep(base, "executor: containerd\n", ""), "executor"},
		{"concurrent 0", rep(base, "concurrent: 4", "concurrent: 0"), "concurrent"},
		{"concurrent 1025", rep(base, "concurrent: 4", "concurrent: 1025"), "concurrent"},
		{"negative max_age", rep(base, "max_age: 168h", "max_age: -1h"), "max_age"},
		{"images empty", rep(base, `images: ["docker.io/library/golang@sha256:*", "ghcr.io/acme/dev:*", "alpine:3.20"]`, "images: []"), "images"},
		{"images missing", rep(base, `images: ["docker.io/library/golang@sha256:*", "ghcr.io/acme/dev:*", "alpine:3.20"]`+"\n", ""), "images"},
		{"image bare star", rep(base, `"alpine:3.20"`, `"*"`), "images"},
		{"image mid wildcard", rep(base, `"alpine:3.20"`, `"al*/x:1"`), "wildcard"},
		{"image tag wildcard", rep(base, `"alpine:3.20"`, `"alpine:3.*"`), "images"},
		{"image invalid", rep(base, `"alpine:3.20"`, `"Not A Ref"`), "images"},
		{"image digest wildcard bad repo", rep(base, `"alpine:3.20"`, `"@sha256:*"`), "images"},
		{"cpus 0", rep(base, "cpus: 4", "cpus: 0"), "cpus"},
		{"cpus negative", rep(base, "cpus: 4", "cpus: -1"), "cpus"},
		{"cpus huge", rep(base, "cpus: 4", "cpus: 100000"), "cpus"},
		{"memory 0", rep(base, "memory: 8GiB", "memory: 0"), "memory"},
		{"memory huge", rep(base, "memory: 8GiB", "memory: 99999TiB"), "memory"},
		{"memory bad unit", rep(base, "memory: 8GiB", "memory: 8GB"), "size"},
		{"memory negative", rep(base, "memory: 8GiB", "memory: -5"), "size"},
		{"memory overflow", rep(base, "memory: 8GiB", "memory: 99999999999999TiB"), "overflow"},
		{"pids 0", rep(base, "pids: 2048", "pids: 0"), "pids"},
		{"pids huge", rep(base, "pids: 2048", "pids: 99999999"), "pids"},
		{"tmpfs 0", rep(base, "tmpfs: 1GiB", "tmpfs: 0"), "tmpfs"},
		{"disk 0", rep(base, "disk: 20GiB", "disk: 0"), "disk"},
		{"max_file 0", rep(base, "max_file: 64MiB", "max_file: 0"), "max_file"},
		{"limit missing", rep(base, "pids: 2048, ", ""), "pids"},
		{"exec_max over 24h", rep(base, "exec_max: 2h", "exec_max: 25h"), "exec_max"},
		{"exec_max negative", rep(base, "exec_max: 2h", "exec_max: -1m"), "exec_max"},
		{"network empty", rep(base, "network: [none, egress]", "network: []"), "network"},
		{"network host", rep(base, "network: [none, egress]", "network: [host]"), "network"},
		{"network dup", rep(base, "network: [none, egress]", "network: [none, none]"), "twice"},
		{"env lowercase", rep(base, "[LANG, CI, GOFLAGS]", "[lang]"), "env_allow"},
		{"env bad", rep(base, "[LANG, CI, GOFLAGS]", `["A-B"]`), "env_allow"},
		{"env denied token", rep(base, "[LANG, CI, GOFLAGS]", "[GH_TOKEN]"), "denylist"},
		{"env denied api key", rep(base, "[LANG, CI, GOFLAGS]", "[FOO_API_KEY]"), "denylist"},
		{"env denied anthropic", rep(base, "[LANG, CI, GOFLAGS]", "[ANTHROPIC_FOO]"), "denylist"},
		{"env denied proxy", rep(base, "[LANG, CI, GOFLAGS]", "[HTTP_PROXY]"), "denylist"},
		{"env denied ssh", rep(base, "[LANG, CI, GOFLAGS]", "[SSH_AUTH_SOCK]"), "denylist"},
		{"containerd block missing", rep(base, "containerd:\n  namespace: mirafleet\n  root: /var/lib/mirafleet/ws\n  secure_runtime: io.containerd.runsc.v1\n", ""), "containerd block required"},
		{"wrong block present", base + osBase[strings.Index(osBase, "opensandbox:"):], "opensandbox block present"},
		{"ns k8s.io", rep(base, "namespace: mirafleet", "namespace: k8s.io"), "namespace"},
		{"ns moby", rep(base, "namespace: mirafleet", "namespace: moby"), "namespace"},
		{"ns empty", rep(base, "namespace: mirafleet", `namespace: ""`), "namespace"},
		{"ns bad chars", rep(base, "namespace: mirafleet", "namespace: a/b"), "namespace"},
		{"root relative", rep(base, "root: /var/lib/mirafleet/ws", "root: var/lib/ws"), "root"},
		{"root slash", rep(base, "root: /var/lib/mirafleet/ws", "root: /"), "root"},
		{"root dotdot", rep(base, "root: /var/lib/mirafleet/ws", "root: /var/lib/../ws"), "root"},
		{"root missing", rep(base, "  root: /var/lib/mirafleet/ws\n", ""), "root"},
		{"address relative", rep(base, "namespace: mirafleet", "namespace: mirafleet\n  address: sock"), "address"},
		{"userns bad", rep(base, "namespace: mirafleet", "namespace: mirafleet\n  userns: optional"), "userns"},
		{"apparmor bad", rep(base, "namespace: mirafleet", "namespace: mirafleet\n  apparmor: unconfined"), "apparmor"},
		{"uid_base low", rep(base, "namespace: mirafleet", "namespace: mirafleet\n  uid_base: 1000"), "uid_base"},
		{"uid_base high", rep(base, "namespace: mirafleet", "namespace: mirafleet\n  uid_base: 4294000000"), "uid_base"},
		{"secure_runtime bad", rep(base, "io.containerd.runsc.v1", "'a b; rm'"), "secure_runtime"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Parse([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("accepted: %+v", c)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q lacks %q", err, tc.want)
			}
		})
	}
}

func TestParseAcceptsVariants(t *testing.T) {
	for name, y := range map[string]string{
		"userns off":       rep(base, "namespace: mirafleet", "namespace: mirafleet\n  userns: off"),
		"apparmor req":     rep(base, "namespace: mirafleet", "namespace: mirafleet\n  apparmor: required"),
		"exec_max 24h":     rep(base, "exec_max: 2h", "exec_max: 24h"),
		"bytes plain":      rep(base, "max_file: 64MiB", "max_file: 4096"),
		"no secure rt":     rep(base, "  secure_runtime: io.containerd.runsc.v1\n", ""),
		"network none":     rep(base, "network: [none, egress]", "network: [none]"),
		"env_allow empty":  rep(base, "[LANG, CI, GOFLAGS]", "[]"),
		"image repo only":  rep(base, `"alpine:3.20"`, `"ghcr.io/x/y"`),
		"image digest pin": rep(base, `"alpine:3.20"`, `"alpine@sha256:`+strings.Repeat("a", 64)+`"`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(y)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOpenSandboxRules(t *testing.T) {
	pin := strings.Repeat("ab", 32)
	cases := []struct{ name, yaml, want string }{
		{"http", rep(osBase, "https://127.0.0.1", "http://127.0.0.1"), "https"},
		{"no scheme", rep(osBase, "https://127.0.0.1:8443", "127.0.0.1:8443"), "opensandbox.server"},
		{"remote host", rep(osBase, "127.0.0.1:8443", "example.com:8443"), "loopback"},
		{"private ip", rep(osBase, "127.0.0.1:8443", "10.0.0.5:8443"), "loopback"},
		{"userinfo trick", rep(osBase, "https://127.0.0.1:8443", "https://127.0.0.1@evil.com"), "credentials"},
		{"prefix trick", rep(osBase, "127.0.0.1:8443", "127.0.0.1.evil.com:8443"), "loopback"},
		{"server empty", rep(osBase, `server: "https://127.0.0.1:8443"`, `server: ""`), "server"},
		{"no ca no pin", rep(osBase, "  ca_file: /etc/mirafleet/ca.pem\n", ""), "ca_file: required"},
		{"pin only refused", rep(osBase, "  ca_file: /etc/mirafleet/ca.pem\n", "  pin_sha256: "+pin+"\n"), "ca_file: required"},
		{"bad pin", rep(osBase, "  ca_file: /etc/mirafleet/ca.pem\n", "  ca_file: /ca.pem\n  pin_sha256: abc\n"), "pin_sha256"},
		{"no client cert", rep(osBase, "  client_cert: /etc/mirafleet/c.pem\n", ""), "client_cert"},
		{"no client key", rep(osBase, "  client_key: /etc/mirafleet/c.key\n", ""), "client_key"},
		{"no api key file", rep(osBase, "  api_key_file: /etc/mirafleet/opensandbox.key\n", ""), "api_key_file"},
		{"port 0", rep(osBase, "127.0.0.1:8443", "127.0.0.1:0"), "port"},
		{"port 65536", rep(osBase, "127.0.0.1:8443", "127.0.0.1:65536"), "port"},
		{"port negative", rep(osBase, "127.0.0.1:8443", "127.0.0.1:-1"), "opensandbox.server"},
		{"port non-numeric", rep(osBase, "127.0.0.1:8443", "127.0.0.1:https"), "opensandbox.server"},
		{"port missing", rep(osBase, "127.0.0.1:8443", "127.0.0.1"), "port"},
		{"port missing trailing colon", rep(osBase, "127.0.0.1:8443", "127.0.0.1:"), "port"},
		{"ipv6 port missing", rep(osBase, "127.0.0.1:8443", "[::1]"), "port"},
		{"server path", rep(osBase, "127.0.0.1:8443", "127.0.0.1:8443/v1"), "no path"},
		{"secure_runtime too long", rep(osBase, "secure_runtime: gvisor", "secure_runtime: "+strings.Repeat("a", 65)), "secure_runtime"},
		{"relative ca", rep(osBase, "/etc/mirafleet/ca.pem", "ca.pem"), "ca_file"},
		{"relative api key", rep(osBase, "/etc/mirafleet/opensandbox.key", "k"), "api_key_file"},
		{"front bad", rep(osBase, "  secure_runtime: gvisor", "  front: none\n  secure_runtime: gvisor"), "front"},
		{"secure_runtime bad", rep(osBase, "secure_runtime: gvisor", "secure_runtime: 'a b'"), "secure_runtime"},
		{"block missing", osBase[:strings.Index(osBase, "opensandbox:")], "opensandbox block required"},
		{"wrong block present", osBase + "containerd: {namespace: mirafleet, root: /r}\n", "containerd block present"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v want %q", err, tc.want)
			}
		})
	}
	for name, y := range map[string]string{
		"ca and pin":    rep(osBase, "  ca_file: /etc/mirafleet/ca.pem\n", "  ca_file: /ca.pem\n  pin_sha256: "+pin+"\n"),
		"external":      rep(osBase, "  secure_runtime: gvisor", "  front: external\n  secure_runtime: gvisor"),
		"ipv6 loopback": rep(osBase, "127.0.0.1:8443", "[::1]:8443"),
		"localhost":     rep(osBase, "127.0.0.1:8443", "localhost:8443"),
		"port 1":        rep(osBase, "127.0.0.1:8443", "127.0.0.1:1"),
		"port 65535":    rep(osBase, "127.0.0.1:8443", "127.0.0.1:65535"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(y)); err != nil {
				t.Fatal(err)
			}
		})
	}
	// front=external must obey the same rules as builtin.
	ext := rep(osBase, "  secure_runtime: gvisor", "  front: external\n  secure_runtime: gvisor")
	if _, err := Parse([]byte(rep(ext, "https://", "http://"))); err == nil {
		t.Fatal("external front accepted http")
	}
}

// stub swaps the injectable stat/uid for one test.
func stub(t *testing.T, st map[string]FileStat, uid int) {
	t.Helper()
	oldS, oldU := statFn, getuid
	statFn = func(p string) (FileStat, error) {
		s, ok := st[p]
		if !ok {
			return FileStat{}, os.ErrNotExist
		}
		return s, nil
	}
	getuid = func() int { return uid }
	t.Cleanup(func() { statFn, getuid = oldS, oldU })
}

func TestCheckFilesContainerd(t *testing.T) {
	c, err := Parse([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	root := c.Containerd.Root
	cases := []struct {
		name string
		st   map[string]FileStat
		want string
	}{
		{"ok", map[string]FileStat{root: {Mode: os.ModeDir | 0o700, UID: 0, IsDir: true}}, ""},
		{"missing", map[string]FileStat{}, "root"},
		{"not dir", map[string]FileStat{root: {Mode: 0o700, UID: 0}}, "not a directory"},
		{"not root owned", map[string]FileStat{root: {Mode: os.ModeDir | 0o700, UID: 1000, IsDir: true}}, "owned by root"},
		{"mode 0755", map[string]FileStat{root: {Mode: os.ModeDir | 0o755, UID: 0, IsDir: true}}, "0700"},
		{"mode 0750", map[string]FileStat{root: {Mode: os.ModeDir | 0o750, UID: 0, IsDir: true}}, "0700"},
		{"mode 0500", map[string]FileStat{root: {Mode: os.ModeDir | 0o500, UID: 0, IsDir: true}}, "0700"},
		{"sticky", map[string]FileStat{root: {Mode: os.ModeDir | os.ModeSticky | 0o700, UID: 0, IsDir: true}}, "0700"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub(t, tc.st, 0)
			err := c.CheckFiles()
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v want %q", err, tc.want)
			}
		})
	}
}

func TestCheckFilesOpenSandbox(t *testing.T) {
	c, err := Parse([]byte(osBase))
	if err != nil {
		t.Fatal(err)
	}
	p, ck := c.OpenSandbox.APIKeyFile, c.OpenSandbox.ClientKey
	pdir := filepath.Dir(p)
	cases := []struct {
		name string
		st   map[string]FileStat
		uid  int
		want string
	}{
		{"0600", map[string]FileStat{p: {Mode: 0o600, UID: 501}}, 501, ""},
		{"0400", map[string]FileStat{p: {Mode: 0o400, UID: 501}}, 501, ""},
		{"missing", map[string]FileStat{}, 501, "api_key_file"},
		{"0644", map[string]FileStat{p: {Mode: 0o644, UID: 501}}, 501, "0600 or 0400"},
		{"0640", map[string]FileStat{p: {Mode: 0o640, UID: 501}}, 501, "0600 or 0400"},
		{"0660", map[string]FileStat{p: {Mode: 0o660, UID: 501}}, 501, "0600 or 0400"},
		{"0700", map[string]FileStat{p: {Mode: 0o700, UID: 501}}, 501, "0600 or 0400"},
		{"other owner", map[string]FileStat{p: {Mode: 0o600, UID: 0}}, 501, "owned by"},
		{"dir", map[string]FileStat{p: {Mode: os.ModeDir | 0o600, UID: 501, IsDir: true}}, 501, "regular"},
		{"device", map[string]FileStat{p: {Mode: os.ModeDevice | 0o600, UID: 501}}, 501, "regular"},
		{"client key 0644", map[string]FileStat{p: {Mode: 0o600, UID: 501}, ck: {Mode: 0o644, UID: 501}}, 501, "client_key"},
		{"client key other owner", map[string]FileStat{p: {Mode: 0o600, UID: 501}, ck: {Mode: 0o600, UID: 0}}, 501, "client_key"},
		{"client key missing", map[string]FileStat{p: {Mode: 0o600, UID: 501}, ck: {}}, 501, "client_key"},
		// E-07: the parent directory of both secrets.
		{"parent root 0755", map[string]FileStat{p: {Mode: 0o600, UID: 501}, pdir: {Mode: os.ModeDir | 0o755, UID: 0, IsDir: true}}, 501, ""},
		{"parent runner 0700", map[string]FileStat{p: {Mode: 0o600, UID: 501}, pdir: {Mode: os.ModeDir | 0o700, UID: 501, IsDir: true}}, 501, ""},
		{"parent other owner", map[string]FileStat{p: {Mode: 0o600, UID: 501}, pdir: {Mode: os.ModeDir | 0o755, UID: 1000, IsDir: true}}, 501, "parent directory"},
		{"parent group writable", map[string]FileStat{p: {Mode: 0o600, UID: 501}, pdir: {Mode: os.ModeDir | 0o775, UID: 0, IsDir: true}}, 501, "group- or world-writable"},
		{"parent world writable", map[string]FileStat{p: {Mode: 0o600, UID: 501}, pdir: {Mode: os.ModeDir | 0o757, UID: 501, IsDir: true}}, 501, "group- or world-writable"},
		{"parent sticky world writable", map[string]FileStat{p: {Mode: 0o600, UID: 501}, pdir: {Mode: os.ModeDir | os.ModeSticky | 0o777, UID: 0, IsDir: true}}, 501, "group- or world-writable"},
		{"parent not a dir", map[string]FileStat{p: {Mode: 0o600, UID: 501}, pdir: {Mode: 0o755, UID: 0}}, 501, "not a directory"},
		{"parent undeterminable", map[string]FileStat{p: {Mode: 0o600, UID: 501}, pdir: {}}, 501, "parent directory"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := tc.st[pdir]; !ok {
				tc.st[pdir] = FileStat{Mode: os.ModeDir | 0o755, UID: 0, IsDir: true} // a good parent unless the case says otherwise
			}
			if _, ok := tc.st[ck]; !ok {
				tc.st[ck] = FileStat{Mode: 0o600, UID: uint32(tc.uid)} // a good client key unless the case says otherwise
			}
			if tc.st[ck] == (FileStat{}) {
				delete(tc.st, ck)
			}
			if tc.st[pdir] == (FileStat{}) {
				delete(tc.st, pdir) // stat fails: ownership undeterminable, must fail closed
			}
			stub(t, tc.st, tc.uid)
			err := c.CheckFiles()
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v want %q", err, tc.want)
			}
		})
	}
}

// TestLoadReal exercises the real stat path on t.TempDir files.
func TestLoadReal(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "api.key")
	if err := os.WriteFile(key, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	ckey := filepath.Join(dir, "client.key")
	if err := os.WriteFile(ckey, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	osBase := rep(osBase, "/etc/mirafleet/c.key", ckey)
	y := rep(osBase, "/etc/mirafleet/opensandbox.key", key)
	cfg := filepath.Join(dir, "workspace.yaml")
	if err := os.WriteFile(cfg, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if c.OpenSandbox.APIKeyFile != key {
		t.Fatal("key path")
	}
	if err := os.Chmod(key, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(cfg); err == nil || !strings.Contains(err.Error(), "0600 or 0400") {
		t.Fatalf("loose key accepted: %v", err)
	}
	// symlinked key is refused
	if err := os.Chmod(key, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.key")
	if err := os.Symlink(key, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	if err := os.WriteFile(cfg, []byte(rep(osBase, "/etc/mirafleet/opensandbox.key", link)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(cfg); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlinked key accepted: %v", err)
	}
	if _, err := Load(filepath.Join(dir, "nope.yaml")); err == nil {
		t.Fatal("missing file accepted")
	}
	// containerd root in a TempDir is never root:0700 for a non-root test user.
	if os.Getuid() != 0 {
		root := filepath.Join(dir, "ws")
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cfg, []byte(rep(base, "/var/lib/mirafleet/ws", root)), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(cfg); err == nil || !strings.Contains(err.Error(), "owned by root") {
			t.Fatalf("non-root-owned root accepted: %v", err)
		}
	}
}

func TestEgressCIDRRulesFlag(t *testing.T) {
	c, err := Parse([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	if c.EgressCIDRRules {
		t.Fatal("egress_cidr_rules must default to false")
	}
	c, err = Parse([]byte(strings.Replace(base, "network: [none, egress]", "network: [none, egress]\negress_cidr_rules: true", 1)))
	if err != nil || !c.EgressCIDRRules {
		t.Fatalf("flag not parsed: %v", err)
	}
}

func TestPinHexOnly(t *testing.T) {
	pin := strings.Repeat("ab", 32)
	ok := strings.Replace(osBase, "  ca_file: /etc/mirafleet/ca.pem\n", "  ca_file: /etc/mirafleet/ca.pem\n  pin_sha256: "+pin+"\n", 1)
	if _, err := Parse([]byte(ok)); err != nil {
		t.Fatal(err)
	}
	bad := strings.Replace(ok, pin, "q2Zo0nS2yR0v1sXq3m2Yk5oQ8cQ8c7p1o2o3o4o5o6o=", 1)
	if _, err := Parse([]byte(bad)); err == nil {
		t.Fatal("base64 pin accepted")
	}
}

func TestHeartbeatImagesDropped(t *testing.T) {
	c, err := Parse([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	if d := c.HeartbeatImagesDropped(); len(d) != 0 {
		t.Errorf("dropped = %v", d)
	}
	c.Images = nil
	for i := 0; i < 34; i++ {
		c.Images = append(c.Images, "alpine:"+strings.Repeat("a", i+1))
	}
	c.Images = append(c.Images, "x:"+strings.Repeat("b", 300))
	if d := c.HeartbeatImagesDropped(); len(d) != 3 {
		t.Errorf("dropped %d, want 3 (2 over the count, 1 too long)", len(d))
	}
}

// F-23: only the immediate parent directory is checked (documented on checkParentDir); the
// ancestors above it are never stat'ed, so a group-writable grandparent does not matter.
func TestCheckParentDirImmediateOnly(t *testing.T) {
	var seen []string
	oldS, oldU := statFn, getuid
	t.Cleanup(func() { statFn, getuid = oldS, oldU })
	getuid = func() int { return 0 }
	statFn = func(p string) (FileStat, error) {
		seen = append(seen, p)
		return FileStat{Mode: os.ModeDir | 0o700, UID: 0, IsDir: true}, nil
	}
	if err := checkParentDir("api_key_file", "/a/b/c/key"); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != "/a/b/c" {
		t.Errorf("stat calls %v, want only /a/b/c", seen)
	}
	statFn = func(p string) (FileStat, error) {
		if p == "/a/b/c" {
			return FileStat{Mode: os.ModeDir | 0o700, UID: 0, IsDir: true}, nil
		}
		return FileStat{Mode: os.ModeDir | 0o777, UID: 4242, IsDir: true}, nil
	}
	if err := checkParentDir("api_key_file", "/a/b/c/key"); err != nil {
		t.Errorf("ancestor above the parent affected the result: %v", err)
	}
}
