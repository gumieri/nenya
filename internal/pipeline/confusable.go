package pipeline

import (
	"regexp"
	"unicode/utf8"
)

// NENYA-349 (Module B Phase 030): confusable and hidden-text candidate
// matching for the Tier-0 secret patterns. The flat patterns anchor on
// ASCII spellings — invisible separators break character classes, a
// fullwidth digit is not \d, a non-breaking space is not the space the
// pattern allows, a typographic hyphen is not '-'. The defense: when the
// text CONTAINS confusable code points, build a normalized candidate
// (invisible characters skipped, fullwidth forms and NBSP/ideographic
// space and typographic dashes folded to their ASCII counterparts),
// match the patterns against the candidate — the checksum validators see
// the folded ASCII digits — and map every match back to the ORIGINAL
// byte span, label included. The span covers the invisible/folded
// characters it was matched across, so a redaction removes the evasion
// with the secret. Clean text takes a fast-path scan (a rune loop over
// the known code points) and pays nothing.

// confusableSpan maps one normalized byte back to the original-byte span
// it was produced from (a full fold sequence, or one literal byte).
type confusableSpan struct{ start, end int }

// containsConfusables reports whether s carries any code point the
// normalizer would treat differently from its ASCII counterpart.
func containsConfusables(s string) bool {
	for _, r := range s {
		switch {
		case isInvisibleRune(r),
			r >= 0xFF01 && r <= 0xFF5E, // fullwidth forms
			r == 0x3000,                // ideographic space
			r == 0x00A0,                // no-break space
			r >= 0x2010 && r <= 0x2015, // typographic dashes
			r == 0x2212:                // minus sign
			return true
		}
	}
	return false
}

// isInvisibleRune reports whether r is an invisible/format character
// that only exists to break pattern matching. The set is the UNION of
// the injection pipeline's strip sets (invisibleChars at
// injection_patterns.go — the injection interceptor strips these from
// the payload, and a strip must never repair an evasion redaction
// missed) plus the soft hyphen, the variation selectors, and the tag
// block — all classic hidden-text separators (NENYA-349 round-1
// review: 206A-206F divergence let injection un-split a secret
// redaction missed).
func isInvisibleRune(r rune) bool {
	switch {
	case r >= 0x200B && r <= 0x200F, // ZWSP, ZWNJ, ZWJ, LRM, RLM
		r == 0x00AD,                  // soft hyphen
		r >= 0x202A && r <= 0x202E,   // directional overrides
		r >= 0x2060 && r <= 0x206F,   // word joiner family + invisible separators/full block
		r >= 0x2066 && r <= 0x2069,   // directional isolates
		r >= 0xFE00 && r <= 0xFE0F,   // variation selectors
		r >= 0xE0000 && r <= 0xE007F, // tags
		r == 0xFEFF,                  // BOM / zero-width no-break space
		r == 0x180E:                  // Mongolian vowel separator
		return true
	}
	return false
}

// foldConfusable maps one confusable rune to its ASCII counterpart
// ("" when the rune is invisible and must be skipped).
func foldConfusable(r rune) string {
	switch {
	case isInvisibleRune(r):
		return ""
	case r == 0x00A0 || r == 0x3000:
		return " "
	case r >= 0x2010 && r <= 0x2015, r == 0x2212:
		return "-"
	case r >= 0xFF01 && r <= 0xFF5E:
		return string(rune(r - 0xFEE0)) // fullwidth block → ASCII 0x21..0x7E
	}
	return string(r)
}

// normalizeConfusables builds the normalized candidate plus the
// normalized-byte → original-byte-span mapping. The consumed size comes
// from utf8.DecodeRuneInString — NOT RuneLen(r): range-over-string
// yields (RuneError, 1) per invalid byte while RuneLen(RuneError) is 3,
// and a span overshoot there would let a match swallow neighboring
// bytes (or panic the edit applier on raw upstream bytes — NENYA-349
// round-1 review). Invalid bytes are carried literally, 1:1.
func normalizeConfusables(s string) (string, []confusableSpan) {
	var out []byte
	var spans []confusableSpan
	i := 0
	for i < len(s) {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size <= 1 {
			// Invalid byte: carry it literally.
			out = append(out, s[i])
			spans = append(spans, confusableSpan{i, i + 1})
			i++
			continue
		}
		folded := foldConfusable(r)
		for _, b := range []byte(folded) {
			out = append(out, b)
			spans = append(spans, confusableSpan{i, i + size})
		}
		i += size
	}
	return string(out), spans
}

// redactConfusablePass runs the confusable candidate pass: no-op on
// clean text (the fast-path scan), otherwise patterns match the
// normalized candidate (checksum validators see the folded ASCII) and
// every match maps back to its original span — label included, so the
// evasion characters are removed with the secret.
func redactConfusablePass(text string, patterns []*regexp.Regexp, label string) string {
	if !containsConfusables(text) {
		return text
	}
	mapped, spans := normalizeConfusables(text)
	toOrig := func(i int) (int, int) {
		return spans[i].start, spans[i].end
	}
	var edits []edit
	for _, re := range patterns {
		edits = collectValidatedEdits(re, mapped, edits, toOrig)
	}
	return applyEdits(text, edits, label)
}

// confusableJSONEdits is the confusable layer for the JSON-aware args
// pass (NENYA-348 + NENYA-349 composition): the candidates run over the
// DECODED document, and every accepted match maps confusable-span →
// decoded-byte → original span through the escape map. The inString
// check is an invariant for confusable bytes (every confusable byte in
// valid JSON is literal content — the structure is ASCII) and the
// DROP-CLAMP for structure-crossing matches: a match spanning structural
// bytes is dropped rather than partially applied.
func confusableJSONEdits(dec string, patterns []*regexp.Regexp, inString []bool, toOrig func(i int) (int, int)) []edit {
	norm, confSpans := normalizeConfusables(dec)
	var edits []edit
	for _, re := range patterns {
		validate, hasValidator := matchValidators[re.String()]
		for _, loc := range re.FindAllStringSubmatchIndex(norm, -1) {
			start, end := loc[0], loc[1]
			if start < 0 || start >= end || end > len(confSpans) {
				continue
			}
			if hasValidator && !validate(norm[start:end]) {
				continue
			}
			decStart, decEnd := confSpans[start].start, confSpans[end-1].end
			if !allInStringFlags(inString[decStart:decEnd]) {
				continue
			}
			sOrig, _ := toOrig(decStart)
			_, eOrig := toOrig(decEnd - 1)
			edits = append(edits, edit{sOrig, eOrig})
		}
	}
	return edits
}
