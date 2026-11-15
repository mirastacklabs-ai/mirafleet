package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// npm generates the seven npm packages (docs/M6.md section 2.9) from the release archives:
// the launcher @mirastacklabs/mirafleet and one platform package per archive. Every archive is
// verified against checksums.txt before anything is read from it. Nothing is published.

const (
	npmScope     = "@mirastacklabs"
	npmWrapper   = "@mirastacklabs/mirafleet"
	npmMaxFile   = 512 << 20 // per extracted file
	npmWrapperTD = "npm/mirafleet"
	npmPlatTD    = "npm/platform"
)

// npmPlatform is one release target: goreleaser os/arch and the npm os/cpu names.
type npmPlatform struct{ GoOS, GoArch, OS, CPU string }

func (p npmPlatform) Name() string { return npmScope + "/mirafleet-" + p.OS + "-" + p.CPU }
func (p npmPlatform) Archive(version string) string {
	ext := ".tar.gz"
	if p.GoOS == "windows" {
		ext = ".zip"
	}
	return "mirafleet_" + version + "_" + p.GoOS + "_" + p.GoArch + ext
}
func (p npmPlatform) Binary() string {
	if p.GoOS == "windows" {
		return "mirafleet.exe"
	}
	return "mirafleet"
}

var npmPlatforms = []npmPlatform{
	{"linux", "amd64", "linux", "x64"},
	{"linux", "arm64", "linux", "arm64"},
	{"darwin", "amd64", "darwin", "x64"},
	{"darwin", "arm64", "darwin", "arm64"},
	{"windows", "amd64", "win32", "x64"},
	{"windows", "arm64", "win32", "arm64"},
}

// npmPackage is package.json. Unknown fields in a template are an error, so a typo cannot slip by.
type npmPackage struct {
	Name                 string            `json:"name"`
	Version              string            `json:"version"`
	Description          string            `json:"description,omitempty"`
	License              string            `json:"license,omitempty"`
	Repository           *npmRepo          `json:"repository,omitempty"`
	Homepage             string            `json:"homepage,omitempty"`
	Bugs                 *npmBugs          `json:"bugs,omitempty"`
	Keywords             []string          `json:"keywords,omitempty"`
	MCPName              string            `json:"mcpName,omitempty"`
	Bin                  map[string]string `json:"bin,omitempty"`
	Files                []string          `json:"files,omitempty"`
	OS                   []string          `json:"os,omitempty"`
	CPU                  []string          `json:"cpu,omitempty"`
	Engines              map[string]string `json:"engines,omitempty"`
	OptionalDependencies map[string]string `json:"optionalDependencies,omitempty"`
	PublishConfig        map[string]any    `json:"publishConfig,omitempty"`
	// Scripts and dependencies are declared only to be rejected: a template must carry neither.
	Scripts      map[string]string `json:"scripts,omitempty"`
	Dependencies map[string]string `json:"dependencies,omitempty"`
}

type npmRepo struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

type npmBugs struct {
	URL string `json:"url"`
}

func init() {
	register(command{
		Name: "npm", Usage: "[-dist DIR] [-out DIR] X.Y.Z",
		Summary: "generate the 7 npm packages from the release archives (no publish)",
		Run:     runNPM,
	})
}

func runNPM(e env, args []string) error {
	fs := flag.NewFlagSet("npm", flag.ContinueOnError)
	fs.SetOutput(e.Err)
	dist := fs.String("dist", "dist", "directory with the release archives and checksums.txt")
	out := fs.String("out", "dist/npm", "output directory (must be absent or empty)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: relgen npm [-dist DIR] [-out DIR] X.Y.Z")
	}
	version := strings.TrimPrefix(fs.Arg(0), "v")
	if !semverRE.MatchString(version) {
		return fmt.Errorf("%q is not a version (want X.Y.Z or X.Y.Z-rc.N)", fs.Arg(0))
	}
	rel := func(p string) string {
		if filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(e.Root, p)
	}
	dirs, err := genNPM(e.Root, rel(*dist), rel(*out), version)
	if err != nil {
		return err
	}
	for _, d := range dirs {
		fmt.Fprintln(e.Out, d)
	}
	return nil
}

