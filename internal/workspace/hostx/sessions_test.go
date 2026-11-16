//go:build linux

package hostx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/fsops"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newMgr(t *testing.T, mut func(*SessionsConfig)) (*Manager, *clock, string) {
	t.Helper()
	base := t.TempDir()
	clk := &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	cfg := SessionsConfig{Root: filepath.Join(base, "work"), StateDir: filepath.Join(base, "state"), Now: clk.now, Files: FilesOptions{Rg: "-"}}
	if mut != nil {
		mut(&cfg)
	}
	m, err := NewManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return m, clk, base
}

func write(t *testing.T, s *SessionDir, path, body string) {
	t.Helper()
	sum := sha256.Sum256([]byte(body))
	_, _, err := s.Do(context.Background(), transport.FSOp{Op: transport.FSWrite, Path: path, Length: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}, []byte(body))
	if err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestSlug(t *testing.T) {
	for _, id := range []string{"abc-1", "a.b_c"} {
		if Slug(id) != id {
			t.Fatalf("%s", id)
		}
	}
	for _, id := range []string{"../x", "a/b", ".hid", "", "h_abc", strings.Repeat("a", 65), "a b"} {
		s := Slug(id)
		if !strings.HasPrefix(s, "h_") || strings.ContainsAny(s, "/.") {
			t.Fatalf("%q -> %q", id, s)
		}
	}
	if Slug("../x") == Slug("../y") {
		t.Fatal("collision")
	}
}

func TestReattach(t *testing.T) {
	m, clk, _ := newMgr(t, nil)
	s, err := m.Open("sess1", "")
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(s.Dir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("mode %v", fi.Mode())
	}
	write(t, s, "a/b.txt", "hello\n")
	s.Close()
	if _, err := os.Stat(filepath.Join(s.Dir, "a", "b.txt")); err != nil {
		t.Fatal("close must keep the directory")
	}
	clk.t = clk.t.Add(time.Hour)
	s2, err := m.Open("sess1", "")
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if s2.Dir != s.Dir {
		t.Fatal("different dir")
	}
	res, _, err := s2.Do(context.Background(), transport.FSOp{Op: transport.FSRead, Path: "a/b.txt"}, nil)
	if err != nil || !strings.Contains(res.(*fsops.ReadResult).Text, "hello") {
		t.Fatalf("%v %v", res, err)
	}
	infos, _ := m.List(true)
	if len(infos) != 1 || infos[0].ID != "sess1" || infos[0].Size != 6 || !infos[0].LastUsed.Equal(clk.t) {
		t.Fatalf("%+v", infos)
	}
}

func TestConfinement(t *testing.T) {
	m, _, base := newMgr(t, nil)
	secret := filepath.Join(base, "secret.txt")
	os.WriteFile(secret, []byte("TOPSECRET\n"), 0o600)
	s, err := m.Open("c1", "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := os.Symlink(secret, filepath.Join(s.Dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(base, filepath.Join(s.Dir, "dirlink")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// File tools reach what the OS user can see: links, "..", absolute paths.
	for _, op := range []transport.FSOp{
		{Op: transport.FSRead, Path: "link"},
		{Op: transport.FSRead, Path: "dirlink/secret.txt"},
		{Op: transport.FSRead, Path: secret},
		{Op: transport.FSLS, Path: base},
	} {
		if _, _, err := s.Do(ctx, op, nil); err != nil {
			t.Errorf("%+v: %v", op, err)
		}
	}
	// Search walks only real files below the path (symlinks are not followed), absolute path included.
	res, _, err := s.Do(ctx, transport.FSOp{Op: transport.FSGrep, Pattern: "TOPSECRET"}, nil)
	if err != nil || len(res.(fsops.GrepResult).Files) != 0 {
		t.Fatalf("grep: %+v %v", res, err)
	}
	res, _, err = s.Do(ctx, transport.FSOp{Op: transport.FSGrep, Pattern: "TOPSECRET", Path: base}, nil)
	if err != nil || !strings.Contains(strings.Join(res.(fsops.GrepResult).Files, ","), secret) {
		t.Fatalf("grep abs: %+v %v", res, err)
	}
	gl, _, err := s.Do(ctx, transport.FSOp{Op: transport.FSGlob, Pattern: "*.txt", Path: base}, nil)
	if err != nil || !strings.Contains(strings.Join(gl.(fsops.GlobResult).Paths, ","), secret) {
		t.Fatalf("glob abs: %+v %v", gl, err)
	}
	// A symlink planted in place of the session dir is refused on reattach.
	m2, _, _ := newMgr(t, nil)
	if err := os.Symlink(base, filepath.Join(m2.cfg.sessionsDir(), "evil")); err != nil {
		t.Fatal(err)
	}
	if _, err := m2.Open("evil", ""); err == nil {
		t.Fatal("symlinked session dir accepted")
	}
}

func TestWorkdir(t *testing.T) {
	m, _, base := newMgr(t, nil)
	wd := filepath.Join(base, "checkout")
	os.MkdirAll(filepath.Join(wd, "src"), 0o755)
	os.WriteFile(filepath.Join(wd, "src", "x.go"), []byte("package x\n"), 0o644)
	os.WriteFile(filepath.Join(base, "outside"), []byte("o"), 0o644)
	if _, err := m.Open("w", "relative/dir"); err == nil {
		t.Fatal("relative workdir accepted")
	}
	if _, err := m.Open("w", filepath.Join(base, "missing")); err == nil {
		t.Fatal("missing workdir accepted")
	}
	s, err := m.Open("w", wd)
	if err != nil {
		t.Fatal(err)
	}
	if s.Own || s.Dir != wd {
		t.Fatalf("%+v", s)
	}
	ctx := context.Background()
	res, _, err := s.Do(ctx, transport.FSOp{Op: transport.FSRead, Path: "src/x.go"}, nil)
	if err != nil || !strings.Contains(res.(*fsops.ReadResult).Text, "package x") {
		t.Fatalf("%v %v", res, err)
	}
	if _, _, err := s.Do(ctx, transport.FSOp{Op: transport.FSRead, Path: "../outside"}, nil); err != nil {
		t.Fatalf("the OS user can read outside the workdir: %v", err)
	}
	s.Close()
	if err := m.Delete("w"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(wd, "src", "x.go")); err != nil {
		t.Fatal("delete must never touch a workdir")
	}
	if _, err := os.Stat(m.metaPath("w")); err == nil {
		t.Fatal("record kept")
	}
	if len(mustList(t, m)) != 0 {
		t.Fatal("listed")
	}
}

func mustList(t *testing.T, m *Manager) []Info {
	t.Helper()
	l, err := m.List(false)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestDispatchAllOps(t *testing.T) {
	m, _, _ := newMgr(t, nil)
	s, _ := m.Open("ops", "")
	defer s.Close()
	ctx := context.Background()
	write(t, s, "f.txt", "one\ntwo\nthree\n")
	do := func(op transport.FSOp) any {
		t.Helper()
		r, _, err := s.Do(ctx, op, nil)
		if err != nil {
			t.Fatalf("%s: %v", op.Op, err)
		}
		return r
	}
	do(transport.FSOp{Op: transport.FSEdit, Path: "f.txt", OldString: "two", NewString: "2"})
	do(transport.FSOp{Op: transport.FSMultiEdit, Path: "f.txt", Edits: []transport.FSEditItem{{OldString: "one", NewString: "1"}, {OldString: "three", NewString: "3"}}})
	if b, _ := os.ReadFile(filepath.Join(s.Dir, "f.txt")); string(b) != "1\n2\n3\n" {
		t.Fatalf("%q", b)
	}
	nb := `{"cells":[{"cell_type":"code","source":["x=1"],"metadata":{},"outputs":[],"execution_count":null,"id":"c1"}],"metadata":{},"nbformat":4,"nbformat_minor":5}`
	write(t, s, "n.ipynb", nb)
	do(transport.FSOp{Op: transport.FSNotebookEdit, Path: "n.ipynb", CellID: "c1", NewSource: "x=2", EditMode: transport.NotebookReplace})
	if g := do(transport.FSOp{Op: transport.FSGrep, Pattern: "2", OutputMode: "content", LineNumbers: true, Path: "f.txt"}).(fsops.GrepResult); len(g.Lines) != 1 || g.Lines[0] != "f.txt:2:2" {
		t.Fatalf("%+v", g)
	}
	do(transport.FSOp{Op: transport.FSGlob, Pattern: "*.txt"})
	do(transport.FSOp{Op: transport.FSLS})
	if in := do(transport.FSOp{Op: transport.FSStat, Path: "f.txt"}).(*fsops.Info); in.Size != 6 {
		t.Fatalf("%+v", in)
	}
	// Write checks the body against length and hash.
	if _, _, err := s.Do(ctx, transport.FSOp{Op: transport.FSWrite, Path: "bad", Length: 3, SHA256: strings.Repeat("0", 64)}, []byte("abc")); err == nil {
		t.Fatal("hash mismatch accepted")
	}
	if _, _, err := s.Do(ctx, transport.FSOp{Op: "rm"}, nil); !errors.Is(err, fsops.ErrInvalid) {
		t.Fatalf("%v", err)
	}
}
