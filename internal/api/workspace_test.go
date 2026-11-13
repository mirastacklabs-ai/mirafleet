package api

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var wsOperator = []string{MethodWorkspaceOpen, MethodWorkspaceExec, MethodWorkspaceReadFile,
	MethodWorkspaceWriteFile, MethodWorkspaceClose, MethodWorkspaceList}
var wsAdmin = []string{MethodFleetWorkspaceSet, MethodWorkspaceAdminList, MethodWorkspaceKill}

func TestWorkspaceMethodSets(t *testing.T) {
	for _, m := range wsOperator {
		if !IsOperatorMethod(m) || IsAdminMethod(m) || IsEnrollMethod(m) {
			t.Errorf("%s misclassified", m)
		}
	}
	for _, m := range wsAdmin {
		if !IsAdminMethod(m) || IsOperatorMethod(m) || IsEnrollMethod(m) {
			t.Errorf("%s misclassified", m)
		}
	}
	// Every admin method (old and new) must be absent from the operator and enroll sets.
	all := append([]string{MethodFleetCreate, MethodTokenCreate, MethodTokenList, MethodTokenRevoke,
		MethodRunnerLifecycle, MethodOperatorCode, MethodOperatorRotateSecret, MethodOperatorList,
		MethodOperatorRevoke, MethodAuditList}, wsAdmin...)
	for _, m := range all {
		if IsOperatorMethod(m) || IsEnrollMethod(m) || !IsAdminMethod(m) {
			t.Errorf("admin method %s leaks", m)
		}
	}
	for _, m := range []string{"workspace", "workspace.", "workspace.create", "workspace.kill ", "Workspace.open"} {
		if IsOperatorMethod(m) || IsAdminMethod(m) || IsEnrollMethod(m) {
			t.Errorf("%q routable", m)
		}
	}
}

func TestWorkspaceExecTimeout(t *testing.T) {
	cases := []struct {
		req, def, max int
		want          time.Duration
	}{
		{0, 0, 0, 2 * time.Minute},
		{0, 300, 1800, 5 * time.Minute},
		{60, 300, 1800, time.Minute},
		{5000, 300, 1800, 30 * time.Minute},
		{0, 5000, 1800, 30 * time.Minute},
		{1 << 40, 120, 1 << 40, 24 * time.Hour},
		{7200, 120, 86400, 2 * time.Hour},
		{1 << 62, 120, 0, 30 * time.Minute},
	}
	for _, c := range cases {
		got, e := WorkspaceExecTimeout(c.req, c.def, c.max)
		if e != nil || got != c.want {
			t.Errorf("%+v: got %v %v", c, got, e)
		}
	}
	if _, e := WorkspaceExecTimeout(-1, 120, 1800); e == nil || e.Code != CodeBadRequest {
		t.Fatal(e)
	}
	// action timeout untouched
	if ActionTimeout(0) != DefaultActionTimeout || ActionTimeout(1<<40) != MaxActionTimeout {
		t.Fatal("ActionTimeout changed")
	}
}

func TestValidateWorkspaceInputs(t *testing.T) {
	if ValidateArgv([]string{"ls", "-l"}) != nil {
		t.Fatal("good argv")
	}
	for _, bad := range [][]string{nil, {}, {""}, {"a\x00b"}, make([]string, MaxArgvElems+1), {strings.Repeat("a", MaxArgvBytes+1)}} {
		if ValidateArgv(bad) == nil {
			t.Errorf("accepted %d-elem argv", len(bad))
		}
	}
	if ValidatePath("src/a.go") != nil || ValidatePath("a\x00") == nil ||
		ValidatePath(strings.Repeat("a", MaxPathBytes+1)) == nil || ValidatePath("\xff") == nil {
		t.Fatal("path validation")
	}
	if ValidateEnv(map[string]string{"FOO_1": "x"}) != nil {
		t.Fatal("good env")
	}
	if ValidatePath("") != nil || ValidatePath("../etc") == nil || ValidatePath("/etc/passwd") == nil || ValidatePath("/workspace/a") != nil {
		t.Fatal("shared path semantics")
	}
	if ValidateEnv(map[string]string{"GITHUB_TOKEN": "x"}) == nil || ValidateEnv(map[string]string{"HTTP_PROXY": "x"}) == nil {
		t.Fatal("denied env accepted")
	}
	for _, bad := range []map[string]string{{"foo": "x"}, {"1A": "x"}, {"A": strings.Repeat("v", MaxEnvValue+1)}, {"A": "a\x00"}} {
		if ValidateEnv(bad) == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	many := map[string]string{}
	for i := 0; i <= MaxEnvVars; i++ {
		many["V"+strings.Repeat("A", i)] = "x"
	}
	if ValidateEnv(many) == nil {
		t.Fatal("too many env")
	}
}

func TestWorkspaceParamsStrict(t *testing.T) {
	var p WorkspaceExecParams
	if e := DecodeParams([]byte(`{"workspace":"w","argv":["ls"],"timeout_s":5,"stdin":true}`), &p); e != nil || !p.Stdin {
		t.Fatal(e)
	}
	if e := DecodeParams([]byte(`{"workspace":"w","argv":["ls"],"shell":"x"}`), &p); e == nil {
		t.Fatal("unknown field accepted")
	}
	var o WorkspaceOpenParams
	if e := DecodeParams([]byte(`{"fleet":"f","session":"s","image":"i"}`), &o); e != nil {
		t.Fatal(e)
	}
	var c FleetCreateParams
	if e := DecodeParams([]byte(`{"name":"f","mode":"workspace","workspace":{"executor":"containerd","memory_mb":4096}}`), &c); e != nil || c.Workspace.MemoryMB != 4096 {
		t.Fatal(e)
	}
}

func TestBodyRoundTrip(t *testing.T) {
	for _, n := range []int{0, 1, 4096, MaxFileChunk} {
		var buf bytes.Buffer
		src := bytes.Repeat([]byte{7}, n)
		if err := WriteBody(&buf, src); err != nil {
			t.Fatal(err)
		}
		got, err := ReadBodyTimeout(&buf, nil, MaxFileChunk)
		if err != nil || !bytes.Equal(got, src) || got == nil {
			t.Fatalf("n=%d: %v", n, err)
		}
	}
	if err := WriteBody(io.Discard, make([]byte, MaxFileChunk+1)); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatal(err)
	}
}

