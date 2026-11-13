package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// darwin and windows binaries must never link SQLite (docs/M2.md §1.9): central and runner
// wiring lives in server_linux.go only.
func TestNoSQLiteOnNonLinux(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go tool not available")
	}
	for _, goos := range []string{"darwin", "windows"} {
		cmd := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", ".")
		cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH=amd64", "CGO_ENABLED=0")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("go list for %s: %v\n%s", goos, err, out)
		}
		for _, pkg := range strings.Fields(string(out)) {
			if strings.HasPrefix(pkg, "modernc.org/") || strings.HasSuffix(pkg, "internal/store/sqlite") {
				t.Errorf("%s binary links %s", goos, pkg)
			}
		}
	}
}
