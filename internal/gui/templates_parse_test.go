package gui

import (
	"bytes"
	"html/template"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestS3TemplatesParse parses every screen template set (layout + partials + screen), executes the
// styleguide page, and lints the files for inline script/style/handlers.
func TestS3TemplatesParse(t *testing.T) {
	dir := "templates"
	files, err := filepath.Glob(filepath.Join(dir, "*.html"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no templates: %v", err)
	}
	assets, err := NewAssets(StaticFS())
	if err != nil {
		t.Fatal(err)
	}
	funcs := assets.TemplateFuncs()
	for _, f := range files {
		base := filepath.Base(f)
		if base == "layout.html" || base == "partials.html" {
			continue
		}
		set, err := template.New(base).Funcs(funcs).ParseFiles(filepath.Join(dir, "layout.html"), filepath.Join(dir, "partials.html"), f)
		if err != nil {
			t.Fatalf("%s: %v", base, err)
		}
		if base != "styleguide.html" {
			continue
		}
		data := map[string]any{
			"Title": "Styleguide", "Nav": "overview", "Authed": true, "CSRF": "tok", "Theme": "system",
			"Flash": map[string]any{"Kind": "ok", "Text": "hello"}, "RequestID": "r1", "Version": "dev",
			"Crumbs": []map[string]string{{"Text": "Home", "URL": "/"}, {"Text": "Styleguide", "URL": ""}},
		}
		var b bytes.Buffer
		if err := set.ExecuteTemplate(&b, "layout", data); err != nil {
			t.Fatalf("execute: %v", err)
		}
		out := b.String()
		if !strings.Contains(out, `name="htmx-config"`) || !strings.Contains(out, `aria-current="page"`) {
			t.Errorf("layout output missing htmx-config or aria-current")
		}
		refs := regexp.MustCompile(`(?:src|href)="(/static/[^"]*)"`).FindAllStringSubmatch(out, -1)
		if len(refs) < 3 {
			t.Errorf("expected app.css, htmx and app.js references, got %d", len(refs))
		}
		for _, m := range refs {
			rec := httptest.NewRecorder()
			assets.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, m[1], nil))
			if rec.Code != 200 {
				t.Errorf("%s: %d", m[1], rec.Code)
			}
		}
	}
	// The lint (inline script/style/handlers, htmx attributes) lives in security_test.go (S14).
}
