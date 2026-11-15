// Package protover is the protocol-level rule for version skew between central, runners and
// orchestrators (docs/M6.md section 2.2). It is a pure rule package: it has no I/O and is not
// yet wired into the SSH handshake.
//
// The protocol level is one integer: the release's minor version while in 0.x (v0.6.x is 6)
// and the major version from 1.0 on (v1.4.2 is 1). Patch and pre-release parts never matter,
// so 0.6.0-rc.1 and 0.6.3 are both level 6.
//
// Rule: central at level N accepts peers at N and N-1. Anything else is refused with a message
// naming who must upgrade. Level 0 means "missing" (a pre-0.6 dev build that sends no proto)
// and is always refused. Upgrade order: central first, then runners, then operators.
//
// Note: the level restarts at 1 with v1.0. A 1.x central therefore does not accept 0.x peers;
// that is a deliberate one-time break at 1.0, to be called out in the 1.0 release notes.
// Callers that hold both release versions use CheckVersions, which refuses a 0.x/1.x mix with
// MixedMajorMessage before comparing levels. `relgen bump` and `relgen check` keep Current equal
// to the level of the release tag.
package protover

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Current is the protocol level of this build (v0.6.x).
const Current = 6

// Version is a parsed semantic version. Pre is the pre-release text without the dash.
type Version struct {
	Major, Minor, Patch int
	Pre                 string
}

// Parse accepts "v0.6.0", "0.6.0" and "0.6.0-rc.1"; build metadata ("+abc") is dropped.
func Parse(s string) (Version, error) {
	orig := s
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	var pre string
	if i := strings.IndexByte(s, '-'); i >= 0 {
		s, pre = s[:i], s[i+1:]
		if !validPre(pre) {
			return Version{}, fmt.Errorf("protover: invalid version %q", orig)
		}
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("protover: invalid version %q", orig)
	}
	var n [3]int
	for i, p := range parts {
		v, err := num(p)
		if err != nil {
			return Version{}, fmt.Errorf("protover: invalid version %q", orig)
		}
		n[i] = v
	}
	return Version{n[0], n[1], n[2], pre}, nil
}

// num parses a non-negative decimal without a sign or leading zeros.
func num(p string) (int, error) {
	if p == "" || len(p) > 6 || (len(p) > 1 && p[0] == '0') {
		return 0, fmt.Errorf("bad number")
	}
	for _, c := range p {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("bad number")
		}
	}
	return strconv.Atoi(p)
}

func validPre(p string) bool {
	if p == "" {
		return false
	}
	for _, id := range strings.Split(p, ".") {
		if id == "" {
			return false
		}
		for _, c := range id {
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-') {
				return false
			}
		}
	}
	return true
}

// Level is the protocol level: the minor in 0.x, the major from 1.0.
func (v Version) Level() int {
	if v.Major == 0 {
		return v.Minor
	}
	return v.Major
}

// LevelOf parses a version string and returns its protocol level.
func LevelOf(s string) (int, error) {
	v, err := Parse(s)
	if err != nil {
		return 0, err
	}
	return v.Level(), nil
}

// Accepts reports whether a central at level central accepts a peer at level peer.
func Accepts(central, peer int) bool {
	return peer >= 1 && (peer == central || peer == central-1)
}

// MixedMajorMessage is the refusal for a 0.x peer talking to a 1.x peer (or the reverse). The
// level restarts at 1 with v1.0, so a bare level comparison cannot tell 0.1 from 1.x.
const MixedMajorMessage = "mixed 0.x and 1.x: upgrade everything to 1.x, central first"

// Check applies the rule for one peer. role is "runner" or "orchestrator". name is the peer's
// name, or "" when unknown. It returns nil when accepted; otherwise an error whose text names the
// side to upgrade, exactly as in docs/M6.md section 2.2:
//
//	runner `db-1` is protocol 4, central needs 5 or 6: upgrade mirafleet on that runner
//	central is protocol 6, this runner is 7: upgrade central first
//
// With an empty name the subject reads "this runner".
func Check(role, name string, central, peer int) error {
	subject := "this " + role
	if name != "" {
		subject = role + " `" + name + "`"
	}
	switch {
	case Accepts(central, peer):
		return nil
	case peer < 1:
		return fmt.Errorf("%s reports no protocol version, central needs %s: upgrade mirafleet on that %s",
			subject, need(central), role)
	case peer > central:
		return fmt.Errorf("central is protocol %d, %s is %d: upgrade central first", central, subject, peer)
	default:
		return fmt.Errorf("%s is protocol %d, central needs %s: upgrade mirafleet on that %s",
			subject, peer, need(central), role)
	}
}

// CheckVersions is Check for callers that hold both release versions (for example "v0.6.3").
// It first refuses any mix of 0.x and 1.x-or-later, so the level restart at 1.0 can neither give
// a wrong-direction message nor accept a 0.1 peer by numeric coincidence, then applies Check to
// the levels.
func CheckVersions(role, name, centralVersion, peerVersion string) error {
	c, err := Parse(centralVersion)
	if err != nil {
		return err
	}
	p, err := Parse(peerVersion)
	if err != nil {
		return err
	}
	if (c.Major == 0) != (p.Major == 0) {
		return errors.New(MixedMajorMessage)
	}
	return Check(role, name, c.Level(), p.Level())
}

// need renders the accepted levels, "5 or 6", or just "1" at the lowest level.
func need(central int) string {
	if central <= 1 {
		return strconv.Itoa(central)
	}
	return fmt.Sprintf("%d or %d", central-1, central)
}
