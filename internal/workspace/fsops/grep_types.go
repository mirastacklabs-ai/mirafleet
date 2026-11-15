package fsops

import (
	"sort"
	"strings"
)

// grepTypes follows ripgrep's default type table (the commonly used part). Globs match the file
// name, never the directory.
var grepTypes = map[string][]string{
	"asm":        {"*.asm", "*.s", "*.S"},
	"awk":        {"*.awk"},
	"bazel":      {"*.bazel", "*.bzl", "BUILD", "WORKSPACE"},
	"c":          {"*.[chH]", "*.[chH].in", "*.cats"},
	"cmake":      {"*.cmake", "CMakeLists.txt"},
	"cpp":        {"*.[ChH]", "*.[ChH].in", "*.[cC][pP][pP]", "*.[cC][xX][xX]", "*.[cC][cC]", "*.[hH][pP][pP]", "*.[hH][xX][xX]", "*.[hH][hH]", "*.inl", "*.ipp"},
	"cs":         {"*.cs"},
	"csharp":     {"*.cs"},
	"css":        {"*.css", "*.scss"},
	"csv":        {"*.csv"},
	"cuda":       {"*.cu", "*.cuh"},
	"dart":       {"*.dart"},
	"docker":     {"*Dockerfile*"},
	"elixir":     {"*.ex", "*.eex", "*.exs"},
	"erlang":     {"*.erl", "*.hrl"},
	"fish":       {"*.fish"},
	"go":         {"*.go"},
	"gradle":     {"*.gradle"},
	"graphql":    {"*.graphql", "*.graphqls"},
	"groovy":     {"*.groovy", "*.gradle"},
	"h":          {"*.h", "*.hpp"},
	"haskell":    {"*.hs", "*.lhs", "*.cabal"},
	"html":       {"*.ejs", "*.htm", "*.html"},
	"java":       {"*.java", "*.jsp", "*.jspx", "*.properties"},
	"jinja":      {"*.j2", "*.jinja", "*.jinja2"},
	"js":         {"*.js", "*.cjs", "*.mjs", "*.jsx"},
	"json":       {"*.json", "composer.lock"},
	"jsonl":      {"*.jsonl"},
	"julia":      {"*.jl"},
	"kotlin":     {"*.kt", "*.kts"},
	"less":       {"*.less"},
	"lisp":       {"*.el", "*.jl", "*.lisp", "*.lsp", "*.sc", "*.scm"},
	"lock":       {"*.lock", "package-lock.json"},
	"log":        {"*.log"},
	"lua":        {"*.lua"},
	"make":       {"[Gg][Nn][Uu]makefile", "[Mm]akefile", "[Gg][Nn][Uu]makefile.am", "[Mm]akefile.am", "[Gg][Nn][Uu]makefile.in", "[Mm]akefile.in", "*.mk", "*.mak"},
	"markdown":   {"*.markdown", "*.md", "*.mdown", "*.mkdn"},
	"md":         {"*.markdown", "*.md", "*.mdown", "*.mkdn"},
	"nim":        {"*.nim", "*.nimble", "*.nims"},
	"nix":        {"*.nix"},
	"objc":       {"*.h", "*.m"},
	"ocaml":      {"*.ml", "*.mli", "*.mll", "*.mly"},
	"perl":       {"*.perl", "*.pl", "*.PL", "*.plh", "*.plx", "*.pm", "*.xs"},
	"php":        {"*.php", "*.php3", "*.php4", "*.php5", "*.php7", "*.php8", "*.phtml"},
	"protobuf":   {"*.proto"},
	"py":         {"*.py", "*.pyi"},
	"python":     {"*.py", "*.pyi"},
	"r":          {"*.R", "*.r", "*.Rmd", "*.Rnw"},
	"rst":        {"*.rst"},
	"ruby":       {"Gemfile", ".irbrc", "Rakefile", "*.gemspec", "*.rb", "*.rake"},
	"rust":       {"*.rs"},
	"sass":       {"*.sass", "*.scss"},
	"scala":      {"*.scala", "*.sbt"},
	"sh":         {"*.bash", "*.bashrc", "*.csh", "*.cshrc", "*.ksh", "*.kshrc", "*.sh", "*.tcsh", "*.zsh", "*.zshrc", ".bashrc", ".zshrc", ".profile", "*.profile"},
	"sql":        {"*.sql", "*.psql"},
	"svelte":     {"*.svelte"},
	"svg":        {"*.svg"},
	"swift":      {"*.swift"},
	"tf":         {"*.tf", "*.tfvars"},
	"toml":       {"*.toml", "Cargo.lock"},
	"ts":         {"*.ts", "*.tsx", "*.cts", "*.mts"},
	"typescript": {"*.ts", "*.tsx", "*.cts", "*.mts"},
	"txt":        {"*.txt"},
	"vue":        {"*.vue"},
	"xml":        {"*.xml", "*.xml.dist", "*.dtd", "*.xsl", "*.xslt", "*.xsd", "*.wsdl", "*.rng", "*.rss", "*.plist"},
	"yaml":       {"*.yaml", "*.yml"},
	"zig":        {"*.zig"},
}

// GrepTypeNames lists the supported type names, sorted.
func GrepTypeNames() []string {
	out := make([]string, 0, len(grepTypes))
	for k := range grepTypes {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func typeMatcher(name string) ([]wildPat, error) {
	globs, ok := grepTypes[name]
	if !ok {
		return nil, fail(ErrInvalid, "unrecognized file type: %s", name)
	}
	out := make([]wildPat, len(globs))
	for i, g := range globs {
		out[i] = compileWild(g)
	}
	return out, nil
}

// splitGlobs splits a Grep glob argument on whitespace and on commas outside braces.
func splitGlobs(s string) []string {
	var out []string
	var cur strings.Builder
	depth := 0
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case r == '{':
			depth++
		case r == '}' && depth > 0:
			depth--
		case r == ' ' || r == '\t' || r == '\n' || (r == ',' && depth == 0):
			flush()
			continue
		}
		cur.WriteRune(r)
	}
	flush()
	return out
}

// globFilter implements ripgrep's --glob: include globs (a file must match one if any exist) and
// !exclude globs. A glob without "/" matches the file name at any depth; otherwise it is
// anchored at the search directory.
type globFilter struct {
	inc, exc []globRule
}

type globRule struct {
	base bool
	segs []pseg
}

func newGlobFilter(spec string) globFilter {
	var f globFilter
	for _, g := range splitGlobs(spec) {
		neg := strings.HasPrefix(g, "!")
		g = strings.TrimPrefix(g, "!")
		for _, a := range expandBraces(g) {
			a = strings.TrimSuffix(strings.TrimPrefix(a, "/"), "/")
			if a == "" {
				continue
			}
			r := globRule{base: !strings.Contains(a, "/")}
			r.segs = compileSegs(a)
			if neg {
				f.exc = append(f.exc, r)
			} else {
				f.inc = append(f.inc, r)
			}
		}
	}
	return f
}

func (r globRule) match(rel string) bool {
	parts := strings.Split(rel, "/")
	if r.base {
		return segsMatch(r.segs, parts[len(parts)-1:])
	}
	return segsMatch(r.segs, parts)
}

func (f globFilter) allows(rel string) bool {
	for _, r := range f.exc {
		if r.match(rel) {
			return false
		}
	}
	if len(f.inc) == 0 {
		return true
	}
	for _, r := range f.inc {
		if r.match(rel) {
			return true
		}
	}
	return false
}
