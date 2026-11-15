package fsops

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

func isNotebook(rel string) bool { return strings.EqualFold(filepath.Ext(rel), ".ipynb") }

// notebook keeps everything it does not understand as raw JSON, so an edit changes only the
// touched cell. Keys are re-emitted sorted with one space of indent, which is how nbformat
// itself writes notebooks.
type notebook struct {
	top   map[string]json.RawMessage
	cells []map[string]json.RawMessage
	minor int
}

func parseNotebook(data []byte) (*notebook, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil || top == nil {
		return nil, fail(ErrInvalid, "Notebook is not valid JSON.")
	}
	var major int
	if raw, ok := top["nbformat"]; ok {
		if json.Unmarshal(raw, &major) != nil || major != 4 {
			return nil, fail(ErrInvalid, "Unsupported notebook format (only nbformat 4 is supported).")
		}
	}
	rawCells, ok := top["cells"]
	if !ok {
		return nil, fail(ErrInvalid, "Notebook has no cells array (only nbformat 4 is supported).")
	}
	var cells []map[string]json.RawMessage
	if err := json.Unmarshal(rawCells, &cells); err != nil {
		return nil, fail(ErrInvalid, "Notebook cells are malformed.")
	}
	for i, c := range cells {
		if c == nil {
			return nil, fail(ErrInvalid, "Notebook cell %d is malformed.", i)
		}
	}
	nb := &notebook{top: top, cells: cells}
	if raw, ok := top["nbformat_minor"]; ok {
		_ = json.Unmarshal(raw, &nb.minor)
	}
	delete(nb.top, "cells")
	return nb, nil
}

func (nb *notebook) marshal() ([]byte, error) {
	cells := nb.cells
	if cells == nil {
		cells = []map[string]json.RawMessage{}
	}
	top := make(map[string]json.RawMessage, len(nb.top)+1)
	for k, v := range nb.top {
		top[k] = v
	}
	raw, err := jsonRaw(cells)
	if err != nil {
		return nil, err
	}
	top["cells"] = raw
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", " ")
	if err := enc.Encode(top); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func jsonRaw(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return json.RawMessage(bytes.TrimSuffix(buf.Bytes(), []byte("\n"))), nil
}

func (nb *notebook) str(i int, key string) string {
	var s string
	_ = json.Unmarshal(nb.cells[i][key], &s)
	return s
}

// cellID is the id of cell i, or "cell-N" when the cell has none.
func (nb *notebook) cellID(i int) string {
	if id := nb.str(i, "id"); id != "" {
		return id
	}
	return "cell-" + strconv.Itoa(i)
}

// find locates a cell by id, by "cell-N", or by a plain index.
func (nb *notebook) find(ref string) int {
	for i := range nb.cells {
		if nb.str(i, "id") == ref {
			return i
		}
	}
	n, ok := strings.CutPrefix(ref, "cell-")
	if !ok {
		n = ref
	}
	if i, err := strconv.Atoi(n); err == nil && i >= 0 && i < len(nb.cells) {
		return i
	}
	return -1
}

// source returns a cell's source whether it is stored as a string or a list of lines.
func (nb *notebook) source(i int) string { return joinText(nb.cells[i]["source"]) }

func joinText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var l []string
	if json.Unmarshal(raw, &l) == nil {
		return strings.Join(l, "")
	}
	return ""
}

// sourceRaw stores source as a list of lines (each keeping its newline), as nbformat does.
func sourceRaw(s string) json.RawMessage {
	lines := []string{}
	for s != "" {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			lines, s = append(lines, s), ""
			break
		}
		lines, s = append(lines, s[:i+1]), s[i+1:]
	}
	raw, _ := jsonRaw(lines)
	return raw
}

var ansi = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]")

const maxOutputText = 64 << 10

func clipOutput(s string) string {
	if len(s) <= maxOutputText {
		return s
	}
	return s[:maxOutputText] + fmt.Sprintf("\n[output truncated: %d bytes omitted]", len(s)-maxOutputText)
}

// render shows the notebook cell by cell with text outputs; image outputs come back as images.
func (nb *notebook) render() (string, []Image) {
	var sb strings.Builder
	var images []Image
	for i := range nb.cells {
		id, typ := nb.cellID(i), nb.str(i, "cell_type")
		if typ == "code" || typ == "" {
			fmt.Fprintf(&sb, "<cell id=%q>\n", id)
		} else {
			fmt.Fprintf(&sb, "<cell id=%q><cell_type>%s</cell_type>\n", id, typ)
		}
		sb.WriteString(nb.source(i))
		sb.WriteString("\n")
		var outs []map[string]json.RawMessage
		_ = json.Unmarshal(nb.cells[i]["outputs"], &outs)
		for _, o := range outs {
			var ot string
			_ = json.Unmarshal(o["output_type"], &ot)
			switch ot {
			case "stream":
				sb.WriteString(clipOutput(joinText(o["text"])))
				sb.WriteString("\n")
			case "error":
				var en, ev string
				_ = json.Unmarshal(o["ename"], &en)
				_ = json.Unmarshal(o["evalue"], &ev)
				var tb []string
				_ = json.Unmarshal(o["traceback"], &tb)
				msg := en + ": " + ev
				if len(tb) > 0 {
					msg = strings.Join(tb, "\n")
				}
				sb.WriteString(clipOutput(ansi.ReplaceAllString(msg, "")))
				sb.WriteString("\n")
			case "execute_result", "display_data":
				var data map[string]json.RawMessage
				_ = json.Unmarshal(o["data"], &data)
				if t, ok := data["text/plain"]; ok {
					sb.WriteString(clipOutput(joinText(t)))
					sb.WriteString("\n")
				}
				for _, mime := range []string{"image/png", "image/jpeg"} {
					if d, ok := data[mime]; ok {
						b64 := strings.Join(strings.Fields(joinText(d)), "")
						if _, err := base64.StdEncoding.DecodeString(b64); err == nil && b64 != "" {
							images = append(images, Image{MIME: mime, Data: b64})
							fmt.Fprintf(&sb, "[%s output attached as image %d]\n", mime, len(images))
						}
						break
					}
				}
			}
		}
		fmt.Fprintf(&sb, "</cell id=%q>\n", id)
	}
	return sb.String(), images
}

