package main

import (
	"bytes"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// relgen licenses assembles THIRD_PARTY_LICENSES.txt (docs/M6.md section 2.13) from
// `go-licenses save` output directories (one per build target) plus the hand-reviewed texts in
// licenses/extra, in a deterministic order. Stdlib-only.

func init() {
	register(command{
		Name: "licenses", Usage: "[-o FILE] SAVEDIR...",
		Summary: "assemble THIRD_PARTY_LICENSES.txt from go-licenses save output and licenses/extra",
		Run:     runLicenses,
	})
}

const extraDir = "licenses/extra"

// extraEntry is one row of the licenses/extra/README.md table.
type extraEntry struct {
	File, Component, Licence string
}

var extraRowRE = regexp.MustCompile("^\\| `([^`|]+)` \\| ([^|]+) \\| ([^|]+) \\|")

// allowedLicences is the CLAUDE.md permissive allowlist (same list as the go-licenses
// --allowed_licenses flag in scripts/dev.sh and scripts/release.sh).
var allowedLicences = map[string]bool{
	"Apache-2.0": true, "MIT": true, "MIT-0": true, "BSD-2-Clause": true,
	"BSD-3-Clause": true, "ISC": true, "0BSD": true,
}

// checkExtraLicence requires an extras entry's licence to be on the allowlist. OFL-1.1 is the
// one exception and only for font files (the file is named *-OFL.txt, as CLAUDE.md allows
// fonts shipped with their licence text).
func checkExtraLicence(e extraEntry) error {
	if allowedLicences[e.Licence] {
		return nil
	}
	if e.Licence == "OFL-1.1" && strings.HasSuffix(e.File, "-OFL.txt") {
		return nil
	}
	return fmt.Errorf("%s/README.md: %s (%s) has licence %q, which is not on the permissive allowlist (OFL-1.1 only for fonts named *-OFL.txt)", extraDir, e.File, e.Component, e.Licence)
}

// readExtras parses the README table and checks it against the directory: every row names an
// existing non-empty file, and every file other than README.md has a row.
func readExtras(root string) ([]extraEntry, error) {
	dir := filepath.Join(root, filepath.FromSlash(extraDir))
	readme, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		return nil, err
	}
	var out []extraEntry
	seen := map[string]bool{}
	for _, line := range strings.Split(string(readme), "\n") {
		m := extraRowRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		e := extraEntry{File: m[1], Component: strings.TrimSpace(m[2]), Licence: strings.TrimSpace(m[3])}
		if e.File != filepath.Base(e.File) || seen[e.File] {
			return nil, fmt.Errorf("%s/README.md: bad or duplicate file %q", extraDir, e.File)
		}
		if err := checkExtraLicence(e); err != nil {
			return nil, err
		}
		seen[e.File] = true
		b, err := os.ReadFile(filepath.Join(dir, e.File))
		if err != nil {
			return nil, fmt.Errorf("%s/README.md lists %s: %w", extraDir, e.File, err)
		}
		if len(bytes.TrimSpace(b)) == 0 {
			return nil, fmt.Errorf("%s/%s is empty", extraDir, e.File)
		}
		out = append(out, e)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s/README.md has no table rows", extraDir)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, de := range ents {
		if de.Name() != "README.md" && !seen[de.Name()] {
			return nil, fmt.Errorf("%s/%s is not listed in README.md", extraDir, de.Name())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	return out, nil
}

var (
	ignoreRE    = regexp.MustCompile(`--ignore ([A-Za-z0-9._/-]+)`)
	assetLicRE  = regexp.MustCompile("`LICENSES/([^`]+)`")
	modulePathR = regexp.MustCompile(`(?m)^module\s+(\S+)`)
	// Base names go-licenses legitimately saves: LICENSE/LICENCE/COPYING/NOTICE*, with an
	// optional extension (LICENSE.txt, LICENSE.md, NOTICE.txt, ...).
	licenceFileRE = regexp.MustCompile(`(?i)^(?:license|licence|copying|notice[a-z0-9_-]*)(?:\.[a-z0-9]+)?$`)
)

// ignoreSet is the set of modules named by --ignore flags in a script.
func ignoreSet(script string) map[string]bool {
	out := map[string]bool{}
	for _, m := range ignoreRE.FindAllStringSubmatch(script, -1) {
		out[m[1]] = true
	}
	return out
}

// checkExtras ties licenses/extra to the --ignore flags in scripts/dev.sh and to the licence
// files listed in internal/gui/static/ASSETS.md.
func checkExtras(root string) error {
	extras, err := readExtras(root)
	if err != nil {
		return err
	}
	byComponent := map[string]bool{}
	byFile := map[string]bool{}
	for _, e := range extras {
		byComponent[e.Component] = true
		byFile[e.File] = true
	}
	dev, err := os.ReadFile(filepath.Join(root, "scripts", "dev.sh"))
	if err != nil {
		return err
	}
	rel, err := os.ReadFile(filepath.Join(root, "scripts", "release.sh"))
	if err != nil {
		return err
	}
	devSet, relSet := ignoreSet(string(dev)), ignoreSet(string(rel))
	if len(devSet) == 0 {
		return fmt.Errorf("scripts/dev.sh: no --ignore found (the licenses target changed?)")
	}
	for m := range devSet {
		if !relSet[m] {
			return fmt.Errorf("scripts/dev.sh ignores %s but scripts/release.sh does not", m)
		}
	}
	for m := range relSet {
		if !devSet[m] {
			return fmt.Errorf("scripts/release.sh ignores %s but scripts/dev.sh does not", m)
		}
	}
	for m := range devSet {
		if !byComponent[m] {
			return fmt.Errorf("scripts/dev.sh ignores %s but %s/README.md has no entry for it", m, extraDir)
		}
	}
	assets, err := os.ReadFile(filepath.Join(root, "internal", "gui", "static", "ASSETS.md"))
	if err != nil {
		return err
	}
	for _, m := range assetLicRE.FindAllStringSubmatch(string(assets), -1) {
		name := m[1]
		if !byFile[name] {
			return fmt.Errorf("ASSETS.md licence file %s is not in %s", name, extraDir)
		}
		want, err := os.ReadFile(filepath.Join(root, "internal", "gui", "static", "LICENSES", name))
		if err != nil {
			return err
		}
		got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(extraDir), name))
		if err != nil {
			return err
		}
		if !bytes.Equal(want, got) {
			return fmt.Errorf("%s/%s differs from internal/gui/static/LICENSES/%s", extraDir, name, name)
		}
	}
	return nil
}

