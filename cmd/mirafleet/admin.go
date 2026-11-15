package main

// The central admin CLI: every subcommand except `central init` and `central run` is a thin
// client of the running central's local admin socket (<data>/admin.sock, internal/api framing,
// docs/M2.md §1.5). The socket call is a seam (adminCLI.call) so argument parsing is tested
// without a central. Nothing here links SQLite; central init/run live in server_linux.go.

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/central"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
)

const defaultCentralData = "/var/lib/mirafleet/central"

// adminCLI runs one central subcommand.
type adminCLI struct {
	call func(sock, method string, params, result any) error // central.AdminCall, or a fake
	out  io.Writer
	err  io.Writer
	exit func(code int) // exec propagates the action's exit code
	// callBody is call for a method with one body frame behind the request (credential.set); in
	// is where credential values are read from (stdin, never argv).
	callBody func(sock, method string, params any, body []byte, result any) error
	in       io.Reader
}

func defaultAdminCLI() *adminCLI {
	return &adminCLI{call: central.AdminCall, out: os.Stdout, err: os.Stderr, exit: os.Exit,
		callBody: adminCallBody, in: os.Stdin}
}

func adminSock(data string) string { return filepath.Join(data, "admin.sock") }

var errAdminUsage = errors.New("usage error")

// centralMain dispatches `mirafleet central <sub> ...`.
func centralMain(args []string) error {
	if len(args) == 0 {
		usage()
	}
	switch args[0] {
	case "init":
		return centralInit(args[1:])
	case "run":
		return centralRun(args[1:])
	case "keygen":
		return centralKeygen(args[1:], os.Stdout, os.Stderr)
	case "secrets":
		return centralSecrets(args[1:], os.Stdin, os.Stdout)
	}
	return defaultAdminCLI().run(args)
}

func (c *adminCLI) run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: missing subcommand", errAdminUsage)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "fleet":
		return c.group(sub, rest, map[string]func([]string) error{"create": c.fleetCreate, "list": c.fleetList,
			"workspace": c.fleetWorkspace, "set": c.fleetSet})
	case "gui":
		return c.group(sub, rest, map[string]func([]string) error{"reset-admin": c.guiResetAdmin})
	case "workspace":
		return c.group(sub, rest, map[string]func([]string) error{"list": c.workspaceList, "kill": c.workspaceKill})
	case "token":
		return c.group(sub, rest, map[string]func([]string) error{"create": c.tokenCreate, "list": c.tokenList, "revoke": c.tokenRevoke})
	case "runner":
		return c.group(sub, rest, map[string]func([]string) error{"list": c.runnerList,
			"approve": c.lifecycle(api.OpApprove), "disable": c.lifecycle(api.OpDisable),
			"enable": c.lifecycle(api.OpEnable), "revoke": c.lifecycle(api.OpRevoke)})
	case "operator":
		return c.group(sub, rest, map[string]func([]string) error{"code": c.operatorCode,
			"rotate-secret": c.operatorRotate, "list": c.operatorList, "revoke": c.operatorRevoke})
	case "credential":
		return c.credential(rest)
	case "exec":
		return c.exec(rest)
	case "audit":
		return c.audit(rest)
	}
	return fmt.Errorf("%w: unknown central subcommand %q", errAdminUsage, sub)
}

func (c *adminCLI) group(name string, args []string, subs map[string]func([]string) error) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: central %s: missing subcommand", errAdminUsage, name)
	}
	f, ok := subs[args[0]]
	if !ok {
		return fmt.Errorf("%w: unknown subcommand central %s %s", errAdminUsage, name, args[0])
	}
	return f(args[1:])
}

// adminFlags builds a FlagSet with the common -data flag.
func (c *adminCLI) flags(name string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet("central "+name, flag.ContinueOnError)
	fs.SetOutput(c.err)
	return fs, fs.String("data", defaultCentralData, "central data directory")
}

// adminParse parses args allowing flags after positionals ("fleet create web -mode workspace")
// and returns the positionals.
func adminParse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, fmt.Errorf("%w: %v", errAdminUsage, err)
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func adminWantArgs(name string, pos []string, n int, what string) error {
	if len(pos) != n {
		return fmt.Errorf("%w: central %s takes %s", errAdminUsage, name, what)
	}
	return nil
}

