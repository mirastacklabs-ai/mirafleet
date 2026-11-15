package fsops

import (
	"bytes"
	"context"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Grep output modes.
const (
	GrepContent          = "content"
	GrepFilesWithMatches = "files_with_matches"
	GrepCount            = "count"
)

// GrepOptions mirrors Claude Code's Grep tool. The regular expression is Go RE2, which is also
// what ripgrep's default engine accepts for these patterns.
type GrepOptions struct {
	Pattern     string
	Path        string // file or directory below the root ("" is the root)
	Glob        string // ripgrep --glob style filter, e.g. "*.{ts,tsx}" or "!vendor/**"
	Type        string // ripgrep type name, see GrepTypeNames
	Mode        string // content, files_with_matches (default) or count
	IgnoreCase  bool
	LineNumbers bool   // content mode: prefix matched lines with the line number
	After       int    // -A
	Before      int    // -B
	Context     int    // -C, applies to whichever of After or Before is zero
	Multiline   bool   // the pattern may span lines and . matches newline
	HeadLimit   int    // most output items (lines, files or counts); 0 is unlimited up to the output cap
	Offset      int    // items skipped before HeadLimit applies
	Hidden      bool   // search dot files and directories (.git never)
	NoIgnore    bool   // do not honour ignore files
	Display     string // prefix of every path in the result (the caller rooted the search below the path the user gave)
	Limits      SearchLimits
}

// FileCount is one count-mode row.
type FileCount struct {
	Path  string
	Count int // matching lines
}

// GrepResult holds the page selected by Offset and HeadLimit. Content lines are "path:12:text"
// for matches and "path-12-text" for context, with "--" between non-adjacent groups when context
// is requested.
type GrepResult struct {
	Mode          string
	Lines         []string
	Files         []string
	Counts        []FileCount
	FilesSearched int
	Truncated     bool
	Reason        string // one of the Reason constants when Truncated
}

const (
	maxGrepFile      = 32 << 20 // larger files are skipped
	maxGrepLineBytes = 4096     // longer output lines are clipped
	maxOutputItems   = 20_000
	maxOutputBytes   = 4 << 20
	binarySniff      = 8192
)

type grepSpec struct {
	re       *regexp.Regexp
	mode     string
	start    string
	before   int
	after    int
	types    []wildPat
	globs    globFilter
	hasGlobs bool
}

func normalizeGrep(o GrepOptions) (grepSpec, error) {
	var s grepSpec
	if o.Pattern == "" {
		return s, fail(ErrInvalid, "pattern is required")
	}
	expr := o.Pattern
	if o.Multiline {
		expr = "(?ms)" + expr
	}
	if o.IgnoreCase {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return s, fail(ErrInvalid, "invalid regex: %v", err)
	}
	s.re = re
	s.mode = o.Mode
	switch s.mode {
	case "":
		s.mode = GrepFilesWithMatches
	case GrepContent, GrepFilesWithMatches, GrepCount:
	default:
		return s, fail(ErrInvalid, "output_mode must be content, files_with_matches or count")
	}
	if o.After < 0 || o.Before < 0 || o.Context < 0 || o.HeadLimit < 0 || o.Offset < 0 {
		return s, fail(ErrInvalid, "negative count or limit")
	}
	s.after, s.before = o.After, o.Before
	if s.after == 0 {
		s.after = o.Context
	}
	if s.before == 0 {
		s.before = o.Context
	}
	if o.Type != "" {
		if s.types, err = typeMatcher(o.Type); err != nil {
			return s, err
		}
	}
	if strings.TrimSpace(o.Glob) != "" {
		s.globs = newGlobFilter(o.Glob)
		s.hasGlobs = true
	}
	if s.start, err = cleanSearchPath(o.Path); err != nil {
		return s, err
	}
	return s, nil
}

type grepRun struct {
	root    *os.Root
	spec    grepSpec
	opt     GrepOptions
	lim     SearchLimits
	res     GrepResult
	items   int
	need    int // stop once this many items are collected (offset + cap + 1)
	capItem int
	outSize int
	bytes   int64
	emitted bool // any content line so far, for "--" separators
	reason  string
}

// Grep searches the files below the search path. Symlinks are never followed, binary files
// (a NUL in the first 8 KiB) and files over 32 MiB are skipped, and the run stops at the
// SearchLimits, reporting Truncated and a Reason.
func Grep(ctx context.Context, root *os.Root, o GrepOptions) (GrepResult, error) {
	spec, err := normalizeGrep(o)
	if err != nil {
		return GrepResult{}, err
	}
	g := &grepRun{root: root, spec: spec, opt: o, lim: o.Limits.withDefaults()}
	g.res.Mode = spec.mode
	g.capItem = o.HeadLimit
	if g.capItem <= 0 || g.capItem > maxOutputItems {
		g.capItem = maxOutputItems
	}
	g.need = o.Offset + g.capItem + 1
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, g.lim.Timeout)
	defer cancel()
	fsys := root.FS()
	fi, err := root.Stat(spec.start)
	if err != nil {
		return GrepResult{}, mapErr(spec.start, err)
	}
	filters := fi.IsDir()
	prefix := spec.start + "/"
	if spec.start == "." {
		prefix = ""
	}
	reason, err := walkTree(ctx, fsys, spec.start, walkOpts{hidden: o.Hidden, noIgnore: o.NoIgnore, maxFiles: g.lim.MaxFiles}, func(rel string) error {
		if filters && !g.accept(strings.TrimPrefix(rel, prefix)) {
			return nil
		}
		return g.searchFile(rel)
	})
	if err != nil {
		return GrepResult{}, mapErr(spec.start, err)
	}
	if parent.Err() != nil {
		return GrepResult{}, parent.Err()
	}
	if g.reason != "" {
		reason = g.reason
	}
	if reason == "" && ctx.Err() != nil {
		reason = ReasonTimeout
	}
	g.finish(reason)
	return g.res, nil
}

