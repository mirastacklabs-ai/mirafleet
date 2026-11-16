package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// goDeps lists the import paths the binary links for goos.
func goDeps(t *testing.T, goos string) []string { return goDepsArch(t, goos, "amd64") }

func goDepsArch(t *testing.T, goos, goarch string) []string {
	t.Helper()
	cmd := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", ".")
	cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
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

// darwin and windows binaries must never link SQLite: central and runner
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

// darwin and windows binaries must never link containerd or gRPC: the containerd
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

// Positive control for TestNoContainerdOnNonLinux: the linux cmd/mirafleet binary itself links
// containerd (runner_ws_linux.go imports ctrd). Without it a broken `go list` filter, or wiring
// moved to a file that is not linux-only, would make the non-linux check pass vacuously.
func TestLinuxLinksContainerd(t *testing.T) {
	requireGo(t)
	for _, pkg := range goDeps(t, "linux") {
		if linksContainerd(pkg) {
			return
		}
	}
	t.Fatal("linux binary links no containerd package: the non-linux check proves nothing")
}

const mcpSDK = "github.com/modelcontextprotocol/go-sdk"

// goListPkgs runs `go list` with args for the whole module and returns the output lines.
func goListPkgs(t *testing.T, goos, goarch string, args ...string) []string {
	t.Helper()
	cmd := exec.Command("go", append([]string{"list"}, args...)...)
	cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list %v for %s/%s: %v\n%s", args, goos, goarch, err, out)
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n")
}

// forbiddenForOrchestrator reports packages the orchestrator side (MCP server, orch core, hooks)
// must never link: the server-side stack, SQLite and containerd.
func forbiddenForOrchestrator(pkg string) bool {
	const mod = "github.com/mirastacklabs-ai/mirafleet/"
	for _, p := range []string{"internal/service", "internal/store", "internal/central", "internal/runner", "internal/workspace/ctrd"} {
		if pkg == mod+p || strings.HasPrefix(pkg, mod+p+"/") {
			return true
		}
	}
	return strings.HasPrefix(pkg, "modernc.org/") || strings.HasPrefix(pkg, "github.com/containerd/") ||
		strings.HasPrefix(pkg, "google.golang.org/grpc") || strings.HasPrefix(pkg, "github.com/golang-jwt/")
}

// The orchestrator side must stay light and pure Go on all six targets (linux, darwin, windows
// x amd64, arm64). Every target package must exist (a missing one fails the test, so a rename
// cannot disable the check). The go-sdk may be imported only by internal/mcpserver.
func TestOrchestratorDeps(t *testing.T) {
	requireGo(t)
	const mod = "github.com/mirastacklabs-ai/mirafleet/"
	targets := []string{"internal/orch", "internal/mcpserver", "integrations/claude-code/hook"}
	for _, goos := range []string{"linux", "darwin", "windows"} {
		for _, goarch := range []string{"amd64", "arm64"} {
			checkOrchestratorDeps(t, mod, targets, goos, goarch)
		}
	}
}

func checkOrchestratorDeps(t *testing.T, mod string, targets []string, goos, goarch string) {
	t.Helper()
	{
		for _, rel := range targets {
			if _, err := os.Stat("../../" + rel); err != nil {
				t.Errorf("%s/%s: target %s is missing: %v", goos, goarch, rel, err)
				continue
			}
			sawSDK := false
			for _, dep := range goListPkgs(t, goos, goarch, "-deps", "-f", "{{.ImportPath}}", "./../../"+rel) {
				if forbiddenForOrchestrator(dep) {
					t.Errorf("%s/%s: %s links %s", goos, goarch, rel, dep)
				}
				if dep == mcpSDK || strings.HasPrefix(dep, mcpSDK+"/") {
					sawSDK = true
				}
			}
			// Positive control: without it a broken filter would make the check pass vacuously.
			if rel == "internal/mcpserver" && !sawSDK {
				t.Errorf("%s/%s: internal/mcpserver links no go-sdk package: the dependency check proves nothing", goos, goarch)
			}
		}

		// go-sdk importers: no non-test package but internal/mcpserver may import it directly.
		// Test files are exempt on purpose: the rule keeps the SDK out of shipped code, and test
		// files never ship. Tests (e.g. the TestM5Done acceptance test) may use the SDK's MCP client
		// to drive the server. So this lists WITHOUT -test: the Imports field then covers only
		// non-test files, and _test / [pkg.test] variants are never visited.
		for _, line := range goListPkgs(t, goos, goarch, "-f", "{{.ImportPath}} {{join .Imports \",\"}}", "../../...") {
			f := strings.SplitN(line, " ", 2)
			if len(f) < 2 {
				continue
			}
			pkg := f[0]
			if pkg == mod+"internal/mcpserver" || strings.HasPrefix(pkg, mod+"internal/mcpserver/") {
				continue
			}
			for _, imp := range strings.FieldsFunc(f[1], func(r rune) bool { return r == ',' || r == ' ' }) {
				if imp == mcpSDK || strings.HasPrefix(imp, mcpSDK+"/") {
					t.Errorf("%s: %s imports %s; only internal/mcpserver may", goos, pkg, imp)
				}
			}
		}
	}
}

// M7 host-mode packages: central/runner-only. The orchestrator side (orch, mcpserver, hook) and
// the darwin/windows binaries must never link them. Packages that do not exist yet are tolerated
// (they simply never show up in the dependency lists), so the check bites as soon as they appear.
var m7ServerOnly = []string{"internal/workspace/hostx", "internal/secrets", "internal/workspace/broker", "internal/workspace/sshagent"}

func isM7ServerOnly(pkg string) bool {
	const mod = "github.com/mirastacklabs-ai/mirafleet/"
	for _, p := range m7ServerOnly {
		if pkg == mod+p || strings.HasPrefix(pkg, mod+p+"/") {
			return true
		}
	}
	return false
}

// serverSide lists the server-side stacks a darwin/windows binary must never link (H-19).
var serverSide = []string{"internal/service", "internal/runner", "internal/central"}

func isServerSide(pkg string) bool {
	const mod = "github.com/mirastacklabs-ai/mirafleet/"
	for _, p := range serverSide {
		if pkg == mod+p || strings.HasPrefix(pkg, mod+p+"/") {
			return true
		}
	}
	return false
}

func TestNoM7ServerPackagesOnOrchestratorOrNonLinux(t *testing.T) {
	requireGo(t)
	const hostx = "github.com/mirastacklabs-ai/mirafleet/internal/workspace/hostx"
	// Positive control: the linux binary links hostx on both architectures, otherwise the
	// non-linux checks below could pass vacuously.
	for _, goarch := range []string{"amd64", "arm64"} {
		found := false
		for _, pkg := range goDepsArch(t, "linux", goarch) {
			found = found || pkg == hostx
		}
		if !found {
			t.Errorf("linux/%s binary does not link %s: the non-linux check proves nothing", goarch, hostx)
		}
	}
	for _, goos := range []string{"darwin", "windows"} {
		for _, goarch := range []string{"amd64", "arm64"} {
			for _, pkg := range goDepsArch(t, goos, goarch) {
				if isM7ServerOnly(pkg) || isServerSide(pkg) {
					t.Errorf("%s/%s binary links %s", goos, goarch, pkg)
				}
			}
		}
	}
	targets := []string{"internal/orch", "internal/mcpserver", "integrations/claude-code/hook"}
	for _, goos := range []string{"linux", "darwin", "windows"} {
		for _, rel := range targets {
			for _, dep := range goListPkgs(t, goos, "amd64", "-deps", "-f", "{{.ImportPath}}", "./../../"+rel) {
				if isM7ServerOnly(dep) {
					t.Errorf("%s: %s links %s", goos, rel, dep)
				}
			}
		}
	}
}

const guiPkg = "github.com/mirastacklabs-ai/mirafleet/internal/gui"

// forbiddenForGUI lists what internal/gui must never import: the GUI talks to
// central only through gui.Backend (*service.Service), never to the store, registry or SSH side.
// direct is true for its own imports; the transitive closure (through internal/service, which
// legitimately uses exactly internal/central/registry) keeps every other one of these out too, plus
// containerd and grpc.
func forbiddenForGUI(pkg string, direct bool) bool {
	const mod = "github.com/mirastacklabs-ai/mirafleet/"
	paths := []string{"internal/store/sqlite", "internal/central", "internal/apiclient", "internal/orch", "internal/mcpserver"}
	for _, p := range paths {
		if pkg == mod+p || strings.HasPrefix(pkg, mod+p+"/") {
			if !direct && pkg == mod+"internal/central/registry" {
				continue
			}
			return true
		}
	}
	return strings.HasPrefix(pkg, "modernc.org/") || strings.HasPrefix(pkg, "github.com/containerd/") ||
		strings.HasPrefix(pkg, "google.golang.org/grpc")
}

// internal/gui imports only allowed packages, on linux (where it is wired) and elsewhere. The
// positive control proves the listing saw the package and its dependencies.
func TestGUIDeps(t *testing.T) {
	requireGo(t)
	for _, goos := range []string{"linux", "darwin", "windows"} {
		for _, dep := range goListPkgs(t, goos, "amd64", "-f", `{{join .Imports "\n"}}`, "./../../internal/gui") {
			if forbiddenForGUI(dep, true) {
				t.Errorf("%s: internal/gui imports %s", goos, dep)
			}
		}
		sawSelf, sawService := false, false
		for _, dep := range goListPkgs(t, goos, "amd64", "-deps", "-f", "{{.ImportPath}}", "./../../internal/gui") {
			if forbiddenForGUI(dep, false) {
				t.Errorf("%s: internal/gui links %s", goos, dep)
			}
			sawSelf = sawSelf || dep == guiPkg
			sawService = sawService || strings.HasSuffix(dep, "/internal/service")
		}
		if !sawSelf || !sawService {
			t.Errorf("%s: dependency listing of internal/gui is empty or incomplete: the check proves nothing", goos)
		}
	}
}

// darwin and windows binaries never link internal/gui; the linux binary does (positive control:
// central_linux.go wires it).
func TestGUILinkedOnlyOnLinux(t *testing.T) {
	requireGo(t)
	linked := func(goos string) bool {
		for _, pkg := range goDeps(t, goos) {
			if pkg == guiPkg {
				return true
			}
		}
		return false
	}
	if !linked("linux") {
		t.Fatal("linux binary does not link internal/gui: the non-linux check proves nothing")
	}
	for _, goos := range []string{"darwin", "windows"} {
		if linked(goos) {
			t.Errorf("%s binary links internal/gui", goos)
		}
	}
}
