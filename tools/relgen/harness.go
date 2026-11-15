package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// sharedCommandPath is the single source of the /mirafleet command body.
const sharedCommandPath = "integrations/commands/mirafleet.md"

func init() {
	register(command{
		Name: "harness", Usage: "[-write]",
		Summary: "fail when a harness command copy drifts from " + sharedCommandPath,
		Run:     runHarness,
	})
}

// harnessCopies lists integrations/<harness>/**/{commands,prompts}/mirafleet.{md,toml}, sorted,
// never following symlinks. The shared directory itself is the source, not a copy.
func harnessCopies(root string) ([]string, error) {
	base := filepath.Join(root, "integrations")
	var out []string
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(base, p)
		rel = filepath.ToSlash(rel)
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			if rel == "commands" {
				return filepath.SkipDir
			}
			return nil
		}
		dir := filepath.Base(filepath.Dir(p))
		if (dir == "commands" || dir == "prompts") && (d.Name() == "mirafleet.md" || d.Name() == "mirafleet.toml") {
			out = append(out, "integrations/"+rel)
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

func normBody(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
}

// splitBody returns the bytes before the command body, the body itself and the bytes after it.
// For .md the body follows an optional YAML front matter; for .toml it is the prompt string.
func splitBody(path, text string) (head, body, tail string, err error) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if strings.HasSuffix(path, ".md") {
		if strings.HasPrefix(text, "---\n") {
			i := strings.Index(text[4:], "\n---\n")
			if i < 0 {
				return "", "", "", errors.New("front matter is not closed")
			}
			n := 4 + i + len("\n---\n")
			return text[:n], text[n:], "", nil
		}
		return "", text, "", nil
	}
	const key = "prompt = "
	idx := 0
	if !strings.HasPrefix(text, key) {
		i := strings.Index(text, "\n"+key)
		if i < 0 {
			return "", "", "", errors.New("no `prompt = ` key")
		}
		idx = i + 1
	}
	start := idx + len(key)
	rest := text[start:]
	var delim string
	switch {
	case strings.HasPrefix(rest, `"""`):
		delim = `"""`
	case strings.HasPrefix(rest, `'''`):
		delim = `'''`
	default:
		return "", "", "", errors.New("prompt must be a multi-line string")
	}
	end := strings.Index(rest[3:], delim)
	if end < 0 {
		return "", "", "", errors.New("prompt string is not closed")
	}
	inner := rest[3 : 3+end]
	if delim == `"""` && strings.Contains(inner, `\`) {
		return "", "", "", errors.New(`prompt uses backslash escapes; use a ''' literal string`)
	}
	return text[:start+3], strings.TrimPrefix(inner, "\n"), text[start+3+end:], nil
}

// harnessBody converts the shared body to the form a copy uses ({{args}} in TOML commands).
func harnessBody(path, shared string) string {
	if strings.HasSuffix(path, ".toml") {
		return strings.ReplaceAll(shared, "$ARGUMENTS", "{{args}}")
	}
	return shared
}

func runHarness(e env, args []string) error {
	f := flag.NewFlagSet("harness", flag.ContinueOnError)
	f.SetOutput(e.Err)
	write := f.Bool("write", false, "rewrite drifted copies instead of failing")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("usage: relgen harness [-write]")
	}
	sb, err := os.ReadFile(filepath.Join(e.Root, filepath.FromSlash(sharedCommandPath)))
	if err != nil {
		return err
	}
	shared := normBody(string(sb))
	if shared == "" {
		return errors.New(sharedCommandPath + " is empty")
	}
	copies, err := harnessCopies(e.Root)
	if err != nil {
		return err
	}
	var drift []string
	for _, rel := range copies {
		full := filepath.Join(e.Root, filepath.FromSlash(rel))
		b, err := os.ReadFile(full)
		if err != nil {
			return err
		}
		want := harnessBody(rel, shared)
		head, body, tail, err := splitBody(rel, string(b))
		if err != nil {
			drift = append(drift, rel+": "+err.Error())
			continue
		}
		if normBody(body) == want {
			continue
		}
		if !*write {
			drift = append(drift, rel+": body differs from "+sharedCommandPath)
			continue
		}
		var out string
		if strings.HasSuffix(rel, ".toml") {
			out = head + "\n" + want + "\n" + tail
		} else {
			out = head + want + "\n"
		}
		if err := os.WriteFile(full, []byte(out), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(e.Out, "rewrote %s\n", rel)
	}
	if len(drift) > 0 {
		return errors.New("harness command drift (run `relgen harness -write`):\n  " + strings.Join(drift, "\n  "))
	}
	fmt.Fprintf(e.Out, "ok: %d harness command copies match %s\n", len(copies), sharedCommandPath)
	return nil
}
