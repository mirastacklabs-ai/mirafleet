package fsops

import "strconv"

// RgArgs returns the ripgrep argv (without the program name) that produces the same file set and
// matches as Grep for o, so a runner with rg installed can delegate. Run it with the
// session directory as the working directory and no shell. Output shape per mode:
//
//	content             --json (parse "match" and "context" events; line numbers are always there)
//	files_with_matches  -l, one path per line
//	count               -c --with-filename, "path:count" per line
//
// The caller applies Offset and HeadLimit to the parsed result (rg has no such flags) and should
// kill rg once enough output is collected. The pattern is validated as RE2 first, so Go and rg
// accept the same expressions.
func RgArgs(o GrepOptions) ([]string, error) {
	s, err := normalizeGrep(o)
	if err != nil {
		return nil, err
	}
	args := []string{"--no-config", "--no-require-git", "--sort", "path"}
	switch s.mode {
	case GrepContent:
		args = append(args, "--json")
		if s.before > 0 {
			args = append(args, "-B", strconv.Itoa(s.before))
		}
		if s.after > 0 {
			args = append(args, "-A", strconv.Itoa(s.after))
		}
	case GrepFilesWithMatches:
		args = append(args, "-l")
	default:
		args = append(args, "-c", "--with-filename")
	}
	if o.IgnoreCase {
		args = append(args, "-i")
	}
	if o.Multiline {
		args = append(args, "-U", "--multiline-dotall")
	}
	if o.Hidden {
		args = append(args, "--hidden")
	}
	if o.NoIgnore {
		args = append(args, "--no-ignore")
	}
	if s.hasGlobs {
		for _, g := range splitGlobs(o.Glob) {
			args = append(args, "--glob", g)
		}
	}
	if o.Type != "" {
		args = append(args, "--type", o.Type)
	}
	return append(args, "-e", o.Pattern, "--", s.start), nil
}
