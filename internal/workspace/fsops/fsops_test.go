package fsops

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

// newFS copies the fixtures into a fresh temp dir and roots an FS there.
func newFS(t *testing.T) (*FS, string) {
	t.Helper()
	dir := t.TempDir()
	ents, err := os.ReadDir("testdata/fixtures")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join("testdata/fixtures", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, e.Name()), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	real, _ := filepath.EvalSymlinks(dir)
	return f, real
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	p := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.WriteFile(p, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != got {
		t.Errorf("%s mismatch\n--- want\n%s\n--- got\n%s", name, want, got)
	}
}

func mustNot(t *testing.T, err error, kind error, contains string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want error containing %q, got nil", contains)
	}
	if kind != nil && !errors.Is(err, kind) {
		t.Errorf("error %v is not %v", err, kind)
	}
	if !strings.Contains(err.Error(), contains) {
		t.Errorf("error %q does not contain %q", err, contains)
	}
}

func TestReadText(t *testing.T) {
	f, _ := newFS(t)
	r, err := f.Read("notes.txt", ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind != KindText || r.TotalLines != 6 || r.More {
		t.Errorf("result %+v", r)
	}
	golden(t, "read_notes", r.Text)

	r, _ = f.Read("notes.txt", ReadOptions{Offset: 2, Limit: 2})
	if !r.More || r.NextOffset != 4 || r.StartLine != 2 || r.EndLine != 3 {
		t.Errorf("range result %+v", r)
	}
	golden(t, "read_notes_range", r.Text)

	r, _ = f.Read("notes.txt", ReadOptions{Offset: 99})
	if !strings.Contains(r.Text, "shorter than the provided offset (99). The file has 6 lines.") {
		t.Errorf("offset warning: %q", r.Text)
	}
	mustNot(t, func() error { _, err := f.Read("notes.txt", ReadOptions{Offset: -1}); return err }(), ErrInvalid, "negative")
}

func TestReadEmptyAndLongLine(t *testing.T) {
	f, dir := newFS(t)
	os.WriteFile(filepath.Join(dir, "empty"), nil, 0o644)
	r, _ := f.Read("empty", ReadOptions{})
	if r.Text != emptyWarning {
		t.Errorf("empty: %q", r.Text)
	}
	long := strings.Repeat("é", 3000)
	os.WriteFile(filepath.Join(dir, "long"), []byte(long+"\nnext"), 0o644)
	r, _ = f.Read("long", ReadOptions{})
	lines := strings.Split(strings.TrimSuffix(r.Text, "\n"), "\n")
	if len(lines) != 2 || !strings.HasSuffix(lines[0], "[line truncated]") || !strings.HasSuffix(lines[1], "\tnext") {
		t.Errorf("long line: %q", r.Text[:40])
	}
	if got := strings.TrimSuffix(strings.SplitN(lines[0], "\t", 2)[1], " ... [line truncated]"); len(got) > lineClip || !strings.HasPrefix(long, got) {
		t.Errorf("clip not at a rune boundary: %d bytes", len(got))
	}
	// A big file stops at the line limit without reading to the end.
	var sb strings.Builder
	for range 5000 {
		sb.WriteString("x\n")
	}
	os.WriteFile(filepath.Join(dir, "big"), []byte(sb.String()), 0o644)
	r, _ = f.Read("big", ReadOptions{})
	if !r.More || r.NextOffset != 2001 || r.EndLine != 2000 || r.TotalLines != 0 {
		t.Errorf("default limit: %+v", r)
	}
}

func TestReadImageBinary(t *testing.T) {
	f, dir := newFS(t)
	var buf bytes.Buffer
	png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2)))
	os.WriteFile(filepath.Join(dir, "p.png"), buf.Bytes(), 0o644)
	r, err := f.Read("p.png", ReadOptions{})
	if err != nil || r.Kind != KindImage || r.Images[0].MIME != "image/png" {
		t.Fatalf("%+v %v", r, err)
	}
	if b, _ := base64.StdEncoding.DecodeString(r.Images[0].Data); !bytes.Equal(b, buf.Bytes()) {
		t.Error("image bytes differ")
	}
	// A png extension without png content is not served as an image.
	os.WriteFile(filepath.Join(dir, "fake.png"), []byte("just text\n"), 0o644)
	if r, _ := f.Read("fake.png", ReadOptions{}); r.Kind != KindText {
		t.Errorf("fake png kind %s", r.Kind)
	}
	small, _ := Open(dir, Options{MaxImage: 10})
	defer small.Close()
	_, err = small.Read("p.png", ReadOptions{})
	mustNot(t, err, ErrTooLarge, "Image file is too large")

	os.WriteFile(filepath.Join(dir, "bin"), []byte("ab\x00cd\xff\xfe"), 0o644)
	r, _ = f.Read("bin", ReadOptions{})
	if r.Kind != KindBinary || r.Size != 7 || len(r.SHA256) != 64 || !strings.Contains(r.Text, "Binary file") {
		t.Errorf("binary: %+v", r)
	}
	os.WriteFile(filepath.Join(dir, "badutf"), []byte("caf\xe9\n"), 0o644)
	// Latin-1 text is text, converted to UTF-8.
	if r, _ := f.Read("badutf", ReadOptions{}); r.Kind != KindText || !strings.Contains(r.Text, "café") {
		t.Errorf("latin-1 read: %+v", r)
	}
	// A high ratio of control characters is binary.
	os.WriteFile(filepath.Join(dir, "ctl"), []byte("\x01\x02\x03\x04\x05\x06abc\n"), 0o644)
	if r, _ := f.Read("ctl", ReadOptions{}); r.Kind != KindBinary {
		t.Errorf("control-heavy kind %s", r.Kind)
	}
}

