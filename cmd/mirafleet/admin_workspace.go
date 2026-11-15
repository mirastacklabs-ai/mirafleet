package main

// Workspace additions to the central admin CLI (docs/M3.md §1.10): the workspace flags shared by
// `fleet create` and `fleet workspace set`, and `workspace list|kill`. Everything here goes
// through the adminCLI.call seam; nothing links SQLite or containerd.

import (
	"flag"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
)

// maxCLICPUs is the CLI's upper bound on -cpus: the store's cap, so the CLI never accepts a
// value the server would reject.
const maxCLICPUs = store.MaxCPUMilli / 1000

// wsFlags are the workspace flags of `fleet create` and `fleet workspace set`.
type wsFlags struct {
	fs       *flag.FlagSet
	executor *string
	image    *string
	images   *string
	pull     *string
	network  *string
	egress   *string
	cpus     *float64
	memory   *string
	pids     *int
	tmpfs    *string
	disk     *string
	idleTTL  *time.Duration
	maxTTL   *time.Duration
	execTO   *time.Duration
	execMax  *time.Duration
}

func addWSFlags(fs *flag.FlagSet) *wsFlags {
	return &wsFlags{
		fs:       fs,
		executor: fs.String("executor", "", "containerd (default), opensandbox or host"),
		image:    fs.String("image", "", "default image reference (must match -images)"),
		images:   fs.String("images", "", "image allowlist, comma separated: exact refs or repo@sha256:* / repo:*"),
		pull:     fs.String("pull", "", "if-not-present (default), always or never"),
		network:  fs.String("network", "", "none (default) or egress"),
		egress:   fs.String("egress", "", "egress allowlist, comma separated host[:port],*.domain:443 (empty clears)"),
		cpus:     fs.Float64("cpus", 0, "CPUs per workspace, e.g. 2 or 0.5"),
		memory:   fs.String("memory", "", "memory per workspace, e.g. 4GiB or 512MiB"),
		pids:     fs.Int("pids", 0, "pids limit per workspace"),
		tmpfs:    fs.String("tmpfs", "", "tmpfs size for /tmp, e.g. 512MiB"),
		disk:     fs.String("disk", "", "disk quota for /workspace, e.g. 10GiB"),
		idleTTL:  fs.Duration("idle-ttl", 0, "close a workspace unused for this long, e.g. 24h"),
		maxTTL:   fs.Duration("max-ttl", 0, "close a workspace after this long, e.g. 168h"),
		execTO:   fs.Duration("exec-timeout", 0, "default exec timeout, e.g. 2m"),
		execMax:  fs.Duration("exec-max", 0, "maximum exec timeout, e.g. 30m (at most 24h)"),
	}
}

// wsFlagNames are the flags above, for "was any of them given".
var wsFlagNames = []string{"executor", "image", "images", "pull", "network", "egress", "cpus", "memory",
	"pids", "tmpfs", "disk", "idle-ttl", "max-ttl", "exec-timeout", "exec-max"}

// given reports which workspace flags were set on the command line.
func (w *wsFlags) given() map[string]bool {
	set := map[string]bool{}
	w.fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	out := map[string]bool{}
	for _, n := range wsFlagNames {
		if set[n] {
			out[n] = true
		}
	}
	return out
}

var sizeMBRE = regexp.MustCompile(`^([0-9]{1,9})\s*(MiB|GiB|TiB)?$`)

// parseSizeMB parses "4GiB", "512MiB" or a bare number of MiB into whole MiB.
func parseSizeMB(flagName, s string) (int, error) {
	m := sizeMBRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, fmt.Errorf("%w: -%s %q: want a size such as 512MiB or 4GiB", errAdminUsage, flagName, s)
	}
	n, _ := strconv.Atoi(m[1])
	mult := map[string]int{"": 1, "MiB": 1, "GiB": 1 << 10, "TiB": 1 << 20}[m[2]]
	if n < 1 || n > math.MaxInt32/mult {
		return 0, fmt.Errorf("%w: -%s %q is out of range", errAdminUsage, flagName, s)
	}
	return n * mult, nil
}