// collectSaved merges go-licenses save directories into path -> content, keyed by the slash path
// relative to its save dir (module/package path plus the licence file name). Identical files from
// several targets collapse; differing content under one key is an error. Entries under skipPrefix
// (this repository's own module path) are left out.
func collectSaved(dirs []string, skipPrefix string) (map[string][]byte, error) {
	files := map[string][]byte{}
	for _, d := range dirs {
		err := filepath.WalkDir(d, func(p string, de fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if de.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(d, p)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if skipPrefix != "" && (rel == skipPrefix || strings.HasPrefix(rel, skipPrefix+"/")) {
				return nil
			}
			if !licenceFileRE.MatchString(de.Name()) {
				return fmt.Errorf("%s: not a licence file name (LICENSE, LICENCE, COPYING or NOTICE*): go-licenses copied source?", rel)
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if old, ok := files[rel]; ok && !bytes.Equal(old, b) {
				return fmt.Errorf("%s differs between save directories", rel)
			}
			files[rel] = b
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no licence files found in %s", strings.Join(dirs, ", "))
	}
	return files, nil
}

const licensesHeader = `THIRD-PARTY LICENSES

mirafleet is licensed under the Apache License, Version 2.0 (see LICENSE). It links and
embeds the third-party components listed below. Each section gives the component and the
full text of its licence or notice file.
`

const rule = "================================================================================\n"

// assembleLicenses renders the file: saved texts sorted by path, then the extras sorted by file.
func assembleLicenses(saved map[string][]byte, extras []extraEntry, readExtra func(string) ([]byte, error)) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(licensesHeader)
	section := func(title string, body []byte) {
		b.WriteString("\n" + rule + title + "\n" + rule + "\n")
		b.Write(bytes.TrimRight(bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n")), "\n"))
		b.WriteString("\n")
	}
	paths := make([]string, 0, len(saved))
	for p := range saved {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		section(p, saved[p])
	}
	for _, e := range extras {
		body, err := readExtra(e.File)
		if err != nil {
			return nil, err
		}
		section(fmt.Sprintf("%s (%s)", e.Component, e.Licence), body)
	}
	return b.Bytes(), nil
}

func runLicenses(e env, args []string) error {
	fset := flag.NewFlagSet("licenses", flag.ContinueOnError)
	fset.SetOutput(e.Err)
	out := fset.String("o", "", "output file (default: standard output)")
	if err := fset.Parse(args); err != nil {
		return err
	}
	if fset.NArg() == 0 {
		return fmt.Errorf("usage: relgen licenses [-o FILE] SAVEDIR...")
	}
	if err := checkExtras(e.Root); err != nil {
		return err
	}
	extras, err := readExtras(e.Root)
	if err != nil {
		return err
	}
	skip := ""
	if gm, err := os.ReadFile(filepath.Join(e.Root, "go.mod")); err == nil {
		if m := modulePathR.FindSubmatch(gm); m != nil {
			skip = string(m[1])
		}
	}
	saved, err := collectSaved(fset.Args(), skip)
	if err != nil {
		return err
	}
	data, err := assembleLicenses(saved, extras, func(f string) ([]byte, error) {
		return os.ReadFile(filepath.Join(e.Root, filepath.FromSlash(extraDir), f))
	})
	if err != nil {
		return err
	}
	if *out == "" {
		_, err = e.Out.Write(data)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	tmp := *out + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, *out); err != nil {
		os.Remove(tmp)
		return err
	}
	fmt.Fprintf(e.Err, "wrote %s (%d bytes, %d saved files, %d extra)\n", *out, len(data), len(saved), len(extras))
	return nil
}
