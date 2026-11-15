package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
)

const decoySecret = "glpat-DECOY-0123456789-never-leaks"

type bodyCall struct {
	sock, method, params string
	body                 []byte
}

// forbidReader fails the test if anything reads it (flag errors must not consume stdin).
type forbidReader struct{ t *testing.T }

func (f forbidReader) Read([]byte) (int, error) {
	f.t.Helper()
	f.t.Error("stdin was read")
	return 0, errors.New("read")
}

// credCLI is fakeAdmin's CLI plus a stdin and a recording body-call seam.
func credCLI(f *fakeAdmin, stdin interface{ Read([]byte) (int, error) }, bodies *[]bodyCall) *adminCLI {
	c := f.cli()
	c.in = stdin
	c.callBody = func(sock, method string, params any, body []byte, result any) error {
		pj, _ := json.Marshal(params)
		*bodies = append(*bodies, bodyCall{sock, method, string(pj), append([]byte(nil), body...)})
		if r, ok := f.results[method]; ok && result != nil {
			b, _ := json.Marshal(r)
			return json.Unmarshal(b, result)
		}
		return nil
	}
	return c
}

func TestCredentialSetValueOnlyInTheBody(t *testing.T) {
	f := &fakeAdmin{results: map[string]any{api.MethodCredentialSet: api.CredentialSetResult{
		Credential: api.CredentialInfo{Name: "gl", Kind: "https-token", Version: 1, Fingerprint: "SHA256:abcd"}}}}
	var bodies []bodyCall
	c := credCLI(f, strings.NewReader(decoySecret+"\r\n"), &bodies)
	err := c.run([]string{"credential", "set", "gl", "-kind", "https-token", "-hosts", "gitlab.example.com, git.example.com",
		"-auth", "bearer", "-expires", "2099-01-31"})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 || len(bodies) != 1 {
		t.Fatalf("calls %+v bodies %d", f.calls, len(bodies))
	}
	b := bodies[0]
	if b.method != api.MethodCredentialSet || b.sock != defaultCentralData+"/admin.sock" {
		t.Fatalf("%+v", b)
	}
	if string(b.body) != decoySecret { // trailing newline trimmed for a token
		t.Fatalf("body = %q", b.body)
	}
	sum := sha256.Sum256(b.body)
	wantParams := `"hosts":["gitlab.example.com","git.example.com"]`
	if !strings.Contains(b.params, wantParams) || !strings.Contains(b.params, `"sha256":"`+hex.EncodeToString(sum[:])+`"`) ||
		!strings.Contains(b.params, `"length":34`) || !strings.Contains(b.params, `"expires":"2099-01-31T00:00:00Z"`) {
		t.Fatalf("params = %s", b.params)
	}
	for name, s := range map[string]string{"params": b.params, "stdout": f.out.String(), "stderr": f.errw.String()} {
		if strings.Contains(s, decoySecret) || strings.Contains(s, "DECOY") {
			t.Errorf("%s contains the secret: %s", name, s)
		}
	}
	if !strings.Contains(f.out.String(), "credential gl created") || !strings.Contains(f.out.String(), "SHA256:abcd") {
		t.Fatalf("stdout = %q", f.out.String())
	}
}

