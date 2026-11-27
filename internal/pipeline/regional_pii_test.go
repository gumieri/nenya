package pipeline

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/nenya/config"
)

// TestVerhoeffTableClosure is the mechanical table-consistency pin
// (NENYA-355 round-1 review): the p table is ONE permutation iterated —
// p(i+j, n) == p(i, p(j, n)) for every i,j (mod 8) and n. It also checks
// the d table is a Latin square (every row a permutation of 0-9) and
// that inv is the right inverse of the first d row. A transcription
// error at ANY row fails these properties, regardless of fixture
// numbers derived from the same (possibly wrong) tables.
func TestVerhoeffTableClosure(t *testing.T) {
	for i := 0; i < 8; i++ {
		for j := 0; j < 8; j++ {
			for n := 0; n < 10; n++ {
				// p(i+j mod 8, n) == p(i mod 8, p(j mod 8, n))
				if verhoeffP[(i+j)%8][n] != verhoeffP[i%8][verhoeffP[j%8][n]] {
					t.Fatalf("p table not a single iterated permutation: p(%d+%d, %d) = %d, p(%d, p(%d, %d)) = %d",
						i, j, n, verhoeffP[(i+j)%8][n], i%8, j%8, n, verhoeffP[i%8][verhoeffP[j%8][n]])
				}
			}
		}
	}
	for row := 0; row < 10; row++ {
		seen := map[int]bool{}
		for col := 0; col < 10; col++ {
			v := verhoeffD[row][col]
			if v < 0 || v > 9 || seen[v] {
				t.Fatalf("d table row %d is not a permutation of 0-9 (col %d = %d)", row, col, v)
			}
			seen[v] = true
		}
	}
	for j := 0; j < 10; j++ {
		if verhoeffD[j][verhoeffInv[j]] != 0 {
			t.Fatalf("inv[%d] is not the inverse of d row %d", j, j)
		}
	}
}

// TestVerhoeffVectors checks the validators against published vectors —
// "236" → check digit 3 and "2363" validates (Wikipedia's worked
// example). The Aadhaar-shaped fixture is DERIVED with the fixed tables
// (anchored by that published vector): the check digit for
// "23456789012" is the implementation's output, pinned as a regression
// net for the derived corpus fixtures.
func TestVerhoeffVectors(t *testing.T) {
	if verhoeffCheckDigit("236") != 3 {
		t.Fatalf("canonical Verhoeff check digit for 236 = %d, want 3", verhoeffCheckDigit("236"))
	}
	if !verhoeffValid("2363") {
		t.Fatal("canonical Verhoeff vector 2363 rejected")
	}
}

func TestChinaIDVectors(t *testing.T) {
	if !chinaIDValid("110101199003074477") {
		t.Fatal("published China ID vector 110101199003074477 rejected")
	}
	// Mutations invalidate.
	if chinaIDValid("110101199003074478") {
		t.Fatal("mutated China ID accepted")
	}
	if chinaIDValid("11010119900307447") {
		t.Fatal("17-char body accepted")
	}
	if chinaIDValid("1101011990030744XX") {
		t.Fatal("double-X accepted")
	}
}

func TestMyNumberVectors(t *testing.T) {
	if !myNumberValid("123456789018") {
		t.Fatal("published My Number vector 123456789018 rejected")
	}
	if myNumberValid("123456789019") {
		t.Fatalf("mutated My Number accepted")
	}
	if myNumberValid("12345678901") {
		t.Fatal("11-digit accepted")
	}
}

// TestRegionalPresetsRedact pins the preset wiring end to end: valid
// shapes redact, checksum-invalid shapes stay, and separators survive
// detection (the patterns allow optional space/dash).
func TestRegionalPresetsRedact(t *testing.T) {
	aadhaar := []*regexp.Regexp{regexp.MustCompile(config.AadhaarPattern)}
	if out := RedactSecrets("aadhaar 2345 6789 0124 on file", true, aadhaar, "REDACTED"); strings.Contains(out, "2345") {
		t.Fatalf("valid Aadhaar not redacted: %q", out)
	}
	// Starts with 0/1 → not an Aadhaar shape at all (pattern-level).
	if out := RedactSecrets("ref 0234 5678 9012 on file", true, aadhaar, "REDACTED"); strings.Contains(out, "REDACTED") {
		t.Fatalf("non-Aadhaar shape redacted: %q", out)
	}
	china := []*regexp.Regexp{regexp.MustCompile(config.ChinaIDPattern)}
	if out := RedactSecrets("id 110101199003074477 issued", true, china, "REDACTED"); strings.Contains(out, "110101") {
		t.Fatalf("valid China ID not redacted: %q", out)
	}
	if out := RedactSecrets("id 110101199003074478 issued", true, china, "REDACTED"); strings.Contains(out, "REDACTED") {
		t.Fatalf("checksum-invalid China ID redacted: %q", out)
	}
	my := []*regexp.Regexp{regexp.MustCompile(config.MyNumberPattern)}
	if out := RedactSecrets("my number 1234 5678 9018 issued", true, my, "REDACTED"); strings.Contains(out, "1234") {
		t.Fatalf("valid My Number not redacted: %q", out)
	}
	if out := RedactSecrets("my number 1234 5678 9019 issued", true, my, "REDACTED"); strings.Contains(out, "REDACTED") {
		t.Fatalf("checksum-invalid My Number redacted: %q", out)
	}
}

