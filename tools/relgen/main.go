// Command relgen is the release helper: it keeps every versioned file in lockstep with the git tag
// and (in later steps) renders the npm, Homebrew, registry, wiki and licence artifacts.
//
// It is stdlib-only and never linked into the mirafleet binary. Each subcommand lives in its own
// file and registers itself from init() via register, so parallel steps never share a file.
//
//	relgen [-root DIR] bump X.Y.Z
//	relgen [-root DIR] check vX.Y.Z
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
)

// env is what a subcommand gets: the repo root and the output streams.
type env struct {
	Root     string
	Out, Err io.Writer
}

// command is one relgen subcommand.
type command struct {
	Name    string
	Summary string
	Usage   string // arguments, for the usage text
	Run     func(e env, args []string) error
}

var commands = map[string]command{}

// register adds a subcommand; call it from init(). A duplicate name is a programming error.
func register(c command) {
	if _, dup := commands[c.Name]; dup {
		panic("relgen: duplicate command " + c.Name)
	}
	commands[c.Name] = c
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: relgen [-root DIR] <command> [args]")
	names := make([]string, 0, len(commands))
	for n := range commands {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		c := commands[n]
		fmt.Fprintf(w, "  %-10s %s\n", n+" "+c.Usage, c.Summary)
	}
}

// run is main without the process exit, so tests can drive it. It returns the exit code.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("relgen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", ".", "repository root")
	fs.Usage = func() { usage(stderr) }
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() == 0 {
		usage(stderr)
		return 2
	}
	c, ok := commands[fs.Arg(0)]
	if !ok {
		fmt.Fprintf(stderr, "relgen: unknown command %q\n", fs.Arg(0))
		usage(stderr)
		return 2
	}
	if err := c.Run(env{Root: *root, Out: stdout, Err: stderr}, fs.Args()[1:]); err != nil {
		fmt.Fprintf(stderr, "relgen %s: %v\n", c.Name, err)
		return 1
	}
	return 0
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
