package workspace

import (
	"strings"
	"testing"
)

func TestIDs(t *testing.T) {
	id := NewID()
	if !ValidID(id) || NewID() == id {
		t.Fatalf("bad id %q", id)
	}
	for _, bad := range []ID{"", "abc", ID(strings.Repeat("A", 32)), ID(strings.Repeat("g", 32)), ID(strings.Repeat("a", 33))} {
		if ValidID(bad) {
			t.Errorf("%q valid", bad)
		}
	}
}

func TestCleanPath(t *testing.T) {
	ok := map[string]string{
		"": "/workspace", ".": "/workspace", "a": "/workspace/a", "a/../b": "/workspace/b",
		"/workspace": "/workspace", "/workspace/x/./y": "/workspace/x/y", "./a//b/": "/workspace/a/b",
		"/workspace/a/../b": "/workspace/b", "..a": "/workspace/..a",
	}
	for in, want := range ok {
		if got, err := CleanPath(in); err != nil || got != want {
			t.Errorf("CleanPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	bad := []string{"..", "../x", "a/../../x", "/etc/passwd", "/", "/workspace/../etc", "/workspacex", "/workspace2/a",
		"a\x00b", "\xff", strings.Repeat("a", MaxPathBytes+1)}
	for _, in := range bad {
		if got, err := CleanPath(in); err == nil {
			t.Errorf("CleanPath(%q) = %q; want error", in, got)
		}
	}
	if _, err := CleanPath(strings.Repeat("a", MaxPathBytes)); err != nil {
		t.Errorf("4 KiB path refused: %v", err)
	}
}

func FuzzCleanPath(f *testing.F) {
	for _, s := range []string{"", "a", "../x", "/etc", "/workspace/../etc", "a\x00", "/workspace//a/../..", "....//", "\xff"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		got, err := CleanPath(in)
		if err != nil {
			return
		}
		if got != Root && !strings.HasPrefix(got, Root+"/") {
			t.Fatalf("%q escaped: %q", in, got)
		}
		if strings.ContainsRune(got, 0) || strings.Contains(got, "/../") || strings.HasSuffix(got, "/..") || strings.HasSuffix(got, "/") || strings.Contains(got, "//") {
			t.Fatalf("%q not clean: %q", in, got)
		}
		if again, err := CleanPath(got); err != nil || again != got {
			t.Fatalf("not idempotent: %q -> %q -> %q, %v", in, got, again, err)
		}
	})
}

func TestValidArgv(t *testing.T) {
	if err := ValidArgv([]string{"ls", "-l"}); err != nil {
		t.Fatal(err)
	}
	many := make([]string, MaxArgvElems)
	for i := range many {
		many[i] = "a"
	}
	if err := ValidArgv(many); err != nil {
		t.Errorf("256 elems: %v", err)
	}
	cases := map[string][]string{
		"nil":      nil,
		"empty0":   {""},
		"nul":      {"a", "b\x00"},
		"257":      append(append([]string(nil), many...), "a"),
		"too big":  {"a", strings.Repeat("x", MaxArgvBytes)},
		"big many": {strings.Repeat("x", MaxArgvBytes/2+1), strings.Repeat("x", MaxArgvBytes/2)},
	}
	for name, a := range cases {
		if ValidArgv(a) == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if err := ValidArgv([]string{strings.Repeat("x", MaxArgvBytes)}); err != nil {
		t.Errorf("exactly 32 KiB refused: %v", err)
	}
}

func TestEnvDenied(t *testing.T) {
	for _, n := range []string{"FOO_API_KEY", "GH_TOKEN", "MY_SECRET", "MY_SECRET_KEY", "ANTHROPIC_BASE_URL", "OPENAI_ORG",
		"AWS_REGION", "SSH_AUTH_SOCK", "GIT_ASKPASS", "HTTP_PROXY", "NO_PROXY", "https_proxy", "ALL_PROXY",
		"LD_PRELOAD", "LD_LIBRARY_PATH", "BASH_ENV", "ENV", "NODE_OPTIONS", "PYTHONSTARTUP", "GIT_SSH_COMMAND",
		"DB_PASSWORD", "SIGNING_KEY", "PASSWORD", "KEY",
		// E-11
		"PYTHONPATH", "PERL5OPT", "PERL5LIB", "RUBYOPT", "JAVA_TOOL_OPTIONS", "_JAVA_OPTIONS", "NODE_PATH",
		"GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_12", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "git_config_nosystem",
		"GIT_EXEC_PATH", "PROMPT_COMMAND", "GCONV_PATH", "GH_PAT", "DEPLOY_PAT", "GCP_CREDENTIALS", "DATABASE_URL",
		"APP_DSN", "SENTRY_DSN", "pythonpath"} {
		if !EnvDenied(n) {
			t.Errorf("%s not denied", n)
		}
	}
	for _, n := range []string{"FOO", "LANG", "PATH", "HOME", "CI", "TOKENIZER_MODE", "GIT_AUTHOR", "ENVIRONMENT", "LDFLAGS", "NODE_ENV", "KEYBOARD", "PYTHONUNBUFFERED", "PATENT", "PAT", "GIT_CONFIG", "GIT_AUTHOR_NAME", "GIT_EXEC", "DSN", "DATABASE_URL_X", "CREDENTIALS_FILE", "PERL5", "RUBY_VERSION", "JAVA_HOME", "NODE_PATHS"} {
		if EnvDenied(n) {
			t.Errorf("%s denied", n)
		}
	}
}

// F-24: exec-hook names and the DATABASE_URL suffix match.
func TestEnvDeniedF24(t *testing.T) {
	cases := map[string]bool{
		"GIT_SSH": true, "GIT_EXTERNAL_DIFF": true, "GIT_PAGER": true, "PAGER": true,
		"GIT_EDITOR": true, "EDITOR": true, "VISUAL": true, "editor": true, "git_ssh": true,
		"DATABASE_URL": true, "APP_DATABASE_URL": true, "POSTGRES_DATABASE_URL": true, "database_url": true,
		"LESSOPEN": true, "LESSCLOSE": true, "MANPAGER": true, "GIT_SEQUENCE_EDITOR": true, "RUBYLIB": true,
		"PYTHONHOME": true, "lessopen": true, "pythonhome": true,
		"LESSOPEN_X": false, "RUBYLIB_X": false, "PYTHONHOMEX": false, "MANPAGER_X": false,
		"DATABASE_URL_X": false, "EDITORCONFIG": false, "PAGER_X": false, "GIT_SSH_X": false, "VISUALS": false,
	}
	for n, want := range cases {
		if got := EnvDenied(n); got != want {
			t.Errorf("EnvDenied(%q) = %v, want %v", n, got, want)
		}
	}
}

func TestFilterEnv(t *testing.T) {
	allow := []string{"FOO", "LANG", "ANTHROPIC_API_KEY", "HTTP_PROXY", "GH_TOKEN"}
	got, err := FilterEnv(map[string]string{"FOO": "1", "LANG": "C"}, allow)
	if err != nil || got["FOO"] != "1" || len(got) != 2 {
		t.Fatalf("%v %v", got, err)
	}
	// Denylist wins even when allowlisted.
	for _, k := range []string{"ANTHROPIC_API_KEY", "HTTP_PROXY", "GH_TOKEN"} {
		if _, err := FilterEnv(map[string]string{k: "x"}, allow); err == nil {
			t.Errorf("%s accepted", k)
		}
	}
	if _, err := FilterEnv(map[string]string{"BAR": "1"}, allow); err == nil {
		t.Error("non-allowlisted accepted")
	}
	if _, err := FilterEnv(map[string]string{"FOO": "1"}, nil); err == nil {
		t.Error("empty allowlist accepted var")
	}
	for _, k := range []string{"foo", "1A", "", "A-B", strings.Repeat("A", 65)} {
		if _, err := FilterEnv(map[string]string{k: "1"}, []string{k}); err == nil {
			t.Errorf("name %q accepted", k)
		}
	}
	if _, err := FilterEnv(map[string]string{"FOO": strings.Repeat("v", MaxEnvValue+1)}, allow); err == nil {
		t.Error("big value")
	}
	if _, err := FilterEnv(map[string]string{"FOO": strings.Repeat("v", MaxEnvValue)}, allow); err != nil {
		t.Error(err)
	}
	if _, err := FilterEnv(map[string]string{"FOO": "a\x00"}, allow); err == nil {
		t.Error("NUL value")
	}
	big := map[string]string{}
	var al []string
	for i := 0; i <= MaxEnvVars; i++ {
		k := "V" + strings.Repeat("A", i)
		big[k] = "1"
		al = append(al, k)
	}
	if _, err := FilterEnv(big, al); err == nil {
		t.Error("33 vars accepted")
	}
	// Errors never leak values.
	_, err = FilterEnv(map[string]string{"GH_TOKEN": "s3cret-value"}, allow)
	if err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Errorf("error leaks value: %v", err)
	}
}

func TestMatchImage(t *testing.T) {
	dg := "sha256:" + strings.Repeat("a", 64)
	dg2 := "sha256:" + strings.Repeat("b", 64)
	tests := []struct {
		pats []string
		ref  string
		want bool
	}{
		{[]string{"alpine"}, "alpine", true},
		{[]string{"alpine"}, "alpine:latest", true},
		{[]string{"alpine:3.20"}, "docker.io/library/alpine:3.20", true},
		{[]string{"alpine:3.20"}, "alpine:3.19", false},
		{[]string{"alpine:3.20"}, "alpine", false},
		{[]string{"docker.io/library/alpine:3.20"}, "alpine:3.20", true},
		{[]string{"index.docker.io/library/alpine:3.20"}, "alpine:3.20", true},
		{[]string{"alpine:*"}, "alpine:3.20", true},
		{[]string{"alpine:*"}, "library/alpine:edge", true},
		{[]string{"alpine:*"}, "alpine", true},
		{[]string{"alpine:*"}, "busybox:1", false},
		{[]string{"alpine:*"}, "evil/alpine:1", false},
		{[]string{"alpine:*"}, "alpine@" + dg, false},
		{[]string{"alpine:*"}, "ghcr.io/alpine:1", false},
		{[]string{"ghcr.io/org/img:*"}, "ghcr.io/org/img:v1", true},
		{[]string{"ghcr.io/org/img:*"}, "ghcr.io/org/img2:v1", false},
		{[]string{"ghcr.io/org/img:*"}, "ghcr.io/org/img:v1@" + dg, true},
		{[]string{"ghcr.io/org/img@sha256:*"}, "ghcr.io/org/img@" + dg, true},
		{[]string{"ghcr.io/org/img@sha256:*"}, "ghcr.io/org/img:v1@" + dg, true},
		{[]string{"ghcr.io/org/img@sha256:*"}, "ghcr.io/org/img:v1", false},
		{[]string{"ghcr.io/org/img@sha256:*"}, "ghcr.io/org/img", false},
		{[]string{"ghcr.io/org/img@" + dg}, "ghcr.io/org/img@" + dg, true},
		{[]string{"ghcr.io/org/img@" + dg}, "ghcr.io/org/img@" + dg2, false},
		// A-18: with digests on both sides the tag is ignored.
		{[]string{"ghcr.io/org/img@" + dg}, "ghcr.io/org/img:v1@" + dg, true},
		{[]string{"ghcr.io/org/img:v1@" + dg}, "ghcr.io/org/img@" + dg, true},
		{[]string{"ghcr.io/org/img:v1@" + dg}, "ghcr.io/org/img:v2@" + dg, true},
		{[]string{"ghcr.io/org/img:v1@" + dg}, "ghcr.io/org/img:v1@" + dg2, false},
		{[]string{"ghcr.io/org/img:v1@" + dg}, "ghcr.io/other/img:v1@" + dg, false},
		{[]string{"ghcr.io/org/img:v1@" + dg}, "ghcr.io/org/img:v1", false},
		{[]string{"localhost:5000/x:*"}, "localhost:5000/x:1", true},
		{[]string{"localhost:5000/x:*"}, "localhost/x:1", false},
		{[]string{"*"}, "alpine", false},
		{[]string{"alp*"}, "alpine", false},
		{[]string{"alpine:*"}, "alpine:*", false},
		{[]string{"*:*"}, "alpine:1", false},
		{[]string{"alpine"}, "ALPINE", false},
		{[]string{"alpine"}, "", false},
		{[]string{""}, "", false},
		{[]string{"alpine"}, "alpine@sha256:abc", false},
		{nil, "alpine", false},
		{[]string{"a", "b:*"}, "b:1", true},
	}
	for _, tc := range tests {
		if got := MatchImage(tc.pats, tc.ref); got != tc.want {
			t.Errorf("MatchImage(%q, %q) = %v; want %v", tc.pats, tc.ref, got, tc.want)
		}
	}
	if n, err := NormalizeImage("alpine"); err != nil || n != "docker.io/library/alpine:latest" {
		t.Errorf("normalize: %q %v", n, err)
	}
	if n, _ := NormalizeImage("user/img:1"); n != "docker.io/user/img:1" {
		t.Errorf("normalize: %q", n)
	}
}
