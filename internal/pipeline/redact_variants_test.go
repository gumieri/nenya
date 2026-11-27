package pipeline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/nenya/config"
)

// redactionPresetFixture carries the compiled pattern sources of one redact
// preset plus the label the gateway would apply with the same config, so the
// corpus exercises the exact request/output redaction pair.
type redactionPresetFixture struct {
	Patterns []*regexp.Regexp
	Label    string
}

// redactionVariantCorpus is the Module B Phase 010 adversarial variant corpus
// for Tier-0 secret and PII redaction (NENYA-343): the shapes redaction is
// suspected to miss, pinned exactly as the code behaves. detected=false cases
// are documented non-detections — the phase named in plannedFix (B020 =
// JSON-aware tool-call arguments, B030 = confusable and hidden-text candidate
// matching) must flip the expectation in the same commit that closes them;
// accepted=true cases are permanent limitations with no planned fix, whose
// notes must argue the tradeoff.
type redactionVariantCorpus struct {
	Cases []struct {
		Name       string `json:"name"`
		Preset     string `json:"preset"`
		Surface    string `json:"surface,omitempty"`
		Text       string `json:"text"`
		Value      string `json:"value"`
		Detected   bool   `json:"detected"`
		Benign     bool   `json:"benign,omitempty"`
		Accepted   bool   `json:"accepted,omitempty"`
		PlannedFix string `json:"plannedFix,omitempty"`
		Note       string `json:"note,omitempty"`
	} `json:"cases"`
}

// knownRedactionSurfaces is the closed set of surface kinds a corpus case
// may declare: "" runs the flat RedactSecrets path, "toolargs" runs the
// JSON-aware encoded-arguments redactor the tool-call surface uses in
// production. A typo'd surface would silently exercise the wrong entry
// point, so the corpus rejects unknown values.
var knownRedactionSurfaces = map[string]bool{
	"":         true,
	"toolargs": true,
}

// knownRedactionPresets guards the corpus against a typo'd preset name:
// expandRedactPreset only warns on unknown names and falls back to the
// credentials patterns, which would silently pin cases against the wrong
// pattern set.
var knownRedactionPresets = map[string]bool{
	"credentials": true,
	"financial":   true,
	"pii":         true,
}

// validRedactionPlannedFixes is the closed set of phases a pinned
// non-detection may name; a typo would otherwise void the flip contract.
var validRedactionPlannedFixes = map[string]bool{
	"B020": true,
	"B030": true,
}

// mustCompileRedactionPatterns compiles pattern sources or panics; shared by
// the corpus test and the fuzz setup so there is exactly one expansion path.
func mustCompileRedactionPatterns(sources []string) []*regexp.Regexp {
	patterns := make([]*regexp.Regexp, 0, len(sources))
	for _, src := range sources {
		patterns = append(patterns, regexp.MustCompile(src))
	}
	return patterns
}

// redactionPresetForTest compiles the named preset through the same expansion
// path the gateway uses (config defaults), so the corpus exercises the real
// pattern sources — including the checksum-validated financial patterns —
// without exporting the package-level preset map.
func redactionPresetForTest(tb testing.TB, name string) *redactionPresetFixture {
	tb.Helper()
	var cfg config.Config
	raw := `{"bouncer": {"enabled": true, "redact_preset": "` + name + `"}}`
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		tb.Fatalf("preset %q config: %v", name, err)
	}
	if err := config.ApplyDefaults(&cfg); err != nil {
		tb.Fatalf("preset %q defaults: %v", name, err)
	}
	if len(cfg.Bouncer.RedactPatterns) == 0 {
		tb.Fatalf("preset %q expanded to no patterns", name)
	}
	return &redactionPresetFixture{
		Patterns: mustCompileRedactionPatterns(cfg.Bouncer.RedactPatterns),
		Label:    cfg.Bouncer.RedactionLabel,
	}
}

