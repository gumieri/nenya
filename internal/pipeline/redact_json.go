package pipeline

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

// RedactJSONEncoded is the redaction entry point for JSON-ENCODED text
// surfaces — tool-call arguments strings (NENYA-348) — where a secret's
// characters may be JSON-escaped (\" or \u0022) so the flat patterns see
// backslashes where they need real separators. Valid-JSON input goes
// through the decode-aware pass alone (its per-run splitting subsumes
// the flat semantics for in-literal matches and keeps structure-crossing
// matches from damaging the document); non-JSON input degrades to the
// flat pass.
func RedactJSONEncoded(text string, enabled bool, patterns []*regexp.Regexp, label string) string {
	if !enabled || len(patterns) == 0 {
		return text
	}
	if !json.Valid([]byte(text)) {
		return RedactSecrets(text, enabled, patterns, label)
	}
	return redactJSONEscapedPass(text, patterns, label)
}

// edit is one pending original-byte replacement.
type edit struct{ start, end int }

// clampToLiteralRuns splits one decoded-text match into its maximal
// in-literal runs (contiguous stretches of string-literal content),
// returning one original-byte edit per run: a structure-crossing pattern
// redacts each literal fragment it touched and NEVER any structural
// byte — the two-literal interior case (contentA | structural | contentB)
// yields two independent edits, so neither the whole-args neutralization
// nor a silent element merge can happen. A match with no in-literal
// bytes yields no edits. toOrig maps a decoded byte index to its
// original-byte span. Bounds guards reject malformed locations.
func clampToLiteralRuns(loc []int, inString []bool, toOrig func(i int) (int, int)) []edit {
	start, end := loc[0], loc[1]
	if start < 0 || start >= end || end > len(inString) {
		return nil
	}
	var edits []edit
	for start < end {
		for start < end && !inString[start] {
			start++
		}
		runStart := start
		for start < end && inString[start] {
			start++
		}
		if start > runStart {
			s, _ := toOrig(runStart)
			_, e := toOrig(start - 1)
			edits = append(edits, edit{s, e})
		}
	}
	return edits
}

// redactJSONEscapedPass applies the decode-aware pass, returning the
// (possibly) rewritten document. Precondition: s is valid JSON (the
// router validated it). Every match is split into its maximal in-literal
// runs and each run is redacted independently: no replacement ever
// touches structural bytes (a structure-crossing pattern redacts each
// literal fragment it touched — for the natural shapes that is the
// whole secret — and the JSON structure survives untouched); the
// walker's wholesale-neutralization guard stays as defense-in-depth for
// other breakage (e.g. a label carrying a quote). Escape-free input
// takes the identity fast path (no decoded copy, no span map — a 1-byte
// structural flag walk instead of the ~19n transient materialization).
func redactJSONEscapedPass(s string, patterns []*regexp.Regexp, label string) string {
	var (
		matches  [][]int
		inString []bool
		toOrig   func(i int) (int, int)
	)
	if strings.ContainsRune(s, '\\') {
		dec, spans, flags := decodeJSONEscapes(s)
		for _, re := range patterns {
			matches = append(matches, re.FindAllStringSubmatchIndex(dec, -1)...)
		}
		inString, toOrig = flags, func(i int) (int, int) {
			return spans[i].start, spans[i].end
		}
	} else {
		// Identity fast path: the decoded text IS s, spans are {i,i+1}.
		for _, re := range patterns {
			matches = append(matches, re.FindAllStringSubmatchIndex(s, -1)...)
		}
		inString, toOrig = literalFlags(s), func(i int) (int, int) {
			return i, i + 1
		}
	}
	var edits []edit
	for _, loc := range matches {
		edits = append(edits, clampToLiteralRuns(loc, inString, toOrig)...)
	}
	if len(edits) == 0 {
		return s
	}
	// First-wins on overlap, longest-wins on ties: sort by start asc,
	// end DESC so two patterns matching at the same decoded start
	// deterministically keep the longer redaction (sort.Slice is
	// unstable — the tie-break must live in the comparator).
	sort.Slice(edits, func(i, j int) bool {
		if edits[i].start != edits[j].start {
			return edits[i].start < edits[j].start
		}
		return edits[i].end > edits[j].end
	})
	// First-wins on overlap: overlapping edits keep the leftmost span.
	var b strings.Builder
	last := 0
	for _, e := range edits {
		if e.start < last {
			continue
		}
		b.WriteString(s[last:e.start])
		b.WriteString(label)
		last = e.end
	}
	b.WriteString(s[last:])
	return b.String()
}

