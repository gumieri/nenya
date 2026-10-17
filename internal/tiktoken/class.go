package tiktoken

import (
	"unicode"
	"unicode/utf8"
)

// Class-weighted fallback estimation (NENYA-157). CountTokens is a real
// cl100k_base BPE encoder, so this path is NOT the normal estimator: it runs
// only when the embedded vocabulary fails to load, where the alternative is a
// zero-token estimate and therefore unbounded budgets. It is deliberately
// coarse, integer-only and single-pass (no allocation proportional to the
// input), and it errs toward over-estimation: an over-estimate costs an
// unnecessary trim, an under-estimate risks a provider-side context error.
//
// The weights are per RUN of one character class, because that is how cl100k
// behaves (measured): a short identifier is its own token however few
// characters it has, "()" is one token, and a run of spaces collapses to one
// token up to ~20 characters. TestEstimateByClass_AgainstBPE pins the accuracy
// band against the real encoder.
const (
	// alnumRunPerToken is the ASCII letter/digit run density (~5 characters
	// per token for English words and identifiers, with a one-token floor).
	alnumRunPerToken = 5
	// denseRunPerToken and the dense-run test model an unbroken alphanumeric
	// run the encoder cannot merge: hex digests, base64 fragments, IDs, and
	// bare numbers are priced near one token per two characters rather than one
	// per five. A run is dense from denseMinRunChars on when it mixes letters
	// and digits, is pure digits, or is a long vowel-poor run (no word that
	// length is pronounceable). Ordinary words are inert to the rule, so it
	// errs toward over-estimation only on the high-entropy content it targets.
	denseRunPerToken = 2
	// denseDigitRunPerToken prices a long PURE-digit run: the encoder groups
	// digits in threes once a number is long enough. A letter anywhere in the
	// run breaks that grouping, so a hex digest keeps denseRunPerToken.
	denseDigitRunPerToken = 3
	denseMinRunChars      = 8
	denseWordMaxChars     = 16
	// punctRunPerToken is the punctuation run density: measured on JSON,
	// code, and symbol-dense text, a run of up to three punctuation
	// characters costs about one token.
	punctRunPerToken = 3
	// whitespaceRunPerToken collapses a run of spaces/tabs: anything up to
	// 20 characters is one token (measured for 4, 8, and 20 spaces).
	whitespaceRunPerToken = 20
	// otherNonASCIIPerToken counts a run of non-ASCII letters that are not
	// CJK (Cyrillic, Greek, accented Latin) as one token per two characters
	// (measured 0.45 tokens per character).
	otherNonASCIIPerToken = 2
	// emojiTokens is the cost of one pictograph; a modifier (skin tone,
	// variation selector, zero-width joiner) appends one more.
	emojiTokens         = 3
	emojiModifierTokens = 1
)

// character classes the estimator tracks. classSolo marks a rune priced by
// itself (emoji, a modifier, a CJK ideograph) rather than by a run.
const (
	classNone = iota
	classAlnum
	classPunct
	classSpace
	classOther
	classNewline
	classSolo
)

// EstimateByClass estimates a token count from character classes alone, with
// no vocabulary:
//
//   - ASCII letter/digit runs: ~5 characters per token, unless the run is
//     high-entropy (see isDense: a letter/digit mix, a pure number, or a long
//     vowel-poor run), which costs ~2 characters per token, or ~3 for a long
//     pure number (the encoder's digit grouping).
//   - Punctuation runs: ~3 characters per token.
//   - CJK/kana/hangul: one token per character (natural Chinese, Japanese, and
//     Korean measure 0.5-1.3).
//   - Other non-ASCII letters (Cyrillic, Greek, accented Latin): one per two.
//   - Emoji: three, plus one per modifier (skin tone, variation selector,
//     zero-width joiner).
//   - Space/tab runs: one token up to 20 characters, except a single space that
//     merely separates two words, which the encoder merges with the following
//     word and therefore costs nothing.
//   - Newlines: one token each (a CRLF pair counts the carriage return as
//     whitespace and the newline as its own token: a one-token over-estimate
//     per line on Windows content).
//
// Callers with a working vocabulary must use CountTokens, whose results this
// function is calibrated against.
func EstimateByClass(text string) int {
	if text == "" {
		return 0
	}
	tokens := 0
	var run classRun
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		i += size
		cls, soloCost := classifyRune(r)
		if isRunClass(cls) {
			run.add(&tokens, cls, r)
			continue
		}
		run.flush(&tokens, false)
		if cls == classNewline {
			tokens++
			run.afterNewline = true
			continue
		}
		tokens += soloCost
	}
	run.flush(&tokens, false)
	return tokens
}

// classRun accumulates one run of a single character class. Run-based pricing
// is what matches the encoder: a short identifier is its own token however few
// characters it has, "()" is one token, and a run of spaces collapses.
type classRun struct {
	class        int
	length       int
	digits       int
	letters      int
	vowels       int
	afterNewline bool
}

