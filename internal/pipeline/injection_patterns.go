package pipeline

import (
	"encoding/base64"
	"encoding/hex"
	"regexp"
	"strings"
)

// injectionCategory classifies a deterministic injection detection for
// metrics triage.
type injectionCategory string

const (
	categoryOverride   injectionCategory = "override"
	categoryExfil      injectionCategory = "exfil"
	categoryForgery    injectionCategory = "forgery"
	categoryHiddenText injectionCategory = "hidden_text"
	categoryEncoded    injectionCategory = "encoded"
	categoryCustom     injectionCategory = "custom"
)

// injectionPattern pairs a compiled detector with its category.
type injectionPattern struct {
	category injectionCategory
	re       *regexp.Regexp
}

// patternSource is a detector declaration prior to compilation.
type patternSource struct {
	category injectionCategory
	source   string
}

// sanitizeMarker replaces neutralized injection spans in message content.
// Accepted limitation: a client can pre-plant this literal to spoof
// neutralization in downstream logs; the metric counters remain the source
// of truth for detection activity.
const sanitizeMarker = "[NENYA: INJECTION NEUTRALIZED]"

// invisibleChars matches invisible formatting characters used to smuggle
// instructions past human review: zero-width space (U+200B), LRM/RLM
// (U+200E/U+200F), bidi overrides (U+202A-U+202E), interlinear annotation
// and format controls (U+2060-U+206F), and BOM (U+FEFF). Deliberately
// exempt: ZWNJ/ZWJ (orthographically required in Persian/Indic scripts and
// emoji sequences) and soft hyphen U+00AD (common in legitimately copied
// web text) — flagging them would reject legitimate traffic.
var invisibleChars = regexp.MustCompile(`[\x{200B}\x{200E}\x{200F}\x{202A}-\x{202E}\x{2060}-\x{206F}\x{FEFF}]`)

// base64Token matches plausibly-encoded blobs in standard and URL-safe
// alphabets (40+ chars keeps ordinary words and short hashes out).
// hexToken matches hex streams of 40+ byte pairs (80+ chars). Neither uses
// \b anchors: hex strings are a subset of word characters, so a single
// glued word character would defeat a boundary-anchored match — false
// positives are already filtered by decode + printable + intent checks.
var (
	base64Token = regexp.MustCompile(`[A-Za-z0-9+/\-_]{40,}={0,2}`)
	hexToken    = regexp.MustCompile(`(?:[0-9a-fA-F]{2}){40,}`)
)

// encodedScanBudget caps decode work per content surface: at most
// maxEncodedTokensPerFormat candidates per alphabet, each at most
// maxEncodedTokenLen characters before chunked decoding takes over.
const (
	maxEncodedTokensPerFormat = 8
	maxEncodedTokenLen        = 8192
	maxEncodedKeepPerFormat   = 4
)

