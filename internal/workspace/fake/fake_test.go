package fake

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
)

func spec() workspace.Spec {
	return workspace.Spec{ID: workspace.NewID(), Image: "alpine:3.20", Labels: map[string]string{"k": "v"}}
}

func TestLifecycle(t *testing.T) {
	ctx := context.Background()
	e := New()
	s := spec()
	if err := e.Create(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err := e.Create(ctx, s); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
	l, _ := e.List(ctx)
	if len(l) != 1 || l[0].ID != s.ID || l[0].Labels["k"] != "v" || l[0].State != workspace.StateRunning {
		t.Fatalf("%+v", l)
	}
	if err := e.Destroy(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.Destroy(ctx, s.ID); err != nil {
		t.Fatalf("destroy not idempotent: %v", err)
	}
	if _, err := e.Exec(ctx, s.ID, workspace.ExecRequest{Argv: []string{"x"}}); !errors.Is(err, workspace.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if l, _ := e.List(ctx); len(l) != 0 {
		t.Fatal(l)
	}
}

func TestCreateRefusals(t *testing.T) {
	ctx := context.Background()
	e := New()
	e.Images = []string{"alpine:*"}
	s := spec()
	s.Image = "busybox"
	if err := e.Create(ctx, s); !errors.Is(err, workspace.ErrRefused) {
		t.Fatalf("%v", err)
	}
	s = spec()
	s.ID = "bad"
	if err := e.Create(ctx, s); !errors.Is(err, workspace.ErrRefused) {
		t.Fatalf("%v", err)
	}
	s = spec()
	s.RequireSecureRuntime = true
	if err := e.Create(ctx, s); !errors.Is(err, workspace.ErrRefused) {
		t.Fatalf("%v", err)
	}
	e.SecureRuntime = true
	if err := e.Create(ctx, s); err != nil {
		t.Fatal(err)
	}
	e.Slots = 1
	if err := e.Create(ctx, spec()); !errors.Is(err, workspace.ErrUnavailable) {
		t.Fatalf("%v", err)
	}
}

func TestExec(t *testing.T) {
	ctx := context.Background()
	e := New()
	s := spec()
	e.Create(ctx, s)
	e.ExecFunc = func(ctx context.Context, id workspace.ID, r workspace.ExecRequest) (workspace.ExecResult, error) {
		in, _ := io.ReadAll(r.Stdin)
		r.Stdout.Write(append([]byte(r.Argv[0]+":"), in...))
		return workspace.ExecResult{ExitCode: 3}, nil
	}
	var out bytes.Buffer
	res, err := e.Exec(ctx, s.ID, workspace.ExecRequest{Argv: []string{"cat"}, Stdin: bytes.NewReader([]byte("hi")), Stdout: &out})
	if err != nil || res.ExitCode != 3 || out.String() != "cat:hi" {
		t.Fatalf("%+v %v %q", res, err, out.String())
	}
	for _, r := range []workspace.ExecRequest{{}, {Argv: []string{"x"}, Cwd: "/etc"}, {Argv: []string{"x"}, Cwd: "../x"}} {
		if _, err := e.Exec(ctx, s.ID, r); !errors.Is(err, workspace.ErrRefused) {
			t.Errorf("%+v: %v", r, err)
		}
	}
}

func TestExecCancelAndTimeout(t *testing.T) {
	e := New()
	s := spec()
	e.Create(context.Background(), s)
	e.ExecFunc = func(ctx context.Context, _ workspace.ID, _ workspace.ExecRequest) (workspace.ExecResult, error) {
		<-ctx.Done()
		return workspace.ExecResult{ExitCode: -1}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	res, err := e.Exec(ctx, s.ID, workspace.ExecRequest{Argv: []string{"sleep"}})
	if !res.TimedOut || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%+v %v", res, err)
	}
	cctx, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if _, err := e.Exec(cctx, s.ID, workspace.ExecRequest{Argv: []string{"x"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
	if err := e.Create(cctx, spec()); !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
}

func TestFiles(t *testing.T) {
	ctx := context.Background()
	e := New()
	s := spec()
	e.Create(ctx, s)
	fi, err := e.WriteFile(ctx, s.ID, "a/b.txt", bytes.NewReader([]byte("hello")), 0, true, 0o644)
	if err != nil || fi.Size != 5 || fi.Mode != 0o644 {
		t.Fatalf("%+v %v", fi, err)
	}
	if _, err := e.WriteFile(ctx, s.ID, "/workspace/a/b.txt", bytes.NewReader([]byte("XY")), 5, false, 0); err != nil {
		t.Fatal(err)
	}
	rc, info, err := e.ReadFile(ctx, s.ID, "/workspace/a/b.txt", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	if string(b) != "helloXY" || info.Size != 7 {
		t.Fatalf("%q %+v", b, info)
	}
	rc, _, _ = e.ReadFile(ctx, s.ID, "a/b.txt", 2, 3)
	if b, _ := io.ReadAll(rc); string(b) != "llo" {
		t.Fatalf("%q", b)
	}
	rc, _, _ = e.ReadFile(ctx, s.ID, "a/b.txt", 2, math.MaxInt64) // A-10: no overflow
	if b, _ := io.ReadAll(rc); string(b) != "lloXY" {
		t.Fatalf("%q", b)
	}
	rc, _, _ = e.ReadFile(ctx, s.ID, "a/b.txt", 100, 3)
	if b, _ := io.ReadAll(rc); len(b) != 0 {
		t.Fatalf("%q", b)
	}
	if _, _, err := e.ReadFile(ctx, s.ID, "nope", 0, 0); !errors.Is(err, workspace.ErrNotFound) {
		t.Fatal(err)
	}
	if _, _, err := e.ReadFile(ctx, s.ID, "../../etc/passwd", 0, 0); !errors.Is(err, workspace.ErrRefused) {
		t.Fatal(err)
	}
	if _, err := e.WriteFile(ctx, s.ID, "/etc/x", bytes.NewReader(nil), 0, true, 0); !errors.Is(err, workspace.ErrRefused) {
		t.Fatal(err)
	}
	e.MaxFile = 8
	if _, err := e.WriteFile(ctx, s.ID, "big", bytes.NewReader(make([]byte, 9)), 0, true, 0o600); !errors.Is(err, workspace.ErrTooLarge) {
		t.Fatal(err)
	}
	if _, err := e.WriteFile(ctx, s.ID, "a/b.txt", bytes.NewReader([]byte("zz")), 7, false, 0); !errors.Is(err, workspace.ErrTooLarge) {
		t.Fatal(err)
	}
	if _, _, err := e.ReadFile(ctx, workspace.NewID(), "a", 0, 0); !errors.Is(err, workspace.ErrNotFound) {
		t.Fatal(err)
	}
	l, _ := e.List(ctx)
	if l[0].DiskBytes != 7 {
		t.Fatalf("%+v", l)
	}
}

func TestClose(t *testing.T) {
	e := New()
	e.Close()
	if err := e.Create(context.Background(), spec()); !errors.Is(err, workspace.ErrUnavailable) {
		t.Fatal(err)
	}
	if e.Name() != "fake" {
		t.Fatal()
	}
}
