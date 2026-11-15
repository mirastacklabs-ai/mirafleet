package main

// `mirafleet central credential ...` (docs/M7.md §4.1): thin clients of the admin socket. The
// secret value is read from stdin only (never a flag, so never in argv or the process list) and
// travels as one body frame right behind the request; params, output and errors carry metadata
// only. Nothing here links the secrets package or SQLite.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// adminCallBody is the admin-socket call for methods that carry a body frame (credential.set):
// the request frame, then one body frame, then the framed response.
func adminCallBody(sock, method string, params any, body []byte, result any) error {
	rawParams, err := json.Marshal(params)
	if err != nil {
		return err
	}
	id := fmt.Sprintf("%x", time.Now().UnixNano())
	payload, err := json.Marshal(api.Request{V: api.Version, ID: id, Method: method, Params: rawParams})
	if err != nil {
		return err
	}
	if len(payload) > api.MaxRequestBytes {
		return api.Errorf(api.CodeTooLarge, "request exceeds %d bytes", api.MaxRequestBytes)
	}
	c, err := net.DialTimeout("unix", sock, 5*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(60 * time.Second))
	if err := api.WriteFrame(c, payload); err != nil {
		return err
	}
	werr := api.WriteBody(c, body) // a refusal may close the socket first: the response wins
	b, err := api.ReadFrame(c, api.MaxResponseBytes)
	if err != nil {
		if werr != nil {
			err = werr
		}
		return err
	}
	var resp api.Response
	if err := json.Unmarshal(b, &resp); err != nil {
		return err
	}
	if resp.Error != nil {
		return resp.Error
	}
	if resp.ID != id {
		return errors.New("admin: response id mismatch")
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(resp.Result, result)
}

func (c *adminCLI) credential(args []string) error {
	return c.group("credential", args, map[string]func([]string) error{
		"set": c.credentialSet, "list": c.credentialList, "delete": c.credentialDelete,
		"assign": c.credentialAssign(true), "unassign": c.credentialAssign(false), "rotate": c.credentialRotate})
}

// parseExpires accepts RFC 3339 or a plain date (midnight UTC).
func parseExpires(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("%w: -expires %q: want RFC 3339 (2027-01-31T00:00:00Z) or a date (2027-01-31)", errAdminUsage, s)
}

// readKnownHosts reads a known_hosts file: one pinned key per line, blanks and comments skipped.
func readKnownHosts(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: -known-hosts: %v", errAdminUsage, err)
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimRight(l, " \t\r")
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		out = append(out, l)
	}
	return out, nil
}

// readCredentialValue reads the secret from the CLI's stdin. An ssh key is kept byte-exact;
// tokens and passwords lose trailing whitespace (`echo tok |` must not add a newline to a header).
func (c *adminCLI) readCredentialValue(kind string) ([]byte, error) {
	if c.in == nil {
		return nil, errors.New("no stdin to read the credential value from")
	}
	if f, ok := c.in.(*os.File); ok {
		if st, err := f.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 {
			return nil, fmt.Errorf("%w: credential set reads the value from stdin; pipe it in (`... < keyfile`), it is never taken from a flag", errAdminUsage)
		}
	}
	b, err := io.ReadAll(io.LimitReader(c.in, api.MaxCredentialValue+1))
	if err != nil {
		return nil, fmt.Errorf("read credential value from stdin: %w", err)
	}
	if len(b) > api.MaxCredentialValue {
		return nil, fmt.Errorf("credential value on stdin is longer than %d bytes", api.MaxCredentialValue)
	}
	if kind != transport.CredSSHKey {
		b = bytes.TrimRight(b, " \t\r\n")
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, errors.New("no credential value on stdin")
	}
	return b, nil
}