func (c *adminCLI) printJSON(v any) error {
	enc := json.NewEncoder(c.out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func (c *adminCLI) table() *tabwriter.Writer { return tabwriter.NewWriter(c.out, 0, 8, 2, ' ', 0) }

func adminTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

func adminTimePtr(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return adminTime(*t)
}

func adminLabels(m map[string]string) string {
	if len(m) == 0 {
		return "-"
	}
	kv := make([]string, 0, len(m))
	for k, v := range m {
		kv = append(kv, k+"="+v)
	}
	sort.Strings(kv) // stable output
	return strings.Join(kv, ",")
}

// adminParseLabels parses "k=v,k=v" (empty is no labels).
func adminParseLabels(s string) (map[string]string, error) {
	if s == "" {
		return nil, nil
	}
	m := map[string]string{}
	for _, kv := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("%w: label %q is not k=v", errAdminUsage, kv)
		}
		m[k] = v
	}
	return m, nil
}

func (c *adminCLI) fleetCreate(args []string) error {
	fs, data := c.flags("fleet create")
	mode := fs.String("mode", "actions", "fleet mode: actions or workspace")
	confirm := fs.Bool("confirm", false, "guarded fleet: exec needs -confirm FLEET")
	wf := addWSFlags(fs)
	creds := fs.String("credentials", "", "credentials to assign to the whole fleet, comma separated (workspace fleets)")
	pos, err := adminParse(fs, args)
	if err != nil {
		return err
	}
	if err := adminWantArgs("fleet create", pos, 1, "one NAME"); err != nil {
		return err
	}
	credNames, err := parseCredentialList(*creds, *mode)
	if err != nil {
		return err
	}
	params := api.FleetCreateParams{Name: pos[0], Mode: *mode, Confirm: *confirm}
	if len(wf.given()) > 0 {
		if *mode != "workspace" {
			return fmt.Errorf("%w: fleet create: workspace flags need -mode workspace", errAdminUsage)
		}
		cfg, err := wf.apply(api.WorkspaceConfig{})
		if err != nil {
			return err
		}
		params.Workspace = &cfg
	}
	var res api.FleetCreateResult
	if err := c.call(adminSock(*data), api.MethodFleetCreate, params, &res); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "fleet %s created (mode %s, confirm %v)\n", res.Fleet.Name, res.Fleet.Mode, res.Fleet.Confirm)
	return c.attachCredentials(adminSock(*data), res.Fleet.Name, credNames)
}

func (c *adminCLI) fleetList(args []string) error {
	fs, data := c.flags("fleet list")
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := adminParse(fs, args)
	if err != nil {
		return err
	}
	if err := adminWantArgs("fleet list", pos, 0, "no arguments"); err != nil {
		return err
	}
	var res api.FleetsListResult
	if err := c.call(adminSock(*data), api.MethodFleetsList, nil, &res); err != nil {
		return err
	}
	if *asJSON {
		return c.printJSON(res)
	}
	tw := c.table()
	fmt.Fprintln(tw, "FLEET\tMODE\tCONFIRM\tRUNNERS")
	for _, f := range res.Fleets {
		fmt.Fprintf(tw, "%s\t%s\t%v\t%d\n", f.Name, f.Mode, f.Confirm, f.Runners)
	}
	return tw.Flush()
}

