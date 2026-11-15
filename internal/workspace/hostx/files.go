//go:build linux

package hostx

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/fsops"
)

// Output caps of a grep page, the same as the Go engine's.
const (
	maxGrepItems = 20_000
	maxGrepBytes = 4 << 20
	maxGrepLine  = 4096
	rgTimeout    = 30 * time.Second
)

// FilesOptions tune a Files.
type FilesOptions struct {
	Rg     string // path of ripgrep; "" looks "rg" up on PATH, "-" never uses it
	Limits fsops.SearchLimits
	FS     fsops.Options
}

// Files runs the file tools in-process. Relative paths start at one directory (the session dir
// or workdir); absolute paths reach everything the OS user can see (docs/M7.md §3.7).
type Files struct {
	dir  string
	fs   *fsops.FS
	root *os.Root // upload/download only (workspace.Executor ReadFile/WriteFile): confined to dir
	rg   string
	opt  FilesOptions
}

// OpenFiles starts the tools at dir, which must exist.
func OpenFiles(dir string, opt FilesOptions) (*Files, error) {
	f, err := fsops.Open(dir, opt.FS)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		f.Close()
		return nil, err
	}
	root, err := os.OpenRoot(real)
	if err != nil {
		f.Close()
		return nil, err
	}
	rg := opt.Rg
	switch rg {
	case "":
		rg, _ = exec.LookPath("rg")
	case "-":
		rg = ""
	}
	return &Files{dir: real, fs: f, root: root, rg: rg, opt: opt}, nil
}

// Close releases the tools.
func (f *Files) Close() error {
	f.fs.Close()
	return f.root.Close()
}

// Dir is the resolved directory relative paths start at.
func (f *Files) Dir() string { return f.dir }

// UsesRg reports whether grep delegates to ripgrep.
func (f *Files) UsesRg() bool { return f.rg != "" }

// Do runs one transport fs op; body is the raw content of a write. The result is one of
// *fsops.ReadResult, *fsops.WriteResult, *fsops.EditResult, *fsops.NotebookEditResult,
// fsops.GrepResult, fsops.GlobResult, *fsops.LSResult or *fsops.Info.
func (f *Files) Do(ctx context.Context, op transport.FSOp, body []byte) (any, error) {
	if err := op.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", fsops.ErrInvalid, err)
	}
	switch op.Op {
	case transport.FSRead:
		return f.fs.Read(op.Path, fsops.ReadOptions{Offset: int(op.Offset), Limit: op.Limit})
	case transport.FSWrite:
		if int64(len(body)) != op.Length {
			return nil, fmt.Errorf("%w: body is %d bytes, expected %d", fsops.ErrInvalid, len(body), op.Length)
		}
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != op.SHA256 {
			return nil, fmt.Errorf("%w: body sha256 mismatch", fsops.ErrInvalid)
		}
		res, err := f.fs.Write(op.Path, string(body))
		if err == nil && res.Created && op.Mode != 0 {
			// The file was just created by this process.
			p := res.Path
			if !filepath.IsAbs(p) {
				p = filepath.Join(f.dir, p)
			}
			_ = os.Chmod(p, os.FileMode(op.Mode&0o777))
		}
		return res, err
	case transport.FSEdit:
		return f.fs.Edit(op.Path, fsops.EditOp{OldString: op.OldString, NewString: op.NewString, ReplaceAll: op.ReplaceAll})
	case transport.FSMultiEdit:
		ops := make([]fsops.EditOp, len(op.Edits))
		for i, e := range op.Edits {
			ops[i] = fsops.EditOp{OldString: e.OldString, NewString: e.NewString, ReplaceAll: e.ReplaceAll}
		}
		return f.fs.MultiEdit(op.Path, ops)
	case transport.FSNotebookEdit:
		return f.fs.NotebookEdit(op.Path, fsops.NotebookEditOp{CellID: op.CellID, NewSource: op.NewSource, CellType: op.CellType, EditMode: op.EditMode})
	case transport.FSGrep:
		return f.Grep(ctx, fsops.GrepOptions{
			Pattern: op.Pattern, Path: op.Path, Glob: op.Glob, Type: op.Type, Mode: op.OutputMode,
			IgnoreCase: op.CaseInsensitive, LineNumbers: op.LineNumbers, After: op.After, Before: op.Before,
			Context: op.Context, Multiline: op.Multiline, HeadLimit: op.HeadLimit,
		})
	case transport.FSGlob:
		root, start, display, err := f.searchTarget(op.Path)
		if err != nil {
			return nil, err
		}
		defer root.Close()
		if start != "" {
			return nil, fmt.Errorf("%w: glob path %s is not a directory", fsops.ErrInvalid, op.Path)
		}
		return fsops.Glob(ctx, root, fsops.GlobOptions{Pattern: op.Pattern, Display: display, Limits: f.opt.Limits})
	case transport.FSLS:
		return f.fs.LS(op.Path, op.Ignore)
	case transport.FSStat:
		return f.fs.Stat(op.Path)
	}
	return nil, fmt.Errorf("%w: unknown fs op", fsops.ErrInvalid)
}

