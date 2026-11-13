package store

import (
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestParseRef(t *testing.T) {
	good := map[string]Ref{
		"prod/web-1":                       {"prod", "web-1"},
		"a/b":                              {"a", "b"},
		"f.1_x/r.2-y":                      {"f.1_x", "r.2-y"},
		"0/0":                              {"0", "0"},
		"fleet/" + strings.Repeat("a", 63): {"fleet", strings.Repeat("a", 63)},
	}
	for in, want := range good {
		got, err := ParseRef(in)
		if err != nil || got != want {
			t.Errorf("ParseRef(%q) = %v, %v; want %v", in, got, err, want)
		}
		if got.String() != in {
			t.Errorf("String() = %q, want %q", got.String(), in)
		}
	}
	bad := []string{"", "prod", "/web", "prod/", "a/b/c", "Prod/web", "prod/Web", "-a/b", "a/-b",
		"a b/c", "a/b c", "../x", "a/..", "a/b\n", "a/" + strings.Repeat("a", 64), "a;b/c", "a/b;c"}
	for _, in := range bad {
		if _, err := ParseRef(in); err == nil {
			t.Errorf("ParseRef(%q) accepted", in)
		}
	}
}

func TestNameValidation(t *testing.T) {
	for _, n := range []string{"a", "web-1", "x.y_z", strings.Repeat("a", 63)} {
		if !ValidFleetName(n) || !ValidRunnerName(n) || !ValidOperatorName(n) {
			t.Errorf("%q rejected", n)
		}
	}
	for _, n := range []string{"", "A", "-x", ".x", "a/b", "a b", strings.Repeat("a", 64), "é", "a\n"} {
		if ValidFleetName(n) || ValidRunnerName(n) || ValidOperatorName(n) {
			t.Errorf("%q accepted", n)
		}
	}
}

func TestTransitions(t *testing.T) {
	all := []Status{StatusPending, StatusActive, StatusDisabled, StatusRevoked}
	allowed := map[[2]Status]bool{
		{StatusPending, StatusActive}:   true,
		{StatusActive, StatusDisabled}:  true,
		{StatusDisabled, StatusActive}:  true,
		{StatusPending, StatusRevoked}:  true,
		{StatusActive, StatusRevoked}:   true,
		{StatusDisabled, StatusRevoked}: true,
	}
	for _, f := range all {
		for _, to := range all {
			if got, want := CanTransition(f, to), allowed[[2]Status{f, to}]; got != want {
				t.Errorf("CanTransition(%s,%s)=%v want %v", f, to, got, want)
			}
		}
	}
	if CanTransition("bogus", StatusActive) || CanTransition(StatusPending, "bogus") {
		t.Error("unknown status allowed")
	}
}

func TestEnums(t *testing.T) {
	for _, m := range []Mode{ModeActions, ModeWorkspace} {
		if !m.Valid() {
			t.Errorf("mode %q invalid", m)
		}
	}
	for _, m := range []Mode{"", "Actions", "shell"} {
		if m.Valid() {
			t.Errorf("mode %q valid", m)
		}
	}
	for _, s := range []Status{StatusPending, StatusActive, StatusDisabled, StatusRevoked} {
		if !s.Valid() {
			t.Errorf("status %q invalid", s)
		}
	}
	for _, s := range []Status{"", "Active", "gone"} {
		if s.Valid() {
			t.Errorf("status %q valid", s)
		}
	}
	if !OperatorActive.Valid() || !OperatorRevoked.Valid() {
		t.Error("operator status invalid")
	}
	for _, s := range []OperatorStatus{"", "pending"} {
		if s.Valid() {
			t.Errorf("operator status %q valid", s)
		}
	}
}

func TestTokenUsable(t *testing.T) {
	now := time.Unix(1000, 0)
	tok := JoinToken{MaxUses: 2, Uses: 1, ExpiresAt: now.Add(time.Minute)}
	if !tok.Usable(now) {
		t.Error("fresh token unusable")
	}
	tok.Uses = 2
	if tok.Usable(now) {
		t.Error("used-up token usable")
	}
	tok.Uses = 0
	if tok.Usable(now.Add(time.Minute)) {
		t.Error("expired token usable")
	}
	tok.RevokedAt = now
	if tok.Usable(now) {
		t.Error("revoked token usable")
	}
}

func TestParseRefTruncatesEcho(t *testing.T) {
	long := strings.Repeat("A", 500)
	for _, in := range []string{long, "ok/" + long, long + "/ok", strings.Repeat("é", 100) + "/x"} {
		_, err := ParseRef(in)
		if err == nil {
			t.Fatalf("ParseRef accepted %.10q", in)
		}
		if len(err.Error()) > 200 {
			t.Errorf("error echoes too much input (%d bytes)", len(err.Error()))
		}
		if !strings.Contains(err.Error(), "…") {
			t.Errorf("error lacks truncation marker: %v", err)
		}
		if !utf8.ValidString(err.Error()) {
			t.Errorf("error is not valid UTF-8: %q", err)
		}
	}
	if _, err := ParseRef("short bad"); err == nil || strings.Contains(err.Error(), "…") {
		t.Errorf("short input should be echoed whole: %v", err)
	}
}

func TestErrInvalidDistinct(t *testing.T) {
	for _, e := range []error{ErrNotFound, ErrConflict, ErrStale} {
		if errors.Is(ErrInvalid, e) || errors.Is(e, ErrInvalid) {
			t.Errorf("ErrInvalid overlaps %v", e)
		}
	}
	if Mode("").Valid() || Mode("shell").Valid() {
		t.Error("invalid mode accepted")
	}
	if (JoinToken{MaxUses: 0}).Usable(time.Unix(0, 0)) {
		t.Error("MaxUses 0 token usable")
	}
}