func (c *adminCLI) tokenCreate(args []string) error {
	fs, data := c.flags("token create")
	fleet := fs.String("fleet", "", "fleet the runner joins (required)")
	labels := fs.String("labels", "", "labels, k=v,k=v")
	approve := fs.Bool("approve", false, "runners join as active instead of pending")
	uses := fs.Int("uses", 0, "number of uses (0 = single use)")
	ttl := fs.Duration("ttl", 0, "lifetime, e.g. 24h (0 = default 24h)")
	pos, err := adminParse(fs, args)
	if err != nil {
		return err
	}
	if err := adminWantArgs("token create", pos, 0, "no positional arguments"); err != nil {
		return err
	}
	if *fleet == "" {
		return fmt.Errorf("%w: token create: -fleet is required", errAdminUsage)
	}
	if *uses < 0 || *ttl < 0 {
		return fmt.Errorf("%w: token create: -uses and -ttl must not be negative", errAdminUsage)
	}
	if *ttl > 0 && *ttl < time.Second {
		return fmt.Errorf("%w: token create: -ttl must be at least 1s", errAdminUsage)
	}
	lm, err := adminParseLabels(*labels)
	if err != nil {
		return err
	}
	var res api.TokenCreateResult
	err = c.call(adminSock(*data), api.MethodTokenCreate, api.TokenCreateParams{
		Fleet: *fleet, Labels: lm, Approve: *approve, Uses: *uses, TTLS: int(*ttl / time.Second)}, &res)
	if err != nil {
		return err
	}
	// The token is shown once, alone on stdout; everything else goes to stderr.
	fmt.Fprintln(c.out, res.Token)
	fmt.Fprintf(c.err, "token %s for fleet %s, expires %s; shown once, store it now\n", res.ID, *fleet, adminTime(res.Expires))
	return nil
}

func (c *adminCLI) tokenList(args []string) error {
	fs, data := c.flags("token list")
	fleet := fs.String("fleet", "", "only this fleet")
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := adminParse(fs, args)
	if err != nil {
		return err
	}
	if err := adminWantArgs("token list", pos, 0, "no positional arguments"); err != nil {
		return err
	}
	var res api.TokenListResult
	if err := c.call(adminSock(*data), api.MethodTokenList, api.TokenListParams{Fleet: *fleet}, &res); err != nil {
		return err
	}
	if *asJSON {
		return c.printJSON(res)
	}
	tw := c.table()
	fmt.Fprintln(tw, "ID\tFLEET\tAPPROVE\tUSES\tEXPIRES\tREVOKED\tLABELS")
	for _, t := range res.Tokens {
		fmt.Fprintf(tw, "%s\t%s\t%v\t%d/%d\t%s\t%v\t%s\n", t.ID, t.Fleet, t.Approve, t.Uses, t.MaxUses,
			adminTime(t.Expires), t.Revoked, adminLabels(t.Labels))
	}
	return tw.Flush()
}

func (c *adminCLI) tokenRevoke(args []string) error {
	fs, data := c.flags("token revoke")
	pos, err := adminParse(fs, args)
	if err != nil {
		return err
	}
	if err := adminWantArgs("token revoke", pos, 1, "one token ID"); err != nil {
		return err
	}
	if err := c.call(adminSock(*data), api.MethodTokenRevoke, api.TokenRevokeParams{ID: pos[0]}, nil); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "token %s revoked\n", pos[0])
	return nil
}

func (c *adminCLI) runnerList(args []string) error {
	fs, data := c.flags("runner list")
	fleet := fs.String("fleet", "", "only this fleet")
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := adminParse(fs, args)
	if err != nil {
		return err
	}
	if err := adminWantArgs("runner list", pos, 0, "no positional arguments"); err != nil {
		return err
	}
	sock := adminSock(*data)
	fleets := []string{*fleet}
	if *fleet == "" {
		var fl api.FleetsListResult
		if err := c.call(sock, api.MethodFleetsList, nil, &fl); err != nil {
			return err
		}
		fleets = fleets[:0]
		for _, f := range fl.Fleets {
			fleets = append(fleets, f.Name)
		}
	}
	type fleetRunners struct {
		Fleet   string           `json:"fleet"`
		Runners []api.RunnerInfo `json:"runners"`
	}
	all := make([]fleetRunners, 0, len(fleets))
	for _, f := range fleets {
		var rl api.RunnersListResult
		if err := c.call(sock, api.MethodRunnersList, api.RunnersListParams{Fleet: f}, &rl); err != nil {
			return err
		}
		all = append(all, fleetRunners{Fleet: f, Runners: rl.Runners})
	}
	if *asJSON {
		if *fleet != "" {
			return c.printJSON(api.RunnersListResult{Runners: all[0].Runners}) // the api DTO
		}
		return c.printJSON(all)
	}
	tw := c.table()
	fmt.Fprintln(tw, "RUNNER\tSTATUS\tONLINE\tLAST SEEN\tLABELS")
	for _, fr := range all {
		for _, r := range fr.Runners {
			fmt.Fprintf(tw, "%s/%s\t%s\t%v\t%s\t%s\n", fr.Fleet, r.Name, r.Status, r.Online, adminTimePtr(r.LastSeen), adminLabels(r.Labels))
		}
	}
	return tw.Flush()
}

