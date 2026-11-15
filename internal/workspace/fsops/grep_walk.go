package fsops

import (
	"context"
	"errors"
	"io/fs"
	"path"
	"strings"
	"time"
)

// SearchLimits bounds one grep or glob run; zero fields select the defaults.
type SearchLimits struct {
	MaxFiles int           // files visited (default 100000)
	MaxBytes int64         // file bytes read by grep (default 256 MiB)
	Timeout  time.Duration // wall clock (default 30 s)
}

func (l SearchLimits) withDefaults() SearchLimits {
	if l.MaxFiles <= 0 {
		l.MaxFiles = 100_000
	}
	if l.MaxBytes <= 0 {
		l.MaxBytes = 256 << 20
	}
	if l.Timeout <= 0 {
		l.Timeout = 30 * time.Second
	}
	return l
}

// Reasons a search stopped early; the result is then partial.
const (
	ReasonHeadLimit = "head_limit"
	ReasonOutputCap = "output_cap"
	ReasonMaxFiles  = "max_files"
	ReasonMaxBytes  = "max_bytes"
	ReasonTimeout   = "timeout"
)

var errStopWalk = errors.New("stop walk")

// cleanSearchPath turns a root-relative path into an fs.FS path ("." for the root).
func cleanSearchPath(p string) (string, error) {
	if p == "" {
		return ".", nil
	}
	p = path.Clean(strings.ReplaceAll(p, "\\", "/"))
	if !fs.ValidPath(p) {
		return "", fail(ErrInvalid, "path %q is outside the workspace", p)
	}
	return p, nil
}

type walkOpts struct {
	hidden, noIgnore bool
	maxFiles         int
}

// walkTree visits the regular files below start in name order. Symlinks, devices, FIFOs and
// sockets are skipped (nothing is followed, so nothing can escape the root); .git is never
// entered; hidden names are skipped unless o.hidden; ignore files apply unless o.noIgnore. If
// start is a regular file it is visited alone. It returns the reason the walk ended early, if any.
func walkTree(ctx context.Context, fsys fs.FS, start string, o walkOpts, visit func(rel string) error) (string, error) {
	fi, err := fs.Stat(fsys, start)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		if !fi.Mode().IsRegular() {
			return "", nil
		}
		if err := visit(start); err != nil && !errors.Is(err, errStopWalk) {
			return "", err
		}
		return "", nil
	}
	var stack ignoreStack
	if !o.noIgnore {
		dir := "."
		parts := []string{"."}
		if start != "." {
			parts = append(parts, strings.Split(start, "/")...)
		}
		for i, p := range parts {
			if i > 0 {
				dir = path.Join(dir, p)
			}
			if l, ok := loadIgnoreLayer(fsys, dir); ok {
				stack = append(stack, l)
			}
		}
	}
	reason := ""
	files := 0
	var rec func(dir string, st ignoreStack) error
	rec = func(dir string, st ignoreStack) error {
		ents, err := fs.ReadDir(fsys, dir)
		if err != nil {
			return nil // unreadable directory: skipped like rg does
		}
		for _, e := range ents {
			if ctx.Err() != nil {
				reason = ReasonTimeout
				return errStopWalk
			}
			name := e.Name()
			if name == ".git" || (!o.hidden && strings.HasPrefix(name, ".")) {
				continue
			}
			typ := e.Type()
			if typ&fs.ModeSymlink != 0 {
				continue
			}
			rel := path.Join(dir, name)
			switch {
			case e.IsDir():
				if !o.noIgnore && st.ignored(rel, true) {
					continue
				}
				sub := st
				if !o.noIgnore {
					if l, ok := loadIgnoreLayer(fsys, rel); ok {
						sub = append(st[:len(st):len(st)], l)
					}
				}
				if err := rec(rel, sub); err != nil {
					return err
				}
			case typ.IsRegular():
				if !o.noIgnore && st.ignored(rel, false) {
					continue
				}
				if files++; files > o.maxFiles {
					reason = ReasonMaxFiles
					return errStopWalk
				}
				if err := visit(rel); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := rec(start, stack); err != nil && !errors.Is(err, errStopWalk) {
		return reason, err
	}
	return reason, nil
}
