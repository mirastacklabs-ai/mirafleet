package store

import "testing"

func TestImageAllowedSharedSemantics(t *testing.T) {
	c := WorkspaceConfig{Images: []string{"golang:1.27", "ghcr.io/acme/dev@sha256:*", "ghcr.io/acme/tool:*"}}
	for ref, want := range map[string]bool{
		"docker.io/library/golang:1.27":     true,
		"golang:1.27":                       true,
		"golang:1.28":                       false,
		"ghcr.io/acme/dev@sha256:" + hex64:  true,
		"ghcr.io/acme/dev:latest":           false,
		"ghcr.io/acme/tool:v2":              true,
		"ghcr.io/acme/tool@sha256:" + hex64: false,
		"ghcr.io/acme/tool:*":               false,
		"ghcr.io/acme/toolbox:v2":           false,
	} {
		if got := c.ImageAllowed(ref); got != want {
			t.Errorf("ImageAllowed(%q) = %v, want %v", ref, got, want)
		}
	}
}

const hex64 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
