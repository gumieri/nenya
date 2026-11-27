package pipeline

import (
	"strings"
	"testing"
)

// NENYA-351 pins: StripInvisibleForOutput — the outbound invisible-text
// normalizer. Narrow mode must leave legitimate emoji (variation
// selectors) and soft hyphens alone; extended mode strips them.

func TestStripInvisibleForOutputNarrow(t *testing.T) {
	in := "ig\u200bnore\u200b this\u200b"
	out, n := StripInvisibleForOutput(in, false)
	if n != 3 || out != "ignore this" {
		t.Fatalf("narrow strip: n=%d out=%q", n, out)
	}
	// Direction marks and BOM are narrow-set members too (4 of them).
	out, n = StripInvisibleForOutput("a\u200eb\u202ec\u2060d\ufeff", false)
	if n != 4 || out != "abcd" {
		t.Fatalf("narrow directionals: n=%d out=%q", n, out)
	}
}

func TestStripInvisibleForOutputNarrowKeepsLegitimate(t *testing.T) {
	// Emoji variation selectors and soft hyphens are EXTENDED-set
	// members: narrow mode must leave them (legitimate emoji modifiers,
	// Persian/Indic shaping).
	in := "\U0001F3F4\uE0067\U0001F3F4 and co\xadoperate"
	out, n := StripInvisibleForOutput(in, false)
	if n != 0 || out != in {
		t.Fatalf("narrow mode mangled legitimate text: n=%d", n)
	}
}

func TestStripInvisibleForOutputExtended(t *testing.T) {
	in := "co\u00adoperate\u200b and \ufe0fselector"
	out, n := StripInvisibleForOutput(in, true)
	if n != 3 {
		t.Fatalf("extended strip count: n=%d out=%q", n, out)
	}
	if strings.ContainsAny(out, "\u00ad\u200b\ufe0f") {
		t.Fatalf("extended-set characters survived: %q", out)
	}
}

func TestStripInvisibleForOutputClean(t *testing.T) {
	in := "plain ascii text"
	if out, n := StripInvisibleForOutput(in, true); n != 0 || out != in {
		t.Fatalf("clean text rewritten: n=%d out=%q", n, out)
	}
}
