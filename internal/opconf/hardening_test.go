package opconf

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func mustReadDir(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return ents
}

// F-07: a second YAML document is an error, in both the contexts file and the project file.
func TestSecondYAMLDocumentRefused(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"second doc":         "contexts:\n- {name: a, addr: x:1}\n---\ncontexts:\n- {name: evil, addr: y:1}\n",
		"second doc, empty":  "contexts: []\n---\n",
		"leading then doc":   "---\ncontexts: []\n---\ncontexts: []\n",
		"second doc garbage": "contexts: []\n---\n: :\n",
	} {
		if err := os.WriteFile(ContextsPath(dir), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadContexts(dir); err == nil {
			t.Errorf("contexts: %s accepted", name)
		}
	}
	// One document with a leading marker is still fine.
	os.WriteFile(ContextsPath(dir), []byte("---\ncontexts:\n- {name: a, addr: x:1}\n"), 0o600)
	if c, err := LoadContexts(dir); err != nil || len(c.Contexts) != 1 {
		t.Fatalf("single doc with marker: %v %v", c, err)
	}

	base := t.TempDir()
	d := filepath.Join(base, "p")
	write(t, filepath.Join(d, ProjectName), "fleet: a\n---\nfleet: b\n")
	if _, _, err := LoadProject(d, base); err == nil || !strings.Contains(err.Error(), "document") {
		t.Fatalf("project second doc accepted: %v", err)
	}
}

// F-08: a root home must not let the walk-up reach the filesystem root.
func TestLoadProjectRootHomeIsNoHome(t *testing.T) {
	root := string(filepath.Separator)
	if vol := filepath.VolumeName(t.TempDir()); vol != "" {
		root = vol + root
	}
	// With home = root, only start itself may be checked (as if outside home): a file in the
	// parent of start must not be found.
	base := t.TempDir()
	write(t, filepath.Join(base, ProjectName), "fleet: parent\n")
	start := filepath.Join(base, "sub")
	os.MkdirAll(start, 0o755)
	if _, path, err := LoadProject(start, root); err != nil || path != "" {
		t.Fatalf("walked up with a root home: %q %v", path, err)
	}
	write(t, filepath.Join(start, ProjectName), "fleet: here\n")
	if p, _, err := LoadProject(start, root); err != nil || p.Fleet != "here" {
		t.Fatalf("start itself not checked: %v %v", p, err)
	}
}

// F-11: concurrent read-modify-write updates must not lose each other's change.
func TestUpdateContextsConcurrent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cfg")
	const n = 16
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- UpdateContexts(dir, func(c *Contexts) error {
				c.Contexts = append(c.Contexts, Context{Name: fmt.Sprintf("c%02d", i), Addr: "h:1"})
				return nil
			})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	c, err := LoadContexts(dir)
	if err != nil || len(c.Contexts) != n {
		t.Fatalf("lost updates: got %d of %d (%v)", len(c.Contexts), n, err)
	}
}

func TestUpdateContextsFnErrorSavesNothing(t *testing.T) {
	dir := t.TempDir()
	if err := SaveContexts(dir, &Contexts{Contexts: []Context{{Name: "a", Addr: "h:1"}}}); err != nil {
		t.Fatal(err)
	}
	want := fmt.Errorf("boom")
	err := UpdateContexts(dir, func(c *Contexts) error {
		c.Contexts = nil
		return want
	})
	if err != want {
		t.Fatalf("err %v", err)
	}
	if c, _ := LoadContexts(dir); len(c.Contexts) != 1 {
		t.Fatalf("saved despite error: %v", c)
	}
	if err := UseContext(dir, "a"); err != nil {
		t.Fatal(err)
	}
	if err := RemoveContext(dir, "a"); err != nil {
		t.Fatal(err)
	}
	if err := RemoveContext(dir, "a"); err == nil {
		t.Fatal("removed twice")
	}
}
