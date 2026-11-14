//go:build linux

package ctrd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"

	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
)

const defaultWriteMode fs.FileMode = 0o644

// Workspace files are accessed on the host through os.Root opened on the workspace directory:
// the kernel-checked root cannot be escaped by "..", absolute symlinks, symlink chains or
// /proc/self/root tricks planted by the (untrusted) container: os.Root resolves symlinks itself
// and refuses any that leave the directory (it also resolves in-workspace ones). Every open
// additionally passes O_NOFOLLOW, uses O_NONBLOCK (a FIFO planted by the container can never
// hang the runner) and is followed by an fstat that must say "regular file".
// New files and directories are chowned to the workspace's mapped uid/gid.

// wsDir is an opened workspace directory.
type wsDir struct {
	root     *os.Root
	uid, gid int
}

func (w *wsDir) Close() error { return w.root.Close() }

// openWorkspace opens the workspace directory of a live workspace. The directory itself must be a
// real directory directly under the executor's root (never a symlink).
func (e *Executor) openWorkspace(ctx context.Context, id workspace.ID) (*wsDir, error) {
	if !workspace.ValidID(id) {
		return nil, refuse("invalid workspace id")
	}
	if _, err := e.client.LoadContainer(namespaces.WithNamespace(ctx, e.ns), ctrName(id)); err != nil {
		if errdefs.IsNotFound(err) {
			return nil, fmt.Errorf("%w: workspace %s", workspace.ErrNotFound, id)
		}
		return nil, unavailable("look up container", err)
	}
	return openWorkspaceDir(filepath.Join(e.root, string(id)))
}

func openWorkspaceDir(dir string) (*wsDir, error) {
	fi, err := os.Lstat(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: workspace directory is missing", workspace.ErrNotFound)
		}
		return nil, unavailable("stat workspace directory", err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || !fi.IsDir() {
		return nil, unavailable("workspace directory", errors.New("is not a plain directory"))
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, unavailable("open workspace directory", err)
	}
	return &wsDir{root: root, uid: int(st.Uid), gid: int(st.Gid)}, nil
}

// relPath re-validates a workspace path (the runner already cleaned it) and returns it relative
// to the workspace directory. The workspace root itself is not a file.
func relPath(p string) (string, error) {
	abs, err := workspace.CleanPath(p)
	if err != nil {
		return "", refuse("path: %v", err)
	}
	if abs == workspace.Root {
		return "", refuse("path is the workspace root, not a file")
	}
	rel := strings.TrimPrefix(abs, workspace.Root+"/")
	for _, part := range strings.Split(rel, "/") {
		if part == "" || part == "." || part == ".." {
			return "", refuse("path: invalid element")
		}
	}
	return rel, nil
}

// fileErr maps an OS error from an open to an executor error.
func fileErr(op, rel string, err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%w: %s: file not found", workspace.ErrNotFound, rel)
	case errors.Is(err, syscall.ELOOP), errors.Is(err, syscall.ENXIO), errors.Is(err, syscall.EISDIR),
		errors.Is(err, syscall.ENOTDIR), errors.Is(err, fs.ErrPermission):
		return refuse("%s %s: %v", op, rel, errors.Unwrap(err))
	case err != nil && strings.Contains(err.Error(), "path escapes from parent"):
		return refuse("%s %s: path escapes the workspace", op, rel)
	}
	return fmt.Errorf("%s %s: %w", op, rel, err)
}

// ReadFile implements workspace.Executor: up to n bytes from off (n <= 0: to the end).
func (e *Executor) ReadFile(ctx context.Context, id workspace.ID, p string, off, n int64) (io.ReadCloser, workspace.FileInfo, error) {
	rel, err := relPath(p)
	if err != nil {
		return nil, workspace.FileInfo{}, err
	}
	ws, err := e.openWorkspace(ctx, id)
	if err != nil {
		return nil, workspace.FileInfo{}, err
	}
	defer ws.Close()
	return ws.read(rel, off, n)
}

func (w *wsDir) read(rel string, off, n int64) (io.ReadCloser, workspace.FileInfo, error) {
	if off < 0 {
		return nil, workspace.FileInfo{}, refuse("negative offset")
	}
	f, err := w.root.OpenFile(rel, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, workspace.FileInfo{}, fileErr("open", rel, err)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, workspace.FileInfo{}, fmt.Errorf("stat %s: %w", rel, err)
	}
	if !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, workspace.FileInfo{}, refuse("%s is not a regular file", rel)
	}
	size := int64(1<<63 - 1)
	if n > 0 && n < size-off {
		size = n
	} else {
		size -= off
	}
	return &sectionCloser{SectionReader: io.NewSectionReader(f, off, size), c: f},
		workspace.FileInfo{Size: fi.Size(), Mode: fi.Mode(), ModTime: fi.ModTime()}, nil
}

type sectionCloser struct {
	*io.SectionReader
	c io.Closer
}

func (s *sectionCloser) Close() error { return s.c.Close() }