// Grep delegates to ripgrep when it is installed and the search can safely run there; otherwise
// (or when rg fails without output) the Go engine answers. The result shape is the same.
func (f *Files) Grep(ctx context.Context, o fsops.GrepOptions) (fsops.GrepResult, error) {
	o.Limits = f.opt.Limits
	if f.rg != "" {
		if args, err := fsops.RgArgs(o); err != nil {
			return fsops.GrepResult{}, err
		} else if f.rgSafe(o.Path) {
			if res, ok := f.runRg(ctx, args, o); ok {
				return res, nil
			}
		}
	}
	root, start, display, err := f.searchTarget(o.Path)
	if err != nil {
		return fsops.GrepResult{}, err
	}
	defer root.Close()
	o.Path, o.Display = start, display
	return fsops.Grep(ctx, root, o)
}

// rgSafe is false when the search path is a symlink or has a backslash: rg follows a symlink
// given as an argument, the Go engine does not, and the two must answer alike.
func (f *Files) rgSafe(p string) bool {
	if p == "" || p == "." {
		return true
	}
	if strings.Contains(p, "\\") {
		return false
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(f.dir, p)
	}
	fi, err := os.Lstat(filepath.Clean(p))
	return err == nil && fi.Mode()&os.ModeSymlink == 0
}

// searchTarget resolves a search path (absolute, or relative to the start directory) for the Go
// engines, which walk below an os.Root. A directory becomes the root (start ""); a file becomes
// its parent as the root and its name as start. display is the prefix that turns the engines'
// root-relative paths back into paths in the form the caller gave. The caller closes root.
func (f *Files) searchTarget(p string) (root *os.Root, start, display string, err error) {
	if strings.ContainsRune(p, 0) {
		return nil, "", "", fmt.Errorf("%w: path contains a NUL byte", fsops.ErrInvalid)
	}
	abs := p
	switch {
	case p == "":
		abs = f.dir
	case !filepath.IsAbs(p):
		abs = filepath.Join(f.dir, p)
	}
	abs = filepath.Clean(abs)
	fi, serr := os.Stat(abs)
	if serr != nil {
		if os.IsNotExist(serr) {
			return nil, "", "", &fsops.Error{Kind: fsops.ErrNotFound, Msg: "Path does not exist: " + p}
		}
		return nil, "", "", &fsops.Error{Kind: fsops.ErrRefused, Msg: p + ": " + serr.Error()}
	}
	shown := filepath.Clean(p)
	base := abs
	if !fi.IsDir() {
		base, start = filepath.Dir(abs), filepath.Base(abs)
		shown = filepath.Dir(shown)
	}
	if p != "" && shown != "." {
		display = strings.TrimSuffix(shown, "/") + "/"
	}
	root, err = os.OpenRoot(base)
	if err != nil {
		return nil, "", "", &fsops.Error{Kind: fsops.ErrRefused, Msg: p + ": " + err.Error()}
	}
	return root, start, display, nil
}

func clip(s string) string {
	if len(s) <= maxGrepLine {
		return s
	}
	n := maxGrepLine
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n] + "…"
}

type rgText struct {
	Text  string `json:"text"`
	Bytes string `json:"bytes"`
}

func (t rgText) String() string {
	if t.Bytes != "" {
		b, _ := base64.StdEncoding.DecodeString(t.Bytes)
		return string(b)
	}
	return t.Text
}

type rgEvent struct {
	Type string `json:"type"`
	Data struct {
		Path       rgText `json:"path"`
		Lines      rgText `json:"lines"`
		LineNumber int    `json:"line_number"`
		Stats      struct {
			Searches int `json:"searches"`
		} `json:"stats"`
	} `json:"data"`
}

func cleanRgPath(p string) string {
	p = strings.TrimPrefix(p, "./")
	return p
}

