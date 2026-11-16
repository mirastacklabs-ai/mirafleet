package integrations

import (
	"bytes"
	"io/fs"
	"path/filepath"
	"testing"
)

// The tests in this package compare file bytes, so every text file under integrations/ must be
// LF. .gitattributes enforces that on checkout; this guard fails loudly if a CRLF slips in (it
// also runs on Windows CI, where a missing .gitattributes would produce CRLF checkouts).
func TestNoCRLFInIntegrations(t *testing.T) {
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !isTextExt(path) {
			return nil
		}
		b, err := readFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(b, []byte("\r\n")) {
			t.Errorf("%s contains CRLF; text files must be LF (see .gitattributes)", filepath.ToSlash(path))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestGitattributesPresent(t *testing.T) {
	b, err := readFile(filepath.Join("..", ".gitattributes"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte("* text=auto eol=lf")) {
		t.Fatal(".gitattributes must set `* text=auto eol=lf`")
	}
	if bytes.Contains(b, []byte("\r\n")) {
		t.Fatal(".gitattributes contains CRLF")
	}
	// Vendored assets are byte-exact (SHA-pinned in ASSETS.md) and must come after the extension
	// rules, because the last matching line wins.
	last := bytes.LastIndex(b, []byte("*.zip binary"))
	for _, rule := range []string{"internal/gui/static/LICENSES/** -text", "internal/gui/static/vendor/** -text", "licenses/extra/** -text"} {
		i := bytes.Index(b, []byte("\n"+rule+"\n"))
		if i < 0 {
			t.Errorf(".gitattributes must contain %q", rule)
		} else if i < last {
			t.Errorf("%q must come after the extension rules (last match wins)", rule)
		}
	}
}

func isTextExt(p string) bool {
	switch filepath.Ext(p) {
	case ".md", ".json", ".go", ".yaml", ".yml", ".txt", ".sh", ".toml":
		return true
	}
	return false
}
