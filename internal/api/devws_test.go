package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const ws1 = "ws-1"

var sha = strings.Repeat("cd", 32)

func decode(t *testing.T, v Validator, js string) *Error {
	t.Helper()
	return DecodeValid(json.RawMessage(js), v)
}

func TestDevMethodClassification(t *testing.T) {
	operator := []string{MethodWorkspaceFS, MethodWorkspaceJobStart, MethodWorkspaceJobStatus,
		MethodWorkspaceJobTail, MethodWorkspaceJobCancel, MethodWorkspaceOutputRead}
	admin := []string{MethodCredentialSet, MethodCredentialList, MethodCredentialDelete,
		MethodCredentialAssign, MethodCredentialUnassign, MethodCredentialRotate}
	for _, m := range operator {
		if !IsOperatorMethod(m) || IsAdminMethod(m) || IsEnrollMethod(m) {
			t.Errorf("%s must be operator-only", m)
		}
	}
	for _, m := range admin {
		if IsOperatorMethod(m) || !IsAdminMethod(m) || IsEnrollMethod(m) {
			t.Errorf("%s must be admin-only", m)
		}
	}
	if IsOperatorMethod("workspace.job_list") || IsAdminMethod("credential.get") {
		t.Error("unknown dev method routable")
	}
}

func TestDecodeValidStrictness(t *testing.T) {
	var p FSParams
	if e := decode(t, &p, `{"workspace":"ws-1","op":"read","path":"a.go","limit":10}`); e != nil {
		t.Fatalf("valid: %v", e)
	}
	if p.Op != "read" || p.Path != "a.go" || p.Workspace != ws1 {
		t.Fatalf("embedded fields not decoded: %+v", p)
	}
	for name, js := range map[string]string{
		"missing params":    ``,
		"null":              `null`,
		"upper key":         `{"Workspace":"ws-1","op":"read","path":"a"}`,
		"upper embedded":    `{"workspace":"ws-1","Op":"read","path":"a"}`,
		"unknown key":       `{"workspace":"ws-1","op":"read","path":"a","exec":"x"}`,
		"duplicate":         `{"workspace":"ws-1","op":"read","path":"a","path":"b"}`,
		"trailing":          `{"workspace":"ws-1","op":"read","path":"a"} {}`,
		"unknown op":        `{"workspace":"ws-1","op":"chmod","path":"a"}`,
		"no workspace":      `{"op":"read","path":"a"}`,
		"bad workspace":     `{"workspace":"a b","op":"read","path":"a"}`,
		"cross-op field":    `{"workspace":"ws-1","op":"read","path":"a","pattern":"x"}`,
		"type confusion":    `{"workspace":"ws-1","op":"read","path":5}`,
		"long confirm":      `{"workspace":"ws-1","op":"read","path":"a","confirm":"` + strings.Repeat("a", 129) + `"}`,
		"nested bad key":    `{"workspace":"ws-1","op":"multi_edit","path":"a","edits":[{"Old_string":"a","new_string":"b"}]}`,
		"write no sha":      `{"workspace":"ws-1","op":"write","path":"a","length":3}`,
		"write fields only": `{"workspace":"ws-1","op":"write","path":"a","new_string":"x","sha256":"` + sha + `"}`,
	} {
		var q FSParams
		if e := decode(t, &q, js); e == nil || e.Code != CodeBadRequest {
			t.Errorf("%s: %v", name, e)
		}
	}
	var w FSParams
	if e := decode(t, &w, `{"workspace":"ws-1","op":"write","path":"a","length":3,"sha256":"`+sha+`","confirm":"prod"}`); e != nil || !w.NeedsConfirm() {
		t.Errorf("write: %v confirm=%v", e, w.NeedsConfirm())
	}
	if p.NeedsConfirm() {
		t.Error("read needs confirm")
	}
}

