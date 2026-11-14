package workspace

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Limits from docs/M3.md section 1.9.
const (
	MaxPathBytes  = 4 << 10
	MaxArgvElems  = 256
	MaxArgvBytes  = 32 << 10
	MaxEnvVars    = 32
	MaxEnvValue   = 4 << 10
	maxImageBytes = 255
)

// CleanPath validates p and returns it as a cleaned absolute path under /workspace.
// p is relative to /workspace or absolute under it; "" and "." mean /workspace itself.
// It refuses NUL, invalid UTF-8, more than 4 KiB, and anything that cleans to a place
// outside /workspace (including via "..").
//
// The check is purely lexical: it knows nothing about symlinks. A symlink inside /workspace can
// point anywhere, so the executor must resolve paths safely at use time (ctrd opens them with
// os.Root, which refuses escapes; opensandbox runs inside the sandbox where only the sandbox's
// own filesystem is reachable). Never use the result to open a host path directly.
func CleanPath(p string) (string, error) {
	if len(p) > MaxPathBytes {
		return "", errors.New("path too long")
	}
	if strings.IndexByte(p, 0) >= 0 {
		return "", errors.New("path contains NUL")
	}
	if !utf8.ValidString(p) {
		return "", errors.New("path is not valid UTF-8")
	}
	var abs string
	if strings.HasPrefix(p, "/") {
		abs = path.Clean(p)
	} else {
		abs = path.Join(Root, p) // Join cleans; ".." beyond Root lands outside and is refused below
	}
	if abs != Root && !strings.HasPrefix(abs, Root+"/") {
		return "", errors.New("path outside " + Root)
	}
	return abs, nil
}

// ValidArgv checks an argv: 1..256 elements, at most 32 KiB in total, no NUL, argv[0] non-empty.
func ValidArgv(argv []string) error {
	if len(argv) == 0 {
		return errors.New("argv is empty")
	}
	if len(argv) > MaxArgvElems {
		return errors.New("argv has too many elements")
	}
	if argv[0] == "" {
		return errors.New("argv[0] is empty")
	}
	total := 0
	for _, a := range argv {
		if strings.IndexByte(a, 0) >= 0 {
			return errors.New("argv contains NUL")
		}
		total += len(a)
		if total > MaxArgvBytes {
			return errors.New("argv too large")
		}
	}
	return nil
}

var envName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)

// EnvDenied reports whether name is on the hard env denylist: *_API_KEY, *_TOKEN, *_SECRET*,
// ANTHROPIC_*, OPENAI_*, AWS_*, SSH_AUTH_SOCK, GIT_ASKPASS, GIT_SSH_COMMAND, *PROXY*, LD_*,
// BASH_ENV, ENV, NODE_OPTIONS, PYTHONSTARTUP, *_PASSWORD, *_KEY, PYTHONPATH, PERL5OPT,
// PERL5LIB, RUBYOPT, JAVA_TOOL_OPTIONS, _JAVA_OPTIONS, NODE_PATH, GIT_CONFIG_* (COUNT,
// KEY_n, VALUE_n, GLOBAL, SYSTEM), GIT_EXEC_PATH, PROMPT_COMMAND, GCONV_PATH, *_PAT,
// *_CREDENTIALS, DATABASE_URL, *_DSN. It applies to every workspace regardless of any
// allowlist, on central and on the runner.
func EnvDenied(name string) bool {
	n := strings.ToUpper(name)
	switch {
	case strings.HasSuffix(n, "_API_KEY"), strings.HasSuffix(n, "_TOKEN"),
		strings.Contains(n, "_SECRET"),
		n == "API_KEY", n == "TOKEN", n == "SECRET" || strings.HasPrefix(n, "SECRET"),
		strings.HasPrefix(n, "ANTHROPIC_"), strings.HasPrefix(n, "OPENAI_"), strings.HasPrefix(n, "AWS_"),
		n == "SSH_AUTH_SOCK", n == "GIT_ASKPASS", n == "GIT_SSH_COMMAND",
		strings.Contains(n, "PROXY"),
		// Variables that change what code runs or what is injected into a process.
		strings.HasPrefix(n, "LD_"), n == "BASH_ENV", n == "ENV", n == "NODE_OPTIONS", n == "PYTHONSTARTUP",
		// Credential-looking names.
		strings.HasSuffix(n, "_PASSWORD"), strings.HasSuffix(n, "_KEY"), n == "PASSWORD", n == "KEY",
		// More code-injection vectors (interpreter search paths and option hooks, git config
		// injection, shell and libc hooks).
		n == "PYTHONPATH", n == "PERL5OPT", n == "PERL5LIB", n == "RUBYOPT",
		n == "JAVA_TOOL_OPTIONS", n == "_JAVA_OPTIONS", n == "NODE_PATH",
		strings.HasPrefix(n, "GIT_CONFIG_"), n == "GIT_EXEC_PATH", n == "PROMPT_COMMAND", n == "GCONV_PATH",
		// More credential-looking names.
		strings.HasSuffix(n, "_PAT"), strings.HasSuffix(n, "_CREDENTIALS"),
		n == "DATABASE_URL", strings.HasSuffix(n, "_DSN"):
		return true
	}
	return false
}

