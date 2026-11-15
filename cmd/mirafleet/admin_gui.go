package main

// M4 admin commands: `central gui reset-admin`, `central fleet set NAME -confirm=BOOL` and the
// audit filter flags (docs/M4.md 2.3, 3.3, 3.8). All are thin clients of the admin socket.

import (
	"flag"
	"fmt"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
)

// guiResetAdmin asks central for a fresh GUI setup token. The token is the only thing on stdout
// (shown once, never logged); the explanation goes to stderr.
func (c *adminCLI) guiResetAdmin(args []string) error {
	fs, data := c.flags("gui reset-admin")
	pos, err := adminParse(fs, args)
	if err != nil {
		return err
	}
	if err := adminWantArgs("gui reset-admin", pos, 0, "no arguments"); err != nil {
		return err
	}
	var res api.GUIResetAdminResult
	if err := c.call(adminSock(*data), api.MethodGUIResetAdmin, nil, &res); err != nil {
		return err
	}
	fmt.Fprintln(c.out, res.SetupToken)
	fmt.Fprintln(c.err, "GUI admin reset: all GUI sessions ended. Open the GUI and paste this setup token (valid 24h, shown once).")
	return nil
}

// fleetSet changes a fleet setting; today only the guard flag: `fleet set NAME -confirm=BOOL`.
func (c *adminCLI) fleetSet(args []string) error {
	fs, data := c.flags("fleet set")
	confirm := fs.Bool("confirm", false, "guarded fleet: exec needs -confirm FLEET (use -confirm=true|false)")
	pos, err := adminParse(fs, args)
	if err != nil {
		return err
	}
	if err := adminWantArgs("fleet set", pos, 1, "one NAME"); err != nil {
		return err
	}
	given := false
	fs.Visit(func(f *flag.Flag) { given = given || f.Name == "confirm" })
	if !given {
		return fmt.Errorf("%w: fleet set: -confirm=BOOL is required", errAdminUsage)
	}
	var res api.FleetSetConfirmResult
	if err := c.call(adminSock(*data), api.MethodFleetSetConfirm,
		api.FleetSetConfirmParams{Fleet: pos[0], Confirm: *confirm}, &res); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "fleet %s: confirm %v\n", res.Fleet.Name, res.Fleet.Confirm)
	return nil
}

// auditTime parses a -from/-to value (RFC 3339, or a plain date meaning 00:00 UTC).
func auditTime(name, s string) (*time.Time, error) {
	if s == "" {
		return nil, nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return &t, nil
		}
	}
	return nil, fmt.Errorf("%w: audit: -%s %q is not RFC 3339 (2026-11-14T10:00:00Z) or YYYY-MM-DD", errAdminUsage, name, s)
}
