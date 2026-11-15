package orch

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
)

func writeLocal(t *testing.T, root, rel string, data []byte) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLocalPathDenyAndConfinement(t *testing.T) {
	root := t.TempDir()
	deny := []string{
		"", ".", "..", "../x", "a/../../x", `..\x`, `a\..\..\x`, "/etc/passwd", `\\host\share\f`, `C:\Windows\x`, "C:x",
		".git/config", "sub/.git/HEAD", ".GIT/config", ".claude/settings.json", ".mirafleet.yaml", ".MiraFleet.YAML",
		".env", ".env.local", "app/.ENV.prod", "k.pem", "a/k.KEY", "id_rsa", "id_ed25519.pub", ".npmrc", ".netrc",
		".ssh/config", "x/.ssh/id", ".mirafleet/key", ".aws/credentials", ".gnupg/x", "Keychains/login.keychain-db",
		"login.keychain", "c.p12", "NUL", "con.txt", "COM1", "lpt9.log", "a/aux", "f.txt.", "f ", "f.txt:stream",
		`sub\.env`, `.ssh\id`,
		"GIT~1/hooks/x", "CLAUDE~1/settings.json", "MIRAFL~1.YAM", "a~b.txt", ".g" + string(rune(0x200d)) + "it/config", string(rune(0xfeff)) + ".git/x", ".cl" + string(rune(0x2060)) + "aude/s",
		".mcp.json", ".vscode/tasks.json", ".opencode/x", "opencode.json", ".kilocode/x", ".husky/pre-commit", "sub/.MCP.json",
	}
	for _, n := range deny {
		if rel, err := LocalPath(root, n); err == nil {
			t.Errorf("%q accepted as %q", n, rel)
		}
	}
	ok := map[string]string{
		"a.txt": "a.txt", "./sub/a.txt": "sub/a.txt", `sub\a.txt`: "sub/a.txt", "sub//a.txt": "sub/a.txt",
		"envelope.txt": "envelope.txt", "idea.md": "idea.md", "keyboard.go": "keyboard.go",
		filepath.Join(root, "x", "y.txt"): "x/y.txt",
	}
	for in, want := range ok {
		if got, err := LocalPath(root, in); err != nil || got != want {
			t.Errorf("%q -> %q, %v (want %q)", in, got, err, want)
		}
	}
	// An absolute path outside the project is refused.
	other := t.TempDir()
	if _, err := LocalPath(root, filepath.Join(other, "f")); err == nil {
		t.Error("absolute path outside the project accepted")
	}
	// Errors never carry the project's absolute path.
	_, err := LocalPath(root, filepath.Join(other, "f"))
	if err != nil && strings.Contains(err.Error(), root) {
		t.Error("error leaks the project directory")
	}
}

func TestUploadChunksAndMode(t *testing.T) {
	o, dc := devOrch(t)
	root := t.TempDir()
	big := make([]byte, 2*api.MaxFileChunk+123)
	_, _ = rand.Read(big)
	writeLocal(t, root, "src/big.bin", big)
	writeLocal(t, root, "empty.txt", nil)
	exact := make([]byte, api.MaxFileChunk)
	writeLocal(t, root, "exact.bin", exact)

	r, err := o.Upload(context.Background(), "a", root, UploadParams{Workspace: wsID, Local: "src/big.bin", Remote: "work/big.bin", Confirm: "f"})
	if err != nil || r.Bytes != int64(len(big)) || r.Local != "src/big.bin" || r.Remote != "work/big.bin" {
		t.Fatalf("%+v %v", r, err)
	}
	if !bytes.Equal(dc.files["work/big.bin"], big) || len(dc.calls) != 3 {
		t.Fatalf("calls=%d equal=%v", len(dc.calls), bytes.Equal(dc.files["work/big.bin"], big))
	}
	if !strings.Contains(string(dc.params[0]), `"truncate":true`) || strings.Contains(string(dc.params[1]), `"truncate"`) ||
		!strings.Contains(string(dc.params[2]), `"confirm":"f"`) {
		t.Fatalf("%s | %s", dc.params[0], dc.params[1])
	}

	// default remote path = local relative path; an empty file is still created.
	if r, err = o.Upload(context.Background(), "a", root, UploadParams{Workspace: wsID, Local: "empty.txt"}); err != nil || r.Bytes != 0 || r.Remote != "empty.txt" {
		t.Fatalf("%+v %v", r, err)
	}
	if v, ok := dc.files["empty.txt"]; !ok || len(v) != 0 {
		t.Fatal("empty file not created")
	}
	// A file that is an exact multiple of the chunk size is not followed by an empty write.
	before := len(dc.calls)
	if _, err = o.Upload(context.Background(), "a", root, UploadParams{Workspace: wsID, Local: "exact.bin"}); err != nil || len(dc.calls)-before != 1 {
		t.Fatalf("%v calls=%d", err, len(dc.calls)-before)
	}
}

