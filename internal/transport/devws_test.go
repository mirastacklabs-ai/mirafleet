package transport

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

const testWS = "0123456789abcdef0123456789abcdef"

var hex64 = strings.Repeat("ab", 32)

func fsReq(o FSOp) *WorkspaceRequest {
	r := &WorkspaceRequest{Op: OpFS, ID: testWS, FS: &o}
	if o.Op == FSWrite {
		r.StdinLen = o.Length
	}
	return r
}

func TestFSOpValidate(t *testing.T) {
	long := strings.Repeat("x", MaxEditString+1)
	ok := []FSOp{
		{Op: FSRead, Path: "a.go", Offset: 10, Limit: 50},
		{Op: FSWrite, Path: "/home/dev/x", Length: 5, SHA256: hex64, Mode: 0o600},
		{Op: FSWrite, Path: "x", SHA256: hex64}, // empty file
		{Op: FSEdit, Path: "a", OldString: "a", NewString: "b", ReplaceAll: true},
		{Op: FSEdit, Path: "a", OldString: "a"}, // deleting text
		{Op: FSMultiEdit, Path: "a", Edits: []FSEditItem{{OldString: "a", NewString: "b"}}},
		{Op: FSNotebookEdit, Path: "n.ipynb", EditMode: NotebookReplace, CellID: "c1", NewSource: "x=1"},
		{Op: FSNotebookEdit, Path: "n.ipynb", EditMode: NotebookInsert, CellType: CellMarkdown, NewSource: "# t"},
		{Op: FSNotebookEdit, Path: "n.ipynb", EditMode: NotebookDelete, CellID: "c1"},
		{Op: FSGrep, Pattern: `func\s+Test`, Glob: "*.go", Type: "go", OutputMode: OutputContent, Context: 3, HeadLimit: 10},
		{Op: FSGlob, Pattern: "**/*.go", Path: "src"},
		{Op: FSLS, Path: "/tmp", Ignore: []string{"*.o"}},
		{Op: FSLS},
		{Op: FSStat, Path: "x"},
	}
	for i, o := range ok {
		if err := o.Validate(); err != nil {
			t.Errorf("ok[%d] %s: %v", i, o.Op, err)
		}
	}
	bad := map[string]FSOp{
		"unknown op":           {Op: "rm", Path: "x"},
		"empty op":             {Path: "x"},
		"read no path":         {Op: FSRead},
		"read with pattern":    {Op: FSRead, Path: "x", Pattern: "a"},
		"read neg limit":       {Op: FSRead, Path: "x", Limit: -1},
		"read big limit":       {Op: FSRead, Path: "x", Limit: MaxLines + 1},
		"read offset":          {Op: FSRead, Path: "x", Offset: -1},
		"write bad sha":        {Op: FSWrite, Path: "x", Length: 1, SHA256: "ABC"},
		"write upper sha":      {Op: FSWrite, Path: "x", SHA256: strings.ToUpper(hex64)},
		"write setuid":         {Op: FSWrite, Path: "x", SHA256: hex64, Mode: 0o4755},
		"write big":            {Op: FSWrite, Path: "x", SHA256: hex64, Length: MaxBodyBytes + 1},
		"write with edits":     {Op: FSWrite, Path: "x", SHA256: hex64, OldString: "a"},
		"edit no old":          {Op: FSEdit, Path: "x", NewString: "b"},
		"edit same":            {Op: FSEdit, Path: "x", OldString: "a", NewString: "a"},
		"edit long":            {Op: FSEdit, Path: "x", OldString: long, NewString: "b"},
		"edit nul":             {Op: FSEdit, Path: "x", OldString: "a\x00", NewString: "b"},
		"edit with edits":      {Op: FSEdit, Path: "x", OldString: "a", NewString: "b", Edits: []FSEditItem{{OldString: "a"}}},
		"multi none":           {Op: FSMultiEdit, Path: "x"},
		"multi too many":       {Op: FSMultiEdit, Path: "x", Edits: make([]FSEditItem, MaxEdits+1)},
		"multi empty old":      {Op: FSMultiEdit, Path: "x", Edits: []FSEditItem{{NewString: "b"}}},
		"multi old_string":     {Op: FSMultiEdit, Path: "x", OldString: "a", Edits: []FSEditItem{{OldString: "a", NewString: "b"}}},
		"nb no mode":           {Op: FSNotebookEdit, Path: "n", CellID: "c"},
		"nb bad mode":          {Op: FSNotebookEdit, Path: "n", CellID: "c", EditMode: "move"},
		"nb replace no cell":   {Op: FSNotebookEdit, Path: "n", EditMode: NotebookReplace},
		"nb insert no type":    {Op: FSNotebookEdit, Path: "n", EditMode: NotebookInsert},
		"nb bad type":          {Op: FSNotebookEdit, Path: "n", EditMode: NotebookInsert, CellType: "html"},
		"nb delete with src":   {Op: FSNotebookEdit, Path: "n", EditMode: NotebookDelete, CellID: "c", NewSource: "x"},
		"grep no pattern":      {Op: FSGrep},
		"grep bad re2":         {Op: FSGrep, Pattern: "(?=a)b"},
		"grep bad mode":        {Op: FSGrep, Pattern: "a", OutputMode: "lines"},
		"grep bad type":        {Op: FSGrep, Pattern: "a", Type: "go;rm"},
		"grep ctx":             {Op: FSGrep, Pattern: "a", Context: MaxContextLines + 1},
		"grep neg after":       {Op: FSGrep, Pattern: "a", After: -1},
		"grep limit":           {Op: FSGrep, Pattern: "a", HeadLimit: MaxLines + 1},
		"grep long pattern":    {Op: FSGrep, Pattern: strings.Repeat("a", MaxPatternBytes+1)},
		"grep old_string":      {Op: FSGrep, Pattern: "a", OldString: "x"},
		"glob no pattern":      {Op: FSGlob},
		"glob with ignore":     {Op: FSGlob, Pattern: "*", Ignore: []string{"a"}},
		"ls ignore many":       {Op: FSLS, Ignore: make([]string, MaxIgnoreGlobs+1)},
		"ls empty ignore":      {Op: FSLS, Ignore: []string{""}},
		"ls with limit":        {Op: FSLS, Limit: 5},
		"stat no path":         {Op: FSStat},
		"stat with offset":     {Op: FSStat, Path: "x", Offset: 1},
		"path nul":             {Op: FSStat, Path: "a\x00b"},
		"path long":            {Op: FSStat, Path: strings.Repeat("a", MaxPathBytes+1)},
		"path invalid utf8":    {Op: FSStat, Path: "\xff"},
		"multi total too big":  bigMulti(),
		"multi item too big":   {Op: FSMultiEdit, Path: "x", Edits: []FSEditItem{{OldString: long, NewString: "b"}}},
		"edit new_source":      {Op: FSEdit, Path: "x", OldString: "a", NewString: "b", NewSource: "c"},
		"notebook with old":    {Op: FSNotebookEdit, Path: "n", EditMode: NotebookDelete, CellID: "c", OldString: "a"},
		"stat with replaceAll": {Op: FSStat, Path: "x", ReplaceAll: true},
	}
	for name, o := range bad {
		if err := o.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for _, op := range FSOps {
		if _, ok := fsAllowed[op]; !ok {
			t.Errorf("op %s has no field table", op)
		}
	}
	if len(fsAllowed) != len(FSOps) {
		t.Error("fsAllowed and FSOps differ")
	}
}

func bigMulti() FSOp {
	e := make([]FSEditItem, 8)
	for i := range e {
		e[i] = FSEditItem{OldString: strings.Repeat("a", 4<<10), NewString: strings.Repeat("b", 4<<10)}
	}
	return FSOp{Op: FSMultiEdit, Path: "x", Edits: e}
}

func TestFSMutating(t *testing.T) {
	want := map[string]bool{FSWrite: true, FSEdit: true, FSMultiEdit: true, FSNotebookEdit: true}
	for _, op := range FSOps {
		if FSMutating(op) != want[op] {
			t.Errorf("FSMutating(%s) = %v", op, FSMutating(op))
		}
	}
	if FSMutating("rm") {
		t.Error("unknown op is mutating")
	}
}

func TestDevRequestRoundTripAndShape(t *testing.T) {
	good := []*WorkspaceRequest{
		fsReq(FSOp{Op: FSRead, Path: "a"}),
		fsReq(FSOp{Op: FSWrite, Path: "a", Length: 7, SHA256: hex64}),
		{Op: OpJobStart, ID: testWS, Job: &JobOp{Command: "make", TimeoutS: 60}},
		{Op: OpJobStatus, ID: testWS, Job: &JobOp{}},
		{Op: OpJobTail, ID: testWS, Job: &JobOp{Job: "j1", Filter: "ERR"}},
		{Op: OpJobCancel, ID: testWS, Job: &JobOp{Job: "j1"}},
		{Op: OpOutputRead, ID: testWS, Out: &OutputOp{Ref: "r-1", TailLines: 50}},
		{Op: OpCredsSync, StdinLen: 100},
	}
	for _, r := range good {
		b, err := EncodeWorkspaceRequest(r)
		if err != nil {
			t.Fatalf("%s: encode: %v", r.Op, err)
		}
		if _, err := DecodeWorkspaceRequest(b); err != nil {
			t.Errorf("%s: decode: %v", r.Op, err)
		}
	}
	bad := map[string]*WorkspaceRequest{
		"fs no payload":      {Op: OpFS, ID: testWS},
		"fs no id":           {Op: OpFS, FS: &FSOp{Op: FSStat, Path: "x"}},
		"fs bad id":          {Op: OpFS, ID: "zz", FS: &FSOp{Op: FSStat, Path: "x"}},
		"fs two payloads":    {Op: OpFS, ID: testWS, FS: &FSOp{Op: FSStat, Path: "x"}, Job: &JobOp{}},
		"fs legacy path":     {Op: OpFS, ID: testWS, Path: "x", FS: &FSOp{Op: FSStat, Path: "x"}},
		"fs legacy argv":     {Op: OpFS, ID: testWS, Argv: []string{"a"}, FS: &FSOp{Op: FSStat, Path: "x"}},
		"fs body on read":    {Op: OpFS, ID: testWS, StdinLen: 3, FS: &FSOp{Op: FSRead, Path: "x"}},
		"fs write mismatch":  {Op: OpFS, ID: testWS, StdinLen: 3, FS: &FSOp{Op: FSWrite, Path: "x", Length: 4, SHA256: hex64}},
		"fs write no body":   {Op: OpFS, ID: testWS, FS: &FSOp{Op: FSWrite, Path: "x", Length: 4, SHA256: hex64}},
		"job start no job":   {Op: OpJobStart, ID: testWS},
		"job wrong payload":  {Op: OpJobStart, ID: testWS, FS: &FSOp{Op: FSStat, Path: "x"}},
		"job start no cmd":   {Op: OpJobStart, ID: testWS, Job: &JobOp{}},
		"job start job id":   {Op: OpJobStart, ID: testWS, Job: &JobOp{Command: "x", Job: "j"}},
		"job tail no job":    {Op: OpJobTail, ID: testWS, Job: &JobOp{}},
		"job cancel filter":  {Op: OpJobCancel, ID: testWS, Job: &JobOp{Job: "j", Filter: "a"}},
		"job status command": {Op: OpJobStatus, ID: testWS, Job: &JobOp{Command: "x"}},
		"job bad id":         {Op: OpJobCancel, ID: testWS, Job: &JobOp{Job: "../x"}},
		"job timeout":        {Op: OpJobStart, ID: testWS, Job: &JobOp{Command: "x", TimeoutS: MaxJobTimeoutS + 1}},
		"job bad filter":     {Op: OpJobTail, ID: testWS, Job: &JobOp{Job: "j", Filter: "("}},
		"out none":           {Op: OpOutputRead, ID: testWS, Out: &OutputOp{}},
		"out both":           {Op: OpOutputRead, ID: testWS, Out: &OutputOp{Ref: "r", Job: "j"}},
		"out bad ref":        {Op: OpOutputRead, ID: testWS, Out: &OutputOp{Ref: "../r"}},
		"out stream on ref":  {Op: OpOutputRead, ID: testWS, Out: &OutputOp{Ref: "r", Stream: "stdout"}},
		"out bad stream":     {Op: OpOutputRead, ID: testWS, Out: &OutputOp{Job: "j", Stream: "stdin"}},
		"out tail+offset":    {Op: OpOutputRead, ID: testWS, Out: &OutputOp{Ref: "r", TailLines: 5, Offset: 1}},
		"out big limit":      {Op: OpOutputRead, ID: testWS, Out: &OutputOp{Ref: "r", Limit: MaxBodyBytes + 1}},
		"out bad grep":       {Op: OpOutputRead, ID: testWS, Out: &OutputOp{Ref: "r", Grep: "("}},
		"creds with id":      {Op: OpCredsSync, ID: testWS, StdinLen: 5},
		"creds no body":      {Op: OpCredsSync},
		"creds big body":     {Op: OpCredsSync, StdinLen: MaxBodyBytes + 1},
		"creds with payload": {Op: OpCredsSync, StdinLen: 5, FS: &FSOp{Op: FSStat, Path: "x"}},
		"legacy with fs":     {Op: OpList, FS: &FSOp{Op: FSStat, Path: "x"}},
		"legacy exec w job":  {Op: OpExec, ID: testWS, Argv: []string{"a"}, Job: &JobOp{}},
	}
	for name, r := range bad {
		if err := r.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestExactCaseAndDuplicateKeys(t *testing.T) {
	for name, raw := range map[string]string{
		"top-level case":  `{"Op":"fs","id":"` + testWS + `","fs":{"op":"stat","path":"x"}}`,
		"nested case":     `{"op":"fs","id":"` + testWS + `","fs":{"op":"stat","Path":"x"}}`,
		"duplicate":       `{"op":"fs","id":"` + testWS + `","fs":{"op":"stat","path":"x","path":"y"}}`,
		"dup top":         `{"op":"fs","op":"fs","id":"` + testWS + `","fs":{"op":"stat","path":"x"}}`,
		"unknown nested":  `{"op":"fs","id":"` + testWS + `","fs":{"op":"stat","path":"x","exec":"y"}}`,
		"trailing":        `{"op":"fs","id":"` + testWS + `","fs":{"op":"stat","path":"x"}} {}`,
		"edits item case": `{"op":"fs","id":"` + testWS + `","fs":{"op":"multi_edit","path":"x","edits":[{"Old_String":"a","new_string":"b"}]}}`,
	} {
		if _, err := DecodeWorkspaceRequest([]byte(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	ok := `{"op":"fs","id":"` + testWS + `","fs":{"op":"multi_edit","path":"x","edits":[{"old_string":"a","new_string":"b"}]}}`
	if _, err := DecodeWorkspaceRequest([]byte(ok)); err != nil {
		t.Fatalf("valid request refused: %v", err)
	}
}

func TestResultValidate(t *testing.T) {
	good := &WorkspaceResult{Code: CodeOK, FS: &FSResult{Kind: KindText, Content: "1\tx", Lines: 1},
		Job: &JobResult{Job: "j1", State: JobRunning}, Jobs: []JobResult{{Job: "j2", State: JobExited}},
		Output: &OutputResult{Content: "x", Size: 1}, Creds: &CredsSyncResult{Generation: 3, Count: 2}}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := WriteResult(&buf, good, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadResult(&buf, 0); err != nil {
		t.Fatalf("round trip: %v", err)
	}
	for name, r := range map[string]*WorkspaceResult{
		"fs kind":       {Code: CodeOK, FS: &FSResult{Kind: "pdf"}},
		"fs neg":        {Code: CodeOK, FS: &FSResult{Kind: KindText, Count: -1}},
		"fs length":     {Code: CodeOK, FS: &FSResult{Kind: KindImage, Length: MaxBodyBytes + 1}},
		"fs sha":        {Code: CodeOK, FS: &FSResult{Kind: KindText, SHA256: "x"}},
		"fs entries":    {Code: CodeOK, FS: &FSResult{Kind: KindEntries, Entries: make([]FSEntry, MaxListEntries+1)}},
		"job state":     {Code: CodeOK, Job: &JobResult{Job: "j", State: "zombie"}},
		"job id":        {Code: CodeOK, Job: &JobResult{Job: "a/b", State: JobRunning}},
		"jobs many":     {Code: CodeOK, Jobs: make([]JobResult, MaxJobList+1)},
		"output neg":    {Code: CodeOK, Output: &OutputResult{Offset: -1}},
		"creds neg":     {Code: CodeOK, Creds: &CredsSyncResult{Count: -1}},
		"creds too big": {Code: CodeOK, Creds: &CredsSyncResult{Count: MaxSyncCreds + 1}},
	} {
		if err := r.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func sshCred(name string) CredMeta {
	return CredMeta{Name: name, Kind: CredSSHKey, Hosts: []string{"gitlab.test"},
		KnownHosts: []string{"gitlab.test ssh-ed25519 AAAAC3Nza"}}
}

func TestCredMetaValidate(t *testing.T) {
	ok := []CredMeta{
		sshCred("deploy"),
		{Name: "d2", Kind: CredSSHKey, Hosts: []string{"git.corp:2222"}, User: "git", HostBinding: BindOff,
			KnownHosts: []string{"[git.corp]:2222 ssh-rsa AAAA"}},
		{Name: "tok", Kind: CredHTTPSToken, Hosts: []string{"gitlab.test"}, Auth: AuthBasic, User: "oauth2"},
		{Name: "tok2", Kind: CredHTTPSToken, Hosts: []string{"gitlab.test"}, Auth: AuthBearer},
		{Name: "tok3", Kind: CredHTTPSToken, Hosts: []string{"a.test"}, Auth: AuthHeader, Header: "PRIVATE-TOKEN"},
		{Name: "reg", Kind: CredRegistry, Hosts: []string{"registry.test:5000"}, User: "robot"},
	}
	for _, m := range ok {
		if err := m.Validate(); err != nil {
			t.Errorf("%s: %v", m.Name, err)
		}
	}
	mut := func(f func(*CredMeta)) CredMeta { m := sshCred("c"); f(&m); return m }
	tok := func(f func(*CredMeta)) CredMeta {
		m := CredMeta{Name: "t", Kind: CredHTTPSToken, Hosts: []string{"a.test"}, Auth: AuthBearer}
		f(&m)
		return m
	}
	bad := map[string]CredMeta{
		"empty name":       mut(func(m *CredMeta) { m.Name = "" }),
		"name slash":       mut(func(m *CredMeta) { m.Name = "a/b" }),
		"name long":        mut(func(m *CredMeta) { m.Name = strings.Repeat("a", 65) }),
		"kind":             mut(func(m *CredMeta) { m.Kind = "password" }),
		"no hosts":         mut(func(m *CredMeta) { m.Hosts = nil }),
		"upper host":       mut(func(m *CredMeta) { m.Hosts = []string{"GitLab.test"} }),
		"host space":       mut(func(m *CredMeta) { m.Hosts = []string{"a b"} }),
		"host wildcard":    mut(func(m *CredMeta) { m.Hosts = []string{"*.test"} }),
		"host port":        mut(func(m *CredMeta) { m.Hosts = []string{"a.test:99999"} }),
		"host port empty":  mut(func(m *CredMeta) { m.Hosts = []string{"a.test:"} }),
		"host port alpha":  mut(func(m *CredMeta) { m.Hosts = []string{"a.test:ab"} }),
		"dup host":         mut(func(m *CredMeta) { m.Hosts = []string{"a.test", "a.test"} }),
		"many hosts":       mut(func(m *CredMeta) { m.Hosts = manyHosts(MaxCredHosts + 1) }),
		"no known_hosts":   mut(func(m *CredMeta) { m.KnownHosts = nil }),
		"known comment":    mut(func(m *CredMeta) { m.KnownHosts = []string{"# x"} }),
		"known newline":    mut(func(m *CredMeta) { m.KnownHosts = []string{"a\nb"} }),
		"known empty":      mut(func(m *CredMeta) { m.KnownHosts = []string{""} }),
		"binding":          mut(func(m *CredMeta) { m.HostBinding = "maybe" }),
		"ssh auth":         mut(func(m *CredMeta) { m.Auth = AuthBasic }),
		"ssh user":         mut(func(m *CredMeta) { m.User = "-oProxyCommand=x" }),
		"tok no auth":      tok(func(m *CredMeta) { m.Auth = "" }),
		"tok bad auth":     tok(func(m *CredMeta) { m.Auth = "digest" }),
		"tok basic nouser": tok(func(m *CredMeta) { m.Auth = AuthBasic }),
		"tok bearer user":  tok(func(m *CredMeta) { m.User = "u" }),
		"tok header none":  tok(func(m *CredMeta) { m.Auth = AuthHeader }),
		"tok header host":  tok(func(m *CredMeta) { m.Auth = AuthHeader; m.Header = "Host" }),
		"tok header crlf":  tok(func(m *CredMeta) { m.Auth = AuthHeader; m.Header = "X\r\nY" }),
		"tok known":        tok(func(m *CredMeta) { m.KnownHosts = []string{"a b c"} }),
		"tok binding":      tok(func(m *CredMeta) { m.HostBinding = BindOff }),
		"reg no user":      {Name: "r", Kind: CredRegistry, Hosts: []string{"a.test"}},
		"reg auth":         {Name: "r", Kind: CredRegistry, Hosts: []string{"a.test"}, User: "u", Auth: AuthBasic},
	}
	for name, m := range bad {
		if err := m.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func manyHosts(n int) []string {
	var h []string
	for i := 0; i < n; i++ {
		h = append(h, fmt.Sprintf("h%d.test", i))
	}
	return h
}

func TestCredsSyncBody(t *testing.T) {
	const decoy = "DECOY-SECRET-VALUE-4f9c"
	body := &CredsSyncBody{Generation: 5, Credentials: []CredSecret{
		{CredMeta: sshCred("deploy"), Version: 2, Value: decoy},
		{CredMeta: CredMeta{Name: "tok", Kind: CredHTTPSToken, Hosts: []string{"gitlab.test"}, Auth: AuthBearer},
			Version: 1, ExpiresMs: 1700000000000, Value: decoy + "-2"},
	}}
	enc, err := EncodeCredsSync(body)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeCredsSync(enc)
	if err != nil || got.Generation != 5 || len(got.Credentials) != 2 || got.Credentials[0].Value != decoy {
		t.Fatalf("round trip: %v %+v", err, got)
	}
	// Empty set (removes everything) is valid.
	if _, err := EncodeCredsSync(&CredsSyncBody{Generation: 1}); err != nil {
		t.Errorf("empty set: %v", err)
	}
	// Redaction: no formatting route prints a value.
	var logBuf bytes.Buffer
	slog.New(slog.NewTextHandler(&logBuf, nil)).Info("sync", "cred", body.Credentials[0], "body", *body)
	for _, s := range []string{
		fmt.Sprint(body.Credentials[0]), fmt.Sprintf("%v %+v %#v %s", *body, *body, *body, body.Credentials[1]),
		fmt.Sprintf("%+v %#v", body.Credentials[0], body.Credentials[0]), logBuf.String(),
	} {
		if strings.Contains(s, decoy) {
			t.Fatalf("value leaked: %s", s)
		}
	}
	// Errors never carry a value.
	dup := &CredsSyncBody{Credentials: []CredSecret{body.Credentials[0], body.Credentials[0]}}
	if err := dup.Validate(); err == nil || strings.Contains(err.Error(), decoy) {
		t.Errorf("duplicate: %v", err)
	}
	for _, raw := range []string{
		`{"generation":1,"credentials":[{"name":"x","kind":"ssh-key","hosts":["a.test"],"known_hosts":["a"],"version":1,"value":"` + decoy + `","extra":1}]}`,
		`{"generation":1,"credentials":[{"Name":"x","kind":"ssh-key","hosts":["a.test"],"known_hosts":["a"],"version":1,"value":"` + decoy + `"}]}`,
		`{"generation":1,"credentials":[]} x`,
	} {
		if _, err := DecodeCredsSync([]byte(raw)); err == nil || strings.Contains(err.Error(), decoy) {
			t.Errorf("accepted or leaked: %v", err)
		}
	}
	bad := map[string]*CredsSyncBody{
		"neg generation": {Generation: -1},
		"too many":       {Credentials: manySecrets(MaxSyncCreds + 1)},
		"no value":       {Credentials: []CredSecret{{CredMeta: sshCred("a"), Version: 1}}},
		"huge value":     {Credentials: []CredSecret{{CredMeta: sshCred("a"), Version: 1, Value: strings.Repeat("a", MaxCredentialData+1)}}},
		"nul value":      {Credentials: []CredSecret{{CredMeta: sshCred("a"), Version: 1, Value: "a\x00"}}},
		"no version":     {Credentials: []CredSecret{{CredMeta: sshCred("a"), Value: "v"}}},
		"neg expiry":     {Credentials: []CredSecret{{CredMeta: sshCred("a"), Version: 1, ExpiresMs: -1, Value: "v"}}},
		"bad meta":       {Credentials: []CredSecret{{CredMeta: CredMeta{Name: "a"}, Version: 1, Value: "v"}}},
	}
	for name, b := range bad {
		if _, err := EncodeCredsSync(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A full-size set fits the body limit.
	full := &CredsSyncBody{}
	for i := 0; i < MaxSyncCreds; i++ {
		c := CredSecret{CredMeta: sshCred(fmt.Sprintf("c%d", i)), Version: 1, Value: strings.Repeat("k", MaxCredentialData)}
		full.Credentials = append(full.Credentials, c)
	}
	if _, err := EncodeCredsSync(full); err != nil {
		t.Errorf("full set: %v", err)
	}
}

func manySecrets(n int) []CredSecret {
	var s []CredSecret
	for i := 0; i < n; i++ {
		s = append(s, CredSecret{CredMeta: sshCred(fmt.Sprintf("c%d", i)), Version: 1, Value: "v"})
	}
	return s
}

func TestHeartbeatDevFields(t *testing.T) {
	h := Heartbeat{Hostname: "vm1", Features: []string{FeatureHost, FeatureFS, FeatureJobs, FeatureCreds, FeatureEvents},
		Credentials: []HeartbeatCredential{{Name: "deploy", Kind: CredSSHKey, Hosts: []string{"gitlab.test"}, Status: CredOK}},
		Busy:        2}
	if err := h.Validate(); err != nil {
		t.Fatal(err)
	}
	mut := func(f func(*Heartbeat)) Heartbeat { x := h; f(&x); return x }
	for name, x := range map[string]Heartbeat{
		"feature":      mut(func(x *Heartbeat) { x.Features = []string{"root"} }),
		"dup feature":  mut(func(x *Heartbeat) { x.Features = []string{FeatureFS, FeatureFS} }),
		"many feature": mut(func(x *Heartbeat) { x.Features = make([]string, MaxHeartbeatFeatures+1) }),
		"cred status": mut(func(x *Heartbeat) {
			x.Credentials = []HeartbeatCredential{{Name: "a", Kind: CredSSHKey, Hosts: []string{"a.test"}, Status: "great"}}
		}),
		"cred kind": mut(func(x *Heartbeat) {
			x.Credentials = []HeartbeatCredential{{Name: "a", Kind: "x", Hosts: []string{"a.test"}, Status: CredOK}}
		}),
		"cred hosts": mut(func(x *Heartbeat) {
			x.Credentials = []HeartbeatCredential{{Name: "a", Kind: CredSSHKey, Status: CredOK}}
		}),
		"cred dup": mut(func(x *Heartbeat) {
			c := HeartbeatCredential{Name: "a", Kind: CredSSHKey, Hosts: []string{"a.test"}, Status: CredOK}
			x.Credentials = []HeartbeatCredential{c, c}
		}),
		"cred many": mut(func(x *Heartbeat) { x.Credentials = make([]HeartbeatCredential, MaxHeartbeatCredentials+1) }),
		"busy neg":  mut(func(x *Heartbeat) { x.Busy = -1 }),
		"busy big":  mut(func(x *Heartbeat) { x.Busy = MaxHeartbeatBusy + 1 }),
	} {
		if err := x.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// An M3/M5 heartbeat without the new fields is still valid.
	if err := (&Heartbeat{Hostname: "old"}).Validate(); err != nil {
		t.Errorf("old heartbeat: %v", err)
	}
}
