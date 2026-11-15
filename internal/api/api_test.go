package api

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func frame(payload string) []byte {
	b := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(b, uint32(len(payload)))
	copy(b[4:], payload)
	return b
}

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFrame(&buf, 100)
	if err != nil || string(got) != `{"a":1}` {
		t.Fatalf("got %q %v", got, err)
	}
	if _, err := ReadFrame(&buf, 100); err != io.EOF {
		t.Fatalf("want EOF, got %v", err)
	}
}

// countingReader serves an endless stream and records how much was consumed.
type countingReader struct {
	hdr  []byte
	read int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		if c.read < len(c.hdr) {
			p[n] = c.hdr[c.read]
		} else {
			p[n] = 'x'
		}
		n++
		c.read++
	}
	return n, nil
}

func TestOversizedLengthRefusedBeforeRead(t *testing.T) {
	cr := &countingReader{hdr: []byte{0xff, 0xff, 0xff, 0xff}}
	_, err := ReadFrame(cr, MaxRequestBytes)
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("got %v", err)
	}
	if cr.read > 4 {
		t.Fatalf("read %d bytes; payload was touched", cr.read)
	}
}

func TestTruncatedAndEmpty(t *testing.T) {
	for _, in := range [][]byte{{0, 0}, frame("hello")[:7]} {
		if _, err := ReadFrame(bytes.NewReader(in), 100); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("%v: got %v", in, err)
		}
	}
	if _, err := ReadFrame(bytes.NewReader([]byte{0, 0, 0, 0}), 100); !errors.Is(err, ErrEmptyFrame) {
		t.Fatalf("got %v", err)
	}
	if err := WriteFrame(io.Discard, nil); !errors.Is(err, ErrEmptyFrame) {
		t.Fatalf("got %v", err)
	}
}

func TestWriteFrameLimit(t *testing.T) {
	if err := WriteFrame(io.Discard, make([]byte, MaxResponseBytes+1)); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("got %v", err)
	}
	if err := WriteFrame(io.Discard, make([]byte, MaxResponseBytes)); err != nil {
		t.Fatal(err)
	}
}

func TestParseRequest(t *testing.T) {
	req, e := ParseRequest([]byte(`{"v":1,"id":"a","method":"hello","params":{"x":1}}`))
	if e != nil || req.Method != "hello" || string(req.Params) != `{"x":1}` {
		t.Fatalf("%+v %v", req, e)
	}
	bad := map[string]string{
		`{"v":1,"id":"a","method":"hello","extra":1}`:                      CodeBadRequest,
		`{"v":1,"id":"","method":"hello"}`:                                 CodeBadRequest,
		`{"v":1,"id":"a"}`:                                                 CodeBadRequest,
		`{"id":"a","method":"hello"}`:                                      CodeBadRequest,
		`{"v":"1","id":"a","method":"hello"}`:                              CodeBadRequest,
		`{"v":1,"id":"a","method":"hello"} {}`:                             CodeBadRequest,
		`{"v":1,"id":"a","method":"hello"}}`:                               CodeBadRequest,
		`{"v":null,"id":"a","method":"hello"}`:                             CodeBadRequest,
		`{"v":1,"id":"` + strings.Repeat("i", 129) + `","method":"hello"}`: CodeBadRequest,
		`not json`:                          CodeBadRequest,
		`{"v":2,"id":"a","method":"hello"}`: CodeUnsupportedVersion,
		`{"v":2,"future":true}`:             CodeUnsupportedVersion,
	}
	for in, code := range bad {
		if _, e := ParseRequest([]byte(in)); e == nil || e.Code != code {
			t.Errorf("%s: got %v want %s", in, e, code)
		}
	}
	_, e = ParseRequest([]byte(`{"v":9,"id":"a","method":"m"}`))
	if len(e.Supported) != 1 || !strings.Contains(e.Message, "supported") {
		t.Fatalf("no supported list: %+v", e)
	}
}