func TestUploadRefusals(t *testing.T) {
	o, dc := devOrch(t)
	root := t.TempDir()
	outside := t.TempDir()
	writeLocal(t, root, "ok.txt", []byte("x"))
	writeLocal(t, root, ".env", []byte("SECRET=1"))
	writeLocal(t, root, ".git/config", []byte("x"))
	writeLocal(t, root, "dir/file.txt", []byte("x"))
	writeLocal(t, outside, "secret.txt", []byte("outside"))
	if runtime.GOOS != "windows" {
		if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "link.txt")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "linkdir")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(".env", filepath.Join(root, "innocent.txt")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(".git", filepath.Join(root, "gitlink")); err != nil {
			t.Fatal(err)
		}
	}
	names := []string{"nope.txt", ".env", ".git/config", "../" + filepath.Base(outside) + "/secret.txt",
		filepath.Join(outside, "secret.txt"), "dir", ""}
	if runtime.GOOS != "windows" {
		names = append(names, "link.txt", "linkdir/secret.txt", "innocent.txt", "gitlink/config")
	}
	for _, n := range names {
		if r, err := o.Upload(context.Background(), "a", root, UploadParams{Workspace: wsID, Local: n}); err == nil {
			t.Errorf("%q uploaded: %+v", n, r)
		} else if strings.Contains(err.Error(), root) || strings.Contains(err.Error(), outside) {
			t.Errorf("%q: error leaks an absolute path: %v", n, err)
		}
	}
	if len(dc.calls) != 0 {
		t.Fatalf("refused uploads reached the backend: %v", dc.calls)
	}
	if _, err := o.Upload(context.Background(), "a", "relative/dir", UploadParams{Workspace: wsID, Local: "ok.txt"}); err == nil {
		t.Error("relative project dir accepted")
	}
}

func TestUploadTooLarge(t *testing.T) {
	o, dc := devOrch(t)
	root := t.TempDir()
	f, err := os.Create(filepath.Join(root, "huge.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(MaxTransferBytes + 1); err != nil {
		t.Skip("sparse files unsupported")
	}
	f.Close()
	if _, err := o.Upload(context.Background(), "a", root, UploadParams{Workspace: wsID, Local: "huge.bin"}); err == nil {
		t.Fatal("oversize file accepted")
	}
	if len(dc.calls) != 0 {
		t.Fatal("oversize upload reached the backend")
	}
}

func TestDownload(t *testing.T) {
	o, dc := devOrch(t)
	root := t.TempDir()
	big := make([]byte, api.MaxFileChunk+77)
	_, _ = rand.Read(big)
	dc.files["out/b.bin"] = big
	dc.files["/abs/remote.txt"] = []byte("remote")
	ctx := context.Background()

	r, err := o.Download(ctx, "a", root, DownloadParams{Workspace: wsID, Remote: "out/b.bin", Local: "got/b.bin"})
	if err != nil || r.Bytes != int64(len(big)) || r.Local != "got/b.bin" {
		t.Fatalf("%+v %v", r, err)
	}
	got, _ := os.ReadFile(filepath.Join(root, "got", "b.bin"))
	if !bytes.Equal(got, big) {
		t.Fatal("content differs")
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(filepath.Join(root, "got", "b.bin")); st.Mode().Perm() != 0o644 {
			t.Fatalf("mode %v", st.Mode())
		}
	}
	// No temp file is left behind.
	ents, _ := os.ReadDir(filepath.Join(root, "got"))
	if len(ents) != 1 {
		t.Fatalf("leftovers: %v", ents)
	}
	// Local defaults to the remote base name.
	if r, err = o.Download(ctx, "a", root, DownloadParams{Workspace: wsID, Remote: "/abs/remote.txt"}); err != nil || r.Local != "remote.txt" {
		t.Fatalf("%+v %v", r, err)
	}
	// No overwrite unless asked.
	writeLocal(t, root, "keep.txt", []byte("mine"))
	dc.files["k"] = []byte("theirs")
	if _, err = o.Download(ctx, "a", root, DownloadParams{Workspace: wsID, Remote: "k", Local: "keep.txt"}); err == nil {
		t.Fatal("overwrote without being asked")
	}
	if b, _ := os.ReadFile(filepath.Join(root, "keep.txt")); string(b) != "mine" {
		t.Fatal("file changed")
	}
	if _, err = o.Download(ctx, "a", root, DownloadParams{Workspace: wsID, Remote: "k", Local: "keep.txt", Overwrite: true}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "keep.txt")); string(b) != "theirs" {
		t.Fatal("overwrite failed")
	}
	// A missing remote file leaves no local file or temp file.
	if _, err = o.Download(ctx, "a", root, DownloadParams{Workspace: wsID, Remote: "missing", Local: "m.txt"}); err == nil {
		t.Fatal("missing remote accepted")
	}
	if _, err := os.Stat(filepath.Join(root, "m.txt")); err == nil {
		t.Fatal("partial file left")
	}
	if ents, _ := os.ReadDir(root); len(ents) != 3 { // got, remote.txt, keep.txt
		for _, e := range ents {
			t.Log(e.Name())
		}
		t.Fatal("unexpected files in the project directory")
	}
}

