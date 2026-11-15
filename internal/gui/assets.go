package gui

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"
)

// staticFS holds the embedded static tree: app.css, app.js, vendor/, fonts/, LICENSES/, ASSETS.md.
//
//go:embed all:static
var staticFS embed.FS

// StaticFS returns the embedded static tree rooted at static/.
func StaticFS() fs.FS {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err) // the embed pattern guarantees the directory
	}
	return sub
}

// servedExt lists the file types served under /static/; each has a fixed content type.
var servedExt = map[string]string{
	".css":   "text/css; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".woff2": "font/woff2",
	".svg":   "image/svg+xml",
}

// vendoredDirs hold third-party files: each must be listed in ASSETS.md with a matching
// SHA-256 and an existing licence file.
var vendoredDirs = []string{"vendor", "fonts"}

// Assets is the set of embedded static files, addressed as /static/<hash>/<path>, where path is
// relative to static/ ("app.css", "vendor/htmx.min.js", "fonts/X.woff2"). The hash covers every
// served file, so any change gives new URLs and caches never go stale; it also lets app.css
// refer to fonts by relative URL.
type Assets struct {
	hash  string
	files map[string]asset // by path relative to static/
}

type asset struct {
	data []byte
	ctyp string
}

// NewAssets reads the static tree from fsys (rooted at static/), checks the vendored files
// against ASSETS.md and their licences, and computes the content hash. Served files are those
// with a servedExt extension outside LICENSES/ (ASSETS.md is never served).
func NewAssets(fsys fs.FS) (*Assets, error) {
	if err := CheckVendored(fsys); err != nil {
		return nil, err
	}
	a := &Assets{files: map[string]asset{}}
	var paths []string
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if strings.HasPrefix(p, "LICENSES/") || servedExt[path.Ext(p)] == "" {
			return nil
		}
		paths = append(paths, p)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	h := sha256.New()
	for _, p := range paths {
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return nil, err
		}
		a.files[p] = asset{data: data, ctyp: servedExt[path.Ext(p)]}
		fmt.Fprintf(h, "%s\x00%d\x00", p, len(data))
		h.Write(data)
	}
	a.hash = hex.EncodeToString(h.Sum(nil))[:16]
	return a, nil
}

// URL returns the hashed URL for a path under static/ such as "app.css" or
// "vendor/htmx.min.js". An unknown name is an error, never an empty URL.
func (a *Assets) URL(name string) (string, error) {
	if _, ok := a.files[name]; !ok {
		return "", fmt.Errorf("gui: unknown static asset %q", name)
	}
	return "/static/" + a.hash + "/" + name, nil
}

// TemplateFuncs returns the template functions the layout needs. "asset" is URL: an unknown
// name makes ExecuteTemplate fail instead of rendering an empty src/href.
func (a *Assets) TemplateFuncs() template.FuncMap {
	return template.FuncMap{"asset": a.URL}
}

// Hash returns the content hash used in static URLs.
func (a *Assets) Hash() string { return a.hash }

// Handler serves GET/HEAD /static/<hash>/<path> with immutable caching (exact lookup of path
// in the file map). A wrong hash or unknown path is a 404. Security headers are the middleware's job.
func (a *Assets) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		rest, ok := strings.CutPrefix(r.URL.Path, "/static/")
		hash, name, ok2 := strings.Cut(rest, "/")
		f, found := a.files[name]
		if !ok || !ok2 || hash != a.hash || !found {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", f.ctyp)
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(f.data))
	})
}

// CheckVendored verifies every file under vendor/ and fonts/ is listed in ASSETS.md with the
// recorded SHA-256, that every ASSETS.md row has a file, and that each row's licence file exists.
func CheckVendored(fsys fs.FS) error {
	manifest, err := fs.ReadFile(fsys, "ASSETS.md")
	if err != nil {
		return fmt.Errorf("gui: ASSETS.md: %w", err)
	}
	type row struct{ sum, lic string }
	rows := map[string]row{}
	for _, line := range strings.Split(string(manifest), "\n") {
		cells := strings.Split(line, "|")
		if len(cells) < 4 {
			continue
		}
		cells = cells[1 : len(cells)-1]
		file := strings.Trim(strings.TrimSpace(cells[0]), "`")
		sum := strings.TrimSpace(cells[len(cells)-1])
		if !isHex64(sum) {
			continue // header, separator or prose
		}
		if len(cells) != 8 {
			return fmt.Errorf("gui: ASSETS.md row %q has %d cells, want 8 (File, Component, Version, Source URL, Licence, Licence file, Form, SHA-256)", file, len(cells))
		}
		rows[file] = row{sum: sum, lic: strings.Trim(strings.TrimSpace(cells[5]), "`")}
	}
	seen := map[string]bool{}
	for _, dir := range vendoredDirs {
		err := fs.WalkDir(fsys, dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			r, ok := rows[p]
			if !ok {
				return fmt.Errorf("gui: vendored file %s is not listed in ASSETS.md", p)
			}
			seen[p] = true
			data, err := fs.ReadFile(fsys, p)
			if err != nil {
				return err
			}
			if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != r.sum {
				return fmt.Errorf("gui: %s does not match its SHA-256 in ASSETS.md", p)
			}
			if !strings.HasPrefix(r.lic, "LICENSES/") {
				return fmt.Errorf("gui: %s has no licence file in ASSETS.md", p)
			}
			if lic, err := fs.ReadFile(fsys, r.lic); err != nil || len(bytes.TrimSpace(lic)) == 0 {
				return fmt.Errorf("gui: licence file %s for %s is missing or empty", r.lic, p)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	for p := range rows {
		if !seen[p] {
			return fmt.Errorf("gui: ASSETS.md lists %s but the file is not embedded", p)
		}
	}
	return nil
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func init() {
	// Make sure the content types we set are what the stdlib would pick; woff2 is not in every table.
	_ = mime.AddExtensionType(".woff2", "font/woff2")
}
