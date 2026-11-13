//go:build linux

package ctrd

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
)

func newWS(t *testing.T) (*wsDir, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "ws")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	w, err := openWorkspaceDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w, dir
}

func readAll(t *testing.T, w *wsDir, rel string, off, n int64) ([]byte, error) {
	t.Helper()
	rc, _, err := w.read(rel, off, n)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func TestRelPath(t *testing.T) {
	for in, want := range map[string]string{
		"a.txt": "a.txt", "/workspace/a/b": "a/b", "a/../b": "b", "./x": "x",
	} {
		got, err := relPath(in)
		if err != nil || got != want {
			t.Errorf("relPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"/workspace", ".", "..", "../x", "a/../../x", "/etc/passwd", "/proc/self/root/etc",
		"/workspace/../etc", "a\x00b", ""} {
		if got, err := relPath(in); !errors.Is(err, workspace.ErrRefused) {
			t.Errorf("relPath(%q) = %q, %v; want ErrRefused", in, got, err)
		}
	}
}

func TestReadOffsetLimitAndInfo(t *testing.T) {
	w, dir := newWS(t)
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("0123456789"), 0o640); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		off, n int64
		want   string
	}{{0, 0, "0123456789"}, {3, 0, "3456789"}, {3, 4, "3456"}, {8, 100, "89"}, {10, 5, ""}, {50, 5, ""}} {
		got, err := readAll(t, w, "f", c.off, c.n)
		if err != nil || string(got) != c.want {
			t.Errorf("read(%d,%d) = %q, %v; want %q", c.off, c.n, got, err, c.want)
		}
	}
	_, fi, err := w.read("f", 0, 1)
	if err != nil || fi.Size != 10 || fi.Mode.Perm() != 0o640 {
		t.Errorf("info = %+v, %v", fi, err)
	}
	if _, err := readAll(t, w, "f", -1, 0); !errors.Is(err, workspace.ErrRefused) {
		t.Errorf("negative offset: %v", err)
	}
	if _, err := readAll(t, w, "missing", 0, 0); !errors.Is(err, workspace.ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
}

// Symlinks planted in the workspace (by the container, here by the test) never reach host files.
func TestReadNeverEscapesViaSymlinks(t *testing.T) {
	w, dir := newWS(t)
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("TOP SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "ok"), []byte("fine"), 0o600); err != nil {
		t.Fatal(err)
	}
	rel, _ := filepath.Rel(dir, secret)
	links := map[string]string{
		"abs":       secret, // absolute host path
		"rel":       rel,    // relative, climbing out
		"dotdot":    "sub/../../../../../../" + secret,
		"procroot":  "/proc/self/root" + secret,
		"proc1root": "/proc/1/root" + secret,
		"dirlink":   outside,  // directory symlink: dirlink/secret
		"inside":    "sub/ok", // stays inside: os.Root resolves it, so it is readable (see below)
		"self":      "self",
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"abs", "rel", "dotdot", "procroot", "proc1root", "dirlink/secret", "dirlink", "self", "sub/../abs"} {
		got, err := readAll(t, w, name, 0, 0)
		if err == nil || strings.Contains(string(got), "SECRET") {
			t.Errorf("read(%s) = %q, %v: must fail", name, got, err)
		}
	}
	// Writes through the same links fail and leave the host file alone.
	for _, name := range []string{"abs", "rel", "procroot", "dirlink/secret"} {
		for _, trunc := range []bool{false, true} {
			_, _, err := w.write(name, strings.NewReader("pwned"), 0, trunc, 0o644, 1<<20)
			if name != "dirlink/secret" && trunc {
				// truncating replaces the link itself by rename, never the target
				if err != nil {
					t.Errorf("truncate-write over link %s: %v", name, err)
				}
				continue
			}
			if err == nil && !trunc {
				t.Errorf("write through %s succeeded", name)
			}
		}
	}
	if b, _ := os.ReadFile(secret); string(b) != "TOP SECRET" {
		t.Errorf("host file modified: %q", b)
	}
	if fi, err := os.Lstat(secret); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("host file mode changed: %v %v", fi, err)
	}
}

func TestFIFOAndSpecialFilesDoNotHang(t *testing.T) {
	w, dir := newWS(t)
	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 2)
	go func() { _, err := readAll(t, w, "pipe", 0, 0); done <- err }()
	go func() { _, _, err := w.write("pipe", strings.NewReader("x"), 0, false, 0o644, 1<<20); done <- err }()
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err == nil {
				t.Error("FIFO access succeeded")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("FIFO access hung")
		}
	}
	// A FIFO with a live reader on the other end: the open succeeds, the fstat says no.
	rd, err := os.OpenFile(fifo, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	if _, _, err := w.write("pipe", strings.NewReader("x"), 0, false, 0o644, 1<<20); !errors.Is(err, workspace.ErrRefused) {
		t.Errorf("write to FIFO with reader: %v", err)
	}
	// Truncating replaces the FIFO by a regular file (rename), which is harmless.
	if _, _, err := w.write("pipe", strings.NewReader("data"), 0, true, 0o644, 1<<20); err != nil {
		t.Errorf("truncate-write over FIFO: %v", err)
	}
	// A directory is not a file.
	if err := os.Mkdir(filepath.Join(dir, "d"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := readAll(t, w, "d", 0, 0); !errors.Is(err, workspace.ErrRefused) {
		t.Errorf("read dir: %v", err)
	}
	if _, _, err := w.write("d", strings.NewReader("x"), 0, true, 0o644, 1<<20); !errors.Is(err, workspace.ErrRefused) {
		t.Errorf("truncate-write over dir: %v", err)
	}
}

func TestWriteModesOwnershipAndAtomicity(t *testing.T) {
	w, dir := newWS(t)
	fi, n, err := w.write("a/b/c.txt", strings.NewReader("hello"), 0, false, 0o640, 1<<20)
	if err != nil || n != 5 || fi.Size != 5 || fi.Mode.Perm() != 0o640 {
		t.Fatalf("write = %+v, %d, %v", fi, n, err)
	}
	st, _ := os.Stat(filepath.Join(dir, "a", "b"))
	if st.Mode().Perm() != 0o755 {
		t.Errorf("parent mode %v", st.Mode().Perm())
	}
	for _, p := range []string{"a", "a/b", "a/b/c.txt"} {
		fi, _ := os.Stat(filepath.Join(dir, p))
		s := fi.Sys().(*syscall.Stat_t)
		if int(s.Uid) != w.uid || int(s.Gid) != w.gid {
			t.Errorf("%s owner %d:%d, want %d:%d", p, s.Uid, s.Gid, w.uid, w.gid)
		}
	}
	// Offset write, no truncate: in place.
	if _, _, err := w.write("a/b/c.txt", strings.NewReader("J"), 1, false, 0o600, 1<<20); err != nil {
		t.Fatal(err)
	}
	if got, _ := readAll(t, w, "a/b/c.txt", 0, 0); string(got) != "hJllo" {
		t.Errorf("after offset write: %q", got)
	}
	// Existing file keeps its mode on an in-place write.
	if fi, _ := os.Stat(filepath.Join(dir, "a/b/c.txt")); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode changed to %v", fi.Mode().Perm())
	}
	// Truncating replacement: new content only, no temp files left.
	if _, _, err := w.write("a/b/c.txt", strings.NewReader("new"), 0, true, 0o600, 1<<20); err != nil {
		t.Fatal(err)
	}
	if got, _ := readAll(t, w, "a/b/c.txt", 0, 0); string(got) != "new" {
		t.Errorf("after truncate: %q", got)
	}
	ents, _ := os.ReadDir(filepath.Join(dir, "a/b"))
	if len(ents) != 1 {
		t.Errorf("temp files left: %v", ents)
	}
	// Sparse offset write.
	if _, _, err := w.write("sp", strings.NewReader("x"), 4, false, 0o644, 1<<20); err != nil {
		t.Fatal(err)
	}
	if got, _ := readAll(t, w, "sp", 0, 0); !bytes.Equal(got, []byte{0, 0, 0, 0, 'x'}) {
		t.Errorf("sparse: %q", got)
	}
	// A parent that is a file is refused.
	if _, _, err := w.write("sp/x", strings.NewReader("x"), 0, false, 0o644, 1<<20); !errors.Is(err, workspace.ErrRefused) {
		t.Errorf("file as parent: %v", err)
	}
}

func TestWriteMaxFile(t *testing.T) {
	w, dir := newWS(t)
	_, _, err := w.write("big", strings.NewReader(strings.Repeat("x", 11)), 0, true, 0o644, 10)
	if !errors.Is(err, workspace.ErrTooLarge) {
		t.Errorf("11 bytes, max 10: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "big")); err == nil {
		t.Error("oversize truncate-write left a file")
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 0 {
		t.Errorf("leftovers: %v", ents)
	}
	if _, _, err := w.write("ok", strings.NewReader(strings.Repeat("x", 10)), 0, false, 0o644, 10); err != nil {
		t.Errorf("exactly max: %v", err)
	}
	if _, _, err := w.write("ok", strings.NewReader("y"), 10, false, 0o644, 10); !errors.Is(err, workspace.ErrTooLarge) {
		t.Errorf("append past max: %v", err)
	}
}

func TestChunkedRoundTrip(t *testing.T) {
	w, _ := newWS(t)
	data := make([]byte, 1536<<10)
	for i := range data {
		data[i] = byte(i*7 + i>>8)
	}
	const chunk = 1 << 20
	for off := 0; off < len(data); off += chunk {
		end := min(off+chunk, len(data))
		if _, _, err := w.write("rt", bytes.NewReader(data[off:end]), int64(off), off == 0, 0o644, 4<<20); err != nil {
			t.Fatal(err)
		}
	}
	var got []byte
	for off := int64(0); ; {
		b, err := readAll(t, w, "rt", off, chunk)
		if err != nil {
			t.Fatal(err)
		}
		if len(b) == 0 {
			break
		}
		got = append(got, b...)
		off += int64(len(b))
	}
	if sha256.Sum256(got) != sha256.Sum256(data) {
		t.Errorf("round trip mismatch: %d bytes vs %d", len(got), len(data))
	}
}

func TestOpenWorkspaceDirRefusesSymlinkAndFile(t *testing.T) {
	base := t.TempDir()
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := openWorkspaceDir(filepath.Join(base, "link")); err == nil {
		t.Error("symlinked workspace dir accepted")
	}
	if err := os.WriteFile(filepath.Join(base, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openWorkspaceDir(filepath.Join(base, "file")); err == nil {
		t.Error("file accepted as workspace dir")
	}
	if _, err := openWorkspaceDir(filepath.Join(base, "none")); !errors.Is(err, workspace.ErrNotFound) {
		t.Errorf("missing dir: %v", err)
	}
}

// A symlink that resolves inside the workspace is followed by os.Root itself (safely).
func TestInWorkspaceSymlinkStaysInside(t *testing.T) {
	w, dir := newWS(t)
	if err := os.WriteFile(filepath.Join(dir, "real"), []byte("fine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	if got, err := readAll(t, w, "alias", 0, 0); err != nil || string(got) != "fine" {
		t.Errorf("read via in-workspace link: %q %v", got, err)
	}
}
