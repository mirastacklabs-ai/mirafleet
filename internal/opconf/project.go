package opconf

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ProjectName is the per-project default file. It holds only a fleet and a context name: a cloned
// repository must never be able to add a central, a key or a cap.
const ProjectName = ".mirafleet.yaml"

// maxProjectBytes bounds the project file.
const maxProjectBytes = 4 << 10

// Project is the contents of .mirafleet.yaml. Every field may be empty. Remote marks a project
// whose tools all run on the remote workspace; Workdir names a shared
// directory on the runner instead of a per-session one (Q4) and needs Remote. Still no addresses,
// keys or caps.
type Project struct {
	Fleet   string `yaml:"fleet,omitempty"`
	Context string `yaml:"context,omitempty"`
	Remote  bool   `yaml:"remote,omitempty"`
	Workdir string `yaml:"workdir,omitempty"`
}

// LoadProject looks for .mirafleet.yaml in start and its parents. It checks a directory first and
// then stops if that directory contains .git (file or directory) or is home; it never goes above
// home. If start is not under home, only start itself is checked. Not found returns the zero
// Project, an empty path and no error. Symlinked, non-regular or oversized files, unknown keys and
// invalid names are errors.
func LoadProject(start, home string) (Project, string, error) {
	dir := filepath.Clean(start)
	if !filepath.IsAbs(dir) {
		return Project{}, "", fmt.Errorf("project dir %q must be absolute", start)
	}
	home = strings.TrimSpace(home)
	if home != "" {
		home = filepath.Clean(home)
		// F-08: HOME=/ (or C:\) would make the walk-up reach the filesystem root, where any
		// user's file could be planted. A root home counts as no home: only start is checked.
		if filepath.Dir(home) == home {
			home = ""
		}
	}
	underHome := home != "" && within(home, dir)
	for {
		p, path, found, err := loadProjectAt(dir)
		if err != nil || found {
			return p, path, err
		}
		if !underHome || dir == home {
			return Project{}, "", nil
		}
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return Project{}, "", nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return Project{}, "", nil
		}
		dir = parent
	}
}

// within reports whether p is root or below it.
func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func loadProjectAt(dir string) (Project, string, bool, error) {
	path := filepath.Join(dir, ProjectName)
	st, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return Project{}, "", false, nil
	}
	if err != nil {
		return Project{}, "", false, err
	}
	if !st.Mode().IsRegular() {
		return Project{}, "", false, fmt.Errorf("%s: must be a regular file, not a symlink or special file", path)
	}
	if st.Size() > maxProjectBytes {
		return Project{}, "", false, fmt.Errorf("%s: larger than %d bytes", path, maxProjectBytes)
	}
	// F-06: open without following a symlink or blocking on a FIFO swapped in after the Lstat,
	// then check the opened file is the regular file that was Lstat-ed.
	f, err := openNoFollow(path)
	if err != nil {
		return Project{}, "", false, fmt.Errorf("%s: %w (must be a regular file, not a symlink or special file)", path, err)
	}
	defer f.Close()
	fst, err := f.Stat()
	if err != nil {
		return Project{}, "", false, err
	}
	if !fst.Mode().IsRegular() || !os.SameFile(st, fst) {
		return Project{}, "", false, fmt.Errorf("%s: changed while opening; must be a regular file, not a symlink or special file", path)
	}
	if fst.Size() > maxProjectBytes {
		return Project{}, "", false, fmt.Errorf("%s: larger than %d bytes", path, maxProjectBytes)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxProjectBytes+1))
	if err != nil {
		return Project{}, "", false, err
	}
	if len(b) > maxProjectBytes {
		return Project{}, "", false, fmt.Errorf("%s: larger than %d bytes", path, maxProjectBytes)
	}
	var p Project
	if err := decodeSingle(b, &p); err != nil {
		return Project{}, "", false, fmt.Errorf("parse %s (only fleet, context, remote and workdir are allowed): %w", path, err)
	}
	if p.Fleet != "" && !nameRE.MatchString(p.Fleet) {
		return Project{}, "", false, fmt.Errorf("%s: invalid fleet name %q", path, p.Fleet)
	}
	if p.Context != "" && !nameRE.MatchString(p.Context) {
		return Project{}, "", false, fmt.Errorf("%s: invalid context name %q", path, p.Context)
	}
	if p.Workdir != "" && !nameRE.MatchString(p.Workdir) {
		return Project{}, "", false, fmt.Errorf("%s: invalid workdir name %q", path, p.Workdir)
	}
	if p.Workdir != "" && !p.Remote {
		return Project{}, "", false, fmt.Errorf("%s: workdir needs remote: true", path)
	}
	return p, path, true, nil
}
