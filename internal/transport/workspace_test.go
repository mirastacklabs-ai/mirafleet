package transport

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
)

const testID = "0123456789abcdef0123456789abcdef"

func TestRequestRoundTrip(t *testing.T) {
	reqs := []*WorkspaceRequest{
		{Op: OpCreate, ID: testID, Spec: &WorkspaceSpec{Image: "alpine:3", NetworkMode: "egress",
			Allow: []EgressRule{{Host: "example.com", Port: 443}}, Labels: map[string]string{"a": "b"}, MaxAgeMs: 5}},
		{Op: OpExec, ID: testID, Argv: []string{"/bin/sh", "-c", "id"}, Cwd: "/workspace", Env: map[string]string{"A": "b"}, StdinLen: 3, TimeoutMs: 1000},
		{Op: OpRead, ID: testID, Path: "a/b", Offset: 5, Limit: 10},
		{Op: OpWrite, ID: testID, Path: "a/b", Truncate: true, Mode: 0o644, Length: 7},
		{Op: OpDestroy, ID: testID},
		{Op: OpList},
	}
	for _, r := range reqs {
		b, err := EncodeWorkspaceRequest(r)
		if err != nil {
			t.Fatalf("%s: %v", r.Op, err)
		}
		got, err := DecodeWorkspaceRequest(b)
		if err != nil {
			t.Fatalf("%s: %v", r.Op, err)
		}
		if !reflect.DeepEqual(got, r) {
			t.Fatalf("%s: round trip mismatch: %+v vs %+v", r.Op, got, r)
		}
	}
}

func TestRequestStrict(t *testing.T) {
	for _, in := range []string{
		`{"op":"list","bogus":1}`,
		`{"op":"list"} {"op":"list"}`,
		`{"op":"list"}x`,
		`{"op":"exec","id":"` + testID + `","spec":{"image":"x","extra":1}}`,
		`[]`, ``, `{"op":"nope"}`,
	} {
		if _, err := DecodeWorkspaceRequest([]byte(in)); err == nil {
			t.Errorf("accepted %q", in)
		}
	}
}

func TestRequestSizeLimit(t *testing.T) {
	big := strings.Repeat(" ", MaxWorkspaceRequestBytes)
	if _, err := DecodeWorkspaceRequest([]byte(`{"op":"list"}` + big)); err == nil {
		t.Fatal("accepted over-size extra data")
	}
	// Exactly at the limit decodes (whitespace padding).
	pad := strings.Repeat(" ", MaxWorkspaceRequestBytes-len(`{"op":"list"}`))
	if _, err := DecodeWorkspaceRequest([]byte(`{"op":"list"}` + pad)); err != nil {
		t.Fatalf("rejected request at the limit: %v", err)
	}
	// A valid-shape request whose encoding exceeds 64 KiB is refused on encode.
	r := &WorkspaceRequest{Op: OpExec, ID: testID, Argv: []string{strings.Repeat("a", MaxArgvBytes)}, Env: map[string]string{}}
	r.Argv = append(r.Argv, strings.Repeat("\x01", 0))
	for i := 0; i < MaxEnvVars; i++ {
		r.Env["V"+strings.Repeat("A", 20)+string(rune('a'+i))] = strings.Repeat("\u0001", MaxEnvValueBytes) // escapes to 6 bytes each
	}
	if _, err := EncodeWorkspaceRequest(r); err == nil {
		t.Fatal("encoded an over-size request")
	}
}

