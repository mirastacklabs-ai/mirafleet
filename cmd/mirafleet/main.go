// mirafleet: central | runner | central exec. See docs/ARCHITECTURE.md.
//
// Central init/run wiring lives in central_linux.go and the runner commands (join/run) in
// server_linux.go; server_other.go fails closed elsewhere, so darwin and windows binaries never
// link SQLite.
package main

import (
	"fmt"
	"os"
	"runtime"

	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/initproc"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "central":
		if err = requireServerOS("central"); err != nil {
			break
		}
		err = centralMain(os.Args[2:])
	case "runner":
		if err = requireServerOS("runner"); err != nil {
			break
		}
		err = runnerMain(os.Args[2:])
	case "mcp":
		err = mcpMain(os.Args[2:], os.Stdin, os.Stdout)
	case "workspace-init": // hidden: PID 1 inside workspace containers, not in usage
		err = initproc.Run()
	case "version":
		fmt.Println(version)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "mirafleet:", err)
		os.Exit(1)
	}
}

// requireServerOS refuses to start central or runner outside linux (see platform_*.go).
func requireServerOS(what string) error {
	if !serverSupported {
		return fmt.Errorf("mirafleet %s is supported on linux only (current: %s)", what, runtime.GOOS)
	}
	return nil
}

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  mirafleet central init [-data DIR] [-listen ADDR]
  mirafleet central run [-data DIR] [-listen ADDR]
  mirafleet central fleet create NAME [-mode actions|workspace] [-confirm]
  mirafleet central fleet list [-json]
  mirafleet central token create -fleet F [-labels k=v,...] [-approve] [-uses N] [-ttl DURATION]
  mirafleet central token list [-fleet F] [-json]
  mirafleet central token revoke ID
  mirafleet central runner list [-fleet F] [-json]
  mirafleet central runner approve|disable|enable|revoke FLEET/RUNNER
  mirafleet central operator code | rotate-secret | list [-json] | revoke NAME
  mirafleet central exec FLEET/RUNNER ACTION [k=v...] [-confirm FLEET]
  mirafleet central audit [-since ID] [-limit N] [-json]
  (admin commands take [-data DIR], default /var/lib/mirafleet/central)
  mirafleet runner join ADDR -name NAME [-data DIR] -token -
  mirafleet runner run [-data DIR] [-actions FILE]
  mirafleet mcp login ADDR -name NAME -code -
  mirafleet mcp api METHOD [JSON]
  mirafleet version
`)
	os.Exit(2)
}

func logf(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }
