package gui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
)

func TestVendoredAssets(t *testing.T) {
	fsys := StaticFS()
	if err := CheckVendored(fsys); err != nil {
		t.Fatal(err)
	}
	// htmx and both font families must be present, each with a licence file.
	for _, p := range []string{"vendor/htmx.min.js", "fonts/Oswald-latin-var.woff2",
		"fonts/JetBrainsMono-400.woff2", "fonts/JetBrainsMono-600.woff2", "fonts/JetBrainsMono-700.woff2",
		"LICENSES/htmx-0BSD.txt", "LICENSES/Oswald-OFL.txt", "LICENSES/JetBrainsMono-OFL.txt"} {
		if _, err := fs.Stat(fsys, p); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
	}
}

func TestVendoredAssetsDetectTampering(t *testing.T) {
	dir := t.TempDir()
	if err := os.CopyFS(dir, StaticFS()); err != nil {
		t.Fatal(err)
	}
	fsys := os.DirFS(dir)
	if err := CheckVendored(fsys); err != nil {
		t.Fatal(err)
	}
	// Unlisted vendored file.
	extra := filepath.Join(dir, "vendor", "extra.js")
	if err := os.WriteFile(extra, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckVendored(fsys); err == nil || !strings.Contains(err.Error(), "not listed") {
		t.Errorf("unlisted file: %v", err)
	}
	os.Remove(extra)
	// Modified file.
	htmx := filepath.Join(dir, "vendor", "htmx.min.js")
	if err := os.WriteFile(htmx, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckVendored(fsys); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Errorf("tampered file: %v", err)
	}
	if _, err := NewAssets(fsys); err == nil {
		t.Error("NewAssets accepted a tampered vendored file")
	}
	// Missing licence.
	orig, _ := fs.ReadFile(StaticFS(), "vendor/htmx.min.js")
	if err := os.WriteFile(htmx, orig, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "LICENSES", "Oswald-OFL.txt")); err != nil {
		t.Fatal(err)
	}
	if err := CheckVendored(fsys); err == nil || !strings.Contains(err.Error(), "licence") {
		t.Errorf("missing licence: %v", err)
	}
}

func TestAssetsServe(t *testing.T) {
	a, err := NewAssets(StaticFS())
	if err != nil {
		t.Fatal(err)
	}
	u, err := a.URL("vendor/htmx.min.js")
	if err != nil || !strings.HasPrefix(u, "/static/"+a.Hash()+"/") {
		t.Fatalf("URL = %q, %v", u, err)
	}
	if v, err := a.URL("htmx.min.js"); err == nil || v != "" {
		t.Fatalf("base name must not resolve: %q, %v", v, err)
	}
	if _, err := a.URL("nope.js"); err == nil {
		t.Fatal("unknown asset must be an error")
	}
	h := a.Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, u, nil))
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") ||
		!strings.HasPrefix(rec.Header().Get("Content-Type"), "text/javascript") || rec.Body.Len() == 0 {
		t.Errorf("serve: %d %v", rec.Code, rec.Header())
	}
	f := httptest.NewRecorder()
	fu, err := a.URL("fonts/Oswald-latin-var.woff2")
	if err != nil {
		t.Fatal(err)
	}
	h.ServeHTTP(f, httptest.NewRequest(http.MethodGet, fu, nil))
	if f.Code != 200 || f.Header().Get("Content-Type") != "font/woff2" {
		t.Errorf("font: %d %v", f.Code, f.Header())
	}
	for _, p := range []string{"/static/deadbeef/vendor/htmx.min.js", "/static/" + a.Hash() + "/ASSETS.md",
		"/static/" + a.Hash() + "/OFL.txt", "/static/" + a.Hash() + "/../x", "/static/" + a.Hash() + "/htmx.min.js",
		"/static/" + a.Hash() + "/LICENSES/htmx-0BSD.txt", "/static/" + a.Hash() + "/fonts/", "/static/"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != 404 {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, u, nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", rec.Code)
	}
}

// Every url(...) in app.css, resolved against the hashed app.css URL, must be served.
func TestCSSURLsResolve(t *testing.T) {
	a, err := NewAssets(StaticFS())
	if err != nil {
		t.Fatal(err)
	}
	css, err := fs.ReadFile(StaticFS(), "app.css")
	if err != nil {
		t.Fatal(err)
	}
	base, err := a.URL("app.css")
	if err != nil {
		t.Fatal(err)
	}
	bu, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	refs := regexp.MustCompile(`url\(\s*["']?([^"')\s]+)["']?\s*\)`).FindAllStringSubmatch(string(css), -1)
	if len(refs) == 0 {
		t.Fatal("no url() in app.css")
	}
	for _, m := range refs {
		ref, err := url.Parse(m[1])
		if err != nil {
			t.Fatal(err)
		}
		u := bu.ResolveReference(ref).String()
		rec := httptest.NewRecorder()
		a.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, u, nil))
		if rec.Code != 200 || rec.Header().Get("Content-Type") != "font/woff2" {
			t.Errorf("%s -> %s: %d %q", m[1], u, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
}

// A manifest row must have exactly 8 cells, with a message that says so.
func TestVendoredAssetsRowNeedsEightCells(t *testing.T) {
	row := "| `vendor/x.js` | x | 1 | https://example.org | MIT | `LICENSES/x.txt` | " + strings.Repeat("a", 64) + " |\n"
	err := CheckVendored(fstest.MapFS{"ASSETS.md": {Data: []byte(row)}})
	if err == nil || !strings.Contains(err.Error(), "has 7 cells, want 8") {
		t.Errorf("short row: %v", err)
	}
}
