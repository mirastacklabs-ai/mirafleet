package fsops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func FuzzApplyEdit(f *testing.F) {
	f.Add("a b a", "a", "c", false)
	f.Add("line\nnext\n", "next", "", false)
	f.Add("aaa", "aa", "b", true)
	f.Add("", "x", "y", true)
	f.Add("é\xff", "\xff", "z", false)
	f.Fuzz(func(t *testing.T, content, old, repl string, all bool) {
		out, n, idx, err := applyEdit(content, EditOp{OldString: old, NewString: repl, ReplaceAll: all})
		if err != nil {
			return
		}
		if old == "" || old == repl || n < 1 || idx < 0 || idx > len(content) {
			t.Fatalf("accepted bad edit: n=%d idx=%d", n, idx)
		}
		if !all && n != 1 {
			t.Fatalf("n=%d without replace_all", n)
		}
		if !all && strings.Count(content, old) != 1 {
			t.Fatalf("replaced a non-unique string")
		}
		if all && !strings.Contains(content, old) {
			t.Fatal("replace_all matched nothing")
		}
		if out == content {
			t.Fatal("success without a change")
		}
		if idx <= len(out) {
			_ = snippet(out, idx, repl)
		}
	})
}

func FuzzMultiEditFile(f *testing.F) {
	f.Add("one two three", "one", "1", "two", "2", "zzz", "3")
	f.Add("x", "x", "yy", "yy", "z", "z", "x")
	dir := f.TempDir()
	fs, err := Open(dir, Options{})
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { fs.Close() })
	f.Fuzz(func(t *testing.T, content, o1, n1, o2, n2, o3, n3 string) {
		p := filepath.Join(dir, "f.txt")
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		ops := []EditOp{{OldString: o1, NewString: n1}, {OldString: o2, NewString: n2, ReplaceAll: true}, {OldString: o3, NewString: n3}}
		_, err := fs.MultiEdit("f.txt", ops)
		got, _ := os.ReadFile(p)
		if err != nil && string(got) != content {
			t.Fatalf("failed multi_edit changed the file: %v", err)
		}
		want, _, _, aerr := applyEdits(content, ops, true, 0)
		if (aerr == nil) != (err == nil) && o1 != "" {
			t.Fatalf("file op and pure op disagree: %v / %v", err, aerr)
		}
		if err == nil && o1 != "" && string(got) != want {
			t.Fatal("file content differs from the pure result")
		}
	})
}

func FuzzParseNotebook(f *testing.F) {
	seed, _ := os.ReadFile("testdata/fixtures/demo.ipynb")
	f.Add(seed)
	f.Add([]byte(`{"cells":[{"cell_type":"code","source":"x"}],"nbformat":4}`))
	f.Add([]byte(`{"cells":[null]}`))
	f.Add([]byte(`{"cells":[{"source":5,"outputs":{}}],"nbformat_minor":"x"}`))
	f.Add([]byte(`[]`))
	f.Fuzz(func(t *testing.T, data []byte) {
		nb, err := parseNotebook(data)
		if err != nil {
			return
		}
		nb.render()
		for i := range nb.cells {
			nb.cellID(i)
		}
		nb.find("cell-0")
		out, err := nb.marshal()
		if err != nil {
			// RawMessage may hold JSON the encoder rejects only if the input was invalid.
			t.Fatalf("marshal of parsed notebook failed: %v", err)
		}
		nb2, err := parseNotebook(out)
		if err != nil {
			t.Fatalf("re-parse failed: %v\n%s", err, out)
		}
		out2, _ := nb2.marshal()
		if string(out) != string(out2) {
			t.Fatal("marshal is not stable")
		}
	})
}

func FuzzReadLines(f *testing.F) {
	f.Add("a\nb\r\nc", 0, 0)
	f.Add(strings.Repeat("é", 5000)+"\n", 1, 1)
	dir := f.TempDir()
	fs, err := Open(dir, Options{})
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { fs.Close() })
	f.Fuzz(func(t *testing.T, content string, off, lim int) {
		os.WriteFile(filepath.Join(dir, "r.txt"), []byte(content), 0o644)
		r, err := fs.Read("r.txt", ReadOptions{Offset: off % 50, Limit: lim % 50})
		if err != nil {
			return
		}
		if r.Kind == KindText && r.More && r.NextOffset <= 0 {
			t.Fatal("More without NextOffset")
		}
	})
}