// WriteFile implements workspace.Executor. Without truncate it writes r at off into the file
// (created if missing). With truncate the content is replaced atomically: r is written at off
// into a fresh temporary file in the same directory which then replaces the target by rename.
// Writes are refused over the disk quota (ErrQuota) and beyond the runner's max_file
// (ErrTooLarge).
func (e *Executor) WriteFile(ctx context.Context, id workspace.ID, p string, r io.Reader, off int64, truncate bool, mode fs.FileMode) (workspace.FileInfo, error) {
	rel, err := relPath(p)
	if err != nil {
		return workspace.FileInfo{}, err
	}
	if off < 0 {
		return workspace.FileInfo{}, refuse("negative offset")
	}
	if mode&^0o777 != 0 {
		return workspace.FileInfo{}, refuse("file mode must be within 0o777")
	}
	if mode == 0 {
		mode = defaultWriteMode
	}
	maxFile := int64(e.cfg.Limits.MaxFile)
	if off > maxFile {
		return workspace.FileInfo{}, fmt.Errorf("%w: offset beyond max_file", workspace.ErrTooLarge)
	}
	ws, err := e.openWorkspace(ctx, id)
	if err != nil {
		return workspace.FileInfo{}, err
	}
	defer ws.Close()
	if err := e.checkQuota(ctx, id, ws); err != nil {
		return workspace.FileInfo{}, err
	}
	fi, written, err := ws.write(rel, r, off, truncate, mode, maxFile)
	if written > 0 {
		e.disks().add(id, written)
	}
	return fi, err
}

func (w *wsDir) write(rel string, r io.Reader, off int64, truncate bool, mode fs.FileMode, maxFile int64) (workspace.FileInfo, int64, error) {
	if err := w.mkParents(rel); err != nil {
		return workspace.FileInfo{}, 0, err
	}
	var (
		f       *os.File
		tmp     string
		created bool
		err     error
	)
	if truncate {
		if st, lerr := w.root.Lstat(rel); lerr == nil && st.IsDir() {
			return workspace.FileInfo{}, 0, refuse("%s is a directory", rel)
		}
		var rb [8]byte
		_, _ = rand.Read(rb[:])
		tmp = path.Join(path.Dir(rel), ".mf-write-"+hex.EncodeToString(rb[:]))
		f, err = w.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
		created = true
	} else {
		f, err = w.root.OpenFile(rel, os.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if errors.Is(err, fs.ErrNotExist) {
			f, err = w.root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
			created = true
		}
	}
	if err != nil {
		return workspace.FileInfo{}, 0, fileErr("open", rel, err)
	}
	cleanup := func() {
		_ = f.Close()
		if tmp != "" {
			_ = w.root.Remove(tmp)
		} else if created {
			_ = w.root.Remove(rel)
		}
	}
	st, err := f.Stat()
	if err != nil {
		cleanup()
		return workspace.FileInfo{}, 0, fmt.Errorf("stat %s: %w", rel, err)
	}
	if !st.Mode().IsRegular() {
		cleanup()
		return workspace.FileInfo{}, 0, refuse("%s is not a regular file", rel)
	}
	if created {
		if err := f.Chown(w.uid, w.gid); err != nil {
			cleanup()
			return workspace.FileInfo{}, 0, fmt.Errorf("chown %s: %w", rel, err)
		}
		if err := f.Chmod(mode); err != nil {
			cleanup()
			return workspace.FileInfo{}, 0, fmt.Errorf("chmod %s: %w", rel, err)
		}
	}

	// At most maxFile-off bytes may arrive; one more byte proves the limit was exceeded.
	allow := maxFile - off
	n, cerr := io.Copy(io.NewOffsetWriter(f, off), io.LimitReader(r, allow+1))
	// Bytes that stay on disk after a failure (an existing file written in place) still count
	// against the quota; a removed temp or new file frees them.
	kept := n
	if tmp != "" || created {
		kept = 0
	}
	if cerr != nil {
		cleanup()
		return workspace.FileInfo{}, kept, fmt.Errorf("write %s: %w", rel, cerr)
	}
	if n > allow {
		cleanup()
		return workspace.FileInfo{}, kept, fmt.Errorf("%w: file would exceed max_file", workspace.ErrTooLarge)
	}
	st, err = f.Stat()
	if err != nil {
		cleanup()
		return workspace.FileInfo{}, 0, fmt.Errorf("stat %s: %w", rel, err)
	}
	if err := f.Close(); err != nil {
		if tmp != "" {
			_ = w.root.Remove(tmp)
		}
		return workspace.FileInfo{}, 0, fmt.Errorf("close %s: %w", rel, err)
	}
	if tmp != "" {
		if err := w.root.Rename(tmp, rel); err != nil {
			_ = w.root.Remove(tmp)
			return workspace.FileInfo{}, 0, fileErr("replace", rel, err)
		}
	}
	return workspace.FileInfo{Size: st.Size(), Mode: st.Mode(), ModTime: st.ModTime()}, n, nil
}

// mkParents creates the missing parent directories of rel (mode 0755, owned by the workspace
// uid). A parent that exists as anything but a directory is an error.
func (w *wsDir) mkParents(rel string) error {
	dir := path.Dir(rel)
	if dir == "." {
		return nil
	}
	cur := ""
	for _, part := range strings.Split(dir, "/") {
		cur = path.Join(cur, part)
		st, err := w.root.Lstat(cur)
		switch {
		case err == nil:
			if !st.IsDir() {
				return refuse("%s exists and is not a directory", cur)
			}
		case errors.Is(err, fs.ErrNotExist):
			if err := w.root.Mkdir(cur, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
				return fileErr("mkdir", cur, err)
			}
			if err := w.root.Lchown(cur, w.uid, w.gid); err != nil {
				return fmt.Errorf("chown %s: %w", cur, err)
			}
		default:
			return fileErr("stat", cur, err)
		}
	}
	return nil
}