func TestDownloadRefusals(t *testing.T) {
	o, dc := devOrch(t)
	root := t.TempDir()
	outside := t.TempDir()
	dc.files["r"] = []byte("x")
	if runtime.GOOS != "windows" {
		if err := os.Symlink(outside, filepath.Join(root, "linkdir")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(outside, "t.txt"), filepath.Join(root, "link.txt")); err != nil {
			t.Fatal(err)
		}
	}
	names := []string{".env", ".git/hooks/pre-commit", ".ssh/authorized_keys", "../escape.txt", `..\escape.txt`,
		filepath.Join(outside, "abs.txt"), "NUL", ".mirafleet.yaml", "id_rsa", "a.pem"}
	if runtime.GOOS != "windows" {
		names = append(names, "linkdir/f.txt", "link.txt")
	}
	for _, n := range names {
		if _, err := o.Download(context.Background(), "a", root, DownloadParams{Workspace: wsID, Remote: "r", Local: n, Overwrite: true}); err == nil {
			t.Errorf("download to %q accepted", n)
		}
	}
	if len(dc.calls) != 0 {
		t.Fatalf("refused downloads reached the backend: %v", dc.calls)
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Fatal("something was written outside the project")
	}
	if _, err := o.Download(context.Background(), "a", root, DownloadParams{Workspace: wsID}); err == nil {
		t.Error("empty remote accepted")
	}
}

func TestUploadAliasNamesRefused(t *testing.T) {
	o, dc := devOrch(t)
	root := t.TempDir()
	for _, n := range []string{"GIT~1/hooks/x", ".g" + string(rune(0x200d)) + "it/config"} {
		writeLocal(t, root, n, []byte("x"))
		if _, err := o.Upload(context.Background(), "a", root, UploadParams{Workspace: wsID, Local: n, Remote: "r"}); err == nil {
			t.Errorf("upload of %q accepted", n)
		}
	}
	if len(dc.calls) != 0 {
		t.Fatalf("refused uploads reached the backend: %v", dc.calls)
	}
}

// shortReader says EOF before the whole file was sent.
type shortReader struct{ *devConn }

func (s *shortReader) CallReadBody(ctx context.Context, m string, p, res any) ([]byte, error) {
	b, err := s.devConn.CallReadBody(ctx, m, p, res)
	r := res.(*api.WorkspaceReadFileResult)
	r.Size, r.EOF = int64(len(b))+5, true
	return b, err
}

func TestDownloadShortFileRefused(t *testing.T) {
	_, dc := devOrch(t)
	root := t.TempDir()
	dc.files["r"] = []byte("hello")
	o := newOrch(t, setupOne(t), &dialer{mk: func(int) (Conn, error) { return &shortReader{dc}, nil }})
	_, err := o.Download(context.Background(), "a", root, DownloadParams{Workspace: wsID, Remote: "r", Local: "x.bin"})
	if err == nil || !strings.Contains(err.Error(), api.CodeUnavailable) {
		t.Fatalf("short download accepted or wrong code: %v", err)
	}
	if ents, _ := os.ReadDir(root); len(ents) != 0 {
		t.Fatalf("files left: %v", ents)
	}
}

func TestDownloadTooLarge(t *testing.T) {
	o, dc := devOrch(t)
	root := t.TempDir()
	// The fake reports Size from the stored bytes; fake a big file cheaply by overriding the size
	// via a sparse allocation is wasteful, so check the guard through a size-lying conn instead.
	dc.files["r"] = make([]byte, 10)
	lying := &sizeLiar{devConn: dc}
	o2 := newOrch(t, setupOne(t), &dialer{mk: func(int) (Conn, error) { return lying, nil }})
	if _, err := o2.Download(context.Background(), "a", root, DownloadParams{Workspace: wsID, Remote: "r", Local: "x.bin"}); err == nil {
		t.Fatal("oversize remote file accepted")
	}
	if _, err := os.Stat(filepath.Join(root, "x.bin")); err == nil {
		t.Fatal("partial file left")
	}
	_ = o
}
