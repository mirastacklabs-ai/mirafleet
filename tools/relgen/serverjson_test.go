package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func serverRoot(t *testing.T, content string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "server.json"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestServerJSONGolden(t *testing.T) {
	src, err := os.ReadFile("testdata/serverjson/server.json")
	if err != nil {
		t.Fatal(err)
	}
	root := serverRoot(t, string(src))
	if c, _, errs := do("-root", root, "serverjson", "v0.6.0"); c != 0 {
		t.Fatalf("code %d %q", c, errs)
	}
	goldenN26(t, "serverjson/server.v0.6.0.golden", read(t, root, "server.json"))
	// A second run changes nothing and -check agrees.
	if c, _, errs := do("-root", root, "serverjson", "-check", "v0.6.0"); c != 0 {
		t.Fatalf("check after write: %d %q", c, errs)
	}
	if c, _, _ := do("-root", root, "serverjson", "-check", "v0.7.0"); c != 1 {
		t.Fatal("check passed for another version")
	}
	if got := read(t, root, "server.json"); !strings.Contains(got, `"version": "0.6.0"`) {
		t.Fatalf("check must not rewrite: %s", got)
	}
}

func TestServerJSONRefusals(t *testing.T) {
	cases := map[string]string{
		"non-npm":       `{"name":"x","version":"1","packages":[{"registryType":"oci","identifier":"i","version":"1","transport":{}}]}`,
		"unknown field": `{"name":"x","version":"1","surprise":1,"packages":[{"registryType":"npm","identifier":"i","version":"1","transport":{}}]}`,
		"no packages":   `{"name":"x","version":"1"}`,
	}
	for name, content := range cases {
		if c, _, errs := do("-root", serverRoot(t, content), "serverjson", "v0.6.0"); c != 1 {
			t.Errorf("%s: code %d %q", name, c, errs)
		}
	}
}