func TestReadRequestOverPipe(t *testing.T) {
	c, s := net.Pipe()
	defer c.Close()
	defer s.Close()
	go c.Write(frame(`{"v":1,"id":"q","method":"fleets.list"}`))
	req, id, err := ReadRequest(s)
	if err != nil || id != "q" || req.Method != MethodFleetsList {
		t.Fatalf("%+v %v", req, err)
	}
	// error path returns the id
	go c.Write(frame(`{"v":1,"id":"z","method":"m","bogus":1}`))
	_, id, err = ReadRequest(s)
	if id != "z" || AsError(err).Code != CodeBadRequest {
		t.Fatalf("%q %v", id, err)
	}
}

func TestReadRequestTooLarge(t *testing.T) {
	_, _, err := ReadRequest(bytes.NewReader([]byte{0, 1, 0, 1}))
	if AsError(err).Code != CodeTooLarge {
		t.Fatalf("got %v", err)
	}
}

func TestParamLimits(t *testing.T) {
	ok := map[string]string{"a": "b"}
	if ValidateActionParams(ok) != nil || ValidateActionParams(nil) != nil {
		t.Fatal("valid params refused")
	}
	many := map[string]string{}
	for i := 0; i <= MaxParams; i++ {
		many[strings.Repeat("k", 1)+string(rune('A'+i))] = "v"
	}
	cases := []map[string]string{
		many,
		{strings.Repeat("k", MaxParamKey+1): "v"},
		{"": "v"},
		{"k": strings.Repeat("v", MaxParamValue+1)},
	}
	for i, c := range cases {
		if e := ValidateActionParams(c); e == nil || e.Code != CodeBadRequest {
			t.Errorf("case %d accepted", i)
		}
	}
	edge := map[string]string{strings.Repeat("k", MaxParamKey): strings.Repeat("v", MaxParamValue)}
	if ValidateActionParams(edge) != nil {
		t.Fatal("edge values refused")
	}
}

func TestInternalDoesNotLeak(t *testing.T) {
	e := AsError(errors.New("secret db path /x"))
	if e.Code != CodeInternal || strings.Contains(e.Message, "secret") {
		t.Fatalf("%+v", e)
	}
}

func TestResponseExactlyOne(t *testing.T) {
	r, _ := NewResult("1", map[string]int{"a": 1})
	if _, err := r.Marshal(); err != nil {
		t.Fatal(err)
	}
	r.Error = Errorf(CodeInternal, "x")
	if _, err := r.Marshal(); err == nil {
		t.Fatal("both set accepted")
	}
	if _, err := (&Response{V: 1, ID: "1"}).Marshal(); err == nil {
		t.Fatal("neither set accepted")
	}
}

func TestWriteResponseOversize(t *testing.T) {
	r, _ := NewResult("1", strings.Repeat("a", MaxResponseBytes))
	var buf bytes.Buffer
	if err := WriteResponse(&buf, r); err != nil {
		t.Fatal(err)
	}
	p, _ := ReadFrame(&buf, MaxResponseBytes)
	if !strings.Contains(string(p), CodeTooLarge) {
		t.Fatalf("%s", p)
	}
}

// Not parallel: mutates the package-level RequestTimeout.
func TestSlowRequestTimesOut(t *testing.T) {
	old := RequestTimeout
	RequestTimeout = 50 * time.Millisecond
	defer func() { RequestTimeout = old }()
	c, s := net.Pipe()
	defer c.Close()
	defer s.Close()
	go c.Write([]byte{0, 0}) // partial header, then silence
	_, _, err := ReadRequest(s)
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != CodeTimeout {
		t.Fatalf("got %v", err)
	}
}

// noDeadline hides SetReadDeadline, like ssh.Channel.
type noDeadline struct{ io.Reader }

// Not parallel: mutates RequestTimeout.
func TestReadRequestTimeoutNoDeadlineMethod(t *testing.T) {
	old := RequestTimeout
	RequestTimeout = 50 * time.Millisecond
	defer func() { RequestTimeout = old }()
	pr, pw := io.Pipe()
	defer pw.Close()
	go pw.Write([]byte{0, 0})
	_, _, err := ReadRequestTimeout(noDeadline{pr}, pr)
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != CodeTimeout {
		t.Fatalf("got %v", err)
	}
	if _, werr := pr.Read(make([]byte, 1)); werr == nil {
		t.Fatal("reader was not closed")
	}
}