func TestValidate(t *testing.T) {
	ok := func() *WorkspaceRequest {
		return &WorkspaceRequest{Op: OpExec, ID: testID, Argv: []string{"id"}}
	}
	cases := map[string]func(r *WorkspaceRequest){
		"bad id":       func(r *WorkspaceRequest) { r.ID = "XYZ" },
		"no id":        func(r *WorkspaceRequest) { r.ID = "" },
		"no argv":      func(r *WorkspaceRequest) { r.Argv = nil },
		"argv items":   func(r *WorkspaceRequest) { r.Argv = make([]string, MaxArgvItems+1) },
		"argv bytes":   func(r *WorkspaceRequest) { r.Argv = []string{strings.Repeat("a", MaxArgvBytes+1)} },
		"argv NUL":     func(r *WorkspaceRequest) { r.Argv = []string{"a\x00b"} },
		"neg stdin":    func(r *WorkspaceRequest) { r.StdinLen = -1 },
		"big stdin":    func(r *WorkspaceRequest) { r.StdinLen = MaxBodyBytes + 1 },
		"neg timeout":  func(r *WorkspaceRequest) { r.TimeoutMs = -1 },
		"big timeout":  func(r *WorkspaceRequest) { r.TimeoutMs = MaxWorkspaceTimeoutMs + 1 },
		"env name":     func(r *WorkspaceRequest) { r.Env = map[string]string{"A=B": "x"} },
		"env value":    func(r *WorkspaceRequest) { r.Env = map[string]string{"A": strings.Repeat("v", MaxEnvValueBytes+1)} },
		"cwd NUL":      func(r *WorkspaceRequest) { r.Cwd = "a\x00" },
		"cwd long":     func(r *WorkspaceRequest) { r.Cwd = strings.Repeat("a", MaxPathBytes+1) },
		"file field":   func(r *WorkspaceRequest) { r.Path = "x" },
		"spec on exec": func(r *WorkspaceRequest) { r.Spec = &WorkspaceSpec{Image: "x"} },
		"unknown op":   func(r *WorkspaceRequest) { r.Op = "rm" },
		"too many env": func(r *WorkspaceRequest) {
			r.Env = map[string]string{}
			for i := 0; i <= MaxEnvVars; i++ {
				r.Env[strings.Repeat("A", i+1)] = ""
			}
		},
	}
	if err := ok().Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range cases {
		r := ok()
		mut(r)
		if err := r.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	files := map[string]*WorkspaceRequest{
		"read no path":     {Op: OpRead, ID: testID},
		"read neg off":     {Op: OpRead, ID: testID, Path: "a", Offset: -1},
		"read big limit":   {Op: OpRead, ID: testID, Path: "a", Limit: MaxBodyBytes + 1},
		"read len":         {Op: OpRead, ID: testID, Path: "a", Length: 1},
		"write big len":    {Op: OpWrite, ID: testID, Path: "a", Length: MaxBodyBytes + 1},
		"write neg len":    {Op: OpWrite, ID: testID, Path: "a", Length: -1},
		"write bad mode":   {Op: OpWrite, ID: testID, Path: "a", Mode: 0o10000},
		"write setuid":     {Op: OpWrite, ID: testID, Path: "a", Mode: 0o4755},
		"write sticky":     {Op: OpWrite, ID: testID, Path: "a", Mode: 0o1777},
		"create no spec":   {Op: OpCreate, ID: testID},
		"create no image":  {Op: OpCreate, ID: testID, Spec: &WorkspaceSpec{}},
		"create neg lim":   {Op: OpCreate, ID: testID, Spec: &WorkspaceSpec{Image: "x", Pids: -1}},
		"create net":       {Op: OpCreate, ID: testID, Spec: &WorkspaceSpec{Image: "x", NetworkMode: "host"}},
		"create port":      {Op: OpCreate, ID: testID, Spec: &WorkspaceSpec{Image: "x", Allow: []EgressRule{{Host: "h", Port: 0}}}},
		"destroy no id":    {Op: OpDestroy},
		"list with id bad": {Op: OpList, ID: "zz"},
		"list with path":   {Op: OpList, Path: "a"},
	}
	for name, r := range files {
		if err := r.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestResultFrameRoundTrip(t *testing.T) {
	in := &WorkspaceResult{
		Code: CodeOK, Message: "m", DurationMs: 9,
		Exec: &WorkspaceExec{ExitCode: 3, Stdout: "o\x00", Stderr: "e", Truncated: true, TimedOut: true, DurationMs: 4},
		File: &WorkspaceFile{Size: 10, Mode: 0o600, Length: 5, EOF: true},
		List: []WorkspaceInfo{{ID: testID, Image: "i", State: "open", CreatedMs: 1, DiskBytes: 2}},
	}
	var buf bytes.Buffer
	if err := WriteResult(&buf, in, 0); err != nil {
		t.Fatal(err)
	}
	buf.WriteString("BODY")
	out, err := ReadResult(&buf, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("mismatch: %+v vs %+v", out, in)
	}
	if buf.String() != "BODY" {
		t.Fatalf("reader over-consumed: %q", buf.String())
	}
}

func TestResultFrameLimits(t *testing.T) {
	big := &WorkspaceResult{Code: CodeOK, Exec: &WorkspaceExec{Stdout: strings.Repeat("a", 2000)}}
	if err := WriteResult(io.Discard, big, 1000); err == nil {
		t.Fatal("wrote frame over max")
	}
	// Header announcing more than max is refused before the body is read (nothing follows it).
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], 1001)
	if _, err := ReadResult(bytes.NewReader(hdr[:]), 1000); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected limit error, got %v", err)
	}
	binary.BigEndian.PutUint32(hdr[:], 0xFFFFFFFF)
	if _, err := ReadResult(bytes.NewReader(hdr[:]), 0); err == nil {
		t.Fatal("accepted huge header")
	}
	// Truncated body and truncated header.
	var buf bytes.Buffer
	if err := WriteResult(&buf, &WorkspaceResult{Code: CodeOK}, 0); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	if _, err := ReadResult(bytes.NewReader(b[:len(b)-1]), 0); err == nil {
		t.Fatal("accepted truncated body")
	}
	if _, err := ReadResult(bytes.NewReader(b[:2]), 0); err == nil {
		t.Fatal("accepted truncated header")
	}
}

