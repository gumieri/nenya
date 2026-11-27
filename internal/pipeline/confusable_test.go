package pipeline

import (
	"regexp"
	"strings"
	"testing"

	"github.com/nenya/config"
)

// NENYA-349 pins: the confusable candidate pass — invisible separators
// skipped, fullwidth/NBSP/typographic folds to ASCII — closes the
// hidden-text evasion class on every text surface (RedactSecrets), with
// the checksum validators judging the folded ASCII.

func confusablePatterns() []*regexp.Regexp {
	return []*regexp.Regexp{
		regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
		regexp.MustCompile(config.FinancialCardPattern),
		regexp.MustCompile(config.FinancialIBANPattern),
		regexp.MustCompile(`\b\d{3}[- ]\d{2}[- ]\d{4}\b`),
	}
}

func TestRedactConfusableZeroWidthAKIA(t *testing.T) {
	patterns := []*regexp.Regexp{regexp.MustCompile(`AKIA[0-9A-Z]{16}`)}
	in := "AKIA\u200bABCDEFGHIJ12ABCD"
	out := RedactSecrets(in, true, patterns, "REDACTED")
	if strings.Contains(out, in) || strings.Contains(out, "ABCDEFGHIJ") {
		t.Fatalf("zero-width-split AKIA survived: %q", out)
	}
	if !strings.Contains(out, "REDACTED") {
		t.Fatalf("label missing: %q", out)
	}
}

func TestRedactConfusableFullwidthCard(t *testing.T) {
	patterns := []*regexp.Regexp{regexp.MustCompile(config.FinancialCardPattern)}
	in := "\uff14\uff11\uff11\uff11 \uff11\uff11\uff11\uff11 \uff11\uff11\uff11\uff11 \uff11\uff11\uff11\uff11"
	out := RedactSecrets(in, true, patterns, "REDACTED")
	if strings.ContainsFunc(out, func(r rune) bool { return r >= 0xFF00 && r <= 0xFF5F }) {
		t.Fatalf("fullwidth card digits survived: %q", out)
	}
	if !strings.Contains(out, "REDACTED") {
		t.Fatalf("label missing: %q", out)
	}
}

func TestRedactConfusableNBSPCardStaysBenign(t *testing.T) {
	// The Luhn gate must keep judging the FOLDED digits: an
	// NBSP-separated NON-Luhn sequence stays untouched.
	patterns := []*regexp.Regexp{regexp.MustCompile(config.FinancialCardPattern)}
	in := "order 4111\u00a01111\u00a01111\u00a01112 shipped"
	out := RedactSecrets(in, true, patterns, "REDACTED")
	if out != in {
		t.Fatalf("non-Luhn NBSP digits redacted: %q", out)
	}
}

func TestRedactConfusableTypographicHyphenSSN(t *testing.T) {
	patterns := []*regexp.Regexp{regexp.MustCompile(`\b\d{3}[- ]\d{2}[- ]\d{4}\b`)}
	in := "SSN 123\u201145\u20116789 on file"
	out := RedactSecrets(in, true, patterns, "REDACTED")
	if strings.Contains(out, "123\u2011") {
		t.Fatalf("typographic-hyphen SSN survived: %q", out)
	}
	if !strings.Contains(out, "REDACTED") {
		t.Fatalf("label missing: %q", out)
	}
}

func TestRedactConfusableCleanTextByteIdentical(t *testing.T) {
	patterns := confusablePatterns()
	// The flat control: a dashed Luhn card is STILL caught exactly as
	// before (the pass must not regress flat behavior).
	in := "pay 4111-1111-1111-1111 today"
	if out := RedactSecrets(in, true, patterns, "REDACTED"); !strings.Contains(out, "REDACTED") || strings.Contains(out, "4111") {
		t.Fatalf("flat control regressed: %q", out)
	}
	// Confusable PROSE with no pattern match must pass through
	// byte-identical (the pass never rewrites without a match).
	plain := []*regexp.Regexp{regexp.MustCompile(`AKIA[0-9A-Z]{16}`)}
	confusable := "invis\u200bble separators in prose 123\u2011456"
	if out := RedactSecrets(confusable, true, plain, "REDACTED"); out != confusable {
		t.Fatalf("confusable prose rewritten without a match: %q", out)
	}
}

func TestRedactConfusableDisabled(t *testing.T) {
	patterns := []*regexp.Regexp{regexp.MustCompile(`AKIA[0-9A-Z]{16}`)}
	in := "AKIA\u200bABCDEFGHIJ12ABCD"
	if out := RedactSecrets(in, false, patterns, "REDACTED"); out != in {
		t.Fatalf("disabled redaction rewrote text: %q", out)
	}
}

// TestRedactConfusableInvalidByteNoPanic pins the invalid-UTF-8 fix
// (round-1 HIGH) with the repro that actually exercises it: an operator
// pattern with a terminal arbitrary-byte class (`secret.+`) and the
// invalid byte at match end near EOF. The round-1 mis-span built span
// {len-1, len+2} there and panicked the edit applier; the fixed walk
// redacts `secret\xff` byte-exactly with no trailing bytes to damage.
// (Shipped presets cannot reach this shape — their classes exclude
// arbitrary bytes — so this guards operator patterns and the fuzz's
// non-decreasing-span invariant, which fails on round-1 code.)
func TestRedactConfusableInvalidByteNoPanic(t *testing.T) {
	patterns := []*regexp.Regexp{regexp.MustCompile(`(?i)password=secret.+`)}
	in := "pass\u2060word=secret\xff"
	out := RedactSecrets(in, true, patterns, "REDACTED")
	if strings.Contains(out, "secret") || strings.Contains(out, "\xff") {
		t.Fatalf("invalid-byte-adjacent match survived: in %q out %q", in, out)
	}
	if !strings.Contains(out, "REDACTED") {
		t.Fatalf("label missing: %q", out)
	}
}

// TestRedactConfusableInvisibleSetUnion pins the widened set (round-1
// MED): U+206A (in the injection strip block redaction previously
// missed) and the soft hyphen must both split-detect.
func TestRedactConfusableInvisibleSetUnion(t *testing.T) {
	patterns := []*regexp.Regexp{regexp.MustCompile(`AKIA[0-9A-Z]{16}`)}
	for _, sep := range []rune{0x206A, 0x00AD, 0xFE07} {
		in := "AKIA" + string(sep) + "ABCDEFGHIJ12ABCD"
		out := RedactSecrets(in, true, patterns, "REDACTED")
		if strings.Contains(out, "ABCDEFGHIJ") || !strings.Contains(out, "REDACTED") {
			t.Fatalf("separator %#x not handled: in %q out %q", sep, in, out)
		}
	}
}