func (g *grepRun) accept(sub string) bool {
	if g.spec.hasGlobs && !g.spec.globs.allows(sub) {
		return false
	}
	if g.spec.types != nil {
		base := sub[strings.LastIndexByte(sub, '/')+1:]
		for _, t := range g.spec.types {
			if t.match(base) {
				return true
			}
		}
		return false
	}
	return true
}

func (g *grepRun) searchFile(rel string) error {
	f, err := g.root.OpenFile(rel, os.O_RDONLY|openFlags, 0)
	if err != nil {
		return nil // vanished or unreadable: skipped
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil
	}
	data, big := readCapped(f, maxGrepFile)
	if big {
		return nil
	}
	g.res.FilesSearched++
	if g.bytes += int64(len(data)); g.bytes > g.lim.MaxBytes {
		g.reason = ReasonMaxBytes
		return errStopWalk
	}
	if bytes.IndexByte(data[:min(len(data), binarySniff)], 0) >= 0 || len(data) == 0 {
		return nil
	}
	starts := []int{0}
	for i, b := range data {
		if b == '\n' && i+1 < len(data) {
			starts = append(starts, i+1)
		}
	}
	nl := len(starts)
	text := func(n int) []byte {
		s, e := starts[n-1], len(data)
		if n < nl {
			e = starts[n] - 1
		}
		if e > s && data[e-1] == '\n' {
			e--
		}
		if e > s && data[e-1] == '\r' {
			e--
		}
		return data[s:e]
	}
	var matched []int
	if g.spec.re != nil && g.opt.Multiline {
		last := 0
		for _, loc := range g.spec.re.FindAllIndex(data, -1) {
			s, e := loc[0], loc[1]
			if s >= len(data) {
				continue
			}
			if e > s {
				e--
			}
			a := sort.Search(nl, func(i int) bool { return starts[i] > s })
			b := sort.Search(nl, func(i int) bool { return starts[i] > e })
			for n := max(a, last+1); n <= b; n++ {
				matched = append(matched, n)
				last = n
			}
		}
	} else {
		for n := 1; n <= nl; n++ {
			if g.spec.re.Match(text(n)) {
				matched = append(matched, n)
			}
		}
	}
	if len(matched) == 0 {
		return nil
	}
	switch g.spec.mode {
	case GrepFilesWithMatches:
		g.res.Files = append(g.res.Files, g.opt.Display+rel)
		if g.count(len(g.opt.Display + rel)) {
			return errStopWalk
		}
	case GrepCount:
		g.res.Counts = append(g.res.Counts, FileCount{Path: g.opt.Display + rel, Count: len(matched)})
		if g.count(len(g.opt.Display+rel) + 8) {
			return errStopWalk
		}
	default:
		return g.emitContent(rel, text, nl, matched)
	}
	return nil
}

// count accounts one output item and reports whether the run has collected enough.
func (g *grepRun) count(size int) bool {
	g.items++
	g.outSize += size
	if g.outSize > maxOutputBytes {
		g.reason = ReasonOutputCap
		return true
	}
	return g.items >= g.need
}

func (g *grepRun) emitContent(rel string, text func(int) []byte, nl int, matched []int) error {
	a, b := g.spec.after, g.spec.before
	ctxOn := a > 0 || b > 0
	isMatch := make(map[int]bool, len(matched))
	type span struct{ from, to int }
	var spans []span
	for _, m := range matched {
		isMatch[m] = true
		from, to := max(1, m-b), min(nl, m+a)
		if k := len(spans) - 1; k >= 0 && from <= spans[k].to+1 {
			spans[k].to = max(spans[k].to, to)
		} else {
			spans = append(spans, span{from, to})
		}
	}
	for _, sp := range spans {
		if ctxOn && g.emitted {
			g.res.Lines = append(g.res.Lines, "--")
			if g.count(2) {
				return errStopWalk
			}
		}
		g.emitted = true
		for n := sp.from; n <= sp.to; n++ {
			sep := "-"
			if isMatch[n] {
				sep = ":"
			}
			line := g.opt.Display + rel + sep
			if g.opt.LineNumbers {
				line += strconv.Itoa(n) + sep
			}
			line += clipLine(string(text(n)))
			g.res.Lines = append(g.res.Lines, line)
			if g.count(len(line) + 1) {
				return errStopWalk
			}
		}
	}
	return nil
}

// finish selects the Offset/HeadLimit page and records why the run was partial.
func (g *grepRun) finish(reason string) {
	off, end := g.opt.Offset, g.opt.Offset+g.capItem
	page := func(n int) (int, int) { return min(off, n), min(end, n) }
	lo, hi := page(len(g.res.Lines))
	g.res.Lines = g.res.Lines[lo:hi]
	lo, hi = page(len(g.res.Files))
	g.res.Files = g.res.Files[lo:hi]
	lo, hi = page(len(g.res.Counts))
	g.res.Counts = g.res.Counts[lo:hi]
	if g.items > end {
		reason = ReasonOutputCap
		if g.opt.HeadLimit > 0 && g.opt.HeadLimit <= maxOutputItems {
			reason = ReasonHeadLimit
		}
	}
	if reason != "" {
		g.res.Truncated = true
		g.res.Reason = reason
	}
}

func clipLine(s string) string {
	if len(s) <= maxGrepLineBytes {
		return s
	}
	n := maxGrepLineBytes
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

func readCapped(f io.Reader, max int64) ([]byte, bool) {
	data, _ := io.ReadAll(io.LimitReader(f, max+1))
	return data, int64(len(data)) > max
}
