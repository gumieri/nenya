package pipeline

import (
	"encoding/json"
	"testing"
	"unicode/utf8"
)

// FuzzRedactJSONEncoded exercises the JSON-aware args redactor (NENYA-348)
// on arbitrary text — the hand-rolled escape decoder parses model-
// controlled bytes and drives a rewrite path, so it gets the §12 fuzz
// treatment. Invariants: the decoder's span map stays aligned with the
// decoded text for every input; decoding terminates (implicitly — the
// fuzz timeout catches a non-advancing loop); valid-JSON inputs produce
// valid-JSON output; and the flat pass's catches are never resurrected
// (the decode pass runs on the flat output and only replaces spans).
func FuzzRedactJSONEncoded(f *testing.F) {
	for _, seed := range []string{
		`{"cmd":"mysql -u root --password=\"hunter2secret\""}`,
		`{"q":"set password=\u0022hunter2secret\u0022 in config"}`,
		`{"a":"\u00e9\u00e9\u00e9"}`,
		`{"pair":"\ud83d\ude00 ok"}`,
		`{"lone":"\ud83d alone"}`,
		`{"bad":"\uZZZZ \u12 trailing \ end\"`,
		`{"deep":"\u0041\u0042\u0043\u0044"}`,
		`{"escaped-backslash":"a\\b"}`,
		`{"nested":{"k":"v","arr":["x\"y", 1, true, null]}}`,
		`{"flat":"export KEY=AKIAIOSFODNN7EXAMPLE"}`,
		"not json at all \\u0041 backslash",
		`{"tricky":"key\":\"value"}`,
		"",
	} {
		f.Add(seed)
	}
	cred := redactionPresetForTest(f, "credentials")
	f.Fuzz(func(t *testing.T, text string) {
		if !utf8.ValidString(text) {
			t.Skip()
		}
		// Decoder invariant: one span and one flag per decoded byte.
		dec, spans, flags := decodeJSONEscapes(text)
		if len(dec) != len(spans) || len(dec) != len(flags) {
			t.Fatalf("decodeJSONEscapes misaligned: %d decoded bytes, %d spans, %d flags (in %q)", len(dec), len(spans), len(flags), text)
		}
		out := RedactJSONEncoded(text, true, cred.Patterns, cred.Label)
		if !utf8.ValidString(out) {
			t.Fatalf("RedactJSONEncoded broke UTF-8: in %q out %q", text, out)
		}
		// Validity regression guard: the FLAT patterns can legitimately
		// match across JSON structure (a quoted-value pattern whose
		// closing quote is the literal's structural terminator), which
		// breaks JSON and lands in the walker's neutralization guard —
		// pre-existing flat behavior. What must never happen is the
		// decode pass breaking validity the flat pass preserved: matches
		// are clamped to string-literal content and the label carries no
		// quote.
		flat := RedactSecrets(text, true, cred.Patterns, cred.Label)
		if json.Valid([]byte(flat)) && !json.Valid([]byte(out)) {
			t.Fatalf("decode pass broke validity the flat pass preserved: in %q flat %q out %q", text, flat, out)
		}
	})
}

// FuzzDecodeJSONEscapesAgnostic hammers the decoder alone with non-JSON
// garbage: the mapping invariant must hold for every byte sequence.
func FuzzDecodeJSONEscapesAgnostic(f *testing.F) {
	for _, seed := range []string{
		`\",\u,\u1,\uZZ,\\,\\\\,trailing\`,
		"\x00\x01\xff\\",
		`a\/\/b\c\d`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		dec, spans, flags := decodeJSONEscapes(text)
		if len(dec) != len(spans) || len(dec) != len(flags) {
			t.Fatalf("misaligned decode: %d bytes, %d spans, %d flags (in %q)", len(dec), len(spans), len(flags), text)
		}
		// Spans must be in-range and non-decreasing (the bytes of one
		// multi-byte decoded rune share that escape's span).
		lastStart, lastEnd := 0, 0
		for _, s := range spans {
			if s.start < lastStart || s.end < lastEnd || s.end > len(text) || s.start >= s.end {
				t.Fatalf("bad span %+v (last %d..%d, len %d, in %q)", s, lastStart, lastEnd, len(text), text)
			}
			lastStart, lastEnd = s.start, s.end
		}
	})
}