func frame(t *testing.T, payload string) io.Reader {
	t.Helper()
	b := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(b, uint32(len(payload)))
	copy(b[4:], payload)
	return bytes.NewReader(b)
}

func TestResultFrameStrict(t *testing.T) {
	for _, p := range []string{
		`{"code":"ok","extra":1}`,
		`{"code":"ok"}{"code":"ok"}`,
		`{"code":"weird"}`,
		`{"code":"ok","exec":{"exit_code":0,"stdout":"","stderr":"","duration_ms":0,"x":1}}`,
		`{"code":"ok","file":{"size":1,"mode":0,"length":-1}}`,
		`null`,
	} {
		if _, err := ReadResult(frame(t, p), 0); err == nil {
			t.Errorf("accepted %s", p)
		}
	}
	if _, err := ReadResult(frame(t, `{"code":"not_found","duration_ms":0}`), 0); err != nil {
		t.Fatal(err)
	}
}

func TestCopyBody(t *testing.T) {
	var dst bytes.Buffer
	if err := CopyBody(&dst, strings.NewReader("hello"), 5); err != nil || dst.String() != "hello" {
		t.Fatalf("exact: %v %q", err, dst.String())
	}
	dst.Reset()
	if err := CopyBody(&dst, strings.NewReader("hel"), 5); !errors.Is(err, ErrBodyLength) {
		t.Fatalf("short: %v", err)
	}
	dst.Reset()
	err := CopyBody(&dst, strings.NewReader("hello!"), 5)
	if !errors.Is(err, ErrBodyLength) {
		t.Fatalf("long: %v", err)
	}
	if dst.Len() > 5 {
		t.Fatalf("wrote past n: %d", dst.Len())
	}
	if err := CopyBody(io.Discard, strings.NewReader(""), 0); err != nil {
		t.Fatalf("zero: %v", err)
	}
	if err := CopyBody(io.Discard, strings.NewReader("x"), 0); !errors.Is(err, ErrBodyLength) {
		t.Fatalf("zero long: %v", err)
	}
	for _, n := range []int64{-1, MaxBodyBytes + 1} {
		if err := CopyBody(io.Discard, strings.NewReader(""), n); err == nil {
			t.Fatalf("accepted n=%d", n)
		}
	}
	// Reader errors surface.
	if err := CopyBody(io.Discard, iotest.ErrReader(errors.New("boom")), 3); err == nil || errors.Is(err, ErrBodyLength) {
		t.Fatalf("reader error: %v", err)
	}
	// Max size works and arrives intact via a one-byte reader.
	data := bytes.Repeat([]byte{7}, MaxBodyBytes)
	dst.Reset()
	if err := CopyBody(&dst, iotest.HalfReader(bytes.NewReader(data)), MaxBodyBytes); err != nil || !bytes.Equal(dst.Bytes(), data) {
		t.Fatalf("max body: %v", err)
	}
}

func TestWriteBody(t *testing.T) {
	var dst bytes.Buffer
	if err := WriteBody(&dst, strings.NewReader("hello"), 5); err != nil || dst.String() != "hello" {
		t.Fatalf("exact: %v", err)
	}
	if err := WriteBody(io.Discard, strings.NewReader("hel"), 5); !errors.Is(err, ErrBodyLength) {
		t.Fatalf("short: %v", err)
	}
	dst.Reset()
	if err := WriteBody(&dst, strings.NewReader("hello!"), 5); !errors.Is(err, ErrBodyLength) {
		t.Fatalf("long: %v", err)
	}
	if dst.Len() != 5 {
		t.Fatalf("wrote %d bytes", dst.Len())
	}
	if err := WriteBody(io.Discard, strings.NewReader(""), MaxBodyBytes+1); err == nil {
		t.Fatal("accepted oversize n")
	}
}