func (c *adminCLI) lifecycle(op string) func([]string) error {
	return func(args []string) error {
		fs, data := c.flags("runner " + op)
		pos, err := adminParse(fs, args)
		if err != nil {
			return err
		}
		if err := adminWantArgs("runner "+op, pos, 1, "one FLEET/RUNNER"); err != nil {
			return err
		}
		ref, err := store.ParseRef(pos[0])
		if err != nil {
			return fmt.Errorf("%w: %v", errAdminUsage, err)
		}
		var ri api.RunnerInfo
		if err := c.call(adminSock(*data), api.MethodRunnerLifecycle,
			api.RunnerLifecycleParams{Fleet: ref.Fleet, Runner: ref.Runner, Op: op}, &ri); err != nil {
			return err
		}
		fmt.Fprintf(c.out, "%s/%s: %s\n", ref.Fleet, ref.Runner, ri.Status)
		return nil
	}
}

func (c *adminCLI) operatorCode(args []string) error {
	fs, data := c.flags("operator code")
	pos, err := adminParse(fs, args)
	if err != nil {
		return err
	}
	if err := adminWantArgs("operator code", pos, 0, "no arguments"); err != nil {
		return err
	}
	var res api.OperatorCodeResult
	if err := c.call(adminSock(*data), api.MethodOperatorCode, nil, &res); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "code:      %s\nvalid for: %ds\nhost key:  %s\n", res.Code, res.SecondsLeft, res.HostFingerprint)
	return nil
}

func (c *adminCLI) operatorRotate(args []string) error {
	fs, data := c.flags("operator rotate-secret")
	pos, err := adminParse(fs, args)
	if err != nil {
		return err
	}
	if err := adminWantArgs("operator rotate-secret", pos, 0, "no arguments"); err != nil {
		return err
	}
	if err := c.call(adminSock(*data), api.MethodOperatorRotateSecret, nil, nil); err != nil {
		return err
	}
	fmt.Fprintln(c.out, "registration secret rotated; every outstanding code is invalid")
	return nil
}

func (c *adminCLI) operatorList(args []string) error {
	fs, data := c.flags("operator list")
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := adminParse(fs, args)
	if err != nil {
		return err
	}
	if err := adminWantArgs("operator list", pos, 0, "no arguments"); err != nil {
		return err
	}
	var res api.OperatorListResult
	if err := c.call(adminSock(*data), api.MethodOperatorList, nil, &res); err != nil {
		return err
	}
	if *asJSON {
		return c.printJSON(res)
	}
	tw := c.table()
	fmt.Fprintln(tw, "NAME\tSTATUS\tFINGERPRINT\tLAST SEEN")
	for _, o := range res.Operators {
		fp := o.Fingerprint
		if fp == "" {
			fp = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", o.Name, o.Status, fp, adminTimePtr(o.LastSeen))
	}
	return tw.Flush()
}

func (c *adminCLI) operatorRevoke(args []string) error {
	fs, data := c.flags("operator revoke")
	pos, err := adminParse(fs, args)
	if err != nil {
		return err
	}
	if err := adminWantArgs("operator revoke", pos, 1, "one operator NAME"); err != nil {
		return err
	}
	if err := c.call(adminSock(*data), api.MethodOperatorRevoke, api.OperatorRevokeParams{Name: pos[0]}, nil); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "operator %s revoked\n", pos[0])
	return nil
}

