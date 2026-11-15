package fsops

import (
	"errors"
	"fmt"
	"strings"
)

// EditOp is one exact string replacement.
type EditOp struct {
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

// EditResult describes a completed edit or multi_edit.
type EditResult struct {
	Path         string `json:"path"`
	Created      bool   `json:"created,omitempty"`
	Replacements int    `json:"replacements"`
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256"`
	Snippet      string `json:"snippet,omitempty"` // numbered lines around a single replacement
}

const notebookEditMsg = "File is a Jupyter Notebook. Use the notebook_edit tool to edit this file."

// Edit replaces old_string with new_string. Without replace_all old_string must occur exactly
// once. An empty old_string creates the file (or fills an empty one).
func (f *FS) Edit(path string, op EditOp) (*EditResult, error) {
	return f.edit(path, []EditOp{op}, false)
}

// MultiEdit applies the edits in order, each to the result of the previous one. Either all are
// applied or the file is left untouched. A first edit with an empty old_string creates the file.
func (f *FS) MultiEdit(path string, ops []EditOp) (*EditResult, error) {
	if len(ops) == 0 {
		return nil, fail(ErrInvalid, "edits must not be empty")
	}
	return f.edit(path, ops, true)
}

func (f *FS) edit(path string, ops []EditOp, multi bool) (*EditResult, error) {
	rel, err := f.rel(path)
	if err != nil {
		return nil, err
	}
	if isNotebook(rel) {
		return nil, fail(ErrInvalid, "%s", notebookEditMsg)
	}
	rel, err = f.target(rel)
	if err != nil {
		return nil, err
	}
	var content string
	var exists bool
	switch data, _, err := f.readAll(rel); {
	case err == nil:
		content, exists = string(data), true
	case errors.Is(err, ErrNotFound):
	default:
		return nil, err
	}
	res := &EditResult{}
	start := 0
	if ops[0].OldString == "" {
		if exists && content != "" {
			return nil, fail(ErrInvalid, "Cannot create new file - file already exists.")
		}
		content, exists, res.Created = ops[0].NewString, true, !exists
		start = 1
	}
	if !exists {
		return nil, fail(ErrNotFound, "File does not exist: %s", rel)
	}
	var firstIdx int
	content, res.Replacements, firstIdx, err = applyEdits(content, ops[start:], multi, start)
	if err != nil {
		return nil, err
	}
	w, err := f.put(rel, []byte(content))
	if err != nil {
		return nil, err
	}
	res.Path, res.Size, res.SHA256 = w.Path, w.Size, w.SHA256
	if len(ops) == 1 && start == 0 && !ops[0].ReplaceAll && firstIdx >= 0 {
		res.Snippet = snippet(content, firstIdx, ops[0].NewString)
	}
	return res, nil
}

// applyEdits applies ops (numbered from base) in order. It returns the new content, the number
// of replacements and the byte offset of the first replacement of the first op (-1 if none).
func applyEdits(content string, ops []EditOp, multi bool, base int) (string, int, int, error) {
	total, first := 0, -1
	for i, op := range ops {
		out, n, idx, err := applyEdit(content, op)
		if err != nil {
			if multi {
				var e *Error
				if errors.As(err, &e) {
					return "", 0, 0, &Error{Kind: e.Kind, Msg: fmt.Sprintf("Edit %d failed: %s", base+i+1, e.Msg)}
				}
			}
			return "", 0, 0, err
		}
		if i == 0 {
			first = idx
		}
		content, total = out, total+n
	}
	return content, total, first, nil
}

func applyEdit(content string, op EditOp) (string, int, int, error) {
	if op.OldString == op.NewString {
		return "", 0, 0, fail(ErrInvalid, "No changes to make: old_string and new_string are exactly the same.")
	}
	if op.OldString == "" {
		return "", 0, 0, fail(ErrInvalid, "old_string must not be empty (only the first edit may create a file).")
	}
	old := op.OldString
	n := strings.Count(content, old)
	if n == 0 {
		return "", 0, 0, fail(ErrInvalid, "String to replace not found in file.\nString: %s", old)
	}
	if n > 1 && !op.ReplaceAll {
		return "", 0, 0, fail(ErrInvalid, "Found %d matches of the string to replace, but replace_all is false. To replace all occurrences, set replace_all to true. To replace only one occurrence, please provide more context to uniquely identify the instance.\nString: %s", n, old)
	}
	// Deleting a whole line also removes its newline, as Claude Code does.
	if op.NewString == "" && !strings.HasSuffix(old, "\n") && strings.Contains(content, old+"\n") {
		old += "\n"
	}
	idx := strings.Index(content, old)
	if op.ReplaceAll {
		return strings.ReplaceAll(content, old, op.NewString), n, idx, nil
	}
	return content[:idx] + op.NewString + content[idx+len(old):], 1, idx, nil
}

// snippet shows the lines around a replacement, numbered like Read.
func snippet(content string, idx int, newString string) string {
	const ctx = 4
	if idx > len(content) {
		return ""
	}
	line := strings.Count(content[:idx], "\n") + 1
	last := line + strings.Count(newString, "\n")
	from := max(1, line-ctx)
	var sb strings.Builder
	for i, l := range strings.Split(strings.TrimSuffix(content, "\n"), "\n") {
		n := i + 1
		if n < from {
			continue
		}
		if n > last+ctx {
			break
		}
		fmt.Fprintf(&sb, "%6d\t%s\n", n, strings.TrimSuffix(l, "\r"))
	}
	return sb.String()
}