func (c *adminCLI) credentialSet(args []string) error {
	fs, data := c.flags("credential set")
	kind := fs.String("kind", "", "ssh-key, https-token or registry (required)")
	hosts := fs.String("hosts", "", "hosts the credential applies to, comma separated host[:port] (required)")
	user := fs.String("user", "", "ssh user (default git), basic or registry user")
	auth := fs.String("auth", "", "https-token: basic, bearer or header")
	header := fs.String("header", "", "https-token with -auth header: the header name")
	khFile := fs.String("known-hosts", "", "ssh-key: file with the pinned host keys (known_hosts lines)")
	binding := fs.String("host-binding", "", "ssh-key: required (default) or off")
	expires := fs.String("expires", "", "expiry, RFC 3339 or YYYY-MM-DD")
	rotate := fs.String("rotate", "", "rotate automatically on central: gitlab_self (https-token; needs -expires and -rotate-project)")
	project := fs.String("rotate-project", "", "GitLab project id or path for -rotate")
	pos, err := adminParse(fs, args)
	if err != nil {
		return err
	}
	if err := adminWantArgs("credential set", pos, 1, "one NAME (the value is read from stdin)"); err != nil {
		return err
	}
	if *kind == "" || *hosts == "" {
		return fmt.Errorf("%w: credential set: -kind and -hosts are required", errAdminUsage)
	}
	p := api.CredentialSetParams{Rotate: *rotate, RotateProject: *project}
	p.Name, p.Kind, p.Hosts = pos[0], *kind, splitList(*hosts)
	p.User, p.Auth, p.Header, p.HostBinding = *user, *auth, *header, *binding
	if *khFile != "" {
		if p.KnownHosts, err = readKnownHosts(*khFile); err != nil {
			return err
		}
	}
	if *expires != "" {
		t, err := parseExpires(*expires)
		if err != nil {
			return err
		}
		p.Expires = &t
	}
	// Validate everything but the value before touching stdin (placeholder length and hash).
	p.Length, p.SHA256 = 1, strings.Repeat("0", 64)
	if e := p.Validate(); e != nil {
		return fmt.Errorf("%w: credential set: %s", errAdminUsage, e.Message)
	}
	if c.callBody == nil {
		return errors.New("credential set is not available")
	}
	value, err := c.readCredentialValue(*kind)
	if err != nil {
		return err
	}
	defer clear(value)
	sum := sha256.Sum256(value)
	p.Length, p.SHA256 = len(value), hex.EncodeToString(sum[:])
	var res api.CredentialSetResult
	if err := c.callBody(adminSock(*data), api.MethodCredentialSet, p, value, &res); err != nil {
		return err
	}
	verb := "created"
	if res.Replaced {
		verb = "replaced"
	}
	fmt.Fprintf(c.out, "credential %s %s (%s, version %d, fingerprint %s)\n", res.Credential.Name, verb,
		res.Credential.Kind, res.Credential.Version, res.Credential.Fingerprint)
	return nil
}

func (c *adminCLI) credentialList(args []string) error {
	fs, data := c.flags("credential list")
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := adminParse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return fmt.Errorf("%w: central credential list takes at most one NAME", errAdminUsage)
	}
	params := api.CredentialListParams{}
	if len(pos) == 1 {
		params.Name = pos[0]
	}
	var res api.CredentialListResult
	if err := c.call(adminSock(*data), api.MethodCredentialList, params, &res); err != nil {
		return err
	}
	if *asJSON {
		return c.printJSON(res)
	}
	tw := c.table()
	fmt.Fprintln(tw, "NAME\tKIND\tHOSTS\tVERSION\tEXPIRES\tASSIGNED TO\tDELIVERY")
	for _, cr := range res.Credentials {
		var as, dl []string
		for _, a := range cr.Assignments {
			if a.Runner == "" {
				as = append(as, a.Fleet+" (fleet)")
			} else {
				as = append(as, a.Fleet+"/"+a.Runner)
			}
		}
		for _, d := range cr.Delivery {
			dl = append(dl, d.Fleet+"/"+d.Runner+"="+d.Status)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\t%s\n", cr.Name, cr.Kind, strings.Join(cr.Hosts, ","), cr.Version,
			adminTimePtr(cr.Expires), dashIfEmpty(strings.Join(as, ",")), dashIfEmpty(strings.Join(dl, ",")))
	}
	return tw.Flush()
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func (c *adminCLI) credentialDelete(args []string) error {
	fs, data := c.flags("credential delete")
	pos, err := adminParse(fs, args)
	if err != nil {
		return err
	}
	if err := adminWantArgs("credential delete", pos, 1, "one NAME"); err != nil {
		return err
	}
	if err := c.call(adminSock(*data), api.MethodCredentialDelete, api.CredentialDeleteParams{Name: pos[0]}, nil); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "credential %s deleted\n", pos[0])
	return nil
}

