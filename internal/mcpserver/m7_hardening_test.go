package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/orch"
)

// Scrubbing neutralises (no byte deleted), also with escaped whitespace and the
// Claude Code tag names.
func TestScrubNeutralisesWithoutDeleting(t *testing.T) {
	for _, in := range []string{
		"a <system-reminder>keep this text</system-reminder> b",
		"a <system-reminder cut off and a long tail " + strings.Repeat("x", 300),
		"<bash-stdout>x</bash-stdout> <bash-stderr>y</bash-stderr> <bash-input>z</bash-input>",
		"<local-command-stderr>e</local-command-stderr> <task-notification>n</task-notification>",
		`<\nsystem-reminder>escaped</system-reminder>`,
		`<\u000a/system-reminder>escaped</system-reminder>`,
		"<\n\tSYSTEM_REMINDER>",
	} {
		var sc scrub
		got := sc.text(in)
		if !sc.warned {
			t.Errorf("%q: no warning", in)
		}
		if len(got) < len(in) {
			t.Errorf("%q: bytes were deleted: %q", in, got)
		}
		if strings.Contains(strings.ToLower(got), "<system") || strings.Contains(got, "<bash-") || strings.Contains(got, "<local-") ||
			strings.Contains(got, "<task-") || strings.Contains(strings.ToLower(got), "<\n") {
			t.Errorf("%q: tag survived: %q", in, got)
		}
		if strings.HasPrefix(in, "a <system-reminder>keep") && !strings.Contains(got, "keep this text") {
			t.Errorf("text after the tag was lost: %q", got)
		}
	}
	// The final pass sees the JSON form.
	for _, name := range []string{"bash-stdout", "local-command-stderr", "task-notification"} {
		b, _ := json.Marshal("<\n" + name + ">")
		if got := string(neutralize(b)); strings.Contains(got, `"<`) || strings.Contains(got, `<`) {
			t.Errorf("%s: %s", name, got)
		}
	}
	if got := string(neutralize([]byte(`1 < 2 <b>ok</b> <bash-other>`))); got != `1 < 2 <b>ok</b> <bash-other>` {
		t.Errorf("ordinary markup changed: %s", got)
	}
}

// The entries share what the content left of the ENCODED budget, not the raw length.
func TestMakeOutBudgetUsesEncodedContent(t *testing.T) {
	r := devRig(t)
	r.f.Cap = 2000
	var ents []api.FSEntry
	for i := 0; i < 60; i++ {
		ents = append(ents, api.FSEntry{Path: "/tmp/file-with-a-longish-name-" + strings.Repeat("a", 20), Size: 1, Mode: 0o644})
	}
	r.f.WorkspaceFSFn = func(context.Context, string, api.FSParams, []byte) (*api.FSResult, []byte, error) {
		// 800 quotes cost 1600 encoded bytes but are 800 raw bytes.
		return &api.FSResult{Kind: "text", Content: strings.Repeat(`"`, 800), Entries: ents}, nil, nil
	}
	v := r.mustCall(t, "read", `{"path":"a"}`)
	total := orch.EncodedCost(v["content"].(string))
	if es, ok := v["entries"].([]any); ok {
		for _, e := range es {
			b, _ := json.Marshal(e)
			total += len(b) + 1
		}
	}
	if total > 2000 {
		t.Fatalf("content and entries use %d encoded bytes, cap 2000", total)
	}
}
