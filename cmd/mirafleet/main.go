// mirafleet: central | runner | central exec. See docs/ARCHITECTURE.md.
//
// Central and runner wiring lives in server_linux.go (server_other.go fails closed elsewhere), so
// darwin and windows binaries never link SQLite.
package main

import (
	"fmt"
	"os"
	"runtime"
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
		if len(os.Args) > 2 && os.Args[2] == "exec" {
			err = centralExec(os.Args[3:])
		} else {
			err = centralRun(os.Args[2:])
		}
	case "runner":
		if err = requireServerOS("runner"); err != nil {
			break
		}
		err = runnerRun(os.Args[2:])
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
  mirafleet central [-data DIR] [-listen ADDR] [-init]
  mirafleet central exec [-data DIR] [-confirm FLEET] <fleet/runner> <action> [k=v...]
  mirafleet runner  [-data DIR] [-central ADDR] [-name NAME] [-actions FILE] [-labels k=v,...] [-init]
  mirafleet version
`)
	os.Exit(2)
}

func logf(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }
