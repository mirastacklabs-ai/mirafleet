package fsops

import (
	"context"
	"os"
	"path"
	"sort"
	"strings"
	"time"
)

// GlobOptions mirrors Claude Code's Glob tool.
type GlobOptions struct {
	Pattern  string // doublestar pattern relative to Path, e.g. "**/*.go" or "src/*.{ts,tsx}"
	Path     string // directory below the root to search ("" or "." is the root)
	Hidden   bool   // ignored: Glob always includes dot files and directories, like Claude Code (.git is never searched)
	NoIgnore bool   // do not honour .gitignore, .ignore and .rgignore
	Limit    int    // most paths returned (default 1000)
	Limits   SearchLimits
}

// GlobResult lists matching regular files, root-relative, newest modification time first
// (ties by path).
type GlobResult struct {
	Paths     []string
	Truncated bool
	Reason    string // why the result is partial, "" when complete
}

const defaultGlobLimit = 1000

// Glob finds files by pattern. Symlinks are never followed or returned.
func Glob(ctx context.Context, root *os.Root, o GlobOptions) (GlobResult, error) {
	var res GlobResult
	pat := strings.TrimPrefix(strings.TrimSpace(o.Pattern), "./")
	if pat == "" || strings.HasPrefix(pat, "/") {
		return res, fail(ErrInvalid, "glob pattern must be non-empty and relative")
	}
	start, err := cleanSearchPath(o.Path)
	if err != nil {
		return res, err
	}
	var alts [][]pseg
	for _, a := range expandBraces(pat) {
		alts = append(alts, compileSegs(path.Clean(a)))
	}
	lim := o.Limits.withDefaults()
	ctx, cancel := context.WithTimeout(ctx, lim.Timeout)
	defer cancel()
	fsys := root.FS()
	type hit struct {
		rel string
		mod time.Time
	}
	var hits []hit
	prefix := start + "/"
	if start == "." {
		prefix = ""
	}
	reason, err := walkTree(ctx, fsys, start, walkOpts{hidden: true, noIgnore: o.NoIgnore, maxFiles: lim.MaxFiles}, func(rel string) error {
		name := strings.Split(strings.TrimPrefix(rel, prefix), "/")
		for _, a := range alts {
			if segsMatch(a, name) {
				var mod time.Time
				if fi, err := root.Lstat(rel); err == nil {
					mod = fi.ModTime()
				}
				hits = append(hits, hit{rel, mod})
				break
			}
		}
		return nil
	})
	if err != nil {
		return res, mapErr(start, err)
	}
	if ctx.Err() != nil && reason == "" {
		reason = ReasonTimeout
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if !hits[i].mod.Equal(hits[j].mod) {
			return hits[i].mod.After(hits[j].mod)
		}
		return hits[i].rel < hits[j].rel
	})
	limit := o.Limit
	if limit <= 0 || limit > defaultGlobLimit*100 {
		limit = defaultGlobLimit
	}
	if len(hits) > limit {
		hits = hits[:limit]
		res.Truncated = true
		reason = ReasonHeadLimit
	}
	for _, h := range hits {
		res.Paths = append(res.Paths, h.rel)
	}
	if reason != "" {
		res.Truncated = true
		res.Reason = reason
	}
	return res, nil
}
