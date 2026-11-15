package fsops

import (
	"io/fs"
	"os"
	"path/filepath"
)

// osRoot is the file access of an FS: plain OS calls where a relative name is taken from cwd and
// an absolute name is used as is. There is no confinement: like local Claude Code, the tools reach
// whatever the OS user can (docs/M7.md §3.7), and the OS permissions are the boundary.
type osRoot struct{ cwd string }

func (r osRoot) p(name string) string {
	if filepath.IsAbs(name) {
		return name
	}
	return filepath.Join(r.cwd, name)
}

func (r osRoot) Close() error                          { return nil }
func (r osRoot) Stat(name string) (fs.FileInfo, error) { return os.Stat(r.p(name)) }
func (r osRoot) Lstat(name string) (fs.FileInfo, error) {
	return os.Lstat(r.p(name))
}
func (r osRoot) Readlink(name string) (string, error) { return os.Readlink(r.p(name)) }
func (r osRoot) Open(name string) (*os.File, error)   { return os.Open(r.p(name)) }
func (r osRoot) OpenFile(name string, flag int, perm fs.FileMode) (*os.File, error) {
	return os.OpenFile(r.p(name), flag, perm)
}
func (r osRoot) MkdirAll(name string, perm fs.FileMode) error { return os.MkdirAll(r.p(name), perm) }
func (r osRoot) Remove(name string) error                     { return os.Remove(r.p(name)) }
func (r osRoot) Rename(oldname, newname string) error {
	return os.Rename(r.p(oldname), r.p(newname))
}
