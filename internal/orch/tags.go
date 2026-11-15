package orch

import "regexp"

// TagRE matches harness control tags (open, close or cut off), both as a literal "<" and as the
// "<" that encoding/json writes. Whitespace between "<" and the name may be literal or
// JSON-escaped (\n, \t, \u000a ...). Group 1 is everything after the "<".
var TagRE = regexp.MustCompile(`(?i)(?:<|\\u003c)((?:\s|\\[nrtfv]|\\u00(?:09|0[a-d]|20))*/?(?:\s|\\[nrtfv]|\\u00(?:09|0[a-d]|20))*` +
	`(?:system[-_ ]?reminder|command-(?:name|message|args)|local-command-(?:stdout|stderr|caveat)|` +
	`bash-(?:stdout|stderr|input)|task-notification|user-prompt-submit-hook|function_calls|function_results|antml:))`)

// NeutralizeTags makes every harness control tag in s inert by turning its leading "<" into the
// visible "&lt;". No byte is deleted. changed reports whether anything was neutralised.
func NeutralizeTags(s string) (out string, changed bool) {
	if !TagRE.MatchString(s) {
		return s, false
	}
	return TagRE.ReplaceAllString(s, "&lt;$1"), true
}
