package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/opconf"
)

func TestParseServeFlags(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "proj")
	for _, d := range []string{"x", filepath.Join("a", "c")} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(cwd, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	var errw bytes.Buffer
	o, err := parseServeFlags([]string{"-context", "c1", "-fleet", "f1", "-session", "s1", "-project-dir", "sub/../x"}, cwd, &errw)
	if err != nil {
		t.Fatal(err)
	}
	if o.context != "c1" || o.fleet != "f1" || o.session != "s1" {
		t.Errorf("flags: %+v", o)
	}
	want, err := filepath.Abs(filepath.Join(cwd, "x"))
	if err != nil {
		t.Fatal(err)
	}
	if o.projectDir != want {
		t.Errorf("relative project-dir = %q, want %q", o.projectDir, want)
	}
	o, err = parseServeFlags(nil, cwd, &errw)
	if err != nil || o.projectDir != cwd {
		t.Errorf("default project-dir = %q, %v", o.projectDir, err)
	}
	abs := filepath.Join(root, "a", "b", "..", "c")
	o, _ = parseServeFlags([]string{"-project-dir", abs}, cwd, &errw)
	if want := filepath.Join(root, "a", "c"); o.projectDir != want {
		t.Errorf("absolute project-dir = %q, want %q", o.projectDir, want)
	}
	if _, err := parseServeFlags([]string{"-bogus"}, cwd, &errw); err == nil {
		t.Error("unknown flag accepted")
	}
	if _, err := parseServeFlags([]string{"extra"}, cwd, &errw); err == nil {
		t.Error("positional argument accepted")
	}
}

func TestParseServeFlagsProjectDir(t *testing.T) {
	cwd := t.TempDir()
	if err := os.Mkdir(filepath.Join(cwd, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, dir string
		want      string // "" for the error cases
		errSub    string
	}{
		{"unexpanded brace", "${workspaceFolder}", "", "unexpanded variable"},
		{"unexpanded paren", "$(pwd)", "", "unexpanded variable"},
		{"embedded variable", "${HOME}/x", "", "unexpanded variable"},
		{"missing", "nope", "", "is not a directory"},
		{"file", "file", "", "is not a directory"},
		{"dot", ".", cwd, ""},
		{"relative", "sub/../sub", filepath.Join(cwd, "sub"), ""},
		{"absolute", filepath.Join(cwd, "sub"), filepath.Join(cwd, "sub"), ""},
		{"empty is cwd", "", cwd, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var args []string
			if c.dir != "" {
				args = []string{"-project-dir", c.dir}
			}
			o, err := parseServeFlags(args, cwd, io.Discard)
			if c.errSub != "" {
				if err == nil || !strings.Contains(err.Error(), c.errSub) || !strings.Contains(err.Error(), c.dir) {
					t.Fatalf("err = %v, want %q naming %q", err, c.errSub, c.dir)
				}
				return
			}
			if err != nil || o.projectDir != c.want {
				t.Fatalf("projectDir = %q, %v; want %q", o.projectDir, err, c.want)
			}
		})
	}
}

func TestServeHelp(t *testing.T) {
	var errw bytes.Buffer
	if _, err := parseServeFlags([]string{"-h"}, t.TempDir(), &errw); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("parse -h err = %v, want flag.ErrHelp", err)
	}
	for _, f := range []string{"-context", "-fleet", "-session", "-project-dir"} {
		if !strings.Contains(errw.String(), f) {
			t.Errorf("help on stderr lacks %s: %q", f, errw.String())
		}
	}
	var out, stderr bytes.Buffer
	if err := runServe(context.Background(), func() {}, []string{"-h"}, strings.NewReader(""), &out, &stderr); err != nil {
		t.Errorf("runServe -h = %v, want nil (exit 0)", err)
	}
	if out.Len() != 0 {
		t.Errorf("help wrote to stdout: %q", out.String())
	}
	if !strings.Contains(stderr.String(), "-project-dir") {
		t.Errorf("help missing from stderr: %q", stderr.String())
	}
}

func TestMCPMainArgs(t *testing.T) {
	var out bytes.Buffer
	for _, args := range [][]string{{""}, {"bogus"}} {
		if err := mcpMain(args, strings.NewReader(""), &out); err == nil || !strings.Contains(err.Error(), "unknown mcp command") {
			t.Errorf("mcp %q: err = %v, want unknown mcp command", args, err)
		}
	}
}

type serveRun struct {
	in     *io.PipeWriter
	out    *bufio.Reader
	stderr *lockedBuf
	done   chan error
	cancel context.CancelFunc
	lines  []string
}

