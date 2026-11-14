package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
)

func TestMCPCallLimitWorkspace(t *testing.T) {
	const m = mcpRunMargin
	for _, tc := range []struct {
		method, params string
		want           time.Duration
	}{
		{api.MethodWorkspaceExec, `{"timeout_s":600}`, 600*time.Second + m},
		{api.MethodWorkspaceExec, `{"timeout_s":3600}`, time.Hour + m},
		{api.MethodWorkspaceExec, `{"timeout_s":999999999}`, 24*time.Hour + m},
		{api.MethodWorkspaceExec, `{"timeout_s":86400}`, 24*time.Hour + m},
		{api.MethodWorkspaceExec, `{}`, 24*time.Hour + m}, // fleet default unknown: the server's deadline rules
		{api.MethodWorkspaceExec, ``, 24*time.Hour + m},
		{api.MethodWorkspaceExec, `{bad`, 24*time.Hour + m},
		{api.MethodWorkspaceExec, `{"timeout_s":-5}`, 24*time.Hour + m},
		{api.MethodWorkspaceOpen, `{}`, 5*time.Minute + m},
		{api.MethodWorkspaceReadFile, `{}`, 100 * time.Second},
		{api.MethodWorkspaceWriteFile, `{}`, 100 * time.Second},
		{api.MethodWorkspaceClose, `{}`, mcpCallTimeout},
		{api.MethodWorkspaceList, ``, mcpCallTimeout},
		{api.MethodRunnersList, `{}`, mcpCallTimeout},
		{api.MethodActionRun, `{"timeout_s":10}`, 10*time.Second + m},
	} {
		if got := mcpCallLimit(tc.method, json.RawMessage(tc.params)); got != tc.want {
			t.Errorf("%s %s = %s, want %s", tc.method, tc.params, got, tc.want)
		}
	}
}

