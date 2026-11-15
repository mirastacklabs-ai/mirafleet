package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// wikiBlobBase is where links to repo files that are not wiki pages are sent.
const wikiBlobBase = "https://github.com/mirastacklabs-ai/mirafleet/blob/main/"

func init() {
	register(command{
		Name: "wiki", Usage: "-out DIR -commit SHA",
		Summary: "render docs/guide and the quickstart into wiki-ready pages (output only)",
		Run:     runWiki,
	})
}

// wikiPage is one generated page.
type wikiPage struct {
	Src   string // repo path, slash-separated
	Name  string // page name without .md
	Title string
	Body  string
}

var (
	wikiChapterRE = regexp.MustCompile(`^[0-9]{2}-[a-z0-9-]+\.md$`)
	wikiDiagramRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*\.svg$`)
	wikiLinkRE    = regexp.MustCompile(`\]\(([^)\s]+)\)`)
	wikiRefDefRE  = regexp.MustCompile(`^(\s{0,3}\[[^\]]+\]:\s*)(\S+)`)
	wikiSchemeRE  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)
	wikiTitleRE   = regexp.MustCompile(`(?m)^# +(.+?)\s*$`)
)

// wikiPageName turns a docs file name into a wiki page name: "01-what-is-mirafleet.md" becomes
// "01-What-Is-Mirafleet".
func wikiPageName(file string) string {
	switch file {
	case "README.md":
		return "Guide"
	case "QUICKSTART.md":
		return "Quickstart"
	}
	parts := strings.Split(strings.TrimSuffix(file, ".md"), "-")
	for i, p := range parts {
		if p != "" {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, "-")
}

// wikiSources lists the repo files that become pages, in sidebar order, plus the diagram names.
func wikiSources(root string) (pages []string, diagrams []string, err error) {
	guide := filepath.Join(root, "docs", "guide")
	ents, err := os.ReadDir(guide)
	if err != nil {
		return nil, nil, err
	}
	var chapters []string
	for _, en := range ents {
		if !en.Type().IsRegular() { // symlinks and dirs are never read
			continue
		}
		switch n := en.Name(); {
		case n == "README.md" || n == "glossary.md":
		case wikiChapterRE.MatchString(n):
			chapters = append(chapters, n)
		}
	}
	sort.Strings(chapters)
	if st, err := os.Lstat(filepath.Join(root, "docs", "guide", "README.md")); err != nil || !st.Mode().IsRegular() {
		return nil, nil, errors.New("docs/guide/README.md is missing")
	}
	if st, err := os.Lstat(filepath.Join(root, "docs", "QUICKSTART.md")); err == nil && st.Mode().IsRegular() {
		pages = append(pages, "docs/QUICKSTART.md")
	}
	pages = append(pages, "docs/guide/README.md")
	for _, c := range chapters {
		pages = append(pages, "docs/guide/"+c)
	}
	if st, err := os.Lstat(filepath.Join(guide, "glossary.md")); err == nil && st.Mode().IsRegular() {
		pages = append(pages, "docs/guide/glossary.md")
	}
	dents, err := os.ReadDir(filepath.Join(guide, "diagrams"))
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, err
	}
	for _, d := range dents {
		if d.Type().IsRegular() && wikiDiagramRE.MatchString(d.Name()) {
			diagrams = append(diagrams, d.Name())
		}
	}
	sort.Strings(diagrams)
	return pages, diagrams, nil
}

