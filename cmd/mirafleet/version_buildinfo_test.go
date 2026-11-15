package main

import "testing"

func TestResolveVersion(t *testing.T) {
	cases := []struct{ linked, module, want string }{
		{"dev", "v0.6.0", "v0.6.0"},
		{"dev", "v0.6.0-rc.1", "v0.6.0-rc.1"},
		{"dev", "(devel)", "dev"},
		{"dev", "", "dev"},
		{"0.6.0", "v0.5.0", "0.6.0"},
		{"0.6.0", "(devel)", "0.6.0"},
	}
	for _, c := range cases {
		if got := resolveVersion(c.linked, c.module); got != c.want {
			t.Errorf("resolveVersion(%q,%q) = %q; want %q", c.linked, c.module, got, c.want)
		}
	}
}

func TestVersionNeverEmpty(t *testing.T) {
	if version == "" {
		t.Fatal("version is empty")
	}
}
