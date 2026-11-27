package pipeline

import (
	"testing"
	"unicode/utf8"
)

// FuzzRedactConfusable hammers the confusable normalizer + pass
// (NENYA-349): the mapping invariant (one span per normalized byte,
// spans in-range and non-decreasing), UTF-8 preservation, and the
// no-match-untouched property — text whose normalized form matches no
// pattern must come back byte-identical.
func FuzzRedactConfusable(f *testing.F) {
	for _, seed := range []string{
		"AKIA\u200bABCDEFGHIJ12ABCD",
		"4111\u200b1111\u200b1111\u200b1111",
		"\uff14\uff11\uff11\uff11 \uff11\uff11\uff11\uff11 \uff11\uff11\uff11\uff11 \uff11\uff11\uff11\uff11",
		"DE89\u00a03704\u00a00044\u00a00532\u00a00130\u00a000",
		"123\u201145\u20116789",
		"mix\u200b\uff11\u00a0123\u2011\u3000AKIA\u200bAAAAAAAAAAAAAAAA",
		"\u2060\u2064\ufeff\u180e invisibles only",
		"clean ascii text",
		"",
	} {
		f.Add(seed)
	}
	cred := redactionPresetForTest(f, "credentials")
	fin := redactionPresetForTest(f, "financial")
	f.Fuzz(func(t *testing.T, text string) {
		// Raw bytes welcome (no valid-UTF-8 skip): upstream error bodies
		// reach RedactSecrets with arbitrary bytes, and the span map
		// must stay aligned on the invalid-byte path too.
		mapped, spans := normalizeConfusables(text)
		if len(mapped) != len(spans) {
			t.Fatalf("normalizeConfusables misaligned: %d bytes, %d spans (in %q)", len(mapped), len(spans), text)
		}
		lastStart, lastEnd := 0, 0
		for _, s := range spans {
			if s.start < lastStart || s.end < lastEnd || s.end > len(text) || s.start >= s.end {
				t.Fatalf("bad span %+v (last %d..%d, len %d, in %q)", s, lastStart, lastEnd, len(text), text)
			}
			lastStart, lastEnd = s.start, s.end
		}
		out := RedactSecrets(text, true, cred.Patterns, cred.Label)
		// Valid input stays valid; invalid bytes pass through byte-exact
		// (byte preservation, not sanitation).
		if utf8.ValidString(text) && !utf8.ValidString(out) {
			t.Fatalf("RedactSecrets broke UTF-8: in %q out %q", text, out)
		}
		outFin := RedactSecrets(text, true, fin.Patterns, fin.Label)
		if utf8.ValidString(text) && !utf8.ValidString(outFin) {
			t.Fatalf("RedactSecrets(financial) broke UTF-8: in %q out %q", text, outFin)
		}
	})
}