func TestJobParams(t *testing.T) {
	var s JobStartParams
	if e := decode(t, &s, `{"workspace":"ws-1","command":"make -j4","description":"build","timeout_s":600,"confirm":"f"}`); e != nil {
		t.Fatal(e)
	}
	if op := s.Op(); op.Command != "make -j4" || op.TimeoutS != 600 {
		t.Fatalf("op: %+v", op)
	}
	var st JobStatusParams
	if e := decode(t, &st, `{"workspace":"ws-1"}`); e != nil { // lists
		t.Errorf("status list: %v", e)
	}
	var tl JobTailParams
	if e := decode(t, &tl, `{"workspace":"ws-1","job":"j1","filter":"ERROR|FAIL"}`); e != nil {
		t.Errorf("tail: %v", e)
	}
	var c JobCancelParams
	if e := decode(t, &c, `{"workspace":"ws-1","job":"j1"}`); e != nil {
		t.Errorf("cancel: %v", e)
	}
	for name, tc := range map[string]struct {
		v  Validator
		js string
	}{
		"start no command":     {&JobStartParams{}, `{"workspace":"ws-1"}`},
		"start neg timeout":    {&JobStartParams{}, `{"workspace":"ws-1","command":"x","timeout_s":-1}`},
		"start huge timeout":   {&JobStartParams{}, `{"workspace":"ws-1","command":"x","timeout_s":86401}`},
		"start nul":            {&JobStartParams{}, `{"workspace":"ws-1","command":"a\u0000b"}`},
		"start job field":      {&JobStartParams{}, `{"workspace":"ws-1","command":"x","job":"j"}`},
		"start case":           {&JobStartParams{}, `{"workspace":"ws-1","Command":"x"}`},
		"status bad job":       {&JobStatusParams{}, `{"workspace":"ws-1","job":"../x"}`},
		"status command":       {&JobStatusParams{}, `{"workspace":"ws-1","command":"x"}`},
		"tail no job":          {&JobTailParams{}, `{"workspace":"ws-1"}`},
		"tail bad filter":      {&JobTailParams{}, `{"workspace":"ws-1","job":"j","filter":"("}`},
		"cancel no job":        {&JobCancelParams{}, `{"workspace":"ws-1"}`},
		"cancel confirm":       {&JobCancelParams{}, `{"workspace":"ws-1","job":"j","confirm":"f"}`},
		"cancel bad workspace": {&JobCancelParams{}, `{"workspace":"","job":"j"}`},
	} {
		if e := decode(t, tc.v, tc.js); e == nil || e.Code != CodeBadRequest {
			t.Errorf("%s: %v", name, e)
		}
	}
}