// rewriteLinks rewrites relative links in a page. pageOf maps repo paths to wiki page names;
// diagrams is the set of repo paths copied under diagrams/. Anything else that stays inside the
// repo becomes a GitHub blob URL; a link that leaves the repo is an error.
func rewriteLinks(src, body string, pageOf map[string]string, diagrams map[string]bool) (string, error) {
	var firstErr error
	fix := func(target string) string {
		if target == "" || strings.HasPrefix(target, "#") || strings.HasPrefix(target, "/") ||
			strings.HasPrefix(target, "//") || wikiSchemeRE.MatchString(target) {
			return target
		}
		file, frag := target, ""
		if i := strings.IndexByte(target, '#'); i >= 0 {
			file, frag = target[:i], target[i:]
		}
		resolved := path.Clean(path.Join(path.Dir(src), file))
		if resolved == ".." || strings.HasPrefix(resolved, "../") || path.IsAbs(resolved) {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: link %q leaves the repository", src, target)
			}
			return target
		}
		if name, ok := pageOf[resolved]; ok {
			return name + frag
		}
		if diagrams[resolved] {
			return "diagrams/" + path.Base(resolved) + frag
		}
		return wikiBlobBase + resolved + frag
	}
	var out []string
	inFence := false
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimLeft(line, " "), "```") || strings.HasPrefix(strings.TrimLeft(line, " "), "~~~") {
			inFence = !inFence
		}
		if !inFence {
			line = wikiLinkRE.ReplaceAllStringFunc(line, func(m string) string {
				return "](" + fix(wikiLinkRE.FindStringSubmatch(m)[1]) + ")"
			})
			if m := wikiRefDefRE.FindStringSubmatch(line); m != nil {
				line = m[1] + fix(m[2]) + line[len(m[0]):]
			}
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n"), firstErr
}

// buildWiki renders every page; the result maps output file name to content.
func buildWiki(root, commit string) (map[string][]byte, error) {
	srcs, diagrams, err := wikiSources(root)
	if err != nil {
		return nil, err
	}
	pageOf := map[string]string{}
	seen := map[string]string{}
	for _, s := range srcs {
		n := wikiPageName(path.Base(s))
		if prev, dup := seen[n]; dup {
			return nil, fmt.Errorf("pages %s and %s both map to %q", prev, s, n)
		}
		seen[n] = s
		pageOf[s] = n
	}
	dset := map[string]bool{}
	for _, d := range diagrams {
		dset["docs/guide/diagrams/"+d] = true
	}
	banner := "> Generated from `docs/` at " + commit + ". Edit the source in the repository, not this page.\n\n"
	files := map[string][]byte{}
	var pages []wikiPage
	for _, s := range srcs {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(s)))
		if err != nil {
			return nil, err
		}
		text := strings.ReplaceAll(string(b), "\r\n", "\n")
		title := pageOf[s]
		if m := wikiTitleRE.FindStringSubmatch(text); m != nil {
			title = m[1]
		}
		body, err := rewriteLinks(s, text, pageOf, dset)
		if err != nil {
			return nil, err
		}
		pages = append(pages, wikiPage{Src: s, Name: pageOf[s], Title: title})
		files[pageOf[s]+".md"] = []byte(banner + body)
	}
	for _, d := range diagrams {
		b, err := os.ReadFile(filepath.Join(root, "docs", "guide", "diagrams", d))
		if err != nil {
			return nil, err
		}
		files["diagrams/"+d] = b
	}
	var list strings.Builder
	for _, p := range pages {
		fmt.Fprintf(&list, "- [%s](%s)\n", p.Title, p.Name)
	}
	files["Home.md"] = []byte("# mirafleet\n\nClaude Code orchestrates fleets of servers through a central server.\n\n" +
		list.String() + "\nSource and issues: " + strings.TrimSuffix(wikiBlobBase, "blob/main/") + "\n\n" +
		"_Generated from `docs/` at " + commit + "._\n")
	files["_Sidebar.md"] = []byte("**[Home](Home)**\n\n" + list.String())
	return files, nil
}

func runWiki(e env, args []string) error {
	fs := flag.NewFlagSet("wiki", flag.ContinueOnError)
	fs.SetOutput(e.Err)
	out := fs.String("out", "", "output directory (must be absent or empty)")
	commit := fs.String("commit", "", "commit id for the banner")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" || *commit == "" || fs.NArg() != 0 {
		return errors.New("usage: relgen wiki -out DIR -commit SHA")
	}
	if !regexp.MustCompile(`^[0-9a-zA-Z._-]{1,64}$`).MatchString(*commit) {
		return fmt.Errorf("bad -commit %q", *commit)
	}
	if ents, err := os.ReadDir(*out); err == nil && len(ents) > 0 {
		return fmt.Errorf("%s is not empty", *out)
	}
	files, err := buildWiki(e.Root, *commit)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		// n is built only from page names (letters, digits, '-') or validated diagram names.
		dst := filepath.Join(*out, filepath.FromSlash(n))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, files[n], 0o644); err != nil {
			return err
		}
	}
	fmt.Fprintf(e.Out, "wrote %d files to %s\n", len(names), *out)
	return nil
}