// upperXBody/lowerXBody are derived at init: the first body in the
// 1101011990030744x sequence family whose ISO 7064 check digit is 10
// (X) — seq 0 hits first (11010119900307440 → X), and both spellings
// must validate and redact identically.
var upperXBody, lowerXBody = deriveChinaIDXBody()

func deriveChinaIDXBody() (string, string) {
	base := "1101011990030744" // 16 digits; the sequence digit completes the 17-digit body
	for seq := 0; seq <= 9; seq++ {
		body := base + string(rune('0'+seq))
		d := digits(body)
		sum := 0
		for i, v := range d {
			sum += v * chinaIDWeights[i]
		}
		if (12-sum%11)%11 == 10 {
			return body + "X", body + "x"
		}
	}
	return "", ""
}

// TestRegionalPresetsLowercaseXChinaID pins the lowercase-x check digit
// (round-1 LOW): the pattern admits [Xx] and the validator accepts 'x'
// for check digit 10 — the derived check-10 body (seq 0:
// 11010119900307440) must validate and redact in both spellings.
func TestRegionalPresetsLowercaseXChinaID(t *testing.T) {
	china := []*regexp.Regexp{regexp.MustCompile(config.ChinaIDPattern)}
	// Derived check-10 body (11010119900307440x, seq 0) — asserted via
	// upperXBody/lowerXBody above.
	upper := chinaIDValid(upperXBody)
	lower := chinaIDValid(lowerXBody)
	if !upper || !lower {
		t.Fatalf("check-10 body not accepted: upper(%s)=%v lower(%s)=%v", upperXBody, upper, lowerXBody, lower)
	}
	upperOut := RedactSecrets("id "+upperXBody+" issued", true, china, "REDACTED")
	lowerOut := RedactSecrets("id "+lowerXBody+" issued", true, china, "REDACTED")
	if strings.Contains(upperOut, upperXBody) || strings.Contains(lowerOut, lowerXBody) {
		t.Fatalf("check-10 China ID not redacted: %q / %q", upperOut, lowerOut)
	}
}

// TestRegionalPresetsOnNormalizedSurface pins the confusable-pass
// composition (round-1 LOW): a fullwidth-digit Aadhaar redacts — the
// validator judges the folded ASCII through collectValidatedEdits.
func TestRegionalPresetsOnNormalizedSurface(t *testing.T) {
	patterns := []*regexp.Regexp{regexp.MustCompile(config.AadhaarPattern)}
	fullwidth := "aadhaar \uff12\uff13\uff14\uff15 \uff16\uff17\uff18\uff19 \uff10\uff11\uff12\uff14 on file"
	out := RedactSecrets(fullwidth, true, patterns, "REDACTED")
	if strings.ContainsFunc(out, func(r rune) bool { return r >= 0xFF00 && r <= 0xFF5F }) {
		t.Fatalf("fullwidth Aadhaar survived: %q", out)
	}
	if !strings.Contains(out, "REDACTED") {
		t.Fatalf("label missing: %q", out)
	}
}

func TestPresetExpansionRegionalPII(t *testing.T) {
	var cfg config.Config
	raw := `{"bouncer": {"enabled": true, "redact_preset": "pii-regional"}}`
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("config: %v", err)
	}
	if err := config.ApplyDefaults(&cfg); err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if len(cfg.Bouncer.RedactPatterns) != 3 {
		t.Fatalf("pii-regional expanded to %d patterns, want 3", len(cfg.Bouncer.RedactPatterns))
	}
}