// exec runs an action on a runner through the running central.
func (c *adminCLI) exec(args []string) error {
	fs, data := c.flags("exec")
	confirm := fs.String("confirm", "", "fleet name, required for guarded fleets")
	pos, err := adminParse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 2 {
		return fmt.Errorf("%w: central exec takes FLEET/RUNNER ACTION [k=v...]", errAdminUsage)
	}
	params := map[string]string{}
	for _, kv := range pos[2:] {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("%w: parameter %q is not k=v", errAdminUsage, kv)
		}
		params[k] = v
	}
	ref, err := store.ParseRef(pos[0])
	if err != nil {
		return fmt.Errorf("%w: %v", errAdminUsage, err)
	}
	var res api.ActionRunResult
	err = c.call(adminSock(*data), api.MethodActionRun, api.ActionRunParams{
		Fleet: ref.Fleet, Runner: ref.Runner, Action: pos[1], Params: params,
		TimeoutS: central.AdminActionTimeoutS, Confirm: *confirm}, &res)
	if err != nil {
		return err
	}
	fmt.Fprint(c.out, res.Stdout)
	fmt.Fprint(c.err, res.Stderr)
	if res.ExitCode != 0 {
		c.exit(res.ExitCode)
	}
	return nil
}

func (c *adminCLI) audit(args []string) error {
	fs, data := c.flags("audit")
	since := fs.Int64("since", 0, "only events with ID greater than this")
	limit := fs.Int("limit", 0, "maximum events (0 = server default)")
	asJSON := fs.Bool("json", false, "print JSON")
	before := fs.Int64("before", 0, "only events with ID less than this")
	fleet := fs.String("fleet", "", "only this fleet")
	runner := fs.String("runner", "", "only this runner")
	actor := fs.String("actor", "", "only this actor")
	source := fs.String("source", "", "only this source (cli, gui, api, ...)")
	outcome := fs.String("outcome", "", "only this outcome")
	action := fs.String("action", "", "only this action, e.g. runner.revoke")
	prefix := fs.Bool("action-prefix", false, "treat -action as a prefix, e.g. -action gui. -action-prefix")
	workspace := fs.String("workspace", "", "only this workspace ID")
	requestID := fs.String("request-id", "", "only this request ID")
	fromS := fs.String("from", "", "only events at or after this time (RFC 3339 or YYYY-MM-DD)")
	toS := fs.String("to", "", "only events at or before this time (RFC 3339 or YYYY-MM-DD)")
	pos, err := adminParse(fs, args)
	if err != nil {
		return err
	}
	if err := adminWantArgs("audit", pos, 0, "no positional arguments"); err != nil {
		return err
	}
	if *since < 0 || *limit < 0 || *before < 0 {
		return fmt.Errorf("%w: audit: -since, -before and -limit must not be negative", errAdminUsage)
	}
	if *prefix && *action == "" {
		return fmt.Errorf("%w: audit: -action-prefix needs -action", errAdminUsage)
	}
	from, err := auditTime("from", *fromS)
	if err != nil {
		return err
	}
	to, err := auditTime("to", *toS)
	if err != nil {
		return err
	}
	if from != nil && to != nil && !from.Before(*to) {
		return fmt.Errorf("%w: audit: -from must be before -to", errAdminUsage)
	}
	var res api.AuditListResult
	if err := c.call(adminSock(*data), api.MethodAuditList, api.AuditListParams{SinceID: *since, Limit: *limit,
		BeforeID: *before, Fleet: *fleet, Runner: *runner, Actor: *actor, Source: *source, Outcome: *outcome,
		Action: *action, ActionPrefix: *prefix, Workspace: *workspace, RequestID: *requestID, From: from, To: to}, &res); err != nil {
		return err
	}
	if *asJSON {
		return c.printJSON(res)
	}
	tw := c.table()
	fmt.Fprintln(tw, "ID\tTIME\tSOURCE\tACTOR\tACTION\tTARGET\tOUTCOME\tMESSAGE")
	for _, e := range res.Events {
		target := e.Fleet
		if e.Runner != "" {
			target += "/" + e.Runner
		}
		if target == "" {
			target = e.Subject
		}
		if target == "" {
			target = "-"
		}
		outcome := e.Outcome
		if outcome == "" {
			outcome = "-"
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", e.ID, adminTime(e.Time), e.Source,
			e.ActorKind+":"+e.Actor, e.Action, target, outcome, e.Message)
	}
	return tw.Flush()
}
