package actions

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const sample = `
actions:
  hello:
    run: ["/bin/echo", "hello", "{{name}}"]
    params: { name: { pattern: "[a-z]{1,8}" } }
  tail:
    run: ["/bin/tail-it", "-n", "{{lines}}", "--tag=x{{lines}}y"]
    params: { lines: { pattern: "^[0-9]{1,4}$", default: "200" } }
  plain:
    run: ["/usr/bin/true"]
  slow:
    run: ["/bin/sleep", "30"]
    timeout: 200ms
  fail:
    run: ["/usr/bin/false"]
`

func mustParse(t *testing.T) *Set {
	t.Helper()
	s, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPlanSubstitution(t *testing.T) {
	s := mustParse(t)
	argv, to, err := s.Plan("hello", map[string]string{"name": "bob"})
	if err != nil || !reflect.DeepEqual(argv, []string{"/bin/echo", "hello", "bob"}) || to != DefaultTimeout {
		t.Fatalf("got %v %v %v", argv, to, err)
	}
	// default + embedded placeholder + multiple occurrences
	argv, _, err = s.Plan("tail", nil)
	if err != nil || !reflect.DeepEqual(argv, []string{"/bin/tail-it", "-n", "200", "--tag=x200y"}) {
		t.Fatalf("got %v %v", argv, err)
	}
	argv, _, _ = s.Plan("tail", map[string]string{"lines": "5"})
	if argv[3] != "5" && argv[2] != "5" {
		t.Fatalf("override not applied: %v", argv)
	}
}

func refused(t *testing.T, s *Set, name string, p map[string]string) {
	t.Helper()
	_, _, err := s.Plan(name, p)
	var re *RefusedError
	if !errors.As(err, &re) {
		t.Fatalf("%s %v: want refusal, got %v", name, p, err)
	}
}

func TestRefusals(t *testing.T) {
	s := mustParse(t)
	refused(t, s, "nope", nil)
	refused(t, s, "hello", nil)                                        // required param missing
	refused(t, s, "hello", map[string]string{"name": "bob", "x": "y"}) // unknown param
	refused(t, s, "plain", map[string]string{"x": "y"})
	refused(t, s, "hello", map[string]string{"name": ""})
	refused(t, s, "hello", map[string]string{"name": "toolongname"})
}

func TestFullMatchRegex(t *testing.T) {
	s := mustParse(t)
	// "name" pattern is unanchored in YAML; it must still be a full match.
	refused(t, s, "hello", map[string]string{"name": "bob!"})
	refused(t, s, "hello", map[string]string{"name": "!bob"})
	refused(t, s, "hello", map[string]string{"name": "bob\n"})
	refused(t, s, "hello", map[string]string{"name": "bob\nrm"})
	// alternation must not escape the anchoring
	s2, err := Parse([]byte(`{actions: {a: {run: [/bin/x, "{{p}}"], params: {p: {pattern: "a|b"}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	refused(t, s2, "a", map[string]string{"p": "a; rm"})
	refused(t, s2, "a", map[string]string{"p": "xb"})
	if _, _, err := s2.Plan("a", map[string]string{"p": "b"}); err != nil {
		t.Fatal(err)
	}
}

func TestInjectionAttempts(t *testing.T) {
	s := mustParse(t)
	for _, bad := range []string{"200; rm -rf /", "$(id)", "`id`", "200 && id", "200|id", "-n 5", "200\n", "../../x", "{{lines}}"} {
		refused(t, s, "tail", map[string]string{"lines": bad})
	}
	// A permissive pattern: hostile text must still be ONE literal argv element, never re-expanded.
	s2, err := Parse([]byte(`{actions: {a: {run: [/bin/echo, "{{v}}", "{{w}}"], params: {v: {pattern: ".*"}, w: {pattern: ".*"}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	evil := "200; rm -rf / {{w}} $(x) 'q' \"d\""
	argv, _, err := s2.Plan("a", map[string]string{"v": evil, "w": "SECOND"})
	if err != nil {
		t.Fatal(err)
	}
	if len(argv) != 3 || argv[1] != evil || argv[2] != "SECOND" {
		t.Fatalf("argv not literal: %q", argv)
	}
}

func TestParseValidation(t *testing.T) {
	bad := map[string]string{
		"empty run":         `{actions: {a: {run: []}}}`,
		"placeholder argv0": `{actions: {a: {run: ["{{p}}"], params: {p: {pattern: "x"}}}}}`,
		"undefined param":   `{actions: {a: {run: [/bin/x, "{{p}}"]}}}`,
		"bad regex":         `{actions: {a: {run: [/bin/x, "{{p}}"], params: {p: {pattern: "("}}}}}`,
		"default mismatch":  `{actions: {a: {run: [/bin/x, "{{p}}"], params: {p: {pattern: "[0-9]+", default: "z"}}}}}`,
		"unknown field":     `{actions: {a: {run: [/bin/x], shell: true}}}`,
		"no pattern":        `{actions: {a: {run: [/bin/x, "{{p}}"], params: {p: {}}}}}`,
		"escape wrapper":    `{actions: {a: {run: [/bin/x, "{{p}}"], params: {p: {pattern: "[0-9]{1,4})|(?:.*"}}}}}`,
		"missing command":   `{actions: {a: {run: [definitely-not-installed-xyz]}}}`,
		"relative path":     `{actions: {a: {run: ["bin/x"]}}}`,
		"dash default":      `{actions: {a: {run: [/bin/x, "{{p}}"], params: {p: {pattern: ".*", default: "-rf"}}}}}`,
	}
	for n, y := range bad {
		if _, err := Parse([]byte(y)); err == nil {
			t.Errorf("%s: expected error", n)
		}
	}
}

func TestRunOutputAndExit(t *testing.T) {
	s := mustParse(t)
	r, err := s.Run(context.Background(), "hello", map[string]string{"name": "bob"})
	if err != nil || strings.TrimSpace(r.Stdout) != "hello bob" || r.ExitCode != 0 {
		t.Fatalf("%+v %v", r, err)
	}
	r, err = s.Run(context.Background(), "fail", nil)
	if err != nil || r.ExitCode != 1 {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err = s.Run(context.Background(), "nope", nil); err == nil {
		t.Fatal("want refusal")
	}
}

func TestTimeoutKills(t *testing.T) {
	s := mustParse(t)
	start := time.Now()
	r, err := s.Run(context.Background(), "slow", nil)
	if err != nil || !r.TimedOut || r.ExitCode != -1 {
		t.Fatalf("%+v %v", r, err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("process not killed promptly")
	}
}

func TestRelativeArgv0ResolvedAtLoad(t *testing.T) {
	s, err := Parse([]byte(`{actions: {a: {run: [echo, hi]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	argv, _, _ := s.Plan("a", nil)
	if !filepath.IsAbs(argv[0]) {
		t.Fatalf("argv[0] not resolved: %v", argv)
	}
}

func TestLeadingDash(t *testing.T) {
	s, err := Parse([]byte(`{actions: {a: {run: [/bin/echo, "{{p}}"], params: {p: {pattern: ".*"}}},
  b: {run: [/bin/echo, "{{p}}"], params: {p: {pattern: ".*", allow_leading_dash: true}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	refused(t, s, "a", map[string]string{"p": "-rf"})
	if _, _, err := s.Plan("a", map[string]string{"p": "ok-"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Plan("b", map[string]string{"p": "-rf"}); err != nil {
		t.Fatal(err)
	}
}

func runYAML(t *testing.T, y string) *Result {
	t.Helper()
	s, err := Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Run(context.Background(), "a", nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestMinimalEnvAndDir(t *testing.T) {
	t.Setenv("MIRAFLEET_SECRET", "leak")
	r := runYAML(t, `{actions: {a: {run: ["/usr/bin/env"]}}}`)
	if strings.Contains(r.Stdout, "MIRAFLEET_SECRET") || !strings.Contains(r.Stdout, "LANG=C") ||
		!strings.Contains(r.Stdout, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin") {
		t.Fatalf("env: %q", r.Stdout)
	}
	r = runYAML(t, `{actions: {a: {run: ["/bin/pwd"]}}}`)
	if strings.TrimSpace(r.Stdout) != "/" {
		t.Fatalf("dir: %q", r.Stdout)
	}
}

func TestOutputCappedCombined(t *testing.T) {
	r := runYAML(t, `{actions: {a: {run: ["/bin/sh", "-c", "head -c 700000 /dev/zero; head -c 700000 /dev/zero >&2"]}}}`)
	if len(r.Stdout)+len(r.Stderr) != MaxOutput || !(r.StdoutTruncated || r.StderrTruncated) {
		t.Fatalf("out=%d err=%d trunc=%v/%v", len(r.Stdout), len(r.Stderr), r.StdoutTruncated, r.StderrTruncated)
	}
	r = runYAML(t, `{actions: {a: {run: ["/bin/echo", "small"]}}}`)
	if r.StdoutTruncated || r.StderrTruncated {
		t.Fatal("small output flagged truncated")
	}
}

func TestParentCancelIsNotTimeout(t *testing.T) {
	s := mustParse(t)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	// "slow" has a 200ms action timeout; the 150ms parent context fires first.
	r, err := s.Run(ctx, "slow", nil)
	if err != nil || r.TimedOut || r.ExitCode != -1 {
		t.Fatalf("%+v %v", r, err)
	}
}