// credentialAssign is `assign` (add true) or `unassign`: exactly one of -runner FLEET/RUNNER and
// -fleet FLEET.
func (c *adminCLI) credentialAssign(add bool) func([]string) error {
	name, method := "unassign", api.MethodCredentialUnassign
	if add {
		name, method = "assign", api.MethodCredentialAssign
	}
	return func(args []string) error {
		fs, data := c.flags("credential " + name)
		runner := fs.String("runner", "", "one runner, FLEET/RUNNER")
		fleet := fs.String("fleet", "", "every runner of this fleet")
		pos, err := adminParse(fs, args)
		if err != nil {
			return err
		}
		if err := adminWantArgs("credential "+name, pos, 1, "one NAME"); err != nil {
			return err
		}
		if (*runner == "") == (*fleet == "") {
			return fmt.Errorf("%w: credential %s: give exactly one of -runner FLEET/RUNNER or -fleet FLEET", errAdminUsage, name)
		}
		fl, rn := *fleet, ""
		if *runner != "" {
			ref, err := store.ParseRef(*runner)
			if err != nil {
				return fmt.Errorf("%w: %v", errAdminUsage, err)
			}
			fl, rn = ref.Fleet, ref.Runner
		}
		var params any = api.CredentialAssignParams{Name: pos[0], Fleet: fl, Runner: rn}
		if !add {
			params = api.CredentialUnassignParams{Name: pos[0], Fleet: fl, Runner: rn}
		}
		var res api.CredentialResult
		if err := c.call(adminSock(*data), method, params, &res); err != nil {
			return err
		}
		target := fl
		if rn != "" {
			target += "/" + rn
		} else {
			target += " (fleet)"
		}
		fmt.Fprintf(c.out, "credential %s %sed %s %s\n", pos[0], name, map[bool]string{true: "to", false: "from"}[add], target)
		return nil
	}
}

func (c *adminCLI) credentialRotate(args []string) error {
	fs, data := c.flags("credential rotate")
	pos, err := adminParse(fs, args)
	if err != nil {
		return err
	}
	if err := adminWantArgs("credential rotate", pos, 1, "one NAME"); err != nil {
		return err
	}
	var res api.CredentialResult
	if err := c.call(adminSock(*data), api.MethodCredentialRotate, api.CredentialRotateParams{Name: pos[0]}, &res); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "credential %s rotated (version %d, expires %s)\n", res.Credential.Name, res.Credential.Version, adminTimePtr(res.Credential.Expires))
	return nil
}

// parseCredentialList parses -credentials NAME,NAME for a fleet of the given mode; credentials
// only exist for workspace fleets.
func parseCredentialList(s, mode string) ([]string, error) {
	names := splitList(s)
	if len(names) == 0 {
		return nil, nil
	}
	if mode != "workspace" {
		return nil, fmt.Errorf("%w: -credentials needs -mode workspace", errAdminUsage)
	}
	seen := map[string]bool{}
	for _, n := range names {
		if !transport.ValidCredName(n) || seen[n] {
			return nil, fmt.Errorf("%w: -credentials: invalid or repeated credential name %q", errAdminUsage, n)
		}
		seen[n] = true
	}
	return names, nil
}

// attachCredentials assigns each credential to the whole fleet (fleet create/set -credentials).
func (c *adminCLI) attachCredentials(sock, fleet string, names []string) error {
	for i, n := range names {
		var res api.CredentialResult
		if err := c.call(sock, api.MethodCredentialAssign, api.CredentialAssignParams{Name: n, Fleet: fleet}, &res); err != nil {
			return fmt.Errorf("assign credential %s to fleet %s: %w (assigned so far: %s)", n, fleet, err, strings.Join(names[:i], ","))
		}
		fmt.Fprintf(c.out, "credential %s assigned to fleet %s\n", n, fleet)
	}
	return nil
}
