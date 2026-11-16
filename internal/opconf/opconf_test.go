package opconf

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDirOverrideAndEnsure(t *testing.T) {
	base := filepath.Join(t.TempDir(), "cfg")
	t.Setenv(EnvDir, base)
	d, err := Dir()
	if err != nil || d != base {
		t.Fatal(d, err)
	}
	if _, err := os.Stat(base); err == nil {
		t.Fatal("Dir must not create")
	}
	got, err := EnsureDir()
	if err != nil || got != base {
		t.Fatal(got, err)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(base); st.Mode().Perm() != 0o700 {
			t.Fatalf("mode %v", st.Mode())
		}
	}
	for _, p := range []string{KeyPath(d), KnownHostsPath(d), ContextsPath(d)} {
		if filepath.Dir(p) != base {
			t.Fatal(p)
		}
	}
	if filepath.Base(KeyPath(d)) != KeyName {
		t.Fatal()
	}
}

func TestEnsureTightensExisting(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix modes")
	}
	base := filepath.Join(t.TempDir(), "cfg")
	os.Mkdir(base, 0o755)
	t.Setenv(EnvDir, base)
	if _, err := EnsureDir(); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(base); st.Mode().Perm() != 0o700 {
		t.Fatalf("mode %v", st.Mode())
	}
}

func TestDirRelativeOverrideRefused(t *testing.T) {
	t.Setenv(EnvDir, "relative/dir")
	if _, err := Dir(); err == nil {
		t.Fatal("relative accepted")
	}
}

func TestDefaultDirUnderUserConfig(t *testing.T) {
	t.Setenv(EnvDir, "")
	cfg := t.TempDir()
	// os.UserConfigDir reads XDG_CONFIG_HOME (linux), HOME (darwin), AppData (windows).
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("HOME", cfg)
	t.Setenv("AppData", cfg)
	// Windows deliberately uses %LocalAppData%, not Roaming.
	t.Setenv("LocalAppData", cfg)
	d, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(d) != "mirafleet" || !strings.HasPrefix(d, cfg) {
		t.Fatal(d)
	}
	if runtime.GOOS == "windows" && d != strings.TrimRight(cfg, `\/`)+`\mirafleet` {
		t.Fatal(d)
	}
}

func TestContextsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	c, err := LoadContexts(dir)
	if err != nil || len(c.Contexts) != 0 {
		t.Fatal(c, err)
	}
	in := &Contexts{Current: "prod", Contexts: []Context{{Name: "prod", Addr: "central.example:2222"}, {Name: "lab", Addr: "10.0.0.1:2222"}}}
	if err := SaveContexts(dir, in); err != nil {
		t.Fatal(err)
	}
	out, err := LoadContexts(dir)
	if err != nil || out.Current != "prod" || len(out.Contexts) != 2 || out.Contexts[1].Addr != "10.0.0.1:2222" {
		t.Fatal(out, err)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(ContextsPath(dir)); st.Mode().Perm()&0o077 != 0 {
			t.Fatalf("mode %v", st.Mode())
		}
	}
	for _, e := range mustReadDir(t, dir) { // only contexts.yaml and the lock file may remain
		if e.Name() != ContextsName && e.Name() != LockName {
			t.Fatalf("temp files left: %v", e.Name())
		}
	}
}

func TestContextsStrict(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"unknown field": "contexts: []\nextra: 1\n",
		"bad name":      "contexts:\n- {name: Bad Name, addr: a:1}\n",
		"no addr":       "contexts:\n- {name: a}\n",
		"dup":           "contexts:\n- {name: a, addr: x:1}\n- {name: a, addr: y:1}\n",
		"bad current":   "current: z\ncontexts:\n- {name: a, addr: x:1}\n",
		"not yaml":      ":\n\t-",
	} {
		os.WriteFile(ContextsPath(dir), []byte(body), 0o600)
		if _, err := LoadContexts(dir); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	os.WriteFile(ContextsPath(dir), nil, 0o600)
	if c, err := LoadContexts(dir); err != nil || len(c.Contexts) != 0 {
		t.Fatal("empty file", err)
	}
	if err := SaveContexts(dir, &Contexts{Contexts: []Context{{Name: "x"}}}); err == nil {
		t.Fatal("invalid saved")
	}
}

func TestSaveContextsCreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "new", "cfg")
	in := &Contexts{Current: "a", Contexts: []Context{{Name: "a", Addr: "127.0.0.1:1"}}}
	if err := SaveContexts(dir, in); err != nil {
		t.Fatal(err)
	}
	out, err := LoadContexts(dir)
	if err != nil || out.Current != "a" || len(out.Contexts) != 1 {
		t.Fatal(out, err)
	}
}

// A contexts.yaml ending in a bare "---" line with nothing after it: yaml.v3 reads the trailer as
// an empty second document, which decodeSingle refuses as more than one document. Pinned here.
func TestLoadContextsTrailingBareSeparator(t *testing.T) {
	dir := t.TempDir()
	body := "current: a\ncontexts:\n  - name: a\n    addr: 127.0.0.1:1\n---\n"
	if err := os.WriteFile(ContextsPath(dir), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cs, err := LoadContexts(dir)
	if err == nil {
		t.Fatalf("accepted a trailing bare ---: %+v", cs)
	}
	if !strings.Contains(err.Error(), "more than one YAML document") {
		t.Fatalf("err = %v, want the multi-document error", err)
	}
}