// add extends the run with one rune of class cls, pricing and closing the
// previous run when the class changes.
func (r *classRun) add(tokens *int, cls int, ch rune) {
	if r.class != cls {
		r.flush(tokens, cls == classAlnum)
		r.class = cls
	}
	r.length++
	if cls == classAlnum {
		switch {
		case ch >= '0' && ch <= '9':
			r.digits++
		case isVowel(ch):
			r.letters++
			r.vowels++
		default:
			r.letters++
		}
	}
}

// isVowel reports whether ch is a vowel (y included: it carries a syllable in
// words like "rhythm", and counting it keeps pronounceable runs out of the
// high-entropy branch).
func isVowel(ch rune) bool {
	switch ch {
	case 'a', 'e', 'i', 'o', 'u', 'y', 'A', 'E', 'I', 'O', 'U', 'Y':
		return true
	default:
		return false
	}
}

// isDense reports whether the accumulated alphanumeric run is high-entropy
// content (see denseMinRunChars): a letter/digit mix, a pure number, or a long
// vowel-poor run — the shapes a word cannot take.
func (r *classRun) isDense() bool {
	if r.class != classAlnum || r.length < denseMinRunChars {
		return false
	}
	switch {
	case r.digits > 0 && r.letters > 0:
		return true // hex digests, base64 fragments, IDs
	case r.letters == 0:
		return true // bare numbers
	default:
		// A long run with fewer than one vowel per four characters is not a
		// word (a pronounceable identifier keeps its normal density).
		return r.length > denseWordMaxChars && r.vowels*4 < r.length
	}
}

// flush prices the accumulated run into tokens and resets it. nextAlnum tells
// whether the run that follows is an alphanumeric one, which decides whether a
// single space merges into it.
func (r *classRun) flush(tokens *int, nextAlnum bool) {
	switch r.class {
	case classAlnum:
		cost := ceilDiv(r.length, alnumRunPerToken)
		// A long high-entropy run (see denseMinRunChars).
		if r.isDense() {
			divisor := denseRunPerToken
			if r.letters == 0 && r.length > denseWordMaxChars {
				divisor = denseDigitRunPerToken
			}
			if dense := ceilDiv(r.length, divisor); dense > cost {
				cost = dense
			}
		}
		*tokens += cost
	case classPunct:
		*tokens += ceilDiv(r.length, punctRunPerToken)
	case classSpace:
		// A single space between a word and the next word is absorbed by the
		// following token; one after a newline or before punctuation is its
		// own token.
		if r.length != 1 || !nextAlnum || r.afterNewline {
			*tokens += ceilDiv(r.length, whitespaceRunPerToken)
		}
	case classOther:
		*tokens += ceilDiv(r.length, otherNonASCIIPerToken)
	}
	*r = classRun{}
}

// isRunClass reports whether a class is priced by its run length rather than
// per character.
func isRunClass(cls int) bool {
	return cls == classSpace || cls == classAlnum || cls == classPunct || cls == classOther
}

// classifyRune maps a rune to its class and, for a classSolo rune, the tokens
// it costs on its own.
func classifyRune(r rune) (int, int) {
	switch {
	case r == ' ' || r == '\t' || r == '\r':
		return classSpace, 0
	case r == '\n':
		return classNewline, 0
	case isASCIIAlnum(r):
		return classAlnum, 0
	case r < 0x80:
		return classPunct, 0
	case isEmojiModifier(r):
		// Checked before the pictograph range: a skin tone or variation
		// selector extends the preceding emoji.
		return classSolo, emojiModifierTokens
	case isEmojiRune(r):
		return classSolo, emojiTokens
	case isWideRune(r):
		return classSolo, 1
	default:
		return classOther, 0
	}
}

// ceilDiv returns ceil(n/d) for non-negative n and positive d.
func ceilDiv(n, d int) int {
	if n <= 0 {
		return 0
	}
	return (n + d - 1) / d
}

func isASCIIAlnum(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
}

// isWideRune reports whether r is a CJK/kana/hangul character. The fallback
// splits it from the other non-ASCII letters, which cost about half a token
// each.
func isWideRune(r rune) bool {
	if r < 0x1100 {
		return false
	}
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana,
		unicode.Hangul, unicode.Bopomofo, unicode.Yi)
}

// isEmojiRune reports whether r is a pictograph or dingbat.
func isEmojiRune(r rune) bool {
	switch {
	case r >= 0x1F000 && r <= 0x1FAFF:
		return true
	case r >= 0x2600 && r <= 0x27BF:
		return true
	default:
		return false
	}
}

// isEmojiModifier reports whether r is a variation selector, skin-tone
// modifier, or zero-width joiner — each of which appends a token to the emoji
// sequence it belongs to.
func isEmojiModifier(r rune) bool {
	switch {
	case r >= 0xFE00 && r <= 0xFE0F:
		return true
	case r >= 0x1F3FB && r <= 0x1F3FF:
		return true // skin tones (also inside the emoji range)
	case r == 0x200D:
		return true
	default:
		return false
	}
}
