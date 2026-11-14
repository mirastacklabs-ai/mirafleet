// Package actions is the runner-side guard: the allowlist from actions.yaml, parameter
// validation and execution. This is the security boundary of actions mode:
//   - commands are argv arrays and are never passed to a shell;
//   - a parameter value is substituted only after it fully matches its regex, and always lands
//     inside one argv element (values are never re-scanned or split);
//   - anything not defined in the file is refused.
package actions

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	DefaultTimeout = 60 * time.Second
	// MaxOutput caps stdout and stderr COMBINED.
	MaxOutput = 1 << 20

	// fixedPath is the only PATH actions ever see, and the one used to resolve relative argv[0].
	fixedPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
)

// RefusedError means the guard rejected the request; nothing was executed.
type RefusedError struct{ Reason string }

func (e *RefusedError) Error() string { return "refused: " + e.Reason }

func refuse(f string, a ...any) error { return &RefusedError{fmt.Sprintf(f, a...)} }

type ParamSpec struct {
	Pattern string  `yaml:"pattern"`
	Default *string `yaml:"default"`
	// AllowLeadingDash permits values starting with '-' (refused by default: option injection).
	AllowLeadingDash bool `yaml:"allow_leading_dash"`
	re               *regexp.Regexp
}

type Action struct {
	Run     []string              `yaml:"run"`
	Params  map[string]*ParamSpec `yaml:"params"`
	Timeout time.Duration         `yaml:"timeout"`
}

type Set struct {
	Actions map[string]*Action `yaml:"actions"`
}

var placeholder = regexp.MustCompile(`\{\{([A-Za-z_][A-Za-z0-9_]*)\}\}`)

// Load reads and validates an actions.yaml file.
func Load(path string) (*Set, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

func Parse(b []byte) (*Set, error) {
	var s Set
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("actions: %w", err)
	}
	for name, a := range s.Actions {
		if a == nil || len(a.Run) == 0 || a.Run[0] == "" {
			return nil, fmt.Errorf("actions: %q: run must be a non-empty argv array", name)
		}
		if placeholder.MatchString(a.Run[0]) {
			return nil, fmt.Errorf("actions: %q: argv[0] must not contain placeholders", name)
		}
		if !filepath.IsAbs(a.Run[0]) {
			// Resolve once, at load time, against a fixed PATH (never the runner's environment).
			exe, err := lookPath(a.Run[0])
			if err != nil {
				return nil, fmt.Errorf("actions: %q: %w", name, err)
			}
			a.Run[0] = exe
		}
		if a.Timeout < 0 {
			return nil, fmt.Errorf("actions: %q: negative timeout", name)
		}
		if a.Timeout == 0 {
			a.Timeout = DefaultTimeout
		}
		for pn, p := range a.Params {
			if p == nil || p.Pattern == "" {
				return nil, fmt.Errorf("actions: %q: param %q needs a pattern", name, pn)
			}
			// Validate the raw pattern ALONE first: wrapping before compiling would let unbalanced
			// parens (`x)|(?:.*`) escape the full-match anchors.
			if _, err := regexp.Compile(p.Pattern); err != nil {
				return nil, fmt.Errorf("actions: %q: param %q: %w", name, pn, err)
			}
			// Force a full match regardless of how the author anchored the pattern.
			re, err := regexp.Compile(`\A(?:` + p.Pattern + `)\z`)
			if err != nil {
				return nil, fmt.Errorf("actions: %q: param %q: %w", name, pn, err)
			}
			p.re = re
			if p.Default != nil && !re.MatchString(*p.Default) {
				return nil, fmt.Errorf("actions: %q: default of %q does not match its pattern", name, pn)
			}
			if p.Default != nil && strings.HasPrefix(*p.Default, "-") && !p.AllowLeadingDash {
				return nil, fmt.Errorf("actions: %q: default of %q starts with '-' (set allow_leading_dash)", name, pn)
			}
		}
		for _, arg := range a.Run {
			for _, m := range placeholder.FindAllStringSubmatch(arg, -1) {
				if _, ok := a.Params[m[1]]; !ok {
					return nil, fmt.Errorf("actions: %q: placeholder {{%s}} has no param definition", name, m[1])
				}
			}
		}
	}
	return &s, nil
}

