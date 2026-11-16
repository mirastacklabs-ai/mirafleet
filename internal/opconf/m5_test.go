package opconf

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsDirs(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	for _, tc := range []struct {
		name, local, cfg, state string
		bad                     bool
	}{
		{"plain", `C:\Users\a\AppData\Local`, `C:\Users\a\AppData\Local\mirafleet`, `C:\Users\a\AppData\Local\mirafleet\state`, false},
		{"trailing slash", `C:\L\`, `C:\L\mirafleet`, `C:\L\mirafleet\state`, false},
		{"unc refused", `\\srv\share`, ``, ``, true},
		{"extended unc refused", `\\?\UNC\srv\share`, ``, ``, true},
		{"unset", ``, ``, ``, true},
		{"relative", `Local`, ``, ``, true},
	} {
		// Roaming must never be consulted.
		e := env(map[string]string{"LocalAppData": tc.local, "AppData": `C:\Users\a\AppData\Roaming`})
		c, err := windowsConfigDir(e)
		s, err2 := windowsStateDir(e)
		if tc.bad {
			if err == nil || err2 == nil {
				t.Errorf("%s: accepted", tc.name)
			}
			continue
		}
		if err != nil || err2 != nil || c != tc.cfg || s != tc.state {
			t.Errorf("%s: %q %q %v %v", tc.name, c, s, err, err2)
		}
	}
}

func TestStateDir(t *testing.T) {
	d := filepath.Join(t.TempDir(), "st")
	t.Setenv(EnvStateDir, d)
	if got, err := StateDir(); err != nil || got != d {
		t.Fatal(got, err)
	}
	t.Setenv(EnvStateDir, "rel")
	if _, err := StateDir(); err == nil {
		t.Fatal("relative accepted")
	}
	t.Setenv(EnvStateDir, "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", home)
	t.Setenv("LocalAppData", home)
	got, err := StateDir()
	if err != nil || filepath.Base(filepath.Dir(got)) == "" || !strings.HasPrefix(got, home) {
		t.Fatal(got, err)
	}
}

func TestContextsM5Fields(t *testing.T) {
	dir := t.TempDir()
	ok := map[string]struct {
		body string
		cap  int
	}{
		"legacy":           {"current: a\ncontexts:\n- {name: a, addr: x:1}\n", DefaultOutputCap},
		"default fleet":    {"contexts:\n- {name: a, addr: x:1, default_fleet: prod}\n", DefaultOutputCap},
		"cap min":          {"output_cap_bytes: 4096\ncontexts: []\n", 4096},
		"cap max":          {"output_cap_bytes: 262144\ncontexts: []\n", 262144},
		"cap absent/empty": {"contexts: []\n", DefaultOutputCap},
	}
	for name, tc := range ok {
		os.WriteFile(ContextsPath(dir), []byte(tc.body), 0o600)
		c, err := LoadContexts(dir)
		if err != nil || c.OutputCap() != tc.cap {
			t.Errorf("%s: %v %v", name, c, err)
		}
	}
	bad := map[string]string{
		"cap low":         "output_cap_bytes: 4095\ncontexts: []\n",
		"cap high":        "output_cap_bytes: 262145\ncontexts: []\n",
		"cap negative":    "output_cap_bytes: -1\ncontexts: []\n",
		"bad fleet":       "contexts:\n- {name: a, addr: x:1, default_fleet: Bad Fleet}\n",
		"unknown ctx key": "contexts:\n- {name: a, addr: x:1, token: s}\n",
		"jump ctx":        "contexts:\n- {name: a, addr: x:1, jump: bastion:22}\n",
		"jump top":        "jump: bastion\ncontexts: []\n",
		"jump nested":     "contexts:\n- name: a\n  addr: x:1\n  extra:\n    jump: b\n",
	}
	for name, body := range bad {
		os.WriteFile(ContextsPath(dir), []byte(body), 0o600)
		_, err := LoadContexts(dir)
		if err == nil {
			t.Errorf("%s accepted", name)
		} else if strings.HasPrefix(name, "jump") && !strings.Contains(err.Error(), "SSH jump hosts are not supported yet") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestContextUseRemove(t *testing.T) {
	dir := t.TempDir()
	in := &Contexts{Current: "a", OutputCapBytes: 8192, Contexts: []Context{
		{Name: "a", Addr: "x:1", DefaultFleet: "f"}, {Name: "b", Addr: "y:1"}}}
	if err := SaveContexts(dir, in); err != nil {
		t.Fatal(err)
	}
	if err := UseContext(dir, "b"); err != nil {
		t.Fatal(err)
	}
	if err := UseContext(dir, "zz"); err == nil {
		t.Fatal("unknown use")
	}
	c, _ := LoadContexts(dir)
	if c.Current != "b" || c.OutputCapBytes != 8192 || c.Contexts[0].DefaultFleet != "f" {
		t.Fatal(c)
	}
	if err := RemoveContext(dir, "b"); err != nil {
		t.Fatal(err)
	}
	c, _ = LoadContexts(dir)
	if c.Current != "" || len(c.Contexts) != 1 || c.Names()[0] != "a" {
		t.Fatal(c)
	}
	if err := RemoveContext(dir, "b"); err == nil {
		t.Fatal("double remove")
	}
	if st, _ := os.Stat(ContextsPath(dir)); st.Mode().Perm()&0o077 != 0 && !isWin() {
		t.Fatalf("mode %v", st.Mode())
	}
}

func isWin() bool { return filepath.Separator == '\\' }

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadProject(t *testing.T) {
	home := t.TempDir()
	// home/.mirafleet.yaml must not be seen from below a .git boundary.
	write(t, filepath.Join(home, ProjectName), "fleet: homefleet\n")
	// repo with .git dir and file at its root; sub dirs below.
	repo := filepath.Join(home, "repo")
	write(t, filepath.Join(repo, ".git", "HEAD"), "x")
	deep := filepath.Join(repo, "a", "b")
	os.MkdirAll(deep, 0o755)
	// worktree: .git is a file and carries the project file at that level.
	wt := filepath.Join(home, "wt")
	write(t, filepath.Join(wt, ".git"), "gitdir: x")
	write(t, filepath.Join(wt, ProjectName), "fleet: wtf\ncontext: lab\n")
	os.MkdirAll(filepath.Join(wt, "src"), 0o755)
	// no git: walks to home.
	plain := filepath.Join(home, "plain", "x")
	os.MkdirAll(plain, 0o755)
	// outside home.
	outside := t.TempDir()
	write(t, filepath.Join(outside, ProjectName), "fleet: out\n")
	os.MkdirAll(filepath.Join(outside, "sub"), 0o755)

	for _, tc := range []struct {
		name, start string
		want        Project
		path        string
	}{
		{"stops at .git dir", deep, Project{}, ""},
		{"git file level counts", filepath.Join(wt, "src"), Project{Fleet: "wtf", Context: "lab"}, filepath.Join(wt, ProjectName)},
		{"walks to home", plain, Project{Fleet: "homefleet"}, filepath.Join(home, ProjectName)},
		{"start itself", wt, Project{Fleet: "wtf", Context: "lab"}, filepath.Join(wt, ProjectName)},
		{"outside home: only start", filepath.Join(outside, "sub"), Project{}, ""},
		{"outside home: start hit", outside, Project{Fleet: "out"}, filepath.Join(outside, ProjectName)},
	} {
		p, path, err := LoadProject(tc.start, home)
		if err != nil || p != tc.want || path != tc.path {
			t.Errorf("%s: %+v %q %v", tc.name, p, path, err)
		}
	}
	// does not go above home
	if _, path, err := LoadProject(filepath.Join(home, "nothing"), home); err != nil || path != filepath.Join(home, ProjectName) {
		t.Fatal(path, err)
	}
	os.Remove(filepath.Join(home, ProjectName))
	if _, path, err := LoadProject(plain, home); err != nil || path != "" {
		t.Fatal("went above home or found phantom:", path, err)
	}
	if _, _, err := LoadProject("relative", home); err == nil {
		t.Fatal("relative start accepted")
	}
}

func TestLoadProjectHostile(t *testing.T) {
	base := t.TempDir()
	cases := map[string]string{
		"extra key addr":  "fleet: a\naddr: evil:22\n",
		"keys":            "context: a\nkeys: [x]\n",
		"caps":            "fleet: a\noutput_cap_bytes: 999999\n",
		"jump":            "jump: h\n",
		"bad fleet":       "fleet: Bad_Fleet\n",
		"bad context":     "context: \"../x\"\n",
		"not yaml":        ":\n\t-",
		"list":            "- a\n",
		"oversize":        "fleet: a\n# " + strings.Repeat("x", 5000) + "\n",
		"multiline fleet": "fleet: \"a\\nb\"\n",
	}
	for name, body := range cases {
		d := filepath.Join(base, strings.ReplaceAll(name, " ", "_"))
		write(t, filepath.Join(d, ProjectName), body)
		if _, _, err := LoadProject(d, base); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	d := filepath.Join(base, "empty")
	write(t, filepath.Join(d, ProjectName), "")
	if p, path, err := LoadProject(d, base); err != nil || p != (Project{}) || path == "" {
		t.Fatal(p, path, err)
	}
}

func TestLoadProjectSymlinkRefused(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "real.yaml")
	write(t, target, "fleet: a\n")
	d := filepath.Join(base, "proj")
	os.MkdirAll(d, 0o755)
	if err := os.Symlink(target, filepath.Join(d, ProjectName)); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if _, _, err := LoadProject(d, base); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatal("symlink accepted:", err)
	}
}