// TestRedactionVariantTaxonomy runs the committed redaction corpus. A
// detected=true case must have its value replaced; a benign case must pass
// through unchanged; a detected=false case is a documented non-detection the
// corpus pins (the value survives) so a fix cannot silently claim coverage —
// closing one requires flipping the expectation in the same commit.
func TestRedactionVariantTaxonomy(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "redaction_variants.json"))
	if err != nil {
		t.Fatalf("reading corpus: %v", err)
	}
	var corpus redactionVariantCorpus
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatalf("parsing corpus: %v", err)
	}
	if len(corpus.Cases) == 0 {
		t.Fatal("corpus is empty")
	}

	seen := make(map[string]bool, len(corpus.Cases))
	presets := make(map[string]*redactionPresetFixture)
	for _, tc := range corpus.Cases {
		if seen[tc.Name] {
			t.Fatalf("duplicate case name %q", tc.Name)
		}
		seen[tc.Name] = true
		if !knownRedactionPresets[tc.Preset] {
			t.Fatalf("case %q: unknown preset %q (expandRedactPreset would silently fall back to credentials)", tc.Name, tc.Preset)
		}
		if !knownRedactionSurfaces[tc.Surface] {
			t.Fatalf("case %q: unknown surface %q (the corpus would silently exercise the wrong redaction entry point)", tc.Name, tc.Surface)
		}
		if !tc.Benign && tc.Value == "" {
			t.Fatalf("case %q: non-benign cases must carry a non-empty value", tc.Name)
		}
		if tc.Accepted && tc.Note == "" {
			t.Fatalf("case %q: an accepted limitation must argue its tradeoff in the note", tc.Name)
		}
		if tc.Benign && tc.Detected {
			t.Fatalf("case %q: benign and detected are mutually exclusive", tc.Name)
		}
		if tc.Accepted && tc.PlannedFix != "" {
			t.Fatalf("case %q: accepted and plannedFix are mutually exclusive", tc.Name)
		}
		if tc.Detected && (tc.PlannedFix != "" || tc.Accepted) {
			t.Fatalf("case %q: a detected=true case must not carry plannedFix or accepted", tc.Name)
		}
		if tc.PlannedFix != "" && !validRedactionPlannedFixes[tc.PlannedFix] {
			t.Fatalf("case %q: plannedFix %q is not a known fixing phase", tc.Name, tc.PlannedFix)
		}
		if tc.Benign && (tc.Value != "" || tc.PlannedFix != "" || tc.Accepted) {
			t.Fatalf("case %q: benign cases carry no value, plannedFix or accepted", tc.Name)
		}
		if !tc.Benign && !strings.Contains(tc.Text, tc.Value) {
			t.Fatalf("case %q: value %q does not occur in the case text, so the assertion would be vacuous", tc.Name, tc.Value)
		}
		if !tc.Detected && !tc.Benign && tc.PlannedFix == "" && !tc.Accepted {
			t.Fatalf("case %q: a detected=false non-benign case must set plannedFix or accepted", tc.Name)
		}
		fixture, ok := presets[tc.Preset]
		if !ok {
			fixture = redactionPresetForTest(t, tc.Preset)
			presets[tc.Preset] = fixture
		}
		t.Run(tc.Name, func(t *testing.T) {
			sanitized := RedactSecrets(tc.Text, true, fixture.Patterns, fixture.Label)
			if tc.Surface == "toolargs" {
				// The tool-call arguments surface runs the JSON-aware
				// redactor in production (NENYA-348): the corpus must
				// exercise the same entry point.
				sanitized = RedactJSONEncoded(tc.Text, true, fixture.Patterns, fixture.Label)
			}
			switch {
			case tc.Benign:
				if sanitized != tc.Text {
					t.Fatalf("benign case was redacted: %q -> %q", tc.Text, sanitized)
				}
			case tc.Detected:
				if strings.Contains(sanitized, tc.Value) {
					t.Fatalf("redaction left the value %q in the sanitized copy: %q", tc.Value, sanitized)
				}
			default:
				if !strings.Contains(sanitized, tc.Value) {
					t.Fatalf("pinned non-detection for %q no longer reproduces (%q -> %q): the fixing phase must flip detected to true in the corpus in the same commit", tc.Value, tc.Text, sanitized)
				}
			}
		})
	}
}

var fuzzCardRe = regexp.MustCompile(config.FinancialCardPattern)

// isRedactionWordByte mirrors the regexp \b word set: a match whose neighbors
// are word bytes is not boundary-clean and the pattern legitimately keeps it.
func isRedactionWordByte(b byte) bool {
	return b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// FuzzRedactVariantScan exercises RedactSecrets on arbitrary text with two
// presets. Invariants: valid UTF-8 in, valid UTF-8 out; every match of an
// unvalidated credentials pattern must be gone (a single non-overlapping
// replace pass cannot resurrect an exact match); every boundary-clean
// Luhn-valid card in the input must be gone from the financial-preset output.
func FuzzRedactVariantScan(f *testing.F) {
	for _, seed := range []string{
		"pay with 4111-1111-1111-1111 today",
		"4111\u200b1111\u200b1111\u200b1111",
		"\uff14\uff11\uff11\uff11 \uff11\uff11\uff11\uff11 \uff11\uff11\uff11\uff11 \uff11\uff11\uff11\uff11",
		"IBAN DE89 3704 0044 0532 0130 00 on file",
		"DE89\u00a03704\u00a00044\u00a00532\u00a00130\u00a000",
		"x4111111111111111",
		"41111111111111114111111111111111",
		"CPF 123.456.789-09 on file",
		"the leaked token is ghp_RkJDTQZxVpSMQvLmNoWwTkXrYbZnHjKlMnPq in the log",
		"set the password = \"hunter2secret\" in the config",
		"open ports 3000-4000 and 8080",
		"",
	} {
		f.Add(seed)
	}
	cred := redactionPresetForTest(f, "credentials")
	fin := redactionPresetForTest(f, "financial")
	f.Fuzz(func(t *testing.T, text string) {
		if !utf8.ValidString(text) {
			t.Skip()
		}
		credOut := RedactSecrets(text, true, cred.Patterns, cred.Label)
		if !utf8.ValidString(credOut) {
			t.Fatalf("RedactSecrets broke UTF-8: in %q out %q", text, credOut)
		}
		// The credentials preset registers no checksum validators, so every
		// input match must be replaced wholesale.
		for _, re := range cred.Patterns {
			for _, loc := range re.FindAllStringIndex(text, -1) {
				if strings.Contains(credOut, text[loc[0]:loc[1]]) {
					t.Fatalf("credentials match %q survived redaction: %q", text[loc[0]:loc[1]], credOut)
				}
			}
		}
		finOut := RedactSecrets(text, true, fin.Patterns, fin.Label)
		if !utf8.ValidString(finOut) {
			t.Fatalf("RedactSecrets broke UTF-8: in %q out %q", text, finOut)
		}
		for _, loc := range fuzzCardRe.FindAllStringIndex(text, -1) {
			match := text[loc[0]:loc[1]]
			if !luhnValid(match) {
				continue
			}
			if loc[0] > 0 && isRedactionWordByte(text[loc[0]-1]) {
				continue
			}
			if loc[1] < len(text) && isRedactionWordByte(text[loc[1]]) {
				continue
			}
			if strings.Contains(finOut, match) {
				t.Fatalf("Luhn-valid card %q survived redaction: %q", match, finOut)
			}
		}
	})
}
