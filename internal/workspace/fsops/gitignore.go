package fsops

import (
	"io"
	"io/fs"
	"path"
	"strings"
)

// Wildcard and gitignore engine shared by grep and glob. Patterns use *, ?, [class], backslash
// escapes within a path segment and a whole-segment ** across segments.

type wtok struct {
	kind byte // '*', '?', '[' (class) or 'l' (literal)
	r    rune
	neg  bool
	rng  [][2]rune
}

func (t wtok) matches(c rune) bool {
	switch t.kind {
	case '?':
		return true
	case 'l':
		return t.r == c
	}
	in := false
	for _, r := range t.rng {
		if c >= r[0] && c <= r[1] {
			in = true
			break
		}
	}
	return in != t.neg
}

type wildPat []wtok

func compileWild(p string) wildPat {
	rs := []rune(p)
	var out wildPat
	for i := 0; i < len(rs); i++ {
		switch rs[i] {
		case '*':
			if n := len(out); n == 0 || out[n-1].kind != '*' {
				out = append(out, wtok{kind: '*'})
			}
		case '?':
			out = append(out, wtok{kind: '?'})
		case '\\':
			if i+1 < len(rs) {
				i++
			}
			out = append(out, wtok{kind: 'l', r: rs[i]})
		case '[':
			j := i + 1
			t := wtok{kind: '['}
			if j < len(rs) && (rs[j] == '!' || rs[j] == '^') {
				t.neg = true
				j++
			}
			start := j
			for j < len(rs) && (rs[j] != ']' || j == start) {
				if rs[j] == '\\' && j+1 < len(rs) {
					j++
				}
				lo := rs[j]
				hi := lo
				if j+2 < len(rs) && rs[j+1] == '-' && rs[j+2] != ']' {
					hi = rs[j+2]
					j += 2
				}
				t.rng = append(t.rng, [2]rune{lo, hi})
				j++
			}
			if j >= len(rs) { // no closing bracket: a literal [
				out = append(out, wtok{kind: 'l', r: '['})
				continue
			}
			out = append(out, t)
			i = j
		default:
			out = append(out, wtok{kind: 'l', r: rs[i]})
		}
	}
	return out
}

// match reports whether the path segment s matches the whole pattern.
func (w wildPat) match(s string) bool {
	sr := []rune(s)
	pi, si, star, mark := 0, 0, -1, 0
	for si < len(sr) {
		switch {
		case pi < len(w) && w[pi].kind == '*':
			star, mark = pi, si
			pi++
		case pi < len(w) && w[pi].matches(sr[si]):
			pi++
			si++
		case star >= 0:
			mark++
			pi, si = star+1, mark
		default:
			return false
		}
	}
	for pi < len(w) && w[pi].kind == '*' {
		pi++
	}
	return pi == len(w)
}

func wildMatch(p, s string) bool { return compileWild(p).match(s) }

type pseg struct {
	dstar bool
	w     wildPat
}

func compileSegs(pattern string) []pseg {
	parts := strings.Split(pattern, "/")
	out := make([]pseg, len(parts))
	for i, p := range parts {
		if p == "**" {
			out[i].dstar = true
		} else {
			out[i].w = compileWild(p)
		}
	}
	return out
}