// genNPM writes the seven package directories under out and returns their paths, wrapper first.
func genNPM(root, dist, out, version string) ([]string, error) {
	sums, err := readChecksums(filepath.Join(dist, "checksums.txt"))
	if err != nil {
		return nil, err
	}
	// Templates first, so a bad template fails before any archive work.
	wrapper, err := loadNPMTemplate(filepath.Join(root, filepath.FromSlash(npmWrapperTD), "package.json"))
	if err != nil {
		return nil, err
	}
	plat, err := loadNPMTemplate(filepath.Join(root, filepath.FromSlash(npmPlatTD), "package.json"))
	if err != nil {
		return nil, err
	}
	if wrapper.Name != npmWrapper {
		return nil, fmt.Errorf("wrapper template name is %q, want %q", wrapper.Name, npmWrapper)
	}
	if err := checkServerJSON(root, wrapper); err != nil {
		return nil, err
	}
	if ents, err := os.ReadDir(out); err == nil && len(ents) > 0 {
		return nil, fmt.Errorf("output directory %s is not empty", out)
	}

	// Verify and read everything into memory before writing anything.
	type built struct {
		p     npmPlatform
		files map[string][]byte // archive base name -> content
	}
	var all []built
	for _, p := range npmPlatforms {
		name := p.Archive(version)
		want, ok := sums[name]
		if !ok {
			return nil, fmt.Errorf("%s is not listed in checksums.txt", name)
		}
		data, err := os.ReadFile(filepath.Join(dist, name))
		if err != nil {
			return nil, err
		}
		if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != want {
			return nil, fmt.Errorf("%s: checksum mismatch", name)
		}
		files, err := extractWanted(name, data, []string{p.Binary(), "LICENSE", "NOTICE", "THIRD_PARTY_LICENSES.txt"})
		if err != nil {
			return nil, err
		}
		for _, req := range []string{p.Binary(), "LICENSE"} {
			if _, ok := files[req]; !ok {
				return nil, fmt.Errorf("%s: no %s in the archive", name, req)
			}
		}
		all = append(all, built{p, files})
	}

	wrapperDir := filepath.Join(root, filepath.FromSlash(npmWrapperTD))
	launcher, err := os.ReadFile(filepath.Join(wrapperDir, "bin", "mirafleet.js"))
	if err != nil {
		return nil, err
	}
	readme, err := os.ReadFile(filepath.Join(wrapperDir, "README.md"))
	if err != nil {
		return nil, err
	}
	platReadme, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(npmPlatTD), "README.md"))
	if err != nil {
		return nil, err
	}
	rootLicense, err := os.ReadFile(filepath.Join(root, "LICENSE"))
	if err != nil {
		return nil, err
	}
	rootNotice, err := os.ReadFile(filepath.Join(root, "NOTICE"))
	if err != nil {
		return nil, err
	}

	var dirs []string
	// Platform packages.
	opt := map[string]string{}
	for _, b := range all {
		pkg := *plat
		pkg.Name = b.p.Name()
		pkg.Version = version
		pkg.Description = "mirafleet binary for " + b.p.GoOS + "/" + b.p.GoArch
		pkg.OS = []string{b.p.OS}
		pkg.CPU = []string{b.p.CPU}
		opt[pkg.Name] = version
		dir := filepath.Join(out, "mirafleet-"+b.p.OS+"-"+b.p.CPU)
		files := []outFile{
			{"bin/" + b.p.Binary(), b.files[b.p.Binary()], 0o755},
			{"LICENSE", b.files["LICENSE"], 0o644},
			{"README.md", bytes.ReplaceAll(platReadme, []byte("PLATFORM"), []byte(b.p.OS+"-"+b.p.CPU)), 0o644},
		}
		if n, ok := b.files["NOTICE"]; ok {
			files = append(files, outFile{"NOTICE", n, 0o644})
		}
		if t, ok := b.files["THIRD_PARTY_LICENSES.txt"]; ok {
			files = append(files, outFile{"THIRD_PARTY_LICENSES.txt", t, 0o644})
		}
		if err := writePackage(dir, &pkg, files); err != nil {
			return nil, err
		}
		dirs = append(dirs, dir)
	}
	// Wrapper.
	w := *wrapper
	w.Version = version
	w.OptionalDependencies = opt
	wdir := filepath.Join(out, "mirafleet")
	if err := writePackage(wdir, &w, []outFile{
		{"bin/mirafleet.js", launcher, 0o755},
		{"README.md", readme, 0o644},
		{"LICENSE", rootLicense, 0o644},
		{"NOTICE", rootNotice, 0o644},
	}); err != nil {
		return nil, err
	}
	return append([]string{wdir}, dirs...), nil
}

