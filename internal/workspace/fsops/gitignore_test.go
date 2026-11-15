package fsops

import (
	"slices"
	"testing"
)

func TestWildMatch(t *testing.T) {
	cases := []struct {
		p, s string
		want bool
	}{
		{"*.go", "a.go", true}, {"*.go", "a.gox", false}, {"*", "", true}, {"a*b*c", "aXXbYYc", true},
		{"a*b*c", "aXXbYY", false}, {"?x", "ax", true}, {"?x", "x", false}, {"[abc]x", "bx", true},
		{"[!abc]x", "bx", false}, {"[a-c]x", "dx", false}, {"[^a-c]x", "dx", true}, {`\*x`, "*x", true},
		{`\*x`, "ax", false}, {"[x", "[x", true}, {"[]a]", "]", true}, {"é?", "éz", true}, {"**a", "xa", true},
		{"a**", "abc", true}, {`a\ `, "a ", true},
	}
	for _, c := range cases {
		if got := wildMatch(c.p, c.s); got != c.want {
			t.Errorf("wildMatch(%q,%q) = %v", c.p, c.s, got)
		}
	}
}

func TestSegsMatch(t *testing.T) {
	cases := []struct {
		p, s string
		want bool
	}{
		{"**/*.go", "a.go", true}, {"**/*.go", "x/y/a.go", true}, {"**/*.go", "x/a.txt", false},
		{"a/**/b", "a/b", true}, {"a/**/b", "a/x/y/b", true}, {"a/**", "a", false}, {"a/**", "a/x", true},
		{"a/*", "a/x/y", false}, {"*", "a/b", false}, {"a/b", "a/b", true}, {"**", "x", true},
	}
	for _, c := range cases {
		if got := segsMatch(compileSegs(c.p), splitSlash(c.s)); got != c.want {
			t.Errorf("segsMatch(%q,%q) = %v", c.p, c.s, got)
		}
	}
}

func splitSlash(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '/' {
			out = append(out, cur)
			cur = ""
		} else {
			cur += string(r)
		}
	}
	return append(out, cur)
}

func TestExpandBraces(t *testing.T) {
	got := expandBraces("src/*.{go,{ts,tsx}}")
	want := []string{"src/*.go", "src/*.ts", "src/*.tsx"}
	if !slices.Equal(got, want) {
		t.Errorf("got %q", got)
	}
	if got := expandBraces("a{b"); !slices.Equal(got, []string{"a{b"}) {
		t.Errorf("unbalanced: %q", got)
	}
	if got := expandBraces("{a}"); !slices.Equal(got, []string{"a"}) {
		t.Errorf("single: %q", got)
	}
}

func TestIgnoreRules(t *testing.T) {
	root := ignoreLayer{base: ".", rules: parseIgnore([]byte("*.log\n!keep.log\nbuild/\n/top.txt\ndocs/*.tmp\n# c\n\\#hash\n/a/**/z\nfoo \nbar\\ \n"))}
	sub := ignoreLayer{base: "sub", rules: parseIgnore([]byte("!debug.log\n*.dat\n"))}
	st := ignoreStack{root, sub}
	cases := []struct {
		rel   string
		isDir bool
		want  bool
	}{
		{"x.log", false, true}, {"keep.log", false, false}, {"deep/x.log", false, true},
		{"build", true, true}, {"build", false, false}, {"src/build", true, true},
		{"top.txt", false, true}, {"sub/top.txt", false, false},
		{"docs/a.tmp", false, true}, {"docs/x/a.tmp", false, false},
		{"#hash", false, true}, {"a/b/c/z", false, true}, {"a/z", false, true}, {"b/z", false, false},
		{"foo", false, true}, {"bar ", false, true},
		{"sub/debug.log", false, false}, {"debug.log", false, true},
		{"sub/x.dat", false, true}, {"x.dat", false, false},
	}
	for _, c := range cases {
		if got := st.ignored(c.rel, c.isDir); got != c.want {
			t.Errorf("ignored(%q, dir=%v) = %v", c.rel, c.isDir, got)
		}
	}
}
