package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/mirastacklabs-ai/mirafleet/internal/protover"
)

// A semantic version without the leading "v": X.Y.Z or X.Y.Z-rc.N.
var semverRE = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-rc\.(0|[1-9][0-9]*))?$`)

// target is one versioned file. get returns every version-bearing value found in the file (it must
// find at least one); set rewrites those values in place, touching nothing else. tag says the file
// carries the tag form (vX.Y.Z) rather than the bare version.
type target struct {
	Path  string // slash-separated, relative to the repo root
	Tag   bool
	Level bool // the file carries the protocol level of the version (internal/protover), not the version
	Get   func(b []byte) ([]string, error)
	Set   func(b []byte, value string) ([]byte, error)
}

// targets is the list of versioned files. npm templates carry no version.
// Later steps (compose image tags, ...) append from their own file's init via addTarget.
var targets = []target{
	{Path: "integrations/claude-code/plugin/.claude-plugin/plugin.json", Get: jsonVersions, Set: setJSONVersions},
	{Path: "integrations/claude-code/directory/.claude-plugin/plugin.json", Get: jsonVersions, Set: setJSONVersions},
	{Path: "cmd/mirafleet/mcp_hook.go", Get: goPluginVersion, Set: setGoPluginVersion},
	{Path: ".claude-plugin/marketplace.json", Tag: true, Get: marketplaceRef, Set: setMarketplaceRef},
	{Path: "server.json", Get: jsonVersions, Set: setJSONVersions},
	{Path: "gemini-extension.json", Get: jsonVersions, Set: setJSONVersions},
	{Path: "integrations/cursor/plugin/.cursor-plugin/plugin.json", Get: jsonVersions, Set: setJSONVersions},
	{Path: "integrations/cursor/plugin/mcp.json", Get: cursorPin, Set: setCursorPin},
	{Path: "internal/protover/protover.go", Level: true, Get: protoLevel, Set: setProtoLevel},
}

func addTarget(t target) { targets = append(targets, t) }

func init() {
	register(command{
		Name: "check", Usage: "vX.Y.Z",
		Summary: "fail unless every versioned file matches the tag",
		Run:     runCheck,
	})
}

// parseTag validates "vX.Y.Z[-rc.N]" and returns the bare version.
func parseTag(tag string) (string, error) {
	v := strings.TrimPrefix(tag, "v")
	if v == tag || !semverRE.MatchString(v) {
		return "", fmt.Errorf("%q is not a release tag (want vX.Y.Z or vX.Y.Z-rc.N)", tag)
	}
	return v, nil
}

func (t target) want(version string) string {
	if t.Level {
		// version is already validated by semverRE, so LevelOf cannot fail.
		l, _ := protover.LevelOf(version)
		return strconv.Itoa(l)
	}
	if t.Tag {
		return "v" + version
	}
	return version
}

func runCheck(e env, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: relgen check vX.Y.Z")
	}
	version, err := parseTag(args[0])
	if err != nil {
		return err
	}
	problems := checkTree(e.Root, version)
	if len(problems) > 0 {
		return errors.New("version mismatch:\n  " + strings.Join(problems, "\n  "))
	}
	fmt.Fprintf(e.Out, "ok: %d versioned files match v%s\n", len(targets), version)
	return nil
}

// checkTree returns one problem line per mismatching or unreadable file.
func checkTree(root, version string) []string {
	var problems []string
	for _, t := range targets {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(t.Path)))
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", t.Path, err))
			continue
		}
		got, err := t.Get(b)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", t.Path, err))
			continue
		}
		for _, g := range got {
			if g != t.want(version) {
				problems = append(problems, fmt.Sprintf("%s: found %q, want %q", t.Path, g, t.want(version)))
			}
		}
	}
	return problems
}

// --- extractors and setters ---

// versionKeyRE matches a JSON "version" key and its string value.
var versionKeyRE = regexp.MustCompile(`("version"\s*:\s*")[^"]*(")`)

// jsonVersions returns the top-level "version" and every packages[].version (server.json).
func jsonVersions(b []byte) ([]string, error) {
	var doc struct {
		Version  *string `json:"version"`
		Packages []struct {
			Version *string `json:"version"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	if doc.Version == nil {
		return nil, errors.New(`no top-level "version"`)
	}
	out := []string{*doc.Version}
	for i, p := range doc.Packages {
		if p.Version == nil {
			return nil, fmt.Errorf("packages[%d] has no version", i)
		}
		out = append(out, *p.Version)
	}
	return out, nil
}

// setJSONVersions rewrites every "version" key in place. It refuses when the number of keys differs
// from what jsonVersions sees, so an unexpected nested "version" is never silently skipped or hit.
func setJSONVersions(b []byte, value string) ([]byte, error) {
	got, err := jsonVersions(b)
	if err != nil {
		return nil, err
	}
	if n := len(versionKeyRE.FindAll(b, -1)); n != len(got) {
		return nil, fmt.Errorf(`found %d "version" keys, expected %d`, n, len(got))
	}
	return versionKeyRE.ReplaceAll(b, []byte("${1}"+value+"${2}")), nil
}

var pluginVersionRE = regexp.MustCompile(`(?m)^(const pluginVersion = ")[^"]*(")`)

func goPluginVersion(b []byte) ([]string, error) {
	m := pluginVersionRE.FindAllSubmatch(b, -1)
	if len(m) != 1 {
		return nil, fmt.Errorf("want exactly one `const pluginVersion = \"...\"`, found %d", len(m))
	}
	loc := pluginVersionRE.FindSubmatchIndex(b)
	return []string{string(b[loc[3]:loc[4]])}, nil
}

func setGoPluginVersion(b []byte, value string) ([]byte, error) {
	if _, err := goPluginVersion(b); err != nil {
		return nil, err
	}
	return pluginVersionRE.ReplaceAll(b, []byte("${1}"+value+"${2}")), nil
}

var protoLevelRE = regexp.MustCompile(`(?m)^(const Current = )([0-9]+)$`)

// protoLevel returns the protover.Current constant, which must equal the tag's protocol level.
func protoLevel(b []byte) ([]string, error) {
	m := protoLevelRE.FindAllSubmatch(b, -1)
	if len(m) != 1 {
		return nil, fmt.Errorf("want exactly one `const Current = N`, found %d", len(m))
	}
	return []string{string(m[0][2])}, nil
}

func setProtoLevel(b []byte, value string) ([]byte, error) {
	if _, err := protoLevel(b); err != nil {
		return nil, err
	}
	return protoLevelRE.ReplaceAll(b, []byte("${1}"+value)), nil
}

// pluginName is the marketplace entry whose source ref tracks the release tag.
const pluginName = "mirafleet"

// newerThan060 reports whether the tag form vX.Y.Z[-rc.N] is above 0.6.0 by X.Y.Z (so 0.7.0-rc.1
// counts, 0.6.0 and 0.6.0-rc.N do not).
func newerThan060(tag string) bool {
	m := semverRE.FindStringSubmatch(strings.TrimPrefix(tag, "v"))
	if m == nil {
		return false
	}
	n := func(s string) int { v, _ := strconv.Atoi(s); return v }
	x, y, z := n(m[1]), n(m[2]), n(m[3])
	return x > 0 || y > 6 || (y == 6 && z > 0)
}

// pluginPath is the marketplace source path written by bump, together with the new ref.
const pluginPath = "integrations/claude-code/plugin"

var pathKeyRE = regexp.MustCompile(`("path"\s*:\s*")[^"]*(")`)

var refKeyRE = regexp.MustCompile(`("ref"\s*:\s*")[^"]*(")`)

func marketplaceRef(b []byte) ([]string, error) {
	var doc struct {
		Plugins []struct {
			Name   string          `json:"name"`
			Source json.RawMessage `json:"source"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	for _, p := range doc.Plugins {
		if p.Name != pluginName {
			continue
		}
		var src struct {
			Ref *string `json:"ref"`
		}
		if err := json.Unmarshal(p.Source, &src); err != nil {
			return nil, fmt.Errorf("plugin %q: source must be an object: %v", pluginName, err)
		}
		if src.Ref == nil {
			return nil, fmt.Errorf("plugin %q: source has no ref", pluginName)
		}
		return []string{*src.Ref}, nil
	}
	return nil, fmt.Errorf("no plugin entry named %q", pluginName)
}

func setMarketplaceRef(b []byte, value string) ([]byte, error) {
	if _, err := marketplaceRef(b); err != nil {
		return nil, err
	}
	if n := len(refKeyRE.FindAll(b, -1)); n != 1 {
		return nil, fmt.Errorf(`found %d "ref" keys, expected exactly 1`, n)
	}
	if n := len(pathKeyRE.FindAll(b, -1)); n != 1 {
		return nil, fmt.Errorf(`found %d "path" keys, expected exactly 1`, n)
	}
	// path and ref move together: the plugin root is integrations/claude-code/plugin from the first
	// release after v0.6.0, and v0.6.0 and older tags (and their rc tags) do not have that path, so
	// for those versions the path is left alone.
	if newerThan060(value) {
		b = pathKeyRE.ReplaceAll(b, []byte("${1}"+pluginPath+"${2}"))
	}
	return refKeyRE.ReplaceAll(b, []byte("${1}"+value+"${2}")), nil
}

// cursorPinRE matches the exact-version npx pin in the Cursor plugin's mcp.json.
var cursorPinRE = regexp.MustCompile(`("@mirastacklabs/mirafleet@)([^"]*)(")`)

// cursorPin returns the version after "@mirastacklabs/mirafleet@"; there must be exactly one pin.
func cursorPin(b []byte) ([]string, error) {
	m := cursorPinRE.FindAllSubmatch(b, -1)
	if len(m) != 1 {
		return nil, fmt.Errorf(`want exactly one "@mirastacklabs/mirafleet@X.Y.Z" pin, found %d`, len(m))
	}
	return []string{string(m[0][2])}, nil
}

func setCursorPin(b []byte, value string) ([]byte, error) {
	if _, err := cursorPin(b); err != nil {
		return nil, err
	}
	return cursorPinRE.ReplaceAll(b, []byte("${1}"+value+"${3}")), nil
}