func segsMatch(pat []pseg, name []string) bool {
	for len(pat) > 0 {
		if pat[0].dstar {
			if len(pat) == 1 {
				return len(name) > 0
			}
			for i := 0; i <= len(name); i++ {
				if segsMatch(pat[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 || !pat[0].w.match(name[0]) {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}

// expandBraces expands {a,b} alternatives (nested allowed), at most 256 results.
func expandBraces(p string) []string {
	open := -1
	for i := 0; i < len(p); i++ {
		if p[i] == '\\' {
			i++
			continue
		}
		if p[i] == '{' {
			open = i
			break
		}
	}
	if open < 0 {
		return []string{p}
	}
	depth, last, closeAt := 0, open+1, -1
	var alts []string
	for i := open; i < len(p) && closeAt < 0; i++ {
		switch p[i] {
		case '\\':
			i++
		case '{':
			depth++
		case ',':
			if depth == 1 {
				alts = append(alts, p[last:i])
				last = i + 1
			}
		case '}':
			depth--
			if depth == 0 {
				alts = append(alts, p[last:i])
				closeAt = i
			}
		}
	}
	if closeAt < 0 {
		return []string{p}
	}
	var out []string
	for _, a := range alts {
		for _, e := range expandBraces(p[:open] + a + p[closeAt+1:]) {
			if len(out) >= 256 {
				return out
			}
			out = append(out, e)
		}
	}
	return out
}

// ---- gitignore ----

type ignoreRule struct {
	neg, dirOnly, anchored bool
	segs                   []pseg
}

type ignoreLayer struct {
	base  string // directory of the ignore file, "." for the search root
	rules []ignoreRule
}

type ignoreStack []ignoreLayer

func parseIgnore(data []byte) []ignoreRule {
	var rules []ignoreRule
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		for strings.HasSuffix(line, " ") && !strings.HasSuffix(line, `\ `) {
			line = line[:len(line)-1]
		}
		if line == "" || line[0] == '#' {
			continue
		}
		var r ignoreRule
		if line[0] == '!' {
			r.neg = true
			line = line[1:]
		}
		if strings.HasPrefix(line, `\#`) || strings.HasPrefix(line, `\!`) {
			line = line[1:]
		}
		if strings.HasSuffix(line, "/") {
			r.dirOnly = true
			line = strings.TrimRight(line, "/")
		}
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "/") {
			r.anchored = true
			line = strings.TrimLeft(line, "/")
		} else if strings.Contains(line, "/") {
			r.anchored = true
		}
		if line == "" {
			continue
		}
		r.segs = compileSegs(line)
		rules = append(rules, r)
	}
	return rules
}

func (r ignoreRule) matches(rel string, isDir bool) bool {
	if r.dirOnly && !isDir {
		return false
	}
	if !r.anchored {
		return r.segs[0].w.match(path.Base(rel))
	}
	return segsMatch(r.segs, strings.Split(rel, "/"))
}

// ignored reports whether rel (relative to the search root) is excluded; deeper ignore files win
// over shallower ones and the last matching rule in a file wins.
func (s ignoreStack) ignored(rel string, isDir bool) bool {
	for i := len(s) - 1; i >= 0; i-- {
		l := s[i]
		sub := rel
		if l.base != "." {
			if !strings.HasPrefix(rel, l.base+"/") {
				continue
			}
			sub = rel[len(l.base)+1:]
		}
		for j := len(l.rules) - 1; j >= 0; j-- {
			if l.rules[j].matches(sub, isDir) {
				return !l.rules[j].neg
			}
		}
	}
	return false
}

const maxIgnoreFile = 1 << 20

// loadIgnoreLayer reads .gitignore, .ignore and .rgignore of dir (later files win) and, for the
// root, .git/info/exclude. Missing or irregular files (a planted FIFO) are skipped.
func loadIgnoreLayer(fsys fs.FS, dir string) (ignoreLayer, bool) {
	names := []string{".gitignore", ".ignore", ".rgignore"}
	if dir == "." {
		names = append([]string{".git/info/exclude"}, names...)
	}
	l := ignoreLayer{base: dir}
	for _, n := range names {
		p := path.Join(dir, n)
		fi, err := fs.Stat(fsys, p)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		f, err := fsys.Open(p)
		if err != nil {
			continue
		}
		data, _ := io.ReadAll(io.LimitReader(f, maxIgnoreFile))
		f.Close()
		l.rules = append(l.rules, parseIgnore(data)...)
	}
	return l, len(l.rules) > 0
}
