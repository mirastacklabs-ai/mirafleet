package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func init() {
	register(command{
		Name: "bump", Usage: "X.Y.Z",
		Summary: "set every versioned file to the version",
		Run:     runBump,
	})
}

func runBump(e env, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: relgen bump X.Y.Z (no leading v)")
	}
	version := args[0]
	if !semverRE.MatchString(version) {
		return fmt.Errorf("%q is not a version (want X.Y.Z or X.Y.Z-rc.N, no leading v)", version)
	}
	changed, err := bumpTree(e.Root, version)
	if err != nil {
		return err
	}
	for _, p := range changed {
		fmt.Fprintf(e.Out, "updated %s\n", p)
	}
	fmt.Fprintf(e.Out, "ok: %d versioned files at v%s (%d changed)\n", len(targets), version, len(changed))
	return nil
}

// renameFn is os.Rename; tests replace it to inject a failure.
var renameFn = os.Rename

type edit struct {
	path string
	abs  string
	orig []byte
	body []byte
	mode os.FileMode
	tmp  string // staged temp file, in the target's directory
}

// bumpTree computes every new file body first, stages each as a synced temp file next to its
// target, and only then renames them over the targets. A failure before the renames leaves the tree
// untouched; a failed rename restores the originals already replaced. It returns the paths whose
// content changed (on error: the paths written, whether or not they were restored).
func bumpTree(root, version string) ([]string, error) {
	var edits []*edit
	for _, t := range targets {
		abs := filepath.Join(root, filepath.FromSlash(t.Path))
		st, err := os.Lstat(abs)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", t.Path, err)
		}
		if !st.Mode().IsRegular() {
			return nil, fmt.Errorf("%s: not a regular file (symlinks and special files are refused)", t.Path)
		}
		b, err := os.ReadFile(abs)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", t.Path, err)
		}
		nb, err := t.Set(b, t.want(version))
		if err != nil {
			return nil, fmt.Errorf("%s: %v", t.Path, err)
		}
		got, err := t.Get(nb)
		if err != nil {
			return nil, fmt.Errorf("%s: result does not parse: %v", t.Path, err)
		}
		for _, g := range got {
			if g != t.want(version) {
				return nil, fmt.Errorf("%s: after bump found %q, want %q", t.Path, g, t.want(version))
			}
		}
		if string(nb) != string(b) {
			edits = append(edits, &edit{path: t.Path, abs: abs, orig: b, body: nb, mode: st.Mode().Perm()})
		}
	}
	// Stage.
	cleanup := func() {
		for _, ed := range edits {
			if ed.tmp != "" {
				os.Remove(ed.tmp)
			}
		}
	}
	for _, ed := range edits {
		tmp, err := stage(ed.abs, ed.body, ed.mode)
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("%s: staging: %v (no file was changed)", ed.path, err)
		}
		ed.tmp = tmp
	}
	// Commit.
	var changed []string
	for i, ed := range edits {
		if err := renameFn(ed.tmp, ed.abs); err != nil {
			cleanup()
			var restoreErrs []string
			for _, done := range edits[:i] {
				if _, err := stageAndRename(done.abs, done.orig, done.mode); err != nil {
					restoreErrs = append(restoreErrs, fmt.Sprintf("%s: %v", done.path, err))
				}
			}
			msg := fmt.Sprintf("%s: %v; written before the failure: [%s]", ed.path, err, strings.Join(changed, ", "))
			if len(restoreErrs) > 0 {
				msg += "; restore FAILED for: " + strings.Join(restoreErrs, "; ")
			} else {
				msg += "; all restored"
			}
			return changed, errors.New(msg)
		}
		ed.tmp = ""
		changed = append(changed, ed.path)
	}
	return changed, nil
}

// stage writes body to a new temp file in the same directory as abs, with the given mode, and
// fsyncs it.
func stage(abs string, body []byte, mode os.FileMode) (string, error) {
	f, err := os.CreateTemp(filepath.Dir(abs), ".relgen-*")
	if err != nil {
		return "", err
	}
	name := f.Name()
	fail := func(err error) (string, error) {
		f.Close()
		os.Remove(name)
		return "", err
	}
	if _, err := f.Write(body); err != nil {
		return fail(err)
	}
	if err := f.Chmod(mode); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}

func stageAndRename(abs string, body []byte, mode os.FileMode) (string, error) {
	tmp, err := stage(abs, body, mode)
	if err != nil {
		return "", err
	}
	if err := os.Rename(tmp, abs); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return abs, nil
}
