package fsops

import (
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const maxLSEntries = 1000

// LSResult is a tree listing in Claude Code's LS format.
type LSResult struct {
	Text      string `json:"text"`
	Count     int    `json:"count"`
	Truncated bool   `json:"truncated,omitempty"`
}

// LS lists a directory recursively as an indented tree. Dot files and __pycache__ are skipped;
// ignore globs match an entry's name or its path below the listed directory. Symlinked
// directories are listed but not entered. At most 1000 entries are returned.
func (f *FS) LS(dir string, ignore []string) (*LSResult, error) {
	if dir == "" {
		dir = "."
	}
	rel, err := f.rel(dir)
	if err != nil {
		return nil, err
	}
	fi, err := f.root.Stat(rel)
	if err != nil {
		return nil, mapErr(rel, err)
	}
	if !fi.IsDir() {
		return nil, fail(ErrInvalid, "%s is not a directory", rel)
	}
	for _, g := range ignore {
		if _, err := path.Match(g, ""); err != nil {
			return nil, fail(ErrInvalid, "invalid ignore pattern %q", g)
		}
	}
	res := &LSResult{}
	var sb strings.Builder
	shown := f.root.p(rel)
	fmt.Fprintf(&sb, "- %s%c\n", shown, filepath.Separator)
	f.walk(&sb, res, rel, "", 1, ignore)
	res.Text = sb.String()
	if res.Truncated {
		res.Text += fmt.Sprintf("\nThere are more than %d entries. Only the first %d are shown; use LS on a subdirectory to see the rest.\n", maxLSEntries, maxLSEntries)
	}
	return res, nil
}

func (f *FS) walk(sb *strings.Builder, res *LSResult, rel, sub string, depth int, ignore []string) {
	d, err := f.root.Open(rel)
	if err != nil {
		return
	}
	ents, err := d.ReadDir(-1)
	_ = d.Close()
	if err != nil {
		return
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
	for _, e := range ents {
		name := e.Name()
		if strings.HasPrefix(name, ".") || name == "__pycache__" {
			continue
		}
		p := path.Join(filepath.ToSlash(sub), name)
		if ignored(ignore, name, p) {
			continue
		}
		if res.Count >= maxLSEntries {
			res.Truncated = true
			return
		}
		res.Count++
		isDir := e.IsDir()
		if e.Type()&fs.ModeSymlink != 0 {
			if fi, err := f.root.Stat(filepath.Join(rel, name)); err == nil && fi.IsDir() {
				fmt.Fprintf(sb, "%s- %s/ (symlink)\n", strings.Repeat("  ", depth), name)
				continue
			}
		}
		suffix := ""
		if isDir {
			suffix = "/"
		}
		fmt.Fprintf(sb, "%s- %s%s\n", strings.Repeat("  ", depth), name, suffix)
		if isDir {
			f.walk(sb, res, filepath.Join(rel, name), filepath.Join(sub, name), depth+1, ignore)
			if res.Truncated {
				return
			}
		}
	}
}

func ignored(globs []string, name, p string) bool {
	for _, g := range globs {
		if ok, _ := path.Match(g, name); ok {
			return true
		}
		if ok, _ := path.Match(g, p); ok {
			return true
		}
	}
	return false
}

// Info describes one path; symlinks are described, not followed.
type Info struct {
	Path    string      `json:"path"`
	Type    string      `json:"type"` // file, dir, symlink or other
	Size    int64       `json:"size"`
	Mode    fs.FileMode `json:"mode"`
	ModTime time.Time   `json:"mod_time"`
	Target  string      `json:"target,omitempty"` // of a symlink
}

// Stat describes a path inside the root.
func (f *FS) Stat(path string) (*Info, error) {
	rel, err := f.rel(path)
	if err != nil {
		return nil, err
	}
	fi, err := f.root.Lstat(rel)
	if err != nil {
		return nil, mapErr(rel, err)
	}
	in := &Info{Path: rel, Size: fi.Size(), Mode: fi.Mode(), ModTime: fi.ModTime(), Type: "other"}
	switch m := fi.Mode(); {
	case m.IsRegular():
		in.Type = "file"
	case m.IsDir():
		in.Type = "dir"
	case m&fs.ModeSymlink != 0:
		in.Type = "symlink"
		in.Target, _ = f.root.Readlink(rel)
	}
	return in, nil
}
