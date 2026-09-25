package pipeline

import (
	"math"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/nenya/config"
)

// truncationMarker separates the kept head/tail in middle-out truncation.
// CodeAware trimming locates this exact marker to paragraph-trim the cut.
const truncationMarker = "\n... [NENYA: MASSIVE PAYLOAD TRUNCATED] ...\n"

// ReplaceValidated replaces all matches of re in text with label, applying
// the checksum validator registered for the pattern source when one exists
// (matches failing validation are kept as-is). Use this instead of bare
// re.ReplaceAllString wherever user-configured secret patterns are applied
// so gating behavior is identical on the request and output-stream paths.
func ReplaceValidated(re *regexp.Regexp, text, label string) string {
	return replaceValidated(re, text, label)
}

// RedactSecrets replaces all matches of the given regex patterns in text
// with the specified label. If disabled or no patterns are provided, the
// original text is returned unchanged. Patterns with a checksum validator
// registered in matchValidators (checksum.go) only redact matches that
// pass validation, keeping false positives out of checksummed identifiers.
func RedactSecrets(text string, enabled bool, patterns []*regexp.Regexp, label string) string {
	if !enabled || len(patterns) == 0 {
		return text
	}

	redacted := text
	for _, re := range patterns {
		redacted = replaceValidated(re, redacted, label)
	}

	return redacted
}

// truncationSplits computes the keep-first/keep-last rune budgets for
// middle-out truncation given the budget available after the separator.
// Negative percentages are clamped to no-keep; proportional splits that
// overflow the budget are rescaled in float64 (int products can overflow
// for rune budgets beyond ~3e9); a zero budget on one populated side is
// raised to a single rune so the kept context never collapses to only one
// end, and a fully collapsed split keeps at least one rune of head when
// the budget allows.
func truncationSplits(available int, firstPct, lastPct float64) (keepFirst, keepLast int) {
	if firstPct < 0 {
		firstPct = 0
	}
	if lastPct < 0 {
		lastPct = 0
	}
	keepFirst = int(float64(available) * firstPct / 100.0)
	keepLast = int(float64(available) * lastPct / 100.0)

	// Subtraction form avoids keepFirst+keepLast overflow (AGENTS.md §7):
	// keepLast can exceed available for absurd percentages.
	if keepFirst > available-keepLast {
		total := float64(keepFirst) + float64(keepLast)
		keepFirst = int(float64(keepFirst) / total * float64(available))
		keepLast = available - keepFirst
	}

	if keepFirst == 0 && keepLast > 0 {
		keepFirst = 1
		keepLast = available - 1
	} else if keepLast == 0 && keepFirst > 0 {
		keepLast = 1
		keepFirst = available - 1
	} else if keepFirst == 0 && keepLast == 0 && available > 0 {
		keepFirst = 1
		keepLast = available - 1
	}

	return keepFirst, keepLast
}

// TruncateMiddleOut truncates text that exceeds maxSize runes by keeping
// the beginning and end, separated by a marker. Used for payloads that
// exceed the hard context limit. Keep percentages are controlled by cfg.
func TruncateMiddleOut(text string, maxSize int, cfg config.ContextConfig) string {
	if maxSize < 0 {
		return ""
	}
	if utf8.RuneCountInString(text) <= maxSize {
		return text
	}
	runes := []rune(text)

	separator := truncationMarker
	separatorRunes := []rune(separator)
	separatorLen := len(separatorRunes)

	available := maxSize - separatorLen
	if available <= 0 {
		return string(separatorRunes[:maxSize])
	}

	keepFirst, keepLast := truncationSplits(available, cfg.TruncationKeepFirstPct, cfg.TruncationKeepLastPct)

	result := make([]rune, 0, maxSize)
	result = append(result, runes[:keepFirst]...)
	result = append(result, separatorRunes...)
	result = append(result, runes[len(runes)-keepLast:]...)

	return string(result)
}

// TruncateMiddleOutCodeAware truncates like TruncateMiddleOut, then trims
// the content around the separator marker to paragraph breaks so the cut
// lands between code blocks rather than mid-block.
func TruncateMiddleOutCodeAware(text string, maxSize int, cfg config.ContextConfig) string {
	if utf8.RuneCountInString(text) <= maxSize {
		return text
	}

	result := TruncateMiddleOut(text, maxSize, cfg)

	sepMarker := truncationMarker
	sepIdx := strings.Index(result, sepMarker)
	if sepIdx < 0 {
		return result
	}

	before := result[:sepIdx]
	after := result[sepIdx+len(sepMarker):]

	if lastBlank := strings.LastIndex(before, "\n\n"); lastBlank > 0 {
		before = before[:lastBlank+2]
	}

	if firstBlank := strings.Index(after, "\n\n"); firstBlank > 0 {
		after = after[firstBlank:]
	}

	return before + sepMarker + after
}

// TruncateMiddleOutByTokens truncates text that exceeds maxTokens using a
// rune-budget approximation of the token count (~3 runes/token) with the
// same keep-first/keep-last percentages as TruncateMiddleOut. curTokens is
// the caller's pre-computed token count of text; the function does not
// recount.
func TruncateMiddleOutByTokens(text string, maxTokens int, curTokens int, cfg config.ContextConfig) string {
	if curTokens <= maxTokens {
		return text
	}
	if maxTokens <= 0 {
		// Negative or zero budgets cannot keep anything; match the
		// caller-side no-op semantics instead of panicking.
		return ""
	}
	if maxTokens > math.MaxInt/3 {
		maxTokens = math.MaxInt / 3
	}

	separatorRunes := []rune(truncationMarker)
	separatorLen := len(separatorRunes)

	availableRunes := maxTokens * 3
	if utf8.RuneCountInString(text)+separatorLen <= availableRunes {
		// Text already fits the approximate rune budget.
		return text
	}
	runes := []rune(text)
	if availableRunes <= separatorLen {
		return string(separatorRunes[:availableRunes])
	}

	available := availableRunes - separatorLen

	keepFirst, keepLast := truncationSplits(available, cfg.TruncationKeepFirstPct, cfg.TruncationKeepLastPct)

	result := make([]rune, 0, availableRunes)
	result = append(result, runes[:keepFirst]...)
	result = append(result, separatorRunes...)
	result = append(result, runes[len(runes)-keepLast:]...)

	return string(result)
}