// builtInInjectionPatterns returns the default deterministic detectors.
// Patterns are deliberately conservative: coding-agent payloads are full
// of prose and API documentation that quotes prompt-engineering material,
// so every pattern requires imperative intent markers or exfil
// infrastructure rather than topical keywords alone.
func builtInInjectionPatterns() ([]injectionPattern, error) {
	return compileInjectionPatterns([]patternSource{
		{categoryOverride, `(?i)\bignore\s+(?:all\s+|any\s+)?(?:previous|prior|preceding|above|earlier)\s+(?:instructions?|prompts?|messages?|directions?|rules?)\b`},
		// NENYA-147: the original shape required a second whitespace run after
		// "all "/"any ", so the natural single-space "disregard all instructions"
		// never matched. The pattern now requires a determiner/possessive or an
		// override adjective (and drops the generic nouns "rules"/"guidelines"),
		// so benign prose ("disregard the rules of the game", "disregard
		// instructions in the README") no longer matches.
		{categoryOverride, `(?i)\bdisregard\s+(?:(?:all|any|these|those|your|the|my|of|previous|prior|preceding|above|earlier|original|initial|system|hidden|true|following|next)\s+)+(?:instructions?|prompts?|directives?)\b`},
		{categoryOverride, `(?i)\b(?:reveal|print|show|output|repeat|display|leak|expose)\s+(?:me\s+)?(?:your|the|any)\s+(?:system\s+prompt|initial\s+instructions?|hidden\s+instructions?|true\s+instructions?|original\s+(?:instructions?|prompt))\b`},
		{categoryOverride, `(?i)\bdo\s+not\s+follow\s+(?:your|the|any)\s+(?:instructions?|rules?|guidelines?)\b`},
		{categoryOverride, `(?i)\byou\s+are\s+now\s+(?:a|an|the)\s+(?:unrestricted|uncensored|unfiltered|jailbroken|dan)\b`},
		{categoryExfil, `(?i)\b(?:send|post|upload|transmit|forward)\s+(?:the\s+|all\s+|any\s+|your\s+)?(?:api[\s_-]?keys?|tokens?|passwords?|passphrases?|credentials?|secrets?|private\s+keys?|\.env\b)[^.]{0,80}https?://`},
		{categoryExfil, `(?i)\bexfiltrate\b[^.]{0,60}\b(?:keys?|tokens?|secrets?|credentials?|envs?)\b`},
		// Collaborator/OAST infrastructure has no benign role in model
		// traffic; generic webhook hosts are deliberately excluded (they
		// appear legitimately in webhook-testing documentation).
		{categoryExfil, `(?i)\b(?:oast\.(?:fun|site|online|pro)|burpcollaborator\.net|interact\.sh)\b`},
		{categoryForgery, `<\|im_start\|>\s*(?:system|developer)`},
		{categoryForgery, `<\|(?:system|endoftext)\|>`},
		// Accepted tier-1 false-positive surfaces: the forgery <system>
		// tags appear in prompt-template docs, and the hidden-comment
		// keyword window fires on any instruction-mentioning HTML comment
		// (e.g. "<!-- see instructions in README -->"). Operators can
		// allowlist both via ignore_patterns.
		{categoryForgery, `(?i)</?system>`},
		{categoryForgery, `(?i)\[\s*(?:system|assistant)\s*\]\s*:`},
		{categoryHiddenText, `(?is)<!--.{0,600}?\b(?:ignore|disregard|reveal|exfiltrate|instructions?|credentials?)\b.{0,600}?-->`},
	})
}

// decodedContentPatterns scans size-capped decodings of base64/hex blobs;
// a blob is flagged only when its decoded text carries imperative intent,
// keeping false positives on ordinary encoded payloads near zero. No \b
// anchors: the payload is already a confirmed encoded blob, and boundary
// anchoring would let an attacker glue the phrase to word characters.
var decodedContentPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)ignore\s+(?:all\s+|any\s+)?(?:previous|prior|above)\s+instructions?`),
	regexp.MustCompile(`(?i)disregard\s+(?:all\s+|any\s+|your|the)\s+instructions?`),
	regexp.MustCompile(`(?i)reveal\s+(?:your|the)\s+system\s+prompt`),
	regexp.MustCompile(`(?i)send\s+(?:your\s+)?(?:api[\s_-]?keys?|tokens?|passwords?|credentials?|secrets?)[^.]{0,80}https?://`),
	regexp.MustCompile(`<\|im_start\|>`),
}

// compileInjectionPatterns compiles detector declarations, preserving order.
func compileInjectionPatterns(sources []patternSource) ([]injectionPattern, error) {
	patterns := make([]injectionPattern, 0, len(sources))
	for _, s := range sources {
		re, err := regexp.Compile(s.source)
		if err != nil {
			return nil, err
		}
		patterns = append(patterns, injectionPattern{category: s.category, re: re})
	}
	return patterns, nil
}

// stripInvisible removes zero-width and invisible format characters.
func stripInvisible(content string) string {
	return invisibleChars.ReplaceAllString(content, "")
}

