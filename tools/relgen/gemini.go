package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func init() {
	register(command{
		Name: "gemini-check", Usage: "vX.Y.Z DIR",
		Summary: "fail unless every Gemini extension archive in DIR has the right layout and version",
		Run:     runGeminiCheck,
	})
}

// geminiArchives are the per-platform names Gemini CLI looks for (docs: releasing extensions).
// A bare generic name is used only when a release has a single asset, so we ship one per platform.
var geminiArchives = []string{"darwin.mirafleet.tar.gz", "linux.mirafleet.tar.gz", "win32.mirafleet.tar.gz"}

// geminiWant is the exact file list of the archive (gemini-extension.json must be at the root).
var geminiWant = []string{"GEMINI.md", "LICENSE", "commands/mirafleet.toml", "gemini-extension.json"}

func runGeminiCheck(e env, args []string) error {
	if len(args) != 2 {
		return errors.New("usage: relgen gemini-check vX.Y.Z DIR")
	}
	version, err := parseTag(args[0])
	if err != nil {
		return err
	}
	var first []byte
	for _, name := range geminiArchives {
		b, err := geminiManifest(filepath.Join(args[1], name), version)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if first == nil {
			first = b
		} else if string(first) != string(b) {
			return fmt.Errorf("%s: manifest differs from %s", name, geminiArchives[0])
		}
	}
	fmt.Fprintf(e.Out, "ok: %d Gemini extension archives match v%s\n", len(geminiArchives), version)
	return nil
}

// geminiManifest checks one archive's file list and manifest version and returns the manifest.
func geminiManifest(path, version string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(zr)
	var names []string
	var manifest []byte
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		names = append(names, h.Name)
		if h.Name == "gemini-extension.json" {
			if manifest, err = io.ReadAll(io.LimitReader(tr, 1<<20)); err != nil {
				return nil, err
			}
		}
	}
	sort.Strings(names)
	if strings.Join(names, ",") != strings.Join(geminiWant, ",") {
		return nil, fmt.Errorf("files are %v, want %v", names, geminiWant)
	}
	var m struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(manifest, &m); err != nil {
		return nil, fmt.Errorf("gemini-extension.json: %w", err)
	}
	if m.Version != version {
		return nil, fmt.Errorf("gemini-extension.json version %q, want %q", m.Version, version)
	}
	return manifest, nil
}