func TestReadRequestTimeoutSuccessStopsTimer(t *testing.T) {
	old := RequestTimeout
	RequestTimeout = 50 * time.Millisecond
	defer func() { RequestTimeout = old }()
	var closed atomic.Bool
	cl := closerFunc(func() error { closed.Store(true); return nil })
	req, _, err := ReadRequestTimeout(bytes.NewReader(frame(`{"v":1,"id":"a","method":"hello"}`)), cl)
	if err != nil || req == nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if closed.Load() {
		t.Fatal("timer fired after completion")
	}
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

// Not parallel: mutates RequestTimeout.
func TestDeadlineClearedAfterSuccess(t *testing.T) {
	old := RequestTimeout
	RequestTimeout = 50 * time.Millisecond
	defer func() { RequestTimeout = old }()
	c, s := net.Pipe()
	defer c.Close()
	defer s.Close()
	go c.Write(frame(`{"v":1,"id":"a","method":"hello"}`))
	if _, _, err := ReadRequest(s); err != nil {
		t.Fatal(err)
	}
	time.Sleep(120 * time.Millisecond) // past the old deadline
	go c.Write(frame(`{"v":1,"id":"b","method":"hello"}`))
	if _, _, err := ReadRequest(s); err != nil {
		t.Fatalf("second read limited by stale deadline: %v", err)
	}
}

func TestActionTimeout(t *testing.T) {
	cases := map[int]time.Duration{
		0: DefaultActionTimeout, -1: DefaultActionTimeout, 30: 30 * time.Second,
		599: 599 * time.Second, 600: MaxActionTimeout, 601: MaxActionTimeout,
		99999: MaxActionTimeout, 1<<31 - 1: MaxActionTimeout,
		math.MaxInt - 1: MaxActionTimeout, math.MaxInt: MaxActionTimeout,
	}
	for in, want := range cases {
		if got := ActionTimeout(in); got != want {
			t.Errorf("ActionTimeout(%d)=%v want %v", in, got, want)
		}
	}
	big := int64(10_000_000_000)
	if int64(int(big)) == big && ActionTimeout(int(big)) != MaxActionTimeout {
		t.Error("1e10 not clamped")
	}
	if ValidateTimeoutS(-1) == nil || ValidateTimeoutS(0) != nil || ValidateTimeoutS(math.MaxInt) != nil {
		t.Fatal("ValidateTimeoutS")
	}
	if e := ValidateTimeoutS(-1); e.Code != CodeBadRequest {
		t.Fatal(e)
	}
}

func TestMethodSetsDisjoint(t *testing.T) {
	operator := []string{MethodHello, MethodFleetsList, MethodRunnersList, MethodActionRun}
	admin := []string{MethodFleetCreate, MethodTokenCreate, MethodTokenList, MethodTokenRevoke,
		MethodRunnerLifecycle, MethodOperatorCode, MethodOperatorRotateSecret, MethodOperatorList,
		MethodOperatorRevoke, MethodAuditList, MethodGUIResetAdmin, MethodFleetSetConfirm}
	for _, m := range operator {
		if !IsOperatorMethod(m) || IsAdminMethod(m) {
			t.Errorf("%s misclassified", m)
		}
	}
	for _, m := range admin {
		if !IsAdminMethod(m) || IsOperatorMethod(m) {
			t.Errorf("%s misclassified", m)
		}
	}
	for _, m := range []string{"", "runner.approve", "runner.disable", "runner.enable", "runner.revoke", "nope"} {
		if IsOperatorMethod(m) || IsAdminMethod(m) {
			t.Errorf("%q routable", m)
		}
	}
}

func TestAsErrorNil(t *testing.T) {
	if AsError(nil) != nil {
		t.Fatal("AsError(nil) != nil")
	}
}

func TestDecodeParams(t *testing.T) {
	var p TokenCreateParams
	if e := DecodeParams(json.RawMessage(`{"fleet":"f","uses":2}`), &p); e != nil || p.Fleet != "f" || p.Uses != 2 {
		t.Fatalf("%+v %v", p, e)
	}
	for _, in := range []string{``, `null`} {
		var q TokenCreateParams
		if e := DecodeParams(json.RawMessage(in), &q); e != nil {
			t.Errorf("%q refused: %v", in, e)
		}
	}
	for _, in := range []string{`{"fleet":"f","bogus":1}`, `{"fleet":"f"} {}`, `{"fleet":"f"}}`, `[1]`, `{"fleet":`} {
		var q TokenCreateParams
		if e := DecodeParams(json.RawMessage(in), &q); e == nil || e.Code != CodeBadRequest {
			t.Errorf("%q accepted", in)
		}
	}
}

type deadlineWriter struct {
	bytes.Buffer
	set []time.Time
}

func (d *deadlineWriter) SetWriteDeadline(t time.Time) error { d.set = append(d.set, t); return nil }

func TestWriteResponseDeadline(t *testing.T) {
	var w deadlineWriter
	r, _ := NewResult("1", map[string]int{"a": 1})
	if err := WriteResponse(&w, r); err != nil {
		t.Fatal(err)
	}
	if len(w.set) != 2 || w.set[0].IsZero() || !w.set[1].IsZero() {
		t.Fatalf("deadlines %v", w.set)
	}
}

func FuzzReadRequest(f *testing.F) {
	f.Add(frame(`{"v":1,"id":"a","method":"hello"}`))
	f.Add(frame(`{"v":2}`))
	f.Add(frame(`{"v":null,"id":"a","method":"hello"}`))
	f.Add(frame(`{"v":1,"id":"a","method":"hello"} {}`))
	f.Add(frame(`{"v":1,"id":"a","method":"hello"}}`))
	f.Add(frame(`{"v":1,"id":"` + strings.Repeat("i", 200) + `","method":"hello"}`))
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})
	f.Add([]byte{0, 0, 0, 0})
	f.Add(frame(`{"v":1,"id":"a","method":"x","params":[1,2`))
	f.Fuzz(func(t *testing.T, in []byte) {
		req, id, err := ReadRequest(bytes.NewReader(in))
		if err != nil {
			var ae *Error
			if err != io.EOF && !errors.As(err, &ae) {
				t.Fatalf("error is neither *Error nor io.EOF: %T %v", err, err)
			}
			return
		}
		if req == nil {
			t.Fatal("nil request without error")
		}
		if req.V != 1 || req.ID == "" || len(req.ID) > 128 || req.Method == "" || id != req.ID {
			t.Fatalf("invalid accepted request: %+v id=%q", req, id)
		}
	})
}

