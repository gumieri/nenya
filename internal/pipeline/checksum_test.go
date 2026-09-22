package pipeline

import (
	"regexp"
	"testing"

	"github.com/nenya/config"
)

func TestLuhnValid(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"visa test card", "4111111111111111", true},
		{"amex test card", "378282246310005", true},
		{"spaced card", "4111 1111 1111 1111", true},
		{"dashed card", "4111-1111-1111-1111", true},
		{"bad check digit", "4111111111111112", false},
		{"twelve digits rejected by length", "411111111111", false},
		{"twenty digits rejected by length", "41111111111111111111", false},
		{"all zeros rejected as dummy", "0000000000000", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := luhnValid(tt.input); got != tt.want {
				t.Errorf("luhnValid(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestIBANMod97Valid(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"german iban", "DE89370400440532013000", true},
		{"uk iban", "GB82WEST12345698765432", true},
		{"french iban", "FR1420041010050500013M02606", true},
		{"spaced german iban", "DE89 3704 0044 0532 0130 00", true},
		{"lowercase german iban", "de89370400440532013000", true},
		{"bad check digits", "DE89370400440532013001", false},
		{"fourteen chars rejected by length", "DE89370400440", false},
		{"thirty-five chars rejected by length", "DE893704004405320130000000000000000", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ibanMod97Valid(tt.input); got != tt.want {
				t.Errorf("ibanMod97Valid(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestCPFValid(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"valid cpf", "111.444.777-35", true},
		{"bad first check digit", "111.444.777-34", false},
		{"bad second check digit", "111.444.777-45", false},
		{"uniform digits rejected as dummy", "111.111.111-11", false},
		{"wrong digit count", "111.444.777-355", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cpfValid(tt.input); got != tt.want {
				t.Errorf("cpfValid(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestCNPJValid(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"valid cnpj", "11.222.333/0001-81", true},
		{"bad first check digit", "11.222.333/0001-80", false},
		{"bad second check digit", "11.222.333/0001-91", false},
		{"uniform digits rejected as dummy", "00.000.000/0000-00", false},
		{"wrong digit count", "11.222.333/0001-811", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cnpjValid(tt.input); got != tt.want {
				t.Errorf("cnpjValid(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

// TestFinancialPresetValidatorCoverage guards against preset drift in both
// directions: every financial preset pattern must have a checksum validator
// registered (else it silently redacts unconditionally), and every
// registered validator must correspond to a live preset pattern (else the
// gating silently stopped applying).
func TestFinancialPresetValidatorCoverage(t *testing.T) {
	preset := config.RedactPresetFinancial()
	presetSet := make(map[string]bool, len(preset))
	for _, src := range preset {
		presetSet[src] = true
		if _, ok := matchValidators[src]; !ok {
			t.Errorf("financial preset pattern %q has no validator in matchValidators", src)
		}
	}
	for src := range matchValidators {
		if !presetSet[src] {
			t.Errorf("matchValidators entry %q does not match any financial preset pattern", src)
		}
	}
}

// TestRedactSecretsFinancialPreset verifies the checksum-gated redaction
// end to end: valid financial identifiers are redacted, invalid lookalikes
// pass through untouched.
func TestRedactSecretsFinancialPreset(t *testing.T) {
	patterns := make([]*regexp.Regexp, 0, len(config.RedactPresetFinancial()))
	for _, src := range config.RedactPresetFinancial() {
		patterns = append(patterns, regexp.MustCompile(src))
	}

	text := "IBAN DE89370400440532013000 card 4111-1111-1111-1111 cpf 111.444.777-35 cnpj 11.222.333/0001-81"
	redacted := RedactSecrets(text, true, patterns, "[REDACTED]")
	want := "IBAN [REDACTED] card [REDACTED] cpf [REDACTED] cnpj [REDACTED]"
	if redacted != want {
		t.Errorf("valid identifiers not all redacted:\n got: %s\nwant: %s", redacted, want)
	}

	spaced := "wire to DE89 3704 0044 0532 0130 00 today"
	gotSpaced := RedactSecrets(spaced, true, patterns, "[REDACTED]")
	if gotSpaced != "wire to [REDACTED] today" {
		t.Errorf("spaced IBAN not fully redacted, got: %s", gotSpaced)
	}

	lower := "iban de89370400440532013000 ok"
	gotLower := RedactSecrets(lower, true, patterns, "[REDACTED]")
	if gotLower != "iban [REDACTED] ok" {
		t.Errorf("lowercase IBAN not redacted, got: %s", gotLower)
	}

	benign := "order 12345678901234567 range 111.444.777-34 note 4111111111111112 dummy 0000000000000"
	if got := RedactSecrets(benign, true, patterns, "[REDACTED]"); got != benign {
		t.Errorf("checksum-invalid lookalikes must not be redacted:\n got: %s\nwant: %s", got, benign)
	}
}