// lookPath finds name in fixedPath. The result is absolute.
func lookPath(name string) (string, error) {
	if strings.Contains(name, "/") {
		return "", fmt.Errorf("argv[0] %q must be an absolute path or a bare command name", name)
	}
	for _, d := range filepath.SplitList(fixedPath) {
		p := filepath.Join(d, name)
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("command %q not found in %s", name, fixedPath)
}

// Names returns the defined action names, sorted.
func (s *Set) Names() []string {
	var n []string
	for k := range s.Actions {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}

// ParamInfo describes one parameter of an action for discovery.
type ParamInfo struct {
	Name    string
	Pattern string
	Default *string // nil: the parameter is required
}

// ActionInfo describes one action for discovery. It carries no argv, path or environment.
type ActionInfo struct {
	Name   string
	Params []ParamInfo
}

// Describe lists the actions, sorted by name, with params sorted by name. It is informational
// (action discovery); authorisation always goes through Plan.
func (s *Set) Describe() []ActionInfo {
	out := make([]ActionInfo, 0, len(s.Actions))
	for _, name := range s.Names() {
		ai := ActionInfo{Name: name}
		a := s.Actions[name]
		pns := make([]string, 0, len(a.Params))
		for pn := range a.Params {
			pns = append(pns, pn)
		}
		sort.Strings(pns)
		for _, pn := range pns {
			p := a.Params[pn]
			pi := ParamInfo{Name: pn, Pattern: p.Pattern}
			if p.Default != nil {
				d := *p.Default
				pi.Default = &d
			}
			ai.Params = append(ai.Params, pi)
		}
		out = append(out, ai)
	}
	return out
}

// Plan validates a request and returns the final argv and timeout, or a *RefusedError.
func (s *Set) Plan(name string, params map[string]string) ([]string, time.Duration, error) {
	a, ok := s.Actions[name]
	if !ok {
		return nil, 0, refuse("unknown action %q", name)
	}
	for k := range params {
		if _, ok := a.Params[k]; !ok {
			return nil, 0, refuse("action %q has no parameter %q", name, k)
		}
	}
	vals := make(map[string]string, len(a.Params))
	for pn, p := range a.Params {
		v, given := params[pn]
		if !given {
			if p.Default == nil {
				return nil, 0, refuse("missing required parameter %q", pn)
			}
			v = *p.Default
		}
		if !p.re.MatchString(v) {
			return nil, 0, refuse("parameter %q does not match its pattern", pn)
		}
		if strings.HasPrefix(v, "-") && !p.AllowLeadingDash {
			return nil, 0, refuse("parameter %q must not start with '-'", pn)
		}
		vals[pn] = v
	}
	argv := make([]string, len(a.Run))
	for i, arg := range a.Run {
		// Single pass: substituted values are never re-scanned for placeholders.
		argv[i] = placeholder.ReplaceAllStringFunc(arg, func(m string) string {
			return vals[placeholder.FindStringSubmatch(m)[1]]
		})
	}
	return argv, a.Timeout, nil
}

type Result struct {
	Stdout, Stderr  string
	StdoutTruncated bool
	StderrTruncated bool
	ExitCode        int
	TimedOut        bool // set only if we killed the process because the deadline passed
	Duration        time.Duration
}

// Run plans and executes an action. Refusals return (nil, *RefusedError).
func (s *Set) Run(ctx context.Context, name string, params map[string]string) (*Result, error) {
	argv, timeout, err := s.Plan(name, params)
	if err != nil {
		return nil, err
	}
	return execArgv(ctx, argv, timeout)
}

const (
	killNone = iota
	killDeadline
	killCancel
)

func execArgv(ctx context.Context, argv []string, timeout time.Duration) (*Result, error) {
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	budget := &outputBudget{left: MaxOutput}
	so, se := &limitedBuffer{budget: budget}, &limitedBuffer{budget: budget}
	var killed atomic.Int32
	cmd := exec.CommandContext(tctx, argv[0], argv[1:]...) // argv, never a shell
	cmd.Stdout, cmd.Stderr = so, se
	cmd.Env = []string{"PATH=" + fixedPath, "LANG=C"} // minimal env: nothing inherited
	cmd.Dir = "/"
	prepareKill(cmd)
	cmd.Cancel = func() error {
		// Called only when the context ends while the process is still running.
		if ctx.Err() == nil && errors.Is(tctx.Err(), context.DeadlineExceeded) { // our deadline, not the caller's
			killed.CompareAndSwap(killNone, killDeadline)
		} else {
			killed.CompareAndSwap(killNone, killCancel)
		}
		return killTree(cmd)
	}
	cmd.WaitDelay = 2 * time.Second // don't hang on pipes held open by grandchildren
	start := time.Now()
	err := cmd.Run()
	// Nothing may outlive the action: whatever the outcome (clean exit, timeout, grandchild
	// holding the pipes), kill the whole process group once Wait has returned.
	if cmd.Process != nil {
		killTree(cmd)
	}
	res := &Result{Stdout: so.String(), Stderr: se.String(), StdoutTruncated: so.truncated, StderrTruncated: se.truncated, Duration: time.Since(start)}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		res.ExitCode = ee.ExitCode()
	case errors.Is(err, exec.ErrWaitDelay):
		// The process itself exited; only a grandchild kept the pipes open. Use its real status.
		if cmd.ProcessState != nil {
			res.ExitCode = cmd.ProcessState.ExitCode()
		}
	default: // could not start (binary missing, not executable)
		res.ExitCode = -1
		res.Stderr += err.Error()
	}
	switch killed.Load() {
	case killDeadline:
		res.TimedOut, res.ExitCode = true, -1
	case killCancel:
		res.ExitCode = -1
	}
	return res, nil
}

// outputBudget is the combined stdout+stderr allowance shared by both buffers.
type outputBudget struct {
	mu   sync.Mutex
	left int
}

type limitedBuffer struct {
	b         bytes.Buffer
	budget    *outputBudget
	truncated bool
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	l.budget.mu.Lock()
	n := min(l.budget.left, len(p))
	l.budget.left -= n
	l.budget.mu.Unlock()
	l.b.Write(p[:n])
	if n < len(p) {
		l.truncated = true
	}
	return len(p), nil
}
func (l *limitedBuffer) String() string { return l.b.String() }