// NotebookEditOp is one NotebookEdit call. EditMode is replace (default), insert or delete.
// CellID is an id, "cell-N" or a plain index; insert adds after that cell (at the start when
// empty). CellType (code, markdown, raw) is required for insert and optional for replace.
type NotebookEditOp struct {
	CellID    string `json:"cell_id,omitempty"`
	NewSource string `json:"new_source,omitempty"`
	CellType  string `json:"cell_type,omitempty"`
	EditMode  string `json:"edit_mode,omitempty"`
}

// NotebookEditResult describes a completed notebook edit.
type NotebookEditResult struct {
	Path      string `json:"path"`
	EditMode  string `json:"edit_mode"`
	CellID    string `json:"cell_id"`
	CellIndex int    `json:"cell_index"`
	CellType  string `json:"cell_type,omitempty"`
	Cells     int    `json:"cells"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
}

func validCellType(t string) bool { return t == "code" || t == "markdown" || t == "raw" }

// NotebookEdit replaces, inserts or deletes one cell and rewrites the notebook atomically.
func (f *FS) NotebookEdit(path string, op NotebookEditOp) (*NotebookEditResult, error) {
	rel, err := f.rel(path)
	if err != nil {
		return nil, err
	}
	if !isNotebook(rel) {
		return nil, fail(ErrInvalid, "File must be a Jupyter notebook (.ipynb file). For editing other file types, use the edit tool.")
	}
	if op.EditMode == "" {
		op.EditMode = "replace"
	}
	if op.EditMode != "replace" && op.EditMode != "insert" && op.EditMode != "delete" {
		return nil, fail(ErrInvalid, "Edit mode must be replace, insert, or delete.")
	}
	if op.CellType != "" && !validCellType(op.CellType) {
		return nil, fail(ErrInvalid, "Cell type must be code or markdown.")
	}
	if op.EditMode == "insert" && op.CellType == "" {
		return nil, fail(ErrInvalid, "Cell type is required when using edit_mode=insert.")
	}
	if op.EditMode != "insert" && op.CellID == "" {
		return nil, fail(ErrInvalid, "Cell ID must be specified when not inserting a new cell.")
	}
	rel, err = f.target(rel)
	if err != nil {
		return nil, err
	}
	data, _, err := f.readAll(rel)
	if err != nil {
		return nil, err
	}
	nb, err := parseNotebook(data)
	if err != nil {
		return nil, err
	}
	res := &NotebookEditResult{EditMode: op.EditMode, CellID: op.CellID}
	idx := -1
	if op.CellID != "" {
		if idx = nb.find(op.CellID); idx < 0 {
			return nil, fail(ErrNotFound, "Cell with ID %q not found in notebook.", op.CellID)
		}
	}
	switch op.EditMode {
	case "replace":
		c := nb.cells[idx]
		c["source"] = sourceRaw(op.NewSource)
		typ := nb.str(idx, "cell_type")
		if op.CellType != "" {
			typ = op.CellType
		}
		c["cell_type"], _ = jsonRaw(typ)
		if typ == "code" {
			c["execution_count"] = json.RawMessage("null")
			c["outputs"] = json.RawMessage("[]")
		} else {
			delete(c, "execution_count")
			delete(c, "outputs")
		}
		res.CellType = typ
	case "insert":
		c := map[string]json.RawMessage{
			"cell_type": mustRaw(op.CellType),
			"metadata":  json.RawMessage("{}"),
			"source":    sourceRaw(op.NewSource),
		}
		if op.CellType == "code" {
			c["execution_count"] = json.RawMessage("null")
			c["outputs"] = json.RawMessage("[]")
		}
		if nb.minor >= 5 {
			var b [4]byte
			_, _ = rand.Read(b[:])
			c["id"] = mustRaw(hex.EncodeToString(b[:]))
			res.CellID = hex.EncodeToString(b[:])
		}
		at := idx + 1
		nb.cells = append(nb.cells, nil)
		copy(nb.cells[at+1:], nb.cells[at:])
		nb.cells[at] = c
		idx = at
		if nb.minor < 5 {
			res.CellID = "cell-" + strconv.Itoa(idx)
		}
		res.CellType = op.CellType
	case "delete":
		res.CellType = nb.str(idx, "cell_type")
		nb.cells = append(nb.cells[:idx], nb.cells[idx+1:]...)
	}
	out, err := nb.marshal()
	if err != nil {
		return nil, err
	}
	w, err := f.put(rel, out)
	if err != nil {
		return nil, err
	}
	res.Path, res.CellIndex, res.Cells, res.Size, res.SHA256 = w.Path, idx, len(nb.cells), w.Size, w.SHA256
	return res, nil
}

func mustRaw(v any) json.RawMessage {
	raw, _ := jsonRaw(v)
	return raw
}
