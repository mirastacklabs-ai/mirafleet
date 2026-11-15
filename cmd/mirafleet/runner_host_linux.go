package main

// Host-mode additions to the runner commands (docs/M7.md §3.3, §3.4): XDG defaults so a runner
// started by an ordinary user needs nothing outside its home, and `runner host prune`.

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/hostx"
)

// resolveRunnerData picks the runner data directory. An explicit -data always wins, and root
// keeps the system default; any other user defaults to $XDG_CONFIG_HOME/mirafleet (created 0700).
func resolveRunnerData(given bool, data string, uid int, paths func() (hostx.Paths, error)) (string, error) {
	if given || uid == 0 {
		return data, nil
	}
	p, err := paths()
	if err != nil {
		return "", err
	}
	if err := p.Ensure(); err != nil {
		return "", err
	}
	return p.Config, nil
}

// runnerDataFlag resolves the -data flag of fs after parsing; given reports an explicit -data.
func runnerDataFlag(fs *flag.FlagSet, data string) (dir string, given bool, err error) {
	fs.Visit(func(f *flag.Flag) { given = given || f.Name == "data" })
	dir, err = resolveRunnerData(given, data, os.Geteuid(), hostx.DefaultPaths)
	return dir, given, err
}

// runnerHost dispatches `mirafleet runner host <sub>`.
func runnerHost(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] != "prune" {
		return errors.New("usage: mirafleet runner host prune [-older-than D] [-yes] [-root DIR]")
	}
	return runnerHostPrune(args[1:], out, hostx.DefaultPaths)
}

// runnerHostPrune lists (dry run, the default) or deletes (-yes) this runner user's session
// directories unused for -older-than (default: the 30 d cleanup policy). Directories in use are
// skipped; a shared workdir is never touched.
func runnerHostPrune(args []string, out io.Writer, paths func() (hostx.Paths, error)) error {
	fs := flag.NewFlagSet("runner host prune", flag.ContinueOnError)
	older := fs.Duration("older-than", 0, "delete directories unused for this long, e.g. 720h (default: 30 days)")
	yes := fs.Bool("yes", false, "delete; without it prune only lists what it would delete")
	root := fs.String("root", "", "parent of the session directories (default: the XDG data dir; set it to workspace.yaml host.root)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return errors.New("runner host prune takes no arguments")
	}
	if *older < 0 {
		return errors.New("runner host prune: -older-than must not be negative")
	}
	p, err := paths()
	if err != nil {
		return err
	}
	if *root == "" {
		*root = p.SessionsRoot()
	}
	m, err := hostx.NewManager(hostx.SessionsConfig{Root: *root, StateDir: p.State})
	if err != nil {
		return err
	}
	rep := m.Prune(hostx.PruneOptions{OlderThan: *older, Yes: *yes})
	now := time.Now()
	shown := rep.Candidates
	if *yes {
		shown = rep.Removed
	}
	if len(shown) == 0 && len(rep.Skipped) == 0 && len(rep.Errors) == 0 {
		fmt.Fprintln(out, "nothing to prune")
		return nil
	}
	if len(shown) > 0 {
		tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
		fmt.Fprintln(tw, "SESSION\tDIR\tLAST USED\tSIZE")
		for _, in := range shown {
			fmt.Fprintf(tw, "%s\t%s\t%s ago\t%d\n", in.ID, in.Dir, now.Sub(in.LastUsed).Round(time.Minute), in.Size)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	for _, in := range rep.Skipped {
		fmt.Fprintf(out, "skipped %s: in use\n", in.ID)
	}
	if *yes {
		fmt.Fprintf(out, "deleted %d session director(ies)\n", len(rep.Removed))
	} else if len(rep.Candidates) > 0 {
		fmt.Fprintf(out, "%d session director(ies) would be deleted; re-run with -yes to do it\n", len(rep.Candidates))
	}
	if len(rep.Errors) > 0 {
		for _, e := range rep.Errors {
			fmt.Fprintf(out, "error: %v\n", e)
		}
		return fmt.Errorf("%d error(s) while pruning", len(rep.Errors))
	}
	return nil
}