func TestCredentialSetSSHKeyIsByteExactWithKnownHosts(t *testing.T) {
	dir := t.TempDir()
	kh := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(kh, []byte("# pinned\n\ngitlab.example.com ssh-ed25519 AAAAC3Nza\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	key := "-----BEGIN OPENSSH PRIVATE KEY-----\n" + decoySecret + "\n-----END OPENSSH PRIVATE KEY-----\n"
	f := &fakeAdmin{results: map[string]any{}}
	var bodies []bodyCall
	c := credCLI(f, strings.NewReader(key), &bodies)
	if err := c.run([]string{"credential", "set", "deploy", "-kind", "ssh-key", "-hosts", "gitlab.example.com:22",
		"-known-hosts", kh, "-user", "git", "-data", "/d"}); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 1 || string(bodies[0].body) != key || bodies[0].sock != "/d/admin.sock" {
		t.Fatalf("%+v", bodies)
	}
	if !strings.Contains(bodies[0].params, `"known_hosts":["gitlab.example.com ssh-ed25519 AAAAC3Nza"]`) ||
		strings.Contains(bodies[0].params, "DECOY") {
		t.Fatalf("params = %s", bodies[0].params)
	}
}

func TestCredentialSetFlagValidationNeverReadsStdin(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"no kind", []string{"set", "gl", "-hosts", "h.example.com"}},
		{"no hosts", []string{"set", "gl", "-kind", "https-token"}},
		{"no name", []string{"set", "-kind", "https-token", "-hosts", "h.example.com", "-auth", "bearer"}},
		{"two names", []string{"set", "a", "b", "-kind", "https-token", "-hosts", "h.example.com", "-auth", "bearer"}},
		{"value as flag", []string{"set", "gl", "-kind", "https-token", "-hosts", "h.example.com", "-auth", "bearer", "-value", decoySecret}},
		{"token as flag", []string{"set", "gl", "-kind", "https-token", "-hosts", "h.example.com", "-auth", "bearer", "-token", decoySecret}},
		{"bad kind", []string{"set", "gl", "-kind", "password", "-hosts", "h.example.com"}},
		{"bad name", []string{"set", "Bad Name", "-kind", "https-token", "-hosts", "h.example.com", "-auth", "bearer"}},
		{"https-token without auth", []string{"set", "gl", "-kind", "https-token", "-hosts", "h.example.com"}},
		{"ssh-key without known hosts", []string{"set", "gl", "-kind", "ssh-key", "-hosts", "h.example.com"}},
		{"registry without user", []string{"set", "gl", "-kind", "registry", "-hosts", "r.example.com"}},
		{"rotate without project", []string{"set", "gl", "-kind", "https-token", "-hosts", "h", "-auth", "bearer", "-rotate", "gitlab_self", "-expires", "2099-01-01"}},
		{"rotate without expires", []string{"set", "gl", "-kind", "https-token", "-hosts", "h", "-auth", "bearer", "-rotate", "gitlab_self", "-rotate-project", "grp/p"}},
		{"rotate on registry", []string{"set", "gl", "-kind", "registry", "-user", "u", "-hosts", "h", "-rotate", "gitlab_self", "-rotate-project", "p", "-expires", "2099-01-01"}},
		{"project without rotate", []string{"set", "gl", "-kind", "https-token", "-hosts", "h", "-auth", "bearer", "-rotate-project", "p"}},
		{"bad expires", []string{"set", "gl", "-kind", "https-token", "-hosts", "h.example.com", "-auth", "bearer", "-expires", "tomorrow"}},
		{"missing known-hosts file", []string{"set", "gl", "-kind", "ssh-key", "-hosts", "h", "-known-hosts", "/nonexistent/kh"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeAdmin{results: map[string]any{}}
			var bodies []bodyCall
			c := credCLI(f, forbidReader{t}, &bodies)
			err := c.run(append([]string{"credential"}, tc.args...))
			if !errors.Is(err, errAdminUsage) {
				t.Fatalf("want a usage error, got %v", err)
			}
			if len(bodies) != 0 || len(f.calls) != 0 {
				t.Fatalf("a refused command must not call central: %+v %+v", bodies, f.calls)
			}
			if strings.Contains(err.Error(), decoySecret) || strings.Contains(f.errw.String(), decoySecret) {
				t.Fatalf("the secret is echoed: %v / %s", err, f.errw.String())
			}
		})
	}
}

func TestCredentialSetEmptyOrOversizedStdin(t *testing.T) {
	for name, in := range map[string]string{"empty": "", "blank": " \n\n", "oversized": strings.Repeat("x", api.MaxCredentialValue+1)} {
		f := &fakeAdmin{results: map[string]any{}}
		var bodies []bodyCall
		c := credCLI(f, strings.NewReader(in), &bodies)
		err := c.run([]string{"credential", "set", "gl", "-kind", "https-token", "-hosts", "h.example.com", "-auth", "bearer"})
		if err == nil || len(bodies) != 0 {
			t.Errorf("%s: err=%v bodies=%d", name, err, len(bodies))
		}
	}
}