type outFile struct {
	Name string
	Data []byte
	Mode os.FileMode
}

func writePackage(dir string, pkg *npmPackage, files []outFile) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(pkg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), append(b, '\n'), 0o644); err != nil {
		return err
	}
	for _, f := range files {
		p := filepath.Join(dir, filepath.FromSlash(f.Name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, f.Data, f.Mode); err != nil {
			return err
		}
		if err := os.Chmod(p, f.Mode); err != nil { // WriteFile is subject to the umask
			return err
		}
	}
	return nil
}

func loadNPMTemplate(p string) (*npmPackage, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var pkg npmPackage
	if err := dec.Decode(&pkg); err != nil {
		return nil, fmt.Errorf("%s: %v", p, err)
	}
	if pkg.Version != "" {
		return nil, fmt.Errorf("%s: templates carry no version", p)
	}
	if len(pkg.Scripts) > 0 {
		return nil, fmt.Errorf("%s: lifecycle scripts are not allowed", p)
	}
	if len(pkg.Dependencies) > 0 || len(pkg.OptionalDependencies) > 0 {
		return nil, fmt.Errorf("%s: templates carry no dependencies (relgen fills optionalDependencies)", p)
	}
	return &pkg, nil
}

// checkServerJSON ties the wrapper to server.json: same registry name as mcpName, same package.
func checkServerJSON(root string, w *npmPackage) error {
	b, err := os.ReadFile(filepath.Join(root, "server.json"))
	if err != nil {
		return err
	}
	var sj struct {
		Name     string `json:"name"`
		Packages []struct {
			RegistryType string `json:"registryType"`
			Identifier   string `json:"identifier"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(b, &sj); err != nil {
		return fmt.Errorf("server.json: %v", err)
	}
	if sj.Name != w.MCPName {
		return fmt.Errorf("server.json name %q != mcpName %q", sj.Name, w.MCPName)
	}
	for _, p := range sj.Packages {
		if p.RegistryType == "npm" && p.Identifier == w.Name {
			return nil
		}
	}
	return fmt.Errorf("server.json has no npm package %q", w.Name)
}

// readChecksums parses a sha256sum-style file: "<64 hex>  <name>".
func readChecksums(p string) (map[string]string, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 2 || len(f[0]) != 64 {
			return nil, fmt.Errorf("%s:%d: malformed line", p, i+1)
		}
		if _, err := hex.DecodeString(f[0]); err != nil {
			return nil, fmt.Errorf("%s:%d: malformed digest", p, i+1)
		}
		name := strings.TrimPrefix(f[1], "*")
		if _, dup := m[name]; dup {
			return nil, fmt.Errorf("%s: %s listed twice", p, name)
		}
		m[name] = strings.ToLower(f[0])
	}
	return m, nil
}

// safeEntry reports whether an archive path is relative and free of "..". An optional single
// leading directory is allowed; the entry's base name is what callers match.
func safeEntry(name string) bool {
	name = strings.ReplaceAll(name, "\\", "/")
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, ":") {
		return false
	}
	for _, s := range strings.Split(name, "/") {
		if s == ".." {
			return false
		}
	}
	return true
}

// extractWanted returns the regular files whose base name is in want (at most one directory deep).
// A path with "..", an absolute path, a link or device entry fails the whole archive, as does a
// wanted file seen twice. Nothing is written to disk here.
func extractWanted(archive string, data []byte, want []string) (map[string][]byte, error) {
	wanted := map[string]bool{}
	for _, w := range want {
		wanted[w] = true
	}
	out := map[string][]byte{}
	take := func(name string, size int64, r io.Reader) error {
		clean := path.Clean(strings.ReplaceAll(name, "\\", "/"))
		if strings.Count(clean, "/") > 1 {
			return nil
		}
		base := path.Base(clean)
		if !wanted[base] {
			return nil
		}
		if size > npmMaxFile {
			return fmt.Errorf("%s: %s is too large", archive, name)
		}
		if _, dup := out[base]; dup {
			return fmt.Errorf("%s: %s appears twice", archive, base)
		}
		b, err := io.ReadAll(io.LimitReader(r, npmMaxFile+1))
		if err != nil {
			return fmt.Errorf("%s: %v", archive, err)
		}
		if int64(len(b)) > npmMaxFile {
			return fmt.Errorf("%s: %s is too large", archive, name)
		}
		out[base] = b
		return nil
	}
	if strings.HasSuffix(archive, ".zip") {
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, fmt.Errorf("%s: %v", archive, err)
		}
		for _, f := range zr.File {
			if !safeEntry(f.Name) {
				return nil, fmt.Errorf("%s: unsafe entry %q", archive, f.Name)
			}
			if f.FileInfo().IsDir() {
				continue
			}
			if !f.FileInfo().Mode().IsRegular() {
				return nil, fmt.Errorf("%s: %q is not a regular file", archive, f.Name)
			}
			rc, err := f.Open()
			if err != nil {
				return nil, fmt.Errorf("%s: %v", archive, err)
			}
			err = take(f.Name, int64(f.UncompressedSize64), rc)
			rc.Close()
			if err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%s: %v", archive, err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %v", archive, err)
		}
		if !safeEntry(h.Name) {
			return nil, fmt.Errorf("%s: unsafe entry %q", archive, h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg:
			if err := take(h.Name, h.Size, tr); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("%s: %q is not a regular file", archive, h.Name)
		}
	}
	return out, nil
}

// npmPackageNames lists the seven package names, wrapper first (used by tests and docs).
func npmPackageNames() []string {
	n := []string{npmWrapper}
	for _, p := range npmPlatforms {
		n = append(n, p.Name())
	}
	sort.Strings(n[1:])
	return n
}

// npmfixture builds a stand-in release directory for scripts/release/npmtest.sh: the six archives
// and checksums.txt, every archive holding the same given binary (only the native platform's is
// ever executed). The version is the caller's; this is test tooling, never used for a release.
func init() {
	register(command{
		Name: "npmfixture", Usage: "-bin FILE [-dist DIR] X.Y.Z",
		Summary: "write six stand-in release archives and checksums.txt for npmtest",
		Run:     runNPMFixture,
	})
}

func runNPMFixture(e env, args []string) error {
	fs := flag.NewFlagSet("npmfixture", flag.ContinueOnError)
	fs.SetOutput(e.Err)
	bin := fs.String("bin", "", "binary to place in every archive")
	dist := fs.String("dist", "dist", "output directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || *bin == "" {
		return errors.New("usage: relgen npmfixture -bin FILE [-dist DIR] X.Y.Z")
	}
	version := strings.TrimPrefix(fs.Arg(0), "v")
	if !semverRE.MatchString(version) {
		return fmt.Errorf("%q is not a version", fs.Arg(0))
	}
	binData, err := os.ReadFile(*bin)
	if err != nil {
		return err
	}
	out := *dist
	if !filepath.IsAbs(out) {
		out = filepath.Join(e.Root, out)
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	var sums strings.Builder
	for _, p := range npmPlatforms {
		files := []outFile{{p.Binary(), binData, 0o755}}
		for _, n := range []string{"LICENSE", "NOTICE"} {
			b, err := os.ReadFile(filepath.Join(e.Root, n))
			if err != nil {
				return err
			}
			files = append(files, outFile{n, b, 0o644})
		}
		files = append(files, outFile{"THIRD_PARTY_LICENSES.txt", []byte("stand-in third party licences\n"), 0o644})
		var data []byte
		if p.GoOS == "windows" {
			data, err = buildZip(files)
		} else {
			data, err = buildTarGz(files)
		}
		if err != nil {
			return err
		}
		name := p.Archive(version)
		if err := os.WriteFile(filepath.Join(out, name), data, 0o644); err != nil {
			return err
		}
		s := sha256.Sum256(data)
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(s[:]), name)
	}
	return os.WriteFile(filepath.Join(out, "checksums.txt"), []byte(sums.String()), 0o644)
}

func buildTarGz(files []outFile) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f.Name, Mode: int64(f.Mode), Size: int64(len(f.Data)), Typeflag: tar.TypeReg}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(f.Data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func buildZip(files []outFile) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range files {
		w, err := zw.Create(f.Name)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(f.Data); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
