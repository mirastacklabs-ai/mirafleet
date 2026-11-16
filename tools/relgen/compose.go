package main

import (
	"fmt"
	"regexp"
)

// The Compose files pin the image tag; relgen bumps and checks it.
var composeImageRE = regexp.MustCompile(`(mirastack/mirafleet:)([0-9]+\.[0-9]+\.[0-9]+(?:-rc\.[0-9]+)?)`)

func composeTags(b []byte) ([]string, error) {
	m := composeImageRE.FindAllSubmatch(b, -1)
	if len(m) == 0 {
		return nil, fmt.Errorf("no mirastack/mirafleet:<version> image reference found")
	}
	var out []string
	for _, x := range m {
		out = append(out, string(x[2]))
	}
	return out, nil
}

func setComposeTags(b []byte, value string) ([]byte, error) {
	if _, err := composeTags(b); err != nil {
		return nil, err
	}
	return composeImageRE.ReplaceAll(b, []byte("${1}"+value)), nil
}

func init() {
	for _, p := range []string{
		"deploy/compose/central/compose.yaml",
		"deploy/compose/runner/compose.yaml",
		"deploy/compose/lab/compose.yaml",
		"deploy/compose/runner-workspace/compose.yaml",
	} {
		addTarget(target{Path: p, Get: composeTags, Set: setComposeTags})
	}
}