func TestParseMCPAPIArgs(t *testing.T) {
	a, err := parseMCPAPIArgs([]string{api.MethodWorkspaceWriteFile, `{"a":1}`, "-in", "f.txt"})
	if err != nil || a.method != api.MethodWorkspaceWriteFile || a.in != "f.txt" || string(a.params) != `{"a":1}` {
		t.Fatalf("%+v %v", a, err)
	}
	// Flags may come first or after the positionals.
	if a, err = parseMCPAPIArgs([]string{"-out", "o.bin", api.MethodWorkspaceReadFile, `{}`}); err != nil || a.out != "o.bin" {
		t.Fatalf("%+v %v", a, err)
	}
	if a, err = parseMCPAPIArgs([]string{"hello"}); err != nil || a.params != nil {
		t.Fatalf("%+v %v", a, err)
	}
	for name, args := range map[string][]string{
		"no method":           {},
		"three positionals":   {"a", "{}", "c"},
		"bad json":            {"hello", "{bad"},
		"in on a plain call":  {"runners.list", "{}", "-in", "x"},
		"out on a plain call": {"runners.list", "{}", "-out", "x"},
		"in on read_file":     {api.MethodWorkspaceReadFile, "{}", "-in", "x", "-out", "y"},
		"out on write_file":   {api.MethodWorkspaceWriteFile, "{}", "-in", "x", "-out", "y"},
		"read without out":    {api.MethodWorkspaceReadFile, "{}"},
		"write without in":    {api.MethodWorkspaceWriteFile, "{}"},
		"unknown flag":        {"hello", "-nope"},
		"out stdout":          {api.MethodWorkspaceReadFile, "{}", "-out", "-"},
	} {
		if _, err := parseMCPAPIArgs(args); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestWithBodyParams(t *testing.T) {
	body := []byte("hello")
	sum := sha256.Sum256(body)
	out, err := withBodyParams(api.MethodWorkspaceWriteFile, json.RawMessage(`{"workspace":"w","path":"a"}`), body)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(out, &m)
	if m["length"] != float64(5) || m["sha256"] != hex.EncodeToString(sum[:]) || m["path"] != "a" {
		t.Errorf("write_file: %s", out)
	}
	// Caller-supplied values win (the server then refuses a mismatch).
	out, _ = withBodyParams(api.MethodWorkspaceWriteFile, json.RawMessage(`{"length":99,"sha256":"x"}`), body)
	json.Unmarshal(out, &m)
	if m["length"] != float64(99) || m["sha256"] != "x" {
		t.Errorf("explicit values overridden: %s", out)
	}
	out, _ = withBodyParams(api.MethodWorkspaceExec, json.RawMessage(`{"workspace":"w","argv":["cat"]}`), body)
	json.Unmarshal(out, &m)
	if m["stdin"] != true {
		t.Errorf("exec: %s", out)
	}
	out, _ = withBodyParams(api.MethodWorkspaceWriteFile, nil, nil)
	json.Unmarshal(out, &m)
	if m["length"] != float64(0) || m["sha256"] != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Errorf("empty body: %s", out)
	}
	if _, err := withBodyParams(api.MethodWorkspaceExec, json.RawMessage(`[1]`), body); err == nil {
		t.Error("non-object params accepted")
	}
}

func TestReadBodyFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "b")
	os.WriteFile(p, []byte("abc"), 0o600)
	if b, err := readBodyFile(p, nil); err != nil || string(b) != "abc" {
		t.Errorf("file: %q %v", b, err)
	}
	if b, err := readBodyFile("-", strings.NewReader("from stdin")); err != nil || string(b) != "from stdin" {
		t.Errorf("stdin: %q %v", b, err)
	}
	if _, err := readBodyFile("-", nil); err == nil {
		t.Error("nil stdin accepted")
	}
	exact := bytes.Repeat([]byte("x"), api.MaxFileChunk)
	os.WriteFile(p, exact, 0o600)
	if b, err := readBodyFile(p, nil); err != nil || len(b) != api.MaxFileChunk {
		t.Errorf("exact limit: %d %v", len(b), err)
	}
	os.WriteFile(p, append(exact, 'y'), 0o600)
	if _, err := readBodyFile(p, nil); err == nil || !strings.Contains(err.Error(), "chunks") {
		t.Errorf("over the limit: %v", err)
	}
	if _, err := readBodyFile(filepath.Join(dir, "missing"), nil); err == nil {
		t.Error("missing file accepted")
	}
}

func TestCheckOutPath(t *testing.T) {
	dir := t.TempDir()
	if err := checkOutPath(filepath.Join(dir, "f.bin")); err != nil {
		t.Errorf("good path: %v", err)
	}
	if err := checkOutPath(filepath.Join(dir, "nodir", "f.bin")); err == nil {
		t.Error("missing directory accepted")
	}
	if err := checkOutPath(dir); err == nil {
		t.Error("directory as output accepted")
	}
}

func TestWriteReadFileBodyErrors(t *testing.T) {
	out := filepath.Join(t.TempDir(), "o.bin")
	// E-09: an undecodable result is reported as such, not as a length mismatch.
	err := writeReadFileBody(out, json.RawMessage(`{"length":"x"}`), []byte("abc"))
	if err == nil || !strings.Contains(err.Error(), "bad result") || strings.Contains(err.Error(), "announces") {
		t.Fatalf("unmarshal error: %v", err)
	}
	err = writeReadFileBody(out, json.RawMessage(`{"length":5}`), []byte("abc"))
	if err == nil || !strings.Contains(err.Error(), "announces 5") || strings.Contains(err.Error(), "bad result") {
		t.Fatalf("length mismatch: %v", err)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("file written despite errors")
	}
	if err := writeReadFileBody(out, json.RawMessage(`{"length":3}`), []byte("abc")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(out); string(b) != "abc" {
		t.Fatalf("body %q", b)
	}
}