// literalFlags computes per-byte string-literal membership for an
// escape-free valid-JSON document (the identity fast path): content
// bytes of string literals are true, everything else — delimiters,
// braces, separators, the quotes themselves — is false.
func literalFlags(s string) []bool {
	flags := make([]bool, len(s))
	inStr := false
	for i, c := range []byte(s) {
		switch {
		case inStr && c == '"':
			inStr = false
		case !inStr && c == '"':
			inStr = true
		default:
			flags[i] = inStr
		}
	}
	return flags
}

// jsonEscapeSpan maps one decoded byte back to the original-byte span it
// was decoded from (an escape sequence's full extent, or one literal
// byte).
type jsonEscapeSpan struct{ start, end int }

// decodeJSONEscapes builds the decoded text of a JSON string document
// plus the byte→span mapping and the per-decoded-byte string-literal
// membership flags (structural bytes — delimiters, braces, separators —
// are false; content bytes, including escape sequences inside a literal,
// are true). Decoding is LENIENT — invalid escapes decode to their
// literal letter and a malformed \u falls back to "u" — because a
// hostile encoder may emit non-standard escapes hoping the consumer
// bails out of matching. Every escape decodes to at least one byte, so
// spans and flags stay aligned with the decoded text.
func decodeJSONEscapes(s string) (string, []jsonEscapeSpan, []bool) {
	var dec []byte
	var spans []jsonEscapeSpan
	var flags []bool
	inStr := false
	i := 0
	emit := func(b byte, span jsonEscapeSpan, content bool) {
		dec = append(dec, b)
		spans = append(spans, span)
		flags = append(flags, content)
	}
	for i < len(s) {
		c := s[i]
		if !inStr {
			// Structural byte (decoded 1:1). An opening quote flips the
			// state but is itself structure.
			if c == '"' {
				inStr = true
			}
			emit(c, jsonEscapeSpan{i, i + 1}, false)
			i++
			continue
		}
		if c == '"' {
			// Closing quote: structure.
			inStr = false
			emit(c, jsonEscapeSpan{i, i + 1}, false)
			i++
			continue
		}
		if c != '\\' {
			emit(c, jsonEscapeSpan{i, i + 1}, true)
			i++
			continue
		}
		start := i
		i++
		if i >= len(s) {
			// Trailing lone backslash: literal content.
			emit('\\', jsonEscapeSpan{start, len(s)}, true)
			break
		}
		decoded, next := decodeEscapeAt(s, i)
		i = next
		for _, b := range []byte(decoded) {
			emit(b, jsonEscapeSpan{start, i}, true)
		}
	}
	return string(dec), spans, flags
}

// decodeEscapeAt decodes one escape sequence; s[i] is the byte after the
// backslash, and the returned index sits just past the sequence. Lenient
// on non-standard forms: a hostile encoder's malformed escape decodes to
// its literal letter rather than aborting the scan.
func decodeEscapeAt(s string, i int) (string, int) {
	e := s[i]
	i++
	switch e {
	case '"', '\\', '/':
		return string(e), i
	case 'b':
		return "\b", i
	case 'f':
		return "\f", i
	case 'n':
		return "\n", i
	case 'r':
		return "\r", i
	case 't':
		return "\t", i
	case 'u':
		return decodeUnicodeEscape(s, i)
	default:
		return string(e), i
	}
}

// decodeUnicodeEscape decodes the \uXXXX sequence whose hex digits start
// at i (just past the 'u'), combining a following low-surrogate escape
// into one rune. Lenient fallback: fewer than four hex digits decode to
// the literal "u".
func decodeUnicodeEscape(s string, i int) (string, int) {
	if i+4 > len(s) || !isHexQuad(s[i:i+4]) {
		return "u", i
	}
	r := hexQuadRune(s[i : i+4])
	i += 4
	if r < 0xD800 || r > 0xDBFF || i+6 > len(s) || s[i] != '\\' || s[i+1] != 'u' || !isHexQuad(s[i+2:i+6]) {
		return string(r), i
	}
	lo := hexQuadRune(s[i+2 : i+6])
	if lo < 0xDC00 || lo > 0xDFFF {
		return string(r), i
	}
	i += 6
	return string(rune(0x10000 + (r-0xD800)<<10 + (lo - 0xDC00))), i
}

// isHexQuad reports whether the four bytes are all lowercase/uppercase
// hex digits.
func isHexQuad(b string) bool {
	for _, c := range []byte(b) {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// hexQuadRune parses four hex bytes into a rune (input pre-validated by
// isHexQuad).
func hexQuadRune(b string) rune {
	var v rune
	for _, c := range []byte(b) {
		v <<= 4
		switch {
		case c >= '0' && c <= '9':
			v |= rune(c - '0')
		case c >= 'a' && c <= 'f':
			v |= rune(c-'a') + 10
		default:
			v |= rune(c-'A') + 10
		}
	}
	return v
}