func TestOutputReadParams(t *testing.T) {
	var p OutputReadParams
	if e := decode(t, &p, `{"workspace":"ws-1","ref":"req-9","offset":100,"limit":4096}`); e != nil {
		t.Fatal(e)
	}
	if p.Ref != "req-9" || p.Offset != 100 {
		t.Fatalf("embedded: %+v", p)
	}
	for name, js := range map[string]string{
		"neither":       `{"workspace":"ws-1"}`,
		"both":          `{"workspace":"ws-1","ref":"r","job":"j"}`,
		"stream on ref": `{"workspace":"ws-1","ref":"r","stream":"stdout"}`,
		"tail+offset":   `{"workspace":"ws-1","ref":"r","tail_lines":5,"offset":2}`,
		"big limit":     `{"workspace":"ws-1","ref":"r","limit":2097152}`,
		"bad grep":      `{"workspace":"ws-1","ref":"r","grep":"[a"}`,
		"case":          `{"workspace":"ws-1","Ref":"r"}`,
	} {
		var q OutputReadParams
		if e := decode(t, &q, js); e == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestValidateCredentialNames(t *testing.T) {
	if e := ValidateCredentialNames([]string{"deploy", "tok.1"}); e != nil {
		t.Error(e)
	}
	if e := ValidateCredentialNames(nil); e != nil {
		t.Error(e)
	}
	for _, l := range [][]string{{"a", "a"}, {"a/b"}, {""}, make([]string, 33)} {
		if e := ValidateCredentialNames(l); e == nil {
			t.Errorf("%v accepted", l)
		}
	}
}

const setOK = `{"name":"deploy","kind":"ssh-key","hosts":["gitlab.test"],"known_hosts":["gitlab.test ssh-ed25519 AAAA"],"length":400,"sha256":"`

func TestCredentialSetParams(t *testing.T) {
	var p CredentialSetParams
	if e := decode(t, &p, setOK+sha+`"}`); e != nil {
		t.Fatal(e)
	}
	if p.Name != "deploy" || p.Kind != "ssh-key" || p.Length != 400 {
		t.Fatalf("embedded: %+v", p)
	}
	tok := `{"name":"t","kind":"https-token","hosts":["gitlab.test"],"auth":"bearer","length":30,"sha256":"` + sha + `"`
	var rot CredentialSetParams
	if e := decode(t, &rot, tok+`,"rotate":"gitlab_self","rotate_project":"acme/app","expires":"2027-01-01T00:00:00Z"}`); e != nil {
		t.Fatalf("rotate: %v", e)
	}
	if rot.Expires == nil || !rot.Expires.After(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("expires: %v", rot.Expires)
	}
	for name, js := range map[string]string{
		"value in params":    `{"name":"deploy","kind":"ssh-key","hosts":["g.test"],"known_hosts":["a"],"length":4,"sha256":"` + sha + `","value":"SECRET"}`,
		"no length":          strings.Replace(setOK, `"length":400,`, ``, 1) + sha + `"}`,
		"huge length":        strings.Replace(setOK, `400`, `16385`, 1) + sha + `"}`,
		"bad sha":            setOK + `abc"}`,
		"upper sha":          setOK + strings.ToUpper(sha) + `"}`,
		"upper key":          strings.Replace(setOK, `"name"`, `"Name"`, 1) + sha + `"}`,
		"bad kind":           strings.Replace(setOK, `ssh-key`, `password`, 1) + sha + `"}`,
		"rotate on ssh":      setOK + sha + `","rotate":"gitlab_self","rotate_project":"a/b","expires":"2027-01-01T00:00:00Z"}`,
		"rotate unknown":     tok + `,"rotate":"vault"}`,
		"rotate no project":  tok + `,"rotate":"gitlab_self","expires":"2027-01-01T00:00:00Z"}`,
		"rotate no expires":  tok + `,"rotate":"gitlab_self","rotate_project":"a/b"}`,
		"project w/o rotate": tok + `,"rotate_project":"a/b"}`,
		"project injection":  tok + `,"rotate":"gitlab_self","rotate_project":"a b?x=1","expires":"2027-01-01T00:00:00Z"}`,
		"bad expires":        tok + `,"expires":"tomorrow"}`,
	} {
		var q CredentialSetParams
		if e := decode(t, &q, js); e == nil || e.Code != CodeBadRequest {
			t.Errorf("%s: %v", name, e)
		}
	}
}

func TestCredentialAdminParams(t *testing.T) {
	var l CredentialListParams
	if e := decode(t, &l, ``); e != nil {
		t.Errorf("list all: %v", e)
	}
	if e := decode(t, &CredentialListParams{}, `{"name":"a/b"}`); e == nil {
		t.Error("list bad name accepted")
	}
	if e := decode(t, &CredentialDeleteParams{}, `{"name":"deploy"}`); e != nil {
		t.Error(e)
	}
	if e := decode(t, &CredentialRotateParams{}, `{"name":"deploy"}`); e != nil {
		t.Error(e)
	}
	if e := decode(t, &CredentialAssignParams{}, `{"name":"deploy","fleet":"dev","runner":"vm1-dev"}`); e != nil {
		t.Error(e)
	}
	if e := decode(t, &CredentialUnassignParams{}, `{"name":"deploy","fleet":"dev"}`); e != nil {
		t.Error(e)
	}
	for name, tc := range map[string]struct {
		v  Validator
		js string
	}{
		"delete none":         {&CredentialDeleteParams{}, `{}`},
		"delete extra":        {&CredentialDeleteParams{}, `{"name":"a","force":true}`},
		"rotate bad":          {&CredentialRotateParams{}, `{"name":""}`},
		"assign no fleet":     {&CredentialAssignParams{}, `{"name":"a"}`},
		"assign bad fleet":    {&CredentialAssignParams{}, `{"name":"a","fleet":"Dev"}`},
		"assign bad runner":   {&CredentialAssignParams{}, `{"name":"a","fleet":"dev","runner":"a/b"}`},
		"assign case":         {&CredentialAssignParams{}, `{"name":"a","Fleet":"dev"}`},
		"unassign no name":    {&CredentialUnassignParams{}, `{"fleet":"dev"}`},
		"unassign bad runner": {&CredentialUnassignParams{}, `{"name":"a","fleet":"dev","runner":"-x/"}`},
	} {
		if e := decode(t, tc.v, tc.js); e == nil || e.Code != CodeBadRequest {
			t.Errorf("%s: %v", name, e)
		}
	}
}

// No credential result type can carry a value: assert on the wire shape.
func TestCredentialInfoNeverCarriesValue(t *testing.T) {
	b, err := json.Marshal(CredentialSetResult{Credential: CredentialInfo{Name: "a", Kind: "ssh-key",
		Hosts: []string{"g.test"}, Version: 1, Fingerprint: "SHA256:x"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{`"value"`, `"secret"`, `"token"`, `"password"`, `"private`} {
		if strings.Contains(string(b), k) {
			t.Errorf("result has %s: %s", k, b)
		}
	}
}

func TestRunnerInfoDevFieldsAdditive(t *testing.T) {
	b, _ := json.Marshal(RunnerInfo{Name: "r"})
	if strings.Contains(string(b), "credentials") || strings.Contains(string(b), "features") || strings.Contains(string(b), "busy") {
		t.Errorf("empty dev fields must be omitted: %s", b)
	}
	b, _ = json.Marshal(RunnerInfo{Name: "r", Features: []string{"fs"}, Busy: 1,
		Credentials: []RunnerCredential{{Name: "d", Kind: "ssh-key", Hosts: []string{"g.test"}, Status: "ok"}}})
	if !strings.Contains(string(b), `"busy":1`) || !strings.Contains(string(b), `"status":"ok"`) {
		t.Errorf("dev fields missing: %s", b)
	}
}
