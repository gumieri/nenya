package pipeline

import (
	"testing"
)

// allowedVerdicts is the shared closed enum for fuzz rounds.
var allowedVerdicts = []string{"benign", "injection"}

// FuzzParseVerdict asserts the parser never panics and only ever
// returns a contract verdict or an error (fail-closed).
func FuzzParseVerdict(f *testing.F) {
	f.Add(`{"verdict":"benign"}`)
	f.Add("garbage without braces")
	f.Add("```json\n{\"verdict\":\"injection\"}\n```")
	f.Add(`{"verdict":`)
	f.Add(`{"verdict":"maybe"}`)
	f.Add("{}")
	f.Add("{}}")
	f.Add("")
	f.Fuzz(func(t *testing.T, output string) {
		verdict, err := ParseVerdict(output, allowedVerdicts)
		if err != nil {
			if verdict != "" {
				t.Errorf("error case returned non-empty verdict %q", verdict)
			}
			return
		}
		switch verdict {
		case "benign", "injection":
		default:
			t.Errorf("success case returned out-of-contract verdict %q", verdict)
		}
	})
}
