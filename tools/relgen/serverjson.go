package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

// serverDoc mirrors server.json field for field, in file order, so a decode/encode round trip
// keeps the layout. Unknown fields are rejected on decode (never silently dropped).
type serverDoc struct {
	Schema      string          `json:"$schema"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Repository  json.RawMessage `json:"repository"`
	Version     string          `json:"version"`
	Packages    []serverPackage `json:"packages"`
}

type serverPackage struct {
	RegistryType     string          `json:"registryType"`
	Identifier       string          `json:"identifier"`
	Version          string          `json:"version"`
	RuntimeHint      string          `json:"runtimeHint,omitempty"`
	Transport        json.RawMessage `json:"transport"`
	PackageArguments json.RawMessage `json:"packageArguments,omitempty"`
}

func init() {
	register(command{
		Name: "serverjson", Usage: "[-check] vX.Y.Z",
		Summary: "set server.json version and npm package version from the tag",
		Run:     runServerJSON,
	})
}

// renderServerJSON decodes b, sets every version to version and re-encodes with two-space indent.
func renderServerJSON(b []byte, version string) ([]byte, error) {
	var doc serverDoc
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	if len(doc.Packages) == 0 {
		return nil, errors.New("no packages")
	}
	doc.Version = version
	for i := range doc.Packages {
		if doc.Packages[i].RegistryType != "npm" {
			return nil, fmt.Errorf("packages[%d]: registryType %q; only npm is published", i, doc.Packages[i].RegistryType)
		}
		doc.Packages[i].Version = version
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	// RawMessage fields keep their source indentation; normalise the whole document.
	var norm bytes.Buffer
	if err := json.Indent(&norm, bytes.TrimSpace(out.Bytes()), "", "  "); err != nil {
		return nil, err
	}
	norm.WriteByte('\n')
	return norm.Bytes(), nil
}

func runServerJSON(e env, args []string) error {
	fs := flag.NewFlagSet("serverjson", flag.ContinueOnError)
	fs.SetOutput(e.Err)
	check := fs.Bool("check", false, "fail instead of rewriting when the file would change")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: relgen serverjson [-check] vX.Y.Z")
	}
	version, err := parseTag(fs.Arg(0))
	if err != nil {
		return err
	}
	path := filepath.Join(e.Root, "server.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	out, err := renderServerJSON(b, version)
	if err != nil {
		return fmt.Errorf("server.json: %v", err)
	}
	if bytes.Equal(out, b) {
		fmt.Fprintf(e.Out, "server.json already at %s\n", version)
		return nil
	}
	if *check {
		return fmt.Errorf("server.json is not at %s", version)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(e.Out, "server.json set to %s\n", version)
	return nil
}