func TestReadMixedEncodingLines(t *testing.T) {
	f, dir := newFS(t)
	os.WriteFile(filepath.Join(dir, "mix"), []byte("ok ü\nna\xefve \xe9\n"), 0o644)
	r, _ := f.Read("mix", ReadOptions{})
	if r.Kind != KindText || !strings.Contains(r.Text, "ok ü") || !strings.Contains(r.Text, "naïve é") {
		t.Errorf("mixed: %+v", r)
	}
}

func TestOwnTextHasNoControlTags(t *testing.T) {
	f, dir := newFS(t)
	os.WriteFile(filepath.Join(dir, "empty"), nil, 0o644)
	os.WriteFile(filepath.Join(dir, "two"), []byte("a\nb\n"), 0o644)
	for _, c := range []struct {
		name string
		opt  ReadOptions
	}{{"empty", ReadOptions{}}, {"two", ReadOptions{Offset: 99}}} {
		r, err := f.Read(c.name, c.opt)
		if err != nil {
			t.Fatal(err)
		}
		low := strings.ToLower(r.Text)
		if r.Text == "" || strings.Contains(low, "system-reminder") || strings.Contains(low, "<") {
			t.Errorf("%s: %q", c.name, r.Text)
		}
	}
}

func TestReadNotebookGolden(t *testing.T) {
	f, _ := newFS(t)
	r, err := f.Read("demo.ipynb", ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind != KindNotebook || len(r.Images) != 1 || r.Images[0].MIME != "image/png" {
		t.Errorf("%+v", r)
	}
	golden(t, "read_notebook", r.Text)
}

func TestNotebookEdit(t *testing.T) {
	f, dir := newFS(t)
	orig, _ := os.ReadFile(filepath.Join(dir, "demo.ipynb"))

	// A no-op round trip through the marshaller reproduces nbformat's own layout byte for byte
	// (the fixture is written the nbformat way), apart from the one cell we change.
	nb, _ := parseNotebook(orig)
	out, err := nb.marshal()
	if err != nil || !bytes.Equal(out, orig) {
		t.Fatalf("round trip differs (%v):\n%s", err, out)
	}

	res, err := f.NotebookEdit("demo.ipynb", NotebookEditOp{CellID: "calc", NewSource: "print(1)\nprint(2)"})
	if err != nil || res.CellIndex != 1 || res.CellType != "code" {
		t.Fatalf("%+v %v", res, err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "demo.ipynb"))
	golden(t, "nb_replace.ipynb", string(got))

	// Insert after a cell, by index form; ids are generated for nbformat >= 4.5.
	res, err = f.NotebookEdit("demo.ipynb", NotebookEditOp{CellID: "intro", NewSource: "new md", CellType: "markdown", EditMode: "insert"})
	if err != nil || res.CellIndex != 1 || len(res.CellID) != 8 || res.Cells != 4 {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err = f.NotebookEdit("demo.ipynb", NotebookEditOp{NewSource: "first", CellType: "code", EditMode: "insert"}); err != nil {
		t.Fatal(err)
	}
	if res, err = f.NotebookEdit("demo.ipynb", NotebookEditOp{CellID: "cell-3", EditMode: "delete"}); err != nil || res.Cells != 4 {
		t.Fatalf("%+v %v", res, err)
	}
	r, _ := f.Read("demo.ipynb", ReadOptions{})
	if !strings.Contains(r.Text, "first") || !strings.Contains(r.Text, "new md") || strings.Contains(r.Text, `id="calc"`) {
		t.Errorf("after edits:\n%s", r.Text)
	}

	_, err = f.NotebookEdit("demo.ipynb", NotebookEditOp{CellID: "nope", NewSource: "x"})
	mustNot(t, err, ErrNotFound, `Cell with ID "nope" not found in notebook.`)
	_, err = f.NotebookEdit("demo.ipynb", NotebookEditOp{NewSource: "x", EditMode: "insert"})
	mustNot(t, err, ErrInvalid, "Cell type is required when using edit_mode=insert.")
	_, err = f.NotebookEdit("demo.ipynb", NotebookEditOp{CellID: "intro", EditMode: "wipe"})
	mustNot(t, err, ErrInvalid, "Edit mode must be replace, insert, or delete.")
	_, err = f.NotebookEdit("notes.txt", NotebookEditOp{CellID: "a"})
	mustNot(t, err, ErrInvalid, "must be a Jupyter notebook")
	_, err = f.Edit("demo.ipynb", EditOp{OldString: "a", NewString: "b"})
	mustNot(t, err, ErrInvalid, "Use the notebook_edit tool")
	os.WriteFile(filepath.Join(dir, "bad.ipynb"), []byte("{not json"), 0o644)
	_, err = f.NotebookEdit("bad.ipynb", NotebookEditOp{CellID: "a"})
	mustNot(t, err, ErrInvalid, "not valid JSON")
}

func TestNotebookReplaceChangesTypeAndKeepsUnknownFields(t *testing.T) {
	f, dir := newFS(t)
	_, err := f.NotebookEdit("demo.ipynb", NotebookEditOp{CellID: "calc", NewSource: "text", CellType: "markdown"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "demo.ipynb"))
	var m struct {
		Cells []map[string]json.RawMessage
		Meta  map[string]json.RawMessage `json:"metadata"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	c := m.Cells[1]
	if _, ok := c["outputs"]; ok {
		t.Error("markdown cell kept outputs")
	}
	if !strings.Contains(string(c["metadata"]), "keep") || m.Meta["kernelspec"] == nil {
		t.Error("unknown fields lost")
	}
	if !bytes.Contains(b, []byte("<b>text</b> & more")) {
		t.Error("HTML characters were escaped")
	}
}

func TestWrite(t *testing.T) {
	f, dir := newFS(t)
	r, err := f.Write("sub/dir/new.txt", "hi\n")
	if err != nil || !r.Created || r.Size != 3 || len(r.SHA256) != 64 {
		t.Fatalf("%+v %v", r, err)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "sub/dir/new.txt")); fi.Mode().Perm() != 0o644 {
		t.Errorf("new mode %v", fi.Mode())
	}
	if runtime.GOOS != "windows" {
		os.Chmod(filepath.Join(dir, "sub/dir/new.txt"), 0o750)
		r, err = f.Write(filepath.Join(dir, "sub/dir/new.txt"), "again")
		if err != nil || r.Created {
			t.Fatalf("%+v %v", r, err)
		}
		if fi, _ := os.Stat(filepath.Join(dir, "sub/dir/new.txt")); fi.Mode().Perm() != 0o750 {
			t.Errorf("mode not kept: %v", fi.Mode())
		}
	}
	if ents, _ := os.ReadDir(filepath.Join(dir, "sub/dir")); len(ents) != 1 {
		t.Errorf("temp file left behind: %v", ents)
	}
	small, _ := Open(dir, Options{MaxFile: 4})
	defer small.Close()
	_, err = small.Write("x", "12345")
	mustNot(t, err, ErrTooLarge, "larger than the 4 byte limit")
	if _, err := os.Stat(filepath.Join(dir, "x")); err == nil {
		t.Error("oversized write left a file")
	}
	_, err = f.Write("sub", "x")
	mustNot(t, err, ErrInvalid, "EISDIR")
}

func TestEdit(t *testing.T) {
	f, dir := newFS(t)
	_, err := f.Edit("notes.txt", EditOp{OldString: "alpha", NewString: "A"})
	mustNot(t, err, ErrInvalid, "Found 2 matches of the string to replace, but replace_all is false.")
	_, err = f.Edit("notes.txt", EditOp{OldString: "zzz", NewString: "A"})
	mustNot(t, err, ErrInvalid, "String to replace not found in file.\nString: zzz")
	_, err = f.Edit("notes.txt", EditOp{OldString: "a", NewString: "a"})
	mustNot(t, err, ErrInvalid, "No changes to make: old_string and new_string are exactly the same.")
	_, err = f.Edit("nope.txt", EditOp{OldString: "a", NewString: "b"})
	mustNot(t, err, ErrNotFound, "File does not exist")

	r, err := f.Edit("notes.txt", EditOp{OldString: "gamma", NewString: "GAMMA"})
	if err != nil || r.Replacements != 1 {
		t.Fatal(r, err)
	}
	golden(t, "edit_snippet", r.Snippet)
	r, err = f.Edit("notes.txt", EditOp{OldString: "alpha", NewString: "A", ReplaceAll: true})
	if err != nil || r.Replacements != 2 || r.Snippet != "" {
		t.Fatal(r, err)
	}
	// Deleting a line removes its newline too.
	if _, err = f.Edit("notes.txt", EditOp{OldString: "delta", NewString: ""}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "notes.txt"))
	golden(t, "edit_result", string(b))

	// Empty old_string creates a file, but never over content.
	if r, err = f.Edit("made/new.txt", EditOp{NewString: "fresh"}); err != nil || !r.Created {
		t.Fatal(r, err)
	}
	_, err = f.Edit("made/new.txt", EditOp{NewString: "again"})
	mustNot(t, err, ErrInvalid, "Cannot create new file - file already exists.")
}

func TestMultiEditAllOrNothing(t *testing.T) {
	f, dir := newFS(t)
	before, _ := os.ReadFile(filepath.Join(dir, "notes.txt"))
	_, err := f.MultiEdit("notes.txt", []EditOp{
		{OldString: "gamma", NewString: "G"},
		{OldString: "G", NewString: "H"}, // sees the first edit's result
		{OldString: "missing", NewString: "x"},
	})
	mustNot(t, err, ErrInvalid, "Edit 3 failed: String to replace not found in file.")
	after, _ := os.ReadFile(filepath.Join(dir, "notes.txt"))
	if !bytes.Equal(before, after) {
		t.Error("failed multi_edit changed the file")
	}
	r, err := f.MultiEdit("notes.txt", []EditOp{{OldString: "gamma", NewString: "G"}, {OldString: "G", NewString: "H"}, {OldString: "alpha", NewString: "a", ReplaceAll: true}})
	if err != nil || r.Replacements != 4 {
		t.Fatal(r, err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "notes.txt"))
	if !strings.Contains(string(b), "\nH\n") || strings.Contains(string(b), "alpha") {
		t.Errorf("%q", b)
	}
	// Creating through the first edit, then editing.
	if _, err = f.MultiEdit("c.txt", []EditOp{{NewString: "one two"}, {OldString: "two", NewString: "2"}}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "c.txt")); string(b) != "one 2" {
		t.Errorf("%q", b)
	}
	_, err = f.MultiEdit("c.txt", nil)
	mustNot(t, err, ErrInvalid, "edits must not be empty")
}

func TestLSAndStat(t *testing.T) {
	f, dir := newFS(t)
	os.MkdirAll(filepath.Join(dir, "src/inner"), 0o755)
	os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	os.WriteFile(filepath.Join(dir, "src/inner/a.go"), nil, 0o644)
	os.WriteFile(filepath.Join(dir, "src/b.log"), nil, 0o644)
	os.WriteFile(filepath.Join(dir, ".hidden"), nil, 0o644)
	r, err := f.LS(".", []string{"*.log"})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "ls", strings.ReplaceAll(r.Text, dir, "<root>"))
	_, err = f.LS("notes.txt", nil)
	mustNot(t, err, ErrInvalid, "is not a directory")
	_, err = f.LS("../x-does-not-exist", nil)
	mustNot(t, err, ErrNotFound, "does not exist")

	in, err := f.Stat("src")
	if err != nil || in.Type != "dir" {
		t.Fatal(in, err)
	}
	in, _ = f.Stat("notes.txt")
	if in.Type != "file" || in.Size != 31 {
		t.Errorf("%+v", in)
	}
	_, err = f.Stat("nope")
	mustNot(t, err, ErrNotFound, "does not exist")
}

func TestLSTruncates(t *testing.T) {
	f, dir := newFS(t)
	os.Mkdir(filepath.Join(dir, "many"), 0o755)
	for i := range maxLSEntries + 5 {
		os.WriteFile(filepath.Join(dir, "many", strings.Repeat("f", 1)+string(rune('a'+i%26))+string(rune('a'+i/26%26))+string(rune('a'+i/676))), nil, 0o644)
	}
	r, _ := f.LS("many", nil)
	if !r.Truncated || r.Count != maxLSEntries {
		t.Errorf("%d %v", r.Count, r.Truncated)
	}
}

// The tools reach everything the OS user can (docs/M7.md §3.7): absolute paths anywhere, "..",
// and symlinks that leave the start directory. Special files stay refused.
func TestReachesOutside(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	f, dir := newFS(t)
	outside := t.TempDir()
	sec := filepath.Join(outside, "secret")
	os.WriteFile(sec, []byte("s3cret"), 0o644)
	os.Symlink(outside, filepath.Join(dir, "linkdir"))
	os.Symlink(sec, filepath.Join(dir, "linkfile"))
	os.Symlink("../"+filepath.Base(outside)+"/secret", filepath.Join(dir, "rel"))
	for _, p := range []string{"linkdir/secret", "linkfile", "rel", sec} {
		if r, err := f.Read(p, ReadOptions{}); err != nil || !strings.Contains(r.Text, "s3cret") {
			t.Errorf("read %s: %v %v", p, r, err)
		}
	}
	if _, err := f.Edit(sec, EditOp{OldString: "s3cret", NewString: "t"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(sec); string(b) != "t" {
		t.Errorf("edit by absolute path: %q", b)
	}
	if _, err := f.Write("linkfile", "via link"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(sec); string(b) != "via link" {
		t.Errorf("%q", b)
	}
	if fi, _ := os.Lstat(filepath.Join(dir, "linkfile")); fi.Mode()&os.ModeSymlink == 0 {
		t.Error("link was replaced")
	}
	if _, err := f.Write(filepath.Join(outside, "new", "x.txt"), "x"); err != nil {
		t.Error(err)
	}
	if _, err := f.Read("/dev/null", ReadOptions{}); !errors.Is(err, ErrRefused) {
		t.Errorf("device: %v", err)
	}
}

func TestSymlinkInside(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	f, dir := newFS(t)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), []byte("s3cret"), 0o644)
	os.Symlink(outside, filepath.Join(dir, "linkdir"))
	os.Symlink(filepath.Join(outside, "secret"), filepath.Join(dir, "linkfile"))
	os.Symlink("../"+filepath.Base(outside)+"/secret", filepath.Join(dir, "rel"))
	os.Symlink("notes.txt", filepath.Join(dir, "inside"))

	_ = outside
	// A symlink inside the root works and a write goes through it, keeping the link.
	if r, err := f.Read("inside", ReadOptions{Limit: 1}); err != nil || !strings.Contains(r.Text, "alpha") {
		t.Errorf("%v %v", r, err)
	}
	if _, err := f.Write("inside", "via link"); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(filepath.Join(dir, "inside")); fi.Mode()&os.ModeSymlink == 0 {
		t.Error("link was replaced")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "notes.txt")); string(b) != "via link" {
		t.Errorf("%q", b)
	}
	// Absolute paths under the root (given or resolved) are accepted.
	if _, err := f.Read(filepath.Join(dir, "notes.txt"), ReadOptions{}); err != nil {
		t.Error(err)
	}
}