func TestCredentialAdminCalls(t *testing.T) {
	const sock = defaultCentralData + "/admin.sock"
	tests := []struct {
		name   string
		args   []string
		method string
		params string
	}{
		{"list", []string{"credential", "list"}, api.MethodCredentialList, `{}`},
		{"list one json", []string{"credential", "list", "gl", "-json"}, api.MethodCredentialList, `{"name":"gl"}`},
		{"delete", []string{"credential", "delete", "gl"}, api.MethodCredentialDelete, `{"name":"gl"}`},
		{"assign runner", []string{"credential", "assign", "gl", "-runner", "dev/vm1"}, api.MethodCredentialAssign, `{"name":"gl","fleet":"dev","runner":"vm1"}`},
		{"assign fleet", []string{"credential", "assign", "gl", "-fleet", "dev"}, api.MethodCredentialAssign, `{"name":"gl","fleet":"dev"}`},
		{"unassign runner", []string{"credential", "unassign", "gl", "-runner", "dev/vm1"}, api.MethodCredentialUnassign, `{"name":"gl","fleet":"dev","runner":"vm1"}`},
		{"unassign fleet", []string{"credential", "unassign", "-fleet", "dev", "gl"}, api.MethodCredentialUnassign, `{"name":"gl","fleet":"dev"}`},
		{"rotate", []string{"credential", "rotate", "gl"}, api.MethodCredentialRotate, `{"name":"gl"}`},
		{"delete no name", []string{"credential", "delete"}, "", ""},
		{"list two", []string{"credential", "list", "a", "b"}, "", ""},
		{"assign neither", []string{"credential", "assign", "gl"}, "", ""},
		{"assign both", []string{"credential", "assign", "gl", "-runner", "a/b", "-fleet", "a"}, "", ""},
		{"assign bad runner ref", []string{"credential", "assign", "gl", "-runner", "vm1"}, "", ""},
		{"rotate no name", []string{"credential", "rotate"}, "", ""},
		{"unknown sub", []string{"credential", "show", "gl"}, "", ""},
		{"no sub", []string{"credential"}, "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeAdmin{results: map[string]any{}}
			var bodies []bodyCall
			err := credCLI(f, forbidReader{t}, &bodies).run(tc.args)
			if tc.method == "" {
				if !errors.Is(err, errAdminUsage) || len(f.calls) != 0 {
					t.Fatalf("want usage error without a call: %v %+v", err, f.calls)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(f.calls) != 1 || f.calls[0].method != tc.method || f.calls[0].params != tc.params || f.calls[0].sock != sock {
				t.Fatalf("calls: %+v", f.calls)
			}
		})
	}
}

func TestCredentialListTableIsMasked(t *testing.T) {
	f := &fakeAdmin{results: map[string]any{api.MethodCredentialList: api.CredentialListResult{Credentials: []api.CredentialInfo{{
		Name: "gl", Kind: "https-token", Hosts: []string{"gitlab.example.com"}, Version: 2, Fingerprint: "SHA256:ff",
		Assignments: []api.CredentialAssignment{{Fleet: "dev"}, {Fleet: "dev", Runner: "vm1"}},
		Delivery:    []api.CredentialDelivery{{Fleet: "dev", Runner: "vm1", Status: "ok", Version: 2}}}}}}}
	var bodies []bodyCall
	if err := credCLI(f, nil, &bodies).run([]string{"credential", "list"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"gl", "gitlab.example.com", "dev (fleet),dev/vm1", "dev/vm1=ok"} {
		if !strings.Contains(f.out.String(), want) {
			t.Errorf("table lacks %q:\n%s", want, f.out.String())
		}
	}
}

func fleetCreated(name string) map[string]any {
	return map[string]any{api.MethodFleetCreate: api.FleetCreateResult{Fleet: api.FleetInfo{Name: name, Mode: "workspace"}}}
}

func TestFleetCreateHostWithCredentials(t *testing.T) {
	f := &fakeAdmin{results: fleetCreated("dev")}
	var bodies []bodyCall
	if err := credCLI(f, nil, &bodies).run([]string{"fleet", "create", "dev", "-mode", "workspace", "-executor", "host", "-credentials", "gl-token, deploy"}); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 3 || f.calls[0].method != api.MethodFleetCreate ||
		!strings.Contains(f.calls[0].params, `"executor":"host"`) ||
		f.calls[1].method != api.MethodCredentialAssign || f.calls[1].params != `{"name":"gl-token","fleet":"dev"}` ||
		f.calls[2].params != `{"name":"deploy","fleet":"dev"}` {
		t.Fatalf("calls: %+v", f.calls)
	}
}

func TestFleetCredentialsFlagValidation(t *testing.T) {
	for _, args := range [][]string{
		{"fleet", "create", "dev", "-credentials", "gl"},                             // needs -mode workspace
		{"fleet", "create", "dev", "-mode", "workspace", "-credentials", "Bad Name"}, // invalid name
		{"fleet", "create", "dev", "-mode", "workspace", "-credentials", "a,a"},      // repeated
	} {
		f := &fakeAdmin{results: fleetCreated("dev")}
		var bodies []bodyCall
		if err := credCLI(f, nil, &bodies).run(args); !errors.Is(err, errAdminUsage) || len(f.calls) != 0 {
			t.Errorf("%v: err=%v calls=%+v", args, err, f.calls)
		}
	}
}

func TestFleetWorkspaceSetCredentialsOnly(t *testing.T) {
	f := &fakeAdmin{results: map[string]any{}}
	var bodies []bodyCall
	if err := credCLI(f, nil, &bodies).run([]string{"fleet", "workspace", "set", "dev", "-credentials", "gl"}); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || f.calls[0].method != api.MethodCredentialAssign {
		t.Fatalf("calls: %+v", f.calls)
	}
}

func TestAdminCallBodyWire(t *testing.T) {
	dir, err := os.MkdirTemp("", "mf")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "a.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	type got struct {
		req  api.Request
		body []byte
	}
	ch := make(chan got, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		raw, err := api.ReadFrame(c, api.MaxRequestBytes)
		if err != nil {
			return
		}
		var r api.Request
		_ = json.Unmarshal(raw, &r)
		body, _ := api.ReadBodyTimeout(c, c, api.MaxFileChunk)
		ch <- got{r, body}
		res, _ := json.Marshal(api.CredentialSetResult{Credential: api.CredentialInfo{Name: "gl"}, Replaced: true})
		out, _ := json.Marshal(api.Response{V: api.Version, ID: r.ID, Result: res})
		_ = api.WriteFrame(c, out)
	}()
	var res api.CredentialSetResult
	if err := adminCallBody(sock, api.MethodCredentialSet, api.CredentialSetParams{Length: 3}, []byte("abc"), &res); err != nil {
		t.Fatal(err)
	}
	g := <-ch
	if g.req.Method != api.MethodCredentialSet || string(g.body) != "abc" || !res.Replaced || strings.Contains(string(g.req.Params), "abc") {
		t.Fatalf("%+v %+v", g, res)
	}
}