func bodyErrCode(t *testing.T, err error) string {
	t.Helper()
	var ae *Error
	if !errors.As(err, &ae) {
		t.Fatalf("not *Error: %v", err)
	}
	return ae.Code
}

// countReader records how many bytes were consumed.
type countReader struct {
	r io.Reader
	n int
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func TestBodyOversizeBeforeAllocation(t *testing.T) {
	hdr := make([]byte, 4)
	binary.BigEndian.PutUint32(hdr, 0xFFFFFFFF)
	cr := &countReader{r: io.MultiReader(bytes.NewReader(hdr), strings.NewReader("junk"))}
	_, err := ReadBodyTimeout(cr, nil, MaxFileChunk)
	if bodyErrCode(t, err) != CodeTooLarge || cr.n != 4 {
		t.Fatalf("code/consumed: %v %d", err, cr.n)
	}
	// custom smaller max
	var buf bytes.Buffer
	WriteBody(&buf, make([]byte, 11))
	if _, err := ReadBodyTimeout(&buf, nil, 10); bodyErrCode(t, err) != CodeTooLarge {
		t.Fatal(err)
	}
	// max above the hard cap is clamped
	binary.BigEndian.PutUint32(hdr, MaxFileChunk+1)
	if _, err := ReadBodyTimeout(bytes.NewReader(hdr), nil, 1<<30); bodyErrCode(t, err) != CodeTooLarge {
		t.Fatal(err)
	}
}

func TestValidateFileMode(t *testing.T) {
	for _, m := range []uint32{0, 0o644, 0o755, 0o777} {
		if ValidateFileMode(m) != nil {
			t.Errorf("%#o should be accepted", m)
		}
	}
	for _, m := range []uint32{0o1000, 0o2755, 0o4755, 0o7777, 1 << 31} {
		if ValidateFileMode(m) == nil {
			t.Errorf("%#o should be refused", m)
		}
	}
}

func TestBodyTruncated(t *testing.T) {
	var buf bytes.Buffer
	WriteBody(&buf, []byte("hello world"))
	full := buf.Bytes()
	for _, cut := range []int{0, 2, 4, 8} {
		_, err := ReadBodyTimeout(bytes.NewReader(full[:cut]), nil, 100)
		if bodyErrCode(t, err) != CodeBadRequest {
			t.Errorf("cut %d: %v", cut, err)
		}
	}
}

// Not parallel: mutates BodyTimeout.
func TestBodySlowSender(t *testing.T) {
	old := BodyTimeout
	BodyTimeout = 50 * time.Millisecond
	defer func() { BodyTimeout = old }()
	pr, pw := io.Pipe()
	defer pw.Close()
	go pw.Write([]byte{0, 0, 0, 9, 'a'}) // header promises 9, one byte then silence
	_, err := ReadBodyTimeout(noDeadline{pr}, pr, 100)
	if bodyErrCode(t, err) != CodeTimeout {
		t.Fatal(err)
	}
	if _, werr := pr.Read(make([]byte, 1)); werr == nil {
		t.Fatal("reader was not closed")
	}
}

// Not parallel: mutates BodyTimeout.
func TestBodySuccessStopsTimer(t *testing.T) {
	old := BodyTimeout
	BodyTimeout = 50 * time.Millisecond
	defer func() { BodyTimeout = old }()
	var closed atomic.Bool
	cl := closerFunc(func() error { closed.Store(true); return nil })
	var buf bytes.Buffer
	WriteBody(&buf, []byte("x"))
	if _, err := ReadBodyTimeout(&buf, cl, 10); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if closed.Load() {
		t.Fatal("timer fired after completion")
	}
}