// ansiEscape matches ANSI/CSI escape sequences (colors, cursor moves, and the
// two-character escapes): an escape sequence spliced into a keyword
// ("ig\x1b[0mnore previous instructions") otherwise hides it from the
// patterns (NENYA-147).
var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b[@-_]`)

// extendedInvisibles matches the invisible/ignorable code points beyond
// invisibleChars: soft hyphen, ZWNJ/ZWJ, the Mongolian vowel separator, Hangul
// fillers, the Hangul choseong/jungseong fillers, variation selectors, and the
// Unicode tag block (U+E0000-U+E007F, used for tag-character smuggling). These
// are removed only when they appear in a *flagged* surface's normalized copy;
// stripInvisible (the sanitize path) keeps its deliberately narrower set so
// legitimate Persian/Indic and emoji text is not mangled.
var extendedInvisibles = regexp.MustCompile(`[\x{00AD}\x{180E}\x{200C}\x{200D}\x{3164}\x{FFA0}\x{115F}\x{1160}\x{FE00}-\x{FE0F}\x{E0000}-\x{E007F}]`)

// controlChars matches the C0 control characters (including a lone ESC that is
// not part of an escape sequence, and DEL): a keyword split by a bare ESC or
// any other control character otherwise evades every pattern, and the removal
// also makes normalizeForMatching idempotent (no ESC survives to form a new
// sequence at a seam).
var controlChars = regexp.MustCompile(`[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]`)

// unicodeSpaces matches the non-ASCII space separators that Go's ASCII-only
// \s and \b do not treat as whitespace, so "ignore<NBSP>previous instructions"
// otherwise evades every pattern. ASCII whitespace (space, tab, newline) is
// deliberately left intact so a sanitized message keeps its structure.
var unicodeSpaces = regexp.MustCompile(`[\x{0085}\x{00A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}]+`)

// foldFullwidth folds the fullwidth forms block (U+FF01-U+FF5E) and the
// ideographic space onto their ASCII equivalents, so a fullwidth homoglyph
// cannot evade a keyword pattern. The fold is rune-count preserving, and other
// confusable blocks (mathematical alphanumerics, script/fraktur lookalikes) are
// documented as outside the tier-1 threat model.
func foldFullwidth(content string) string {
	if !strings.ContainsFunc(content, func(r rune) bool {
		return (r >= 0xFF01 && r <= 0xFF5E) || r == 0x3000
	}) {
		return content
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 0xFF01 && r <= 0xFF5E:
			return r - 0xFEE0
		case r == 0x3000:
			return ' '
		}
		return r
	}, content)
}

// normalizeForMatching applies the tier-1 pre-match normalization (NENYA-147)
// and returns two candidate copies, because an invisible/format character can
// hide either a missing separator ("ignore<ZWSP>previous") or a split word
// ("ig<ZWSP>nore"):
//
//   - joined: the format characters are removed, so a split word rejoins;
//   - spaced: the format characters become a space, so a hidden separator
//     reappears.
//
// Both copies drop ANSI escape sequences, fold fullwidth homoglyphs, and
// collapse the non-ASCII space separators to a plain space; ASCII whitespace is
// preserved so a sanitized message keeps its structure.
func normalizeForMatching(content string) (joined, spaced string) {
	joined = stripForMatching(content, "")
	spaced = stripForMatching(content, " ")
	return joined, spaced
}

// stripForMatching removes the escape, control, invisible, and tag characters
// that hide a keyword, replacing each with replacement ("" joins a split word,
// " " restores a hidden separator), then folds fullwidth homoglyphs and
// collapses the non-ASCII space separators.
func stripForMatching(content, replacement string) string {
	cleaned := ansiEscape.ReplaceAllString(content, replacement)
	cleaned = controlChars.ReplaceAllString(cleaned, replacement)
	cleaned = extendedInvisibles.ReplaceAllString(cleaned, replacement)
	cleaned = invisibleChars.ReplaceAllString(cleaned, replacement)
	return unicodeSpaces.ReplaceAllString(foldFullwidth(cleaned), " ")
}

// sampleCandidates takes up to cap matches per format, keeping the first
// and last maxEncodedKeepPerFormat when flooded so tail-padded malicious
// blobs cannot hide beyond the scan budget.
func sampleCandidates(matches []string) []string {
	if len(matches) <= maxEncodedTokensPerFormat {
		return matches
	}
	return append(matches[:maxEncodedKeepPerFormat:maxEncodedKeepPerFormat], matches[len(matches)-maxEncodedKeepPerFormat:]...)
}

// encodedCandidates extracts size-capped base64 and hex token candidates
// from a content surface, sampling per format so neither alphabet starves
// the other.
func encodedCandidates(content string) []string {
	var candidates []string
	for _, re := range []*regexp.Regexp{base64Token, hexToken} {
		candidates = append(candidates, sampleCandidates(re.FindAllString(content, -1))...)
	}
	return candidates
}

// decodeVariants returns every successful printable decoding of the token.
// Hex strings are a subset of the base64 alphabet, so a token may decode
// under multiple alphabets; callers must check intent against all variants
// rather than the first success.
func decodeVariants(token string) []string {
	var out []string
	if decoded, err := base64.StdEncoding.DecodeString(token); err == nil && printableText(decoded) {
		out = append(out, string(decoded))
	}
	trimmed := strings.TrimRight(token, "=")
	for _, enc := range []*base64.Encoding{base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if decoded, err := enc.DecodeString(trimmed); err == nil && printableText(decoded) {
			out = append(out, string(decoded))
		}
	}
	if len(token)%2 == 0 {
		if decoded, err := hex.DecodeString(token); err == nil && printableText(decoded) {
			out = append(out, string(decoded))
		}
	}
	return out
}

// printableText reports whether b is predominantly printable text. This is
// a byte-level heuristic: bytes >= 0x80 count as printable (so arbitrary
// binary occasionally passes and wastes one intent-regex pass — no
// security impact), while UTF-16-encoded intent text (interleaved NUL
// bytes) is rejected as an accepted detection gap.
func printableText(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	printable := 0
	for _, c := range b {
		if c == '\n' || c == '\r' || c == '\t' || (c >= 0x20 && c < 0x7F) || c >= 0x80 {
			printable++
		}
	}
	return printable*100/len(b) >= 90
}

// scanEncodedTokens decodes candidates and reports the count of blobs whose
// decoded text carries imperative injection intent, plus the matched token
// strings for neutralization. Oversized tokens are decoded in aligned
// chunks so a single large blob cannot evade scanning by size alone.
func scanEncodedTokens(content string) (hits int, matchedTokens []string) {
	var seen []string
	for _, token := range encodedCandidates(content) {
		dup := false
		for _, s := range seen {
			if s == token {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		seen = append(seen, token)
		if scanDecodedText(token) {
			hits++
			matchedTokens = append(matchedTokens, token)
		}
	}
	return hits, matchedTokens
}

// scanDecodedText reports whether any decoding of the token carries
// imperative injection intent.
func scanDecodedText(token string) bool {
	if len(token) <= maxEncodedTokenLen {
		for _, decoded := range decodeVariants(token) {
			if matchesDecodedIntent(decoded) {
				return true
			}
		}
		return false
	}
	// Chunk-decode oversized tokens. A 4-char step keeps both alphabets
	// aligned (base64 groups are 4 chars; hex needs even boundaries), and
	// each chunk overlaps its predecessor by decodedChunkOverlap so intent
	// phrases crossing a seam are still seen whole. chunk is always far
	// larger than the overlap, so the step stays positive.
	chunk := (maxEncodedTokenLen / 4) * 4
	step := chunk - decodedChunkOverlap
	for start := 0; start < len(token); start += step {
		end := start + chunk
		if end > len(token) {
			end = len(token)
		}
		for _, decoded := range decodeVariants(token[start:end]) {
			if matchesDecodedIntent(decoded) {
				return true
			}
		}
		if end == len(token) {
			break
		}
	}
	return false
}

// decodedChunkOverlap keeps intent phrases spanning a chunk seam detectable.
const decodedChunkOverlap = 128

// matchesDecodedIntent reports whether decoded text carries imperative
// injection intent.
func matchesDecodedIntent(decoded string) bool {
	for _, re := range decodedContentPatterns {
		if re.MatchString(decoded) {
			return true
		}
	}
	return false
}
