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

// targets is the list of versioned files (docs/M6.md section 2.1). npm templates carry no version.
// Later steps (compose image tags, ...) append from their own file's init via addTarget.
var targets = []target{
	{Path: "integrations/claude-code/.claude-plugin/plugin.json", Get: jsonVersions, Set: setJSONVersions},
	{Path: "cmd/mirafleet/mcp_hook.go", Get: goPluginVersion, Set: setGoPluginVersion},
	{Path: ".claude-plugin/marketplace.json", Tag: true, Get: marketplaceRef, Set: setMarketplaceRef},
	{Path: "server.json", Get: jsonVersions, Set: setJSONVersions},
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
	return refKeyRE.ReplaceAll(b, []byte("${1}"+value+"${2}")), nil
}