func startServe(t *testing.T) *serveRun {
	t.Helper()
	t.Setenv(opconf.EnvDir, t.TempDir())
	t.Setenv(opconf.EnvStateDir, t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "decoy-anthropic-key-s10")
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	r := &serveRun{in: inW, out: bufio.NewReader(outR), stderr: &lockedBuf{}, done: make(chan error, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	t.Cleanup(cancel)
	go func() {
		err := runServe(ctx, func() {}, []string{"-project-dir", t.TempDir()}, inR, outW, r.stderr)
		outW.Close()
		r.done <- err
	}()
	return r
}

func (r *serveRun) send(t *testing.T, s string) {
	t.Helper()
	if _, err := io.WriteString(r.in, s+"\n"); err != nil {
		t.Fatal(err)
	}
}

// recv reads the next stdout line and asserts it is a JSON-RPC 2.0 message.
func (r *serveRun) recv(t *testing.T) map[string]any {
	t.Helper()
	type res struct {
		line string
		err  error
	}
	ch := make(chan res, 1)
	go func() { l, err := r.out.ReadString('\n'); ch <- res{l, err} }()
	select {
	case x := <-ch:
		if x.err != nil {
			t.Fatalf("read stdout: %v", x.err)
		}
		r.lines = append(r.lines, x.line)
		var m map[string]any
		if err := json.Unmarshal([]byte(x.line), &m); err != nil || m["jsonrpc"] != "2.0" {
			t.Fatalf("stdout line is not JSON-RPC: %q (%v)", x.line, err)
		}
		return m
	case <-time.After(10 * time.Second):
		t.Fatal("timeout reading stdout")
	}
	return nil
}

func TestServeOverPipes(t *testing.T) {
	r := startServe(t)
	r.send(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`)
	if m := r.recv(t); m["error"] != nil || m["id"] != float64(1) {
		t.Fatalf("initialize: %v", m)
	}
	r.send(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	r.send(t, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	m := r.recv(t)
	tools, _ := m["result"].(map[string]any)["tools"].([]any)
	var names []string
	for _, x := range tools {
		names = append(names, x.(map[string]any)["name"].(string))
	}
	if len(names) == 0 || !strings.Contains(strings.Join(names, ","), "list_fleets") {
		t.Fatalf("tools/list = %v", names)
	}
	r.send(t, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_fleets","arguments":{}}}`)
	m = r.recv(t)
	b, _ := json.Marshal(m)
	if !strings.Contains(string(b), "not set up") || !strings.Contains(string(b), "mirafleet mcp login") {
		t.Fatalf("expected not-set-up error, got %s", b)
	}

	// stdin EOF: clean, quick exit 0.
	start := time.Now()
	r.in.Close()
	select {
	case err := <-r.done:
		if err != nil {
			t.Fatalf("exit on EOF: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not exit on stdin EOF")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("EOF shutdown took %s", d)
	}
	// Hygiene: every stdout line was JSON-RPC (checked in recv); drain any extra.
	rest, _ := io.ReadAll(r.out)
	for _, l := range strings.Split(strings.TrimSpace(string(rest)), "\n") {
		if l != "" {
			t.Errorf("extra stdout after shutdown: %q", l)
		}
	}
	all := strings.Join(r.lines, "") + string(rest) + r.stderr.String()
	if strings.Contains(all, "decoy-anthropic-key-s10") {
		t.Error("decoy env value leaked into output")
	}
}

func TestServeSignalCancel(t *testing.T) {
	r := startServe(t)
	r.cancel()
	select {
	case err := <-r.done:
		if err != nil {
			t.Fatalf("exit on cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not exit on context cancel")
	}
}

func TestServeMalformedFrameExitsNonZero(t *testing.T) {
	r := startServe(t)
	r.send(t, `{not json`)
	select {
	case err := <-r.done:
		if err == nil {
			t.Fatal("malformed frame exited 0")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not exit on malformed frame")
	}
	if strings.Contains(strings.Join(r.lines, ""), "not json") {
		t.Error("input echoed to stdout")
	}
}

func TestMCPContextCommands(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(opconf.EnvDir, dir)
	var out bytes.Buffer
	if err := mcpMain([]string{"context", "list"}, nil, &out); err != nil || !strings.Contains(out.String(), "no contexts") {
		t.Fatalf("empty list: %v %q", err, out.String())
	}
	c := &opconf.Contexts{
		Current: "a",
		Contexts: []opconf.Context{
			{Name: "a", Addr: "10.0.0.1:2222", DefaultFleet: "web"},
			{Name: "b", Addr: "10.0.0.2:2222"},
		},
	}
	if err := opconf.SaveContexts(dir, c); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := mcpMain([]string{"context", "list"}, nil, &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{"* a", "10.0.0.1:2222", "default_fleet=web", "  b", "default_fleet=-"} {
		if !strings.Contains(s, want) {
			t.Errorf("list missing %q in %q", want, s)
		}
	}
	if err := mcpMain([]string{"context", "use", "b"}, nil, &out); err != nil {
		t.Fatal(err)
	}
	got, _ := opconf.LoadContexts(dir)
	if got.Current != "b" {
		t.Errorf("current = %q", got.Current)
	}
	if err := mcpMain([]string{"context", "use", "nope"}, nil, &out); err == nil {
		t.Error("use of unknown context accepted")
	}
	if err := mcpMain([]string{"context", "remove", "nope"}, nil, &out); err == nil {
		t.Error("remove of unknown context accepted")
	}
	if err := mcpMain([]string{"context", "remove", "a"}, nil, &out); err != nil {
		t.Fatal(err)
	}
	got, _ = opconf.LoadContexts(dir)
	if len(got.Contexts) != 1 || got.Contexts[0].Name != "b" {
		t.Errorf("after remove: %+v", got.Contexts)
	}
	for _, bad := range [][]string{{}, {"bogus"}, {"use"}, {"list", "x"}} {
		if err := mcpMain(append([]string{"context"}, bad...), nil, &out); err == nil {
			t.Errorf("context %v accepted", bad)
		}
	}
}