// FilterEnv validates env and returns a copy. Fail-closed: any variable that is malformed,
// too large, not in allow (exact names), or on the denylist is an error (the denylist wins
// even when the name is allowlisted). Values are never included in errors.
func FilterEnv(env map[string]string, allow []string) (map[string]string, error) {
	if len(env) > MaxEnvVars {
		return nil, errors.New("too many env vars")
	}
	ok := make(map[string]bool, len(allow))
	for _, a := range allow {
		ok[a] = true
	}
	out := make(map[string]string, len(env))
	for k, v := range env {
		if !envName.MatchString(k) {
			return nil, fmt.Errorf("invalid env name %q", k)
		}
		if EnvDenied(k) {
			return nil, fmt.Errorf("env %s is denied", k)
		}
		if !ok[k] {
			return nil, fmt.Errorf("env %s is not allowed", k)
		}
		if len(v) > MaxEnvValue {
			return nil, fmt.Errorf("env %s value too large", k)
		}
		if strings.IndexByte(v, 0) >= 0 || !utf8.ValidString(v) {
			return nil, fmt.Errorf("env %s value invalid", k)
		}
		out[k] = v
	}
	return out, nil
}

// ---- image references ----

var (
	imgDomain = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]{1,5})?$`)
	imgPath   = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*$`)
	imgTag    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	imgDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

type imageRef struct{ domain, repo, tag, digest string }

func (r imageRef) name() string { return r.domain + "/" + r.repo }

func (r imageRef) String() string {
	s := r.name()
	if r.tag != "" {
		s += ":" + r.tag
	}
	if r.digest != "" {
		s += "@" + r.digest
	}
	return s
}

// parseImage parses and normalises a reference without defaulting the tag.
// Short names get docker.io and, for single-component names, library/.
// Digest wildcards are the caller's concern; digest here must be a full sha256.
func parseImage(ref string, wildDigest bool) (imageRef, error) {
	var r imageRef
	if ref == "" || len(ref) > maxImageBytes {
		return r, errors.New("invalid image reference")
	}
	rest := ref
	if i := strings.IndexByte(rest, '@'); i >= 0 {
		r.digest, rest = rest[i+1:], rest[:i]
		if !(imgDigest.MatchString(r.digest) || wildDigest && r.digest == "sha256:*") {
			return r, errors.New("invalid image digest")
		}
	}
	// A tag is a ':' after the last '/'.
	if i := strings.LastIndexByte(rest, ':'); i >= 0 && i > strings.LastIndexByte(rest, '/') {
		r.tag, rest = rest[i+1:], rest[:i]
		if !imgTag.MatchString(r.tag) {
			return r, errors.New("invalid image tag")
		}
	}
	first, remainder, hasSlash := strings.Cut(rest, "/")
	if hasSlash && (strings.ContainsAny(first, ".:") || first == "localhost") {
		if !imgDomain.MatchString(first) {
			return r, errors.New("invalid image registry")
		}
		r.domain, r.repo = first, remainder
	} else {
		r.domain, r.repo = "docker.io", rest
	}
	if r.domain == "index.docker.io" || r.domain == "registry-1.docker.io" {
		r.domain = "docker.io"
	}
	if r.domain == "docker.io" && !strings.Contains(r.repo, "/") {
		r.repo = "library/" + r.repo
	}
	if !imgPath.MatchString(r.repo) {
		return r, errors.New("invalid image repository")
	}
	return r, nil
}

// NormalizeImage returns the canonical form of a reference: registry host always present
// (docker.io for short names, library/ added for single-component docker.io names) and
// ":latest" added when there is neither a tag nor a digest. Tags and digests are kept.
// No external reference library is used; the grammar is the common subset of OCI references.
func NormalizeImage(ref string) (string, error) {
	r, err := parseImage(ref, false)
	if err != nil {
		return "", err
	}
	if r.tag == "" && r.digest == "" {
		r.tag = "latest"
	}
	return r.String(), nil
}

// MatchImage reports whether ref is allowed by patterns. Patterns are exact references,
// "repo@sha256:*" (any digest-pinned ref of repo), or "repo:*" (any tagged ref of repo).
// Both sides are normalised, so "alpine" matches "docker.io/library/alpine:latest".
// Invalid refs or patterns never match, and a digest-only ref never matches "repo:*".
// When both pattern and ref carry a digest, the tag is ignored: the digest decides.
func MatchImage(patterns []string, ref string) bool {
	if strings.Contains(ref, "*") {
		return false
	}
	want, err := parseImage(ref, false)
	if err != nil {
		return false
	}
	norm := want
	if norm.tag == "" && norm.digest == "" {
		norm.tag = "latest"
	}
	for _, p := range patterns {
		switch {
		case strings.HasSuffix(p, "@sha256:*"):
			pr, err := parseImage(p, true)
			if err != nil || pr.tag != "" {
				continue
			}
			if pr.name() == want.name() && want.digest != "" {
				return true
			}
		case strings.HasSuffix(p, ":*"):
			pr, err := parseImage(strings.TrimSuffix(p, ":*"), false)
			if err != nil || pr.tag != "" || pr.digest != "" {
				continue
			}
			if pr.name() == want.name() && norm.tag != "" { // untagged refs resolve to :latest
				return true
			}
		case strings.Contains(p, "*"):
			continue
		default:
			pr, err := parseImage(p, false)
			if err != nil {
				continue
			}
			if pr.digest != "" && want.digest != "" {
				// A digest identifies the content; the tag is informational and ignored, so
				// "repo@sha256:X" matches "repo:tag@sha256:X" and vice versa.
				if pr.name() == want.name() && pr.digest == want.digest {
					return true
				}
				continue
			}
			if pr.tag == "" && pr.digest == "" {
				pr.tag = "latest"
			}
			if pr.String() == norm.String() {
				return true
			}
		}
	}
	return false
}
