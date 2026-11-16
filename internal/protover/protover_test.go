package protover

import "testing"

func TestParse(t *testing.T) {
	ok := map[string]Version{
		"0.6.0":         {0, 6, 0, ""},
		"v0.6.3":        {0, 6, 3, ""},
		"0.6.0-rc.1":    {0, 6, 0, "rc.1"},
		"v1.2.3+abc":    {1, 2, 3, ""},
		"1.0.0-alpha-1": {1, 0, 0, "alpha-1"},
	}
	for in, want := range ok {
		got, err := Parse(in)
		if err != nil || got != want {
			t.Errorf("Parse(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "dev", "(devel)", "0.6", "0.6.0.1", "0.06.0", "-1.0.0", "0.6.x", "0.6.0-", "0.6.0-a..b", "v", "1.2.3-é"} {
		if _, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) accepted", in)
		}
	}
}

func TestLevelOf(t *testing.T) {
	cases := map[string]int{"v0.6.0": 6, "0.6.0-rc.1": 6, "0.7.9": 7, "1.0.0": 1, "v2.5.1": 2, "0.0.1": 0}
	for in, want := range cases {
		if got, err := LevelOf(in); err != nil || got != want {
			t.Errorf("LevelOf(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	if _, err := LevelOf("dev"); err == nil {
		t.Error("LevelOf(dev) accepted")
	}
}

func TestCheck(t *testing.T) {
	cases := []struct {
		role          string
		central, peer int
		want          string // empty = accepted
	}{
		{"runner", 6, 6, ""},
		{"runner", 6, 5, ""},
		{"orchestrator", 6, 6, ""},
		{"orchestrator", 6, 5, ""},
		{"runner", 1, 1, ""},
		{"runner", 6, 4, "this runner is protocol 4, central needs 5 or 6: upgrade mirafleet on that runner"},
		{"orchestrator", 6, 3, "this orchestrator is protocol 3, central needs 5 or 6: upgrade mirafleet on that orchestrator"},
		{"runner", 6, 7, "central is protocol 6, this runner is 7: upgrade central first"},
		{"orchestrator", 6, 8, "central is protocol 6, this orchestrator is 8: upgrade central first"},
		{"runner", 6, 0, "this runner reports no protocol version, central needs 5 or 6: upgrade mirafleet on that runner"},
		{"runner", 6, -1, "this runner reports no protocol version, central needs 5 or 6: upgrade mirafleet on that runner"},
		{"runner", 1, 0, "this runner reports no protocol version, central needs 1: upgrade mirafleet on that runner"},
		{"runner", 1, 2, "central is protocol 1, this runner is 2: upgrade central first"},
	}
	for _, c := range cases {
		err := Check(c.role, "", c.central, c.peer)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("Check(%s,%d,%d) = %v; want accepted", c.role, c.central, c.peer, err)
		case c.want != "" && (err == nil || err.Error() != c.want):
			t.Errorf("Check(%s,%d,%d) = %v; want %q", c.role, c.central, c.peer, err, c.want)
		}
	}
}

// The refusal texts, verbatim.
func TestCheckSpecText(t *testing.T) {
	if got := Check("runner", "db-1", 6, 4).Error(); got !=
		"runner `db-1` is protocol 4, central needs 5 or 6: upgrade mirafleet on that runner" {
		t.Errorf("lower: %q", got)
	}
	if got := Check("runner", "", 6, 7).Error(); got !=
		"central is protocol 6, this runner is 7: upgrade central first" {
		t.Errorf("higher: %q", got)
	}
	if got := Check("runner", "db-1", 6, 7).Error(); got !=
		"central is protocol 6, runner `db-1` is 7: upgrade central first" {
		t.Errorf("higher named: %q", got)
	}
	if got := Check("orchestrator", "ci", 6, 0).Error(); got !=
		"orchestrator `ci` reports no protocol version, central needs 5 or 6: upgrade mirafleet on that orchestrator" {
		t.Errorf("missing: %q", got)
	}
}

func TestCheckVersions(t *testing.T) {
	const mixed = "mixed 0.x and 1.x: upgrade everything to 1.x, central first"
	cases := []struct{ central, peer, want string }{
		{"v0.6.3", "v0.6.0", ""},
		{"v0.6.3", "0.5.9", ""},
		{"1.0.0", "1.0.4", ""},
		{"1.1.0", "1.0.0", ""},
		{"1.0.0", "0.1.0", mixed}, // level 1 == 1 by coincidence
		{"1.0.0", "0.6.0", mixed},
		{"0.6.0", "1.0.0", mixed},
		{"0.1.0", "1.0.0", mixed},
		{"2.0.0", "0.1.0", mixed},
		{"v0.6.0", "0.4.0", "this runner is protocol 4, central needs 5 or 6: upgrade mirafleet on that runner"},
		{"v0.6.0", "0.7.0", "central is protocol 6, this runner is 7: upgrade central first"},
		{"1.2.0", "1.4.0", ""}, // minor and patch never matter from 1.0
		{"1.2.0", "2.0.0", "central is protocol 1, this runner is 2: upgrade central first"},
	}
	for _, c := range cases {
		err := CheckVersions("runner", "", c.central, c.peer)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("CheckVersions(%s,%s) = %v; want accepted", c.central, c.peer, err)
		case c.want != "" && (err == nil || err.Error() != c.want):
			t.Errorf("CheckVersions(%s,%s) = %v; want %q", c.central, c.peer, err, c.want)
		}
	}
	if CheckVersions("runner", "", "dev", "0.6.0") == nil || CheckVersions("runner", "", "0.6.0", "") == nil {
		t.Error("unparsable versions accepted")
	}
}

func TestCurrentMatchesPlan(t *testing.T) {
	if Current != 6 {
		t.Fatalf("Current = %d; v0.6.x must be 6", Current)
	}
}
