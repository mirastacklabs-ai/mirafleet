package opconf

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ProjectName is the per-project default file. It holds only a fleet and a context name: a cloned
// repository must never be able to add a central, a key or a cap.
const ProjectName = ".mirafleet.yaml"

// maxProjectBytes bounds the project file.
const maxProjectBytes = 4 << 10

// Project is the contents of .mirafleet.yaml. Either field may be empty.
type Project struct {
	Fleet   string `yaml:"fleet,omitempty"`
	Context string `yaml:"context,omitempty"`
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
	f, err := os.Open(path)
	if err != nil {
		return Project{}, "", false, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxProjectBytes+1))
	if err != nil {
		return Project{}, "", false, err
	}
	if len(b) > maxProjectBytes {
		return Project{}, "", false, fmt.Errorf("%s: larger than %d bytes", path, maxProjectBytes)
	}
	var p Project
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil && !errors.Is(err, io.EOF) {
		return Project{}, "", false, fmt.Errorf("parse %s (only fleet and context are allowed): %w", path, err)
	}
	if p.Fleet != "" && !nameRE.MatchString(p.Fleet) {
		return Project{}, "", false, fmt.Errorf("%s: invalid fleet name %q", path, p.Fleet)
	}
	if p.Context != "" && !nameRE.MatchString(p.Context) {
		return Project{}, "", false, fmt.Errorf("%s: invalid context name %q", path, p.Context)
	}
	return p, path, true, nil
}
