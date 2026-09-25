package pipeline

import (
	"regexp"
	"strings"

	"github.com/nenya/config"
)

// matchValidators associates redaction regex sources with checksum
// validators, keyed by the exact pattern source string (regexp.Regexp.String()
// round-trips the source). A match is redacted only when its pattern's
// validator accepts the matched text; patterns without an entry redact
// unconditionally. Keeping the keys in one place next to config's exported
// pattern constants prevents drift between the preset and its validators.
var matchValidators = map[string]func(string) bool{
	config.FinancialCardPattern: luhnValid,
	config.FinancialIBANPattern: ibanMod97Valid,
	config.FinancialCPFPattern:  cpfValid,
	config.FinancialCNPJPattern: cnpjValid,
}

// MatchValidator returns the checksum validator registered for a pattern
// source string, if any. Used by packages applying secret patterns
// outside RedactSecrets (e.g. the output-stream filter) so validator
// gating stays consistent everywhere.
func MatchValidator(patternSource string) (func(string) bool, bool) {
	validate, ok := matchValidators[patternSource]
	return validate, ok
}

// replaceValidated applies a single validated pattern replacement.
func replaceValidated(re *regexp.Regexp, text, label string) string {
	validate, hasValidator := matchValidators[re.String()]
	if !hasValidator {
		return re.ReplaceAllString(text, label)
	}
	return re.ReplaceAllStringFunc(text, func(match string) string {
		if validate(match) {
			return label
		}
		return match
	})
}

// digits extracts the ASCII digit characters from s.
func digits(s string) []int {
	out := make([]int, 0, len(s))
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= '0' && c <= '9' {
			out = append(out, int(c-'0'))
		}
	}
	return out
}

// allSameDigit reports whether every digit in d is identical. Uniform
// digit sequences (000...0, 111...1) checksum-accept in several schemes
// but are placeholder/dummy values, not real identifiers.
func allSameDigit(d []int) bool {
	if len(d) == 0 {
		return false
	}
	for _, v := range d {
		if v != d[0] {
			return false
		}
	}
	return true
}

// luhnValid reports whether s passes the Luhn checksum after stripping
// non-digit separators. Used to gate card-shaped redaction matches so
// ordinary number ranges are not redacted.
func luhnValid(s string) bool {
	d := digits(s)
	if len(d) < 13 || len(d) > 19 || allSameDigit(d) {
		return false
	}
	sum := 0
	for i := len(d) - 1; i >= 0; i-- {
		digit := d[i]
		// Double every second digit from the right, skipping the check digit.
		if (len(d)-1-i)%2 == 1 {
			digit *= 2
			if digit > 9 {
				digit -= 9
			}
		}
		sum += digit
	}
	return sum%10 == 0
}

// ibanMod97Valid reports whether s passes ISO 13616 mod-97 validation
// (move the first four characters to the end, expand letters to numbers,
// and the remainder modulo 97 must equal 1). Accepts single-space group
// separators and is case-insensitive, mirroring the preset pattern.
func ibanMod97Valid(s string) bool {
	s = strings.ToUpper(strings.ReplaceAll(s, " ", ""))
	if len(s) < 15 || len(s) > 34 {
		return false
	}
	rearranged := s[4:] + s[:4]
	remainder := 0
	for i := 0; i < len(rearranged); i++ {
		c := rearranged[i]
		var val int
		switch {
		case c >= '0' && c <= '9':
			val = int(c - '0')
		case c >= 'A' && c <= 'Z':
			val = int(c-'A') + 10
		default:
			return false
		}
		if val < 10 {
			remainder = (remainder*10 + val) % 97
		} else {
			remainder = (remainder*100 + val) % 97
		}
	}
	return remainder == 1
}

// cpfValid reports whether the check digits of a Brazilian CPF (11 digits)
// are consistent. Match is expected in the formatted XXX.XXX.XXX-XX form.
func cpfValid(s string) bool {
	d := digits(s)
	if len(d) != 11 || allSameDigit(d) {
		return false
	}
	sum := 0
	for i := 0; i < 9; i++ {
		sum += d[i] * (10 - i)
	}
	dv := 11 - (sum % 11)
	if dv >= 10 {
		dv = 0
	}
	if dv != d[9] {
		return false
	}
	sum = 0
	for i := 0; i < 10; i++ {
		sum += d[i] * (11 - i)
	}
	dv = 11 - (sum % 11)
	if dv >= 10 {
		dv = 0
	}
	return dv == d[10]
}

// cnpjValid reports whether the check digits of a Brazilian CNPJ (14 digits)
// are consistent. Match is expected in the formatted XX.XXX.XXX/XXXX-XX form.
func cnpjValid(s string) bool {
	d := digits(s)
	if len(d) != 14 || allSameDigit(d) {
		return false
	}
	weights1 := []int{5, 4, 3, 2, 9, 8, 7, 6, 5, 4, 3, 2}
	weights2 := []int{6, 5, 4, 3, 2, 9, 8, 7, 6, 5, 4, 3, 2}
	sum := 0
	for i, w := range weights1 {
		sum += d[i] * w
	}
	dv := 11 - (sum % 11)
	if dv >= 10 {
		dv = 0
	}
	if dv != d[12] {
		return false
	}
	sum = 0
	for i, w := range weights2 {
		sum += d[i] * w
	}
	dv = 11 - (sum % 11)
	if dv >= 10 {
		dv = 0
	}
	return dv == d[13]
}
