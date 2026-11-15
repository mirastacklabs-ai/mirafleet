// Package fsops implements the remote file tools (Read, Write, Edit, MultiEdit, NotebookEdit, LS,
// Stat) as a pure Go library rooted at a directory. It follows Claude Code's local tool
// semantics and error messages closely.
//
// The tools reach everything the OS user can see (docs/M7.md §3.7, parity with local Claude
// Code): an absolute path works anywhere, a relative one is taken from the directory the FS was
// opened on; the OS permissions are the boundary. Anything that is not a regular file (FIFO,
// device, socket) is refused before and after open, and opens are non-blocking where the OS has
// that flag, so a planted FIFO cannot hang the caller. The package has no OS specific behaviour
// other than that one open flag.
package fsops

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Error kinds, matched with errors.Is.
var (
	ErrNotFound = errors.New("not found")
	ErrRefused  = errors.New("refused")
	ErrInvalid  = errors.New("invalid request")
	ErrTooLarge = errors.New("too large")
)

// Error carries a Claude Code style message and a kind for errors.Is.
type Error struct {
	Kind error
	Msg  string
}

func (e *Error) Error() string { return e.Msg }
func (e *Error) Unwrap() error { return e.Kind }

func fail(kind error, format string, a ...any) error {
	return &Error{Kind: kind, Msg: fmt.Sprintf(format, a...)}
}

// Options are the size limits; zero values select the defaults.
type Options struct {
	MaxFile  int64 // largest file Edit, Write and the notebook tools handle (default 64 MiB)
	MaxImage int64 // largest image Read returns (default 5 MiB)
}

const (
	defaultMaxFile  = 64 << 20
	defaultMaxImage = 5 << 20
	defaultDirMode  = 0o755
	defaultFileMode = 0o644
)

// FS is a set of file tools whose relative paths start at one directory.
type FS struct {
	root osRoot
	dirs []string // the directory as given and with symlinks resolved; absolute paths under either are shown relative
	opt  Options
}

// Open starts the tools at dir, which must exist.
func Open(dir string, opt Options) (*FS, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	if fi, err := os.Stat(real); err != nil {
		return nil, err
	} else if !fi.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	if opt.MaxFile <= 0 {
		opt.MaxFile = defaultMaxFile
	}
	if opt.MaxImage <= 0 {
		opt.MaxImage = defaultMaxImage
	}
	dirs := []string{real}
	if abs != real {
		dirs = append(dirs, abs)
	}
	return &FS{root: osRoot{cwd: real}, dirs: dirs, opt: opt}, nil
}

// Close releases the FS.
func (f *FS) Close() error { return f.root.Close() }

// rel cleans a caller path. A path inside the start directory comes back relative to it (".." is
// resolved lexically); any other comes back absolute. Relative names start at the directory.
func (f *FS) rel(p string) (string, error) {
	if p == "" {
		return "", fail(ErrInvalid, "file_path is required")
	}
	if strings.ContainsRune(p, 0) {
		return "", fail(ErrInvalid, "path contains a NUL byte")
	}
	c := filepath.Clean(p)
	if !filepath.IsAbs(c) {
		c = filepath.Join(f.dirs[0], c)
	}
	for _, d := range f.dirs {
		if r, err := filepath.Rel(d, c); err == nil && filepath.IsLocal(r) {
			return r, nil
		}
	}
	return c, nil
}

// mapErr turns an OS error into a tool error.
func mapErr(rel string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrNotExist):
		return fail(ErrNotFound, "File does not exist: %s", rel)
	case strings.Contains(err.Error(), "path escapes from parent"):
		return fail(ErrRefused, "%s: path escapes the root directory", rel)
	case errors.Is(err, fs.ErrPermission):
		return fail(ErrRefused, "%s: permission denied", rel)
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return fmt.Errorf("%s: %s: %w", pe.Op, rel, pe.Err)
	}
	return fmt.Errorf("%s: %w", rel, err)
}

// target follows symlinks of the final element (the OS resolves them for reads, but a rename
// would replace the link itself). A dangling link yields its target.
func (f *FS) target(rel string) (string, error) {
	for range 16 {
		fi, err := f.root.Lstat(rel)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return rel, nil
			}
			return "", mapErr(rel, err)
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			return rel, nil
		}
		t, err := f.root.Readlink(rel)
		if err != nil {
			return "", mapErr(rel, err)
		}
		if !filepath.IsAbs(t) {
			t = filepath.Join(filepath.Dir(f.root.p(rel)), t)
		}
		if rel, err = f.rel(t); err != nil {
			return "", err
		}
	}
	return "", fail(ErrRefused, "%s: too many levels of symbolic links", rel)
}

// openRegular opens rel for reading. Directories and anything that is not a regular file are
// refused before the open (so a device is never opened) and checked again on the open handle.
func (f *FS) openRegular(rel string) (*os.File, fs.FileInfo, error) {
	fi, err := f.root.Stat(rel)
	if err != nil {
		return nil, nil, mapErr(rel, err)
	}
	if err := checkRegular(rel, fi); err != nil {
		return nil, nil, err
	}
	file, err := f.root.OpenFile(rel, os.O_RDONLY|openFlags, 0)
	if err != nil {
		return nil, nil, mapErr(rel, err)
	}
	fi, err = file.Stat()
	if err == nil {
		err = checkRegular(rel, fi)
	}
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	return file, fi, nil
}

func checkRegular(rel string, fi fs.FileInfo) error {
	if fi.IsDir() {
		return fail(ErrInvalid, "EISDIR: illegal operation on a directory, read '%s'", rel)
	}
	if !fi.Mode().IsRegular() {
		return fail(ErrRefused, "%s is not a regular file (FIFOs, devices and sockets are refused)", rel)
	}
	return nil
}

// readAll reads a whole regular file, refusing one over MaxFile.
func (f *FS) readAll(rel string) ([]byte, fs.FileMode, error) {
	file, fi, err := f.openRegular(rel)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()
	if fi.Size() > f.opt.MaxFile {
		return nil, 0, fail(ErrTooLarge, "File is too large (%d bytes, max %d bytes)", fi.Size(), f.opt.MaxFile)
	}
	data, err := io.ReadAll(io.LimitReader(file, f.opt.MaxFile+1))
	if err != nil {
		return nil, 0, mapErr(rel, err)
	}
	if int64(len(data)) > f.opt.MaxFile {
		return nil, 0, fail(ErrTooLarge, "File is too large (more than %d bytes)", f.opt.MaxFile)
	}
	return data, fi.Mode().Perm(), nil
}