// runRg runs rg and builds the page. ok is false when rg could not answer (the caller falls back).
func (f *Files) runRg(ctx context.Context, args []string, o fsops.GrepOptions) (res fsops.GrepResult, ok bool) {
	lim := f.opt.Limits
	timeout := lim.Timeout
	if timeout <= 0 {
		timeout = rgTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args = append([]string{"--no-ignore-parent", "--no-ignore-global"}, args...)
	cmd := exec.CommandContext(cctx, f.rg, args...)
	cmd.Dir = f.dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=/nonexistent", "LC_ALL=C.UTF-8"}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return res, false
	}
	if err := cmd.Start(); err != nil {
		return res, false
	}

	mode := o.Mode
	if mode == "" {
		mode = fsops.GrepFilesWithMatches
	}
	res.Mode = mode
	capItem := maxGrepItems
	if o.HeadLimit > 0 && o.HeadLimit <= maxGrepItems {
		capItem = o.HeadLimit
	}
	end := o.Offset + capItem
	need := end + 1
	after, before := o.After, o.Before
	if after == 0 {
		after = o.Context
	}
	if before == 0 {
		before = o.Context
	}
	ctxOn := after > 0 || before > 0

	var (
		items, size int
		stopped     bool
		all         []string // content lines / files
		counts      []fsops.FileCount
		lastFile    string
		lastLine    int
		emitted     bool
		searched    int
	)
	add := func(n int) bool { // account one item, true when enough
		items++
		size += n
		return items >= need || size > maxGrepBytes
	}
	sc := bufio.NewReaderSize(out, 1<<20)
loop:
	for {
		line, err := sc.ReadBytes('\n')
		if len(line) > 0 {
			line = bytes.TrimRight(line, "\r\n")
			switch mode {
			case fsops.GrepFilesWithMatches:
				p := cleanRgPath(string(line))
				all = append(all, p)
				if add(len(p) + 1) {
					stopped = true
					break loop
				}
			case fsops.GrepCount:
				s := string(line)
				i := strings.LastIndexByte(s, ':')
				if i > 0 {
					n, _ := strconv.Atoi(s[i+1:])
					counts = append(counts, fsops.FileCount{Path: cleanRgPath(s[:i]), Count: n})
					if add(len(s) + 1) {
						stopped = true
						break loop
					}
				}
			default:
				var ev rgEvent
				if json.Unmarshal(line, &ev) != nil {
					break
				}
				if ev.Type == "summary" {
					searched = ev.Data.Stats.Searches
					break
				}
				if ev.Type != "match" && ev.Type != "context" {
					break
				}
				path := cleanRgPath(ev.Data.Path.String())
				sep := "-"
				if ev.Type == "match" {
					sep = ":"
				}
				text := strings.TrimSuffix(ev.Data.Lines.String(), "\n")
				for i, tl := range strings.Split(text, "\n") {
					n := ev.Data.LineNumber + i
					if ctxOn && emitted && (path != lastFile || n != lastLine+1) {
						all = append(all, "--")
						if add(3) {
							stopped = true
							break loop
						}
					}
					emitted, lastFile, lastLine = true, path, n
					l := path + sep
					if o.LineNumbers {
						l += strconv.Itoa(n) + sep
					}
					l += clip(strings.TrimSuffix(tl, "\r"))
					all = append(all, l)
					if add(len(l) + 1) {
						stopped = true
						break loop
					}
				}
			}
		}
		if err != nil {
			break
		}
	}
	if stopped {
		cancel()
	}
	werr := cmd.Wait()
	timedOut := !stopped && errors.Is(cctx.Err(), context.DeadlineExceeded)
	if !stopped && !timedOut && ctx.Err() != nil {
		return res, false
	}
	if !stopped && !timedOut {
		var ee *exec.ExitError
		if errors.As(werr, &ee) && ee.ExitCode() != 1 && items == 0 {
			return res, false // rg error (bad flag, unreadable path): let the Go engine answer
		}
	}

	page := func(n int) (int, int) { return min(o.Offset, n), min(end, n) }
	switch mode {
	case fsops.GrepContent:
		lo, hi := page(len(all))
		res.Lines = all[lo:hi]
	case fsops.GrepFilesWithMatches:
		lo, hi := page(len(all))
		res.Files = all[lo:hi]
	default:
		lo, hi := page(len(counts))
		res.Counts = counts[lo:hi]
	}
	res.FilesSearched = searched
	reason := ""
	if timedOut {
		reason = fsops.ReasonTimeout
	}
	if items > end || (stopped && size > maxGrepBytes) {
		reason = fsops.ReasonOutputCap
		if o.HeadLimit > 0 && o.HeadLimit <= maxGrepItems {
			reason = fsops.ReasonHeadLimit
		}
	}
	if reason != "" {
		res.Truncated, res.Reason = true, reason
	}
	return res, true
}
