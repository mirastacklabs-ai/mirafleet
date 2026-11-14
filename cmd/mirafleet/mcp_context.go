package main

import (
	"errors"
	"fmt"
	"io"

	"github.com/mirastacklabs-ai/mirafleet/internal/opconf"
)

// mcpContext implements `mirafleet mcp context list|use NAME|remove NAME`. It prints names,
// addresses and default fleets only: never key material or fingerprints.
func mcpContext(args []string, stdout io.Writer) error {
	const usage = "usage: mirafleet mcp context list | use NAME | remove NAME"
	if len(args) == 0 {
		return errors.New(usage)
	}
	dir, err := opconf.Dir()
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return errors.New(usage)
		}
		c, err := opconf.LoadContexts(dir)
		if err != nil {
			return err
		}
		if len(c.Contexts) == 0 {
			fmt.Fprintln(stdout, "no contexts; run `mirafleet mcp login ADDR -name NAME -code -`")
			return nil
		}
		for _, x := range c.Contexts {
			mark := " "
			if x.Name == c.Current {
				mark = "*"
			}
			fl := x.DefaultFleet
			if fl == "" {
				fl = "-"
			}
			fmt.Fprintf(stdout, "%s %s\t%s\tdefault_fleet=%s\n", mark, x.Name, x.Addr, fl)
		}
		return nil
	case "use", "remove":
		if len(args) != 2 {
			return errors.New(usage)
		}
		if args[0] == "use" {
			err = opconf.UseContext(dir, args[1])
		} else {
			err = opconf.RemoveContext(dir, args[1])
		}
		if err != nil {
			return err
		}
		if args[0] == "use" {
			fmt.Fprintf(stdout, "current context: %s\n", args[1])
		} else {
			fmt.Fprintf(stdout, "removed context %s\n", args[1])
		}
		return nil
	}
	return fmt.Errorf("unknown context command %q (want list, use or remove)", args[0])
}