// slowReader delivers its data only after d, so the arrival timer fires first.
type slowReader struct {
	r io.Reader
	d time.Duration
}

func (s *slowReader) Read(p []byte) (int, error) {
	time.Sleep(s.d)
	return s.r.Read(p)
}

// A request that completes after the timer fired must report a timeout, and a successful
// return must never be followed by a close from the timer. Not parallel: mutates RequestTimeout.
func TestReadRequestTimerNeverClosesAfterSuccess(t *testing.T) {
	old := RequestTimeout
	RequestTimeout = 20 * time.Millisecond
	defer func() { RequestTimeout = old }()
	for _, delay := range []time.Duration{0, 60 * time.Millisecond} {
		var closed atomic.Bool
		cl := closerFunc(func() error { closed.Store(true); return nil })
		sr := &slowReader{r: bytes.NewReader(frame(`{"v":1,"id":"a","method":"hello"}`)), d: delay}
		req, _, err := ReadRequestTimeout(sr, cl)
		if err == nil {
			if req == nil {
				t.Fatal("nil request")
			}
			time.Sleep(60 * time.Millisecond)
			if closed.Load() {
				t.Fatal("timer closed the channel after a successful read")
			}
			continue
		}
		var ae *Error
		if !errors.As(err, &ae) || ae.Code != CodeTimeout {
			t.Fatalf("delay %s: got %v", delay, err)
		}
	}
}

func TestEnrollMethodsDisjoint(t *testing.T) {
	for _, m := range []string{MethodEnrollJoin, MethodEnrollRegister} {
		if !IsEnrollMethod(m) || IsOperatorMethod(m) || IsAdminMethod(m) {
			t.Errorf("%s misclassified", m)
		}
	}
	for _, m := range []string{MethodHello, MethodActionRun, MethodFleetCreate, MethodAuditList, "", "enroll"} {
		if IsEnrollMethod(m) {
			t.Errorf("%q routable on enroll", m)
		}
	}
}