func TestHeartbeatCompat(t *testing.T) {
	// An M2-shaped heartbeat decodes with zero workspace fields.
	m2 := `{"hostname":"h","os":"linux","arch":"amd64","version":"v0.2","labels":{"a":"b"}}`
	var hb Heartbeat
	if err := json.Unmarshal([]byte(m2), &hb); err != nil {
		t.Fatal(err)
	}
	if hb.Slots != 0 || hb.Executor != "" || hb.SecureRuntime != "" || hb.Images != nil || hb.Validate() != nil {
		t.Fatalf("unexpected: %+v", hb)
	}
	// And an M2-shaped heartbeat re-encodes without the new keys.
	b, _ := json.Marshal(hb)
	for _, k := range []string{"slots", "executor", "secure_runtime", "images"} {
		if strings.Contains(string(b), k) {
			t.Fatalf("zero heartbeat emits %q: %s", k, b)
		}
	}
	// An M3 heartbeat decoded by an M2-style struct (no new fields) still works.
	m3, _ := json.Marshal(Heartbeat{Hostname: "h", Slots: 2, Executor: "containerd", SecureRuntime: "gvisor", Images: []string{"alpine:*"}})
	var old struct {
		Hostname string `json:"hostname"`
	}
	if err := json.Unmarshal(m3, &old); err != nil || old.Hostname != "h" {
		t.Fatalf("old decode: %v", err)
	}
	var back Heartbeat
	if err := json.Unmarshal(m3, &back); err != nil || back.Slots != 2 || back.Executor != "containerd" || back.SecureRuntime != "gvisor" || len(back.Images) != 1 {
		t.Fatalf("round trip: %+v %v", back, err)
	}
}

func TestHeartbeatBounds(t *testing.T) {
	imgs := func(n, l int) []string {
		s := make([]string, n)
		for i := range s {
			s[i] = strings.Repeat("a", l)
		}
		return s
	}
	good := []Heartbeat{
		{}, {Slots: MaxHeartbeatSlots}, {Images: imgs(32, 256)},
	}
	for i, h := range good {
		if err := h.Validate(); err != nil {
			t.Errorf("good %d: %v", i, err)
		}
	}
	bad := []Heartbeat{
		{Slots: -1}, {Slots: MaxHeartbeatSlots + 1}, {Images: imgs(33, 1)}, {Images: imgs(1, 257)},
		{Executor: strings.Repeat("x", 65)}, {SecureRuntime: strings.Repeat("x", 65)},
	}
	for i, h := range bad {
		if err := h.Validate(); err == nil {
			t.Errorf("bad %d accepted", i)
		}
	}
}

func TestChanWorkspaceName(t *testing.T) {
	if ChanWorkspace != "workspace@mirafleet" {
		t.Fatal(ChanWorkspace)
	}
}

func TestWorkspaceResultCap(t *testing.T) {
	if MaxWorkspaceResultBytes != MaxBodyBytes+64<<10 || MaxWorkspaceResultBytes >= MaxResultBytes {
		t.Fatalf("cap %d", MaxWorkspaceResultBytes)
	}
	// A frame under the exec cap but over the workspace cap is refused by default.
	big := &WorkspaceResult{Code: CodeOK, Message: strings.Repeat("x", 100)}
	var buf bytes.Buffer
	if err := WriteResult(&buf, big, 50); err == nil {
		t.Error("write over explicit max accepted")
	}
	hdr := make([]byte, 4)
	binary.BigEndian.PutUint32(hdr, MaxWorkspaceResultBytes+1)
	if _, err := ReadResult(bytes.NewReader(hdr), 0); err == nil || !strings.Contains(err.Error(), "exceeds limit") {
		t.Errorf("default read cap: %v", err)
	}
	buf.Reset()
	if err := WriteResult(&buf, big, 0); err != nil {
		t.Fatal(err)
	}
	if r, err := ReadResult(&buf, 0); err != nil || r.Code != CodeOK {
		t.Fatalf("%v %v", r, err)
	}
}

func TestHeartbeatEnumAndCharset(t *testing.T) {
	good := []Heartbeat{
		{Executor: "containerd"}, {Executor: "fake"}, {Executor: "opensandbox", SecureRuntime: "io.containerd.runsc.v1"},
		{SecureRuntime: "gvisor"}, {Images: []string{"alpine:*", "ghcr.io/o/i@sha256:*", "a/b:1.2_3-x"}},
	}
	for i, h := range good {
		if err := h.Validate(); err != nil {
			t.Errorf("good %d: %v", i, err)
		}
	}
	bad := []Heartbeat{
		{Executor: "1x"}, {Executor: "Containerd"}, {Executor: "x y"}, {Executor: strings.Repeat("a", 33)},
		{SecureRuntime: "gvisor\n"}, {SecureRuntime: "a b"}, {SecureRuntime: "é"},
		{Images: []string{"alpine latest"}}, {Images: []string{"a\x00b"}}, {Images: []string{"a\nb"}}, {Images: []string{""}},
	}
	for i, h := range bad {
		if err := h.Validate(); err == nil {
			t.Errorf("bad %d accepted", i)
		}
	}
}