func durationS(flagName string, d time.Duration) (int, error) {
	if d < time.Second || d%time.Second != 0 || d > 100*24*365*time.Hour {
		return 0, fmt.Errorf("%w: -%s %s must be a whole number of seconds, at least 1s", errAdminUsage, flagName, d)
	}
	return int(d / time.Second), nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// apply layers the given flags over base and returns the result. The base is never modified.
// For `fleet create` base is empty (the server fills documented defaults); for `fleet workspace
// set` it is the fleet's current config, so the result is the complete config to send.
func (w *wsFlags) apply(base api.WorkspaceConfig) (api.WorkspaceConfig, error) {
	c := base
	c.Images = append([]string(nil), base.Images...)
	c.Egress = append([]string(nil), base.Egress...)
	g := w.given()
	if g["executor"] {
		c.Executor = *w.executor
	}
	if g["image"] {
		c.DefaultImage = *w.image
	}
	if g["images"] {
		c.Images = splitList(*w.images)
	}
	if g["pull"] {
		c.Pull = *w.pull
	}
	if g["network"] {
		c.Network = *w.network
	}
	if g["egress"] {
		c.Egress = splitList(*w.egress)
	}
	if g["cpus"] {
		// Below 0.001 the millicpu value would round to 0, which the server reads as "default".
		// The negated form also rejects NaN (every NaN comparison is false).
		if !(*w.cpus >= 0.001 && *w.cpus <= maxCLICPUs) {
			return c, fmt.Errorf("%w: -cpus must be between 0.001 and %d", errAdminUsage, maxCLICPUs)
		}
		c.CPUMilli = int(math.Round(*w.cpus * 1000))
	}
	var err error
	for _, f := range []struct {
		name string
		set  bool
		raw  string
		dst  *int
	}{
		{"memory", g["memory"], *w.memory, &c.MemoryMB},
		{"tmpfs", g["tmpfs"], *w.tmpfs, &c.TmpfsMB},
		{"disk", g["disk"], *w.disk, &c.DiskMB},
	} {
		if f.set {
			if *f.dst, err = parseSizeMB(f.name, f.raw); err != nil {
				return c, err
			}
		}
	}
	if g["pids"] {
		if *w.pids < 1 {
			return c, fmt.Errorf("%w: -pids must be at least 1", errAdminUsage)
		}
		c.Pids = *w.pids
	}
	for _, f := range []struct {
		name string
		set  bool
		d    time.Duration
		dst  *int
	}{
		{"idle-ttl", g["idle-ttl"], *w.idleTTL, &c.IdleTTLS},
		{"max-ttl", g["max-ttl"], *w.maxTTL, &c.MaxTTLS},
		{"exec-timeout", g["exec-timeout"], *w.execTO, &c.ExecDefaultS},
		{"exec-max", g["exec-max"], *w.execMax, &c.ExecMaxS},
	} {
		if f.set {
			if *f.dst, err = durationS(f.name, f.d); err != nil {
				return c, err
			}
		}
	}
	return c, nil
}

// fleetWorkspace dispatches `central fleet workspace <sub>`.
func (c *adminCLI) fleetWorkspace(args []string) error {
	return c.group("fleet workspace", args, map[string]func([]string) error{"set": c.fleetWorkspaceSet})
}

// fleetWorkspaceSet reads the fleet's complete current config, applies the flags and sends the
// complete result: fleet.workspace_set is a full replace, so unset flags keep their values and a
// list such as -egress can be cleared by passing an empty value.
func (c *adminCLI) fleetWorkspaceSet(args []string) error {
	fs, data := c.flags("fleet workspace set")
	wf := addWSFlags(fs)
	creds := fs.String("credentials", "", "credentials to assign to the whole fleet, comma separated")
	pos, err := adminParse(fs, args)
	if err != nil {
		return err
	}
	if err := adminWantArgs("fleet workspace set", pos, 1, "one fleet NAME"); err != nil {
		return err
	}
	credNames, err := parseCredentialList(*creds, "workspace")
	if err != nil {
		return err
	}
	if len(wf.given()) == 0 && len(credNames) == 0 {
		return fmt.Errorf("%w: fleet workspace set: give at least one flag to change", errAdminUsage)
	}
	sock := adminSock(*data)
	if len(wf.given()) == 0 { // credentials only
		return c.attachCredentials(sock, pos[0], credNames)
	}
	var fl api.FleetsListResult
	if err := c.call(sock, api.MethodFleetsList, nil, &fl); err != nil {
		return err
	}
	var cur *api.WorkspaceConfig
	found := false
	for _, f := range fl.Fleets {
		if f.Name == pos[0] {
			found = true
			if f.Workspace != nil {
				cur = f.Workspace.Config
			}
		}
	}
	if !found {
		return fmt.Errorf("fleet %q not found", pos[0])
	}
	if cur == nil {
		return fmt.Errorf("fleet %q is not a workspace fleet (or central did not return its config)", pos[0])
	}
	next, err := wf.apply(*cur)
	if err != nil {
		return err
	}
	var res api.FleetWorkspaceSetResult
	if err := c.call(sock, api.MethodFleetWorkspaceSet, api.FleetWorkspaceSetParams{Fleet: pos[0], Workspace: next}, &res); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "fleet %s workspace config updated\n", res.Fleet.Name)
	return c.attachCredentials(sock, pos[0], credNames)
}

func (c *adminCLI) workspaceList(args []string) error {
	fs, data := c.flags("workspace list")
	fleet := fs.String("fleet", "", "only this fleet")
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := adminParse(fs, args)
	if err != nil {
		return err
	}
	if err := adminWantArgs("workspace list", pos, 0, "no positional arguments"); err != nil {
		return err
	}
	var res api.WorkspaceAdminListResult
	if err := c.call(adminSock(*data), api.MethodWorkspaceAdminList, api.WorkspaceAdminListParams{Fleet: *fleet}, &res); err != nil {
		return err
	}
	if *asJSON {
		return c.printJSON(res)
	}
	tw := c.table()
	fmt.Fprintln(tw, "ID\tFLEET\tRUNNER\tOPERATOR\tSESSION\tSTATE\tIMAGE\tLAST USED\tEXPIRES")
	for _, w := range res.Workspaces {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", w.ID, w.Fleet, w.Runner, w.Operator, w.Session,
			w.State, w.Image, adminTime(w.LastUsed), adminTime(w.Expires))
	}
	return tw.Flush()
}

func (c *adminCLI) workspaceKill(args []string) error {
	fs, data := c.flags("workspace kill")
	del := fs.Bool("delete", false, "also delete the session directory (host fleets)")
	pos, err := adminParse(fs, args)
	if err != nil {
		return err
	}
	if err := adminWantArgs("workspace kill", pos, 1, "one workspace ID"); err != nil {
		return err
	}
	var res api.WorkspaceKillResult
	if err := c.call(adminSock(*data), api.MethodWorkspaceKill, api.WorkspaceKillParams{ID: pos[0], Delete: *del}, &res); err != nil {
		return err
	}
	if res.State == "closing" {
		fmt.Fprintf(c.out, "workspace %s closing; the reaper will finish\n", pos[0])
		return nil
	}
	fmt.Fprintf(c.out, "workspace %s killed\n", pos[0])
	return nil
}
