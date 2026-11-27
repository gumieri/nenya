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
	config.AadhaarPattern:       verhoeffValid,
	config.ChinaIDPattern:       chinaIDValid,
	config.MyNumberPattern:      myNumberValid,
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

// verhoeffD is the Verhoeff dihedral-group D5 multiplication table —
// Wikipedia's canonical table (cross-checked against the published
// check-digit vectors in the pin tests: "236" → 3, "2363" valid).
var verhoeffD = [10][10]int{
	{0, 1, 2, 3, 4, 5, 6, 7, 8, 9},
	{1, 2, 3, 4, 0, 6, 7, 8, 9, 5},
	{2, 3, 4, 0, 1, 7, 8, 9, 5, 6},
	{3, 4, 0, 1, 2, 8, 9, 5, 6, 7},
	{4, 0, 1, 2, 3, 9, 5, 6, 7, 8},
	{5, 9, 8, 7, 6, 0, 4, 3, 2, 1},
	{6, 5, 9, 8, 7, 1, 0, 4, 3, 2},
	{7, 6, 5, 9, 8, 2, 1, 0, 4, 3},
	{8, 7, 6, 5, 9, 3, 2, 1, 0, 4},
	{9, 8, 7, 6, 5, 4, 3, 2, 1, 0},
}

// verhoeffP is the Verhoeff permutation table (positions mod 8).
var verhoeffP = [8][10]int{
	{0, 1, 2, 3, 4, 5, 6, 7, 8, 9},
	{1, 5, 7, 6, 2, 8, 3, 0, 9, 4},
	{5, 8, 0, 3, 7, 9, 6, 1, 4, 2},
	{8, 9, 1, 6, 0, 4, 3, 5, 2, 7},
	{9, 4, 5, 3, 1, 2, 6, 8, 7, 0},
	{4, 2, 8, 6, 5, 7, 3, 9, 0, 1},
	{2, 7, 9, 3, 8, 0, 6, 4, 1, 5},
	{7, 0, 4, 6, 9, 1, 3, 2, 5, 8},
}

// verhoeffInv is the Verhoeff inversion table.
var verhoeffInv = [10]int{0, 4, 3, 2, 1, 5, 6, 7, 8, 9}

// verhoeffCheckDigit computes the Verhoeff check digit for the given
// digit string (per Wikipedia: append a 0, run the checksum, the
// correct digit is inv(c) — the appended 0 consumes permutation index 0,
// so the real digits start at p index 1).
func verhoeffCheckDigit(num string) int {
	d := digits(num + "0")
	c := 0
	for i, digit := range reversed(d) {
		c = verhoeffD[c][verhoeffP[i%8][digit]]
	}
	return verhoeffInv[c]
}

// verhoeffValid reports whether s (digits only, separators stripped) is
// a valid Verhoeff-protected number — the Aadhaar checksum. An empty
// digit sequence is invalid.
func verhoeffValid(s string) bool {
	d := digits(s)
	if len(d) == 0 {
		return false
	}
	c := 0
	// Digits run right-to-left through the permutation table, i from 0
	// (Wikipedia's convention — NOT i+1; the off-by-one pairs with a
	// different p-table and silently validates the wrong numbers).
	for i, digit := range reversed(d) {
		c = verhoeffD[c][verhoeffP[i%8][digit]]
	}
	return c == 0
}

// reversed returns a reversed copy of d.
func reversed(d []int) []int {
	out := make([]int, len(d))
	for i, v := range d {
		out[len(d)-1-i] = v
	}
	return out
}

// chinaIDWeights are the ISO 7064 MOD 11-2 weights for the 17 body
// digits of the Chinese resident identity card number.
var chinaIDWeights = [17]int{7, 9, 10, 5, 8, 4, 2, 1, 6, 3, 7, 9, 10, 5, 8, 4, 2}

// chinaIDValid reports whether s is a valid Chinese resident identity
// card number: 17 digits plus a check digit ('0'-'9' or 'X'/'x'), the
// check computed as ISO 7064 MOD 11-2 over the body weights. The last
// digit may be a literal 'X'.
func chinaIDValid(s string) bool {
	if len(s) != 18 {
		return false
	}
	body := digits(s[:17])
	if len(body) != 17 {
		return false
	}
	sum := 0
	for i, d := range body {
		sum += d * chinaIDWeights[i]
	}
	check := (12 - sum%11) % 11
	want := "X"
	if check != 10 {
		want = string(rune('0' + check))
	}
	last := s[17]
	return last == want[0] || (check == 10 && (last == 'x'))
}

// myNumberWeights are the MOD 11 weights for the first 11 digits of the
// Japanese personal number (My Number).
var myNumberWeights = [11]int{6, 5, 4, 3, 2, 7, 6, 5, 4, 3, 2}

// myNumberValid reports whether s is a valid Japanese personal number:
// 12 digits, the check digit computed as (11 - (weighted sum of the
// first 11 digits) % 11), where a remainder of 0 or 1 yields 0.
func myNumberValid(s string) bool {
	d := digits(s)
	if len(d) != 12 {
		return false
	}
	sum := 0
	for i := 0; i < 11; i++ {
		sum += d[i] * myNumberWeights[i]
	}
	r := sum % 11
	check := 0
	if r > 1 {
		check = 11 - r
	}
	return d[11] == check
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
