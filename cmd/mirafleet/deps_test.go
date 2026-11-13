package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// goDeps lists the import paths the binary links for goos.
func goDeps(t *testing.T, goos string) []string {
	t.Helper()
	cmd := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", ".")
	cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH=amd64", "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list for %s: %v\n%s", goos, err, out)
	}
	return strings.Fields(string(out))
}

func linksSQLite(pkg string) bool {
	return strings.HasPrefix(pkg, "modernc.org/") || strings.HasSuffix(pkg, "internal/store/sqlite")
}

// requireGo skips when the go tool is missing, except in CI, where a missing tool must fail the
// test instead of silently disabling the check.
func requireGo(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("go tool not available but CI is set: this check must not be skipped in CI")
		}
		t.Skip("go tool not available")
	}
}

// darwin and windows binaries must never link SQLite (docs/M2.md §1.9): central and runner
// wiring lives in server_linux.go only.
func TestNoSQLiteOnNonLinux(t *testing.T) {
	requireGo(t)
	for _, goos := range []string{"darwin", "windows"} {
		for _, pkg := range goDeps(t, goos) {
			if linksSQLite(pkg) {
				t.Errorf("%s binary links %s", goos, pkg)
			}
		}
	}
}

// Positive control: the linux binary does link SQLite. Without it, a broken `go list` filter (or
// moved wiring) would make TestNoSQLiteOnNonLinux pass vacuously.
func TestLinuxLinksSQLite(t *testing.T) {
	requireGo(t)
	for _, pkg := range goDeps(t, "linux") {
		if linksSQLite(pkg) {
			return
		}
	}
	t.Fatal("linux binary links no SQLite package: the non-linux check proves nothing")
}

func linksContainerd(pkg string) bool {
	return strings.HasPrefix(pkg, "github.com/containerd/") || strings.HasPrefix(pkg, "google.golang.org/grpc")
}

// darwin and windows binaries must never link containerd or gRPC (docs/M3.md §8): the containerd
// executor lives under internal/workspace/ctrd and is wired in from linux-only files.
func TestNoContainerdOnNonLinux(t *testing.T) {
	requireGo(t)
	for _, goos := range []string{"darwin", "windows"} {
		for _, pkg := range goDeps(t, goos) {
			if linksContainerd(pkg) {
				t.Errorf("%s binary links %s", goos, pkg)
			}
		}
	}
}

// Positive control for TestNoContainerdOnNonLinux. Nothing in cmd imports ctrd yet (S0), so this
// checks ./internal/workspace/ctrd directly. TODO(M3 wiring): once server_linux.go imports ctrd,
// switch it to goDeps(t, "linux") so it also proves the binary links containerd.
func TestLinuxLinksContainerd(t *testing.T) {
	requireGo(t)
	cmd := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", "./../../internal/workspace/ctrd")
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list ctrd: %v\n%s", err, out)
	}
	for _, pkg := range strings.Fields(string(out)) {
		if linksContainerd(pkg) {
			return
		}
	}
	t.Fatal("linux ctrd links no containerd package: the non-linux check proves nothing")
}
