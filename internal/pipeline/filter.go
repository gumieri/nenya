package pipeline

import (
	"regexp"
	"strings"

	"nenya/config"
)

// RedactSecrets replaces all matches of the given regex patterns in text
// with the specified label. If disabled or no patterns are provided, the
// original text is returned unchanged.
func RedactSecrets(text string, enabled bool, patterns []*regexp.Regexp, label string) string {
	if !enabled || len(patterns) == 0 {
		return text
	}

	redacted := text
	for _, re := range patterns {
		redacted = re.ReplaceAllString(redacted, label)
	}

	return redacted
}

// TruncateMiddleOut truncates text that exceeds maxSize runes by keeping
// the beginning and end, separated by a marker. Used for payloads that
// exceed the hard context limit.
func TruncateMiddleOut(text string, maxSize int, cfg config.GovernanceConfig) string {
	runes := []rune(text)
	if len(runes) <= maxSize {
		return text
	}

	separator := "\n... [NENYA: MASSIVE PAYLOAD TRUNCATED] ...\n"
	separatorRunes := []rune(separator)
	separatorLen := len(separatorRunes)

	available := maxSize - separatorLen
	if available <= 0 {
		return string(separatorRunes[:maxSize])
	}

	keepFirst := int(float64(available) * cfg.TruncationKeepFirstPct / 100.0)
	keepLast := int(float64(available) * cfg.TruncationKeepLastPct / 100.0)

	if keepFirst+keepLast > available {
		total := keepFirst + keepLast
		keepFirst = keepFirst * available / total
		keepLast = available - keepFirst
	}

	if keepFirst == 0 && keepLast > 0 {
		keepFirst = 1
		keepLast = available - 1
	} else if keepLast == 0 && keepFirst > 0 {
		keepLast = 1
		keepFirst = available - 1
	}

	result := make([]rune, 0, maxSize)
	result = append(result, runes[:keepFirst]...)
	result = append(result, separatorRunes...)
	result = append(result, runes[len(runes)-keepLast:]...)

	return string(result)
}

func TruncateMiddleOutCodeAware(text string, maxSize int, cfg config.GovernanceConfig) string {
	runes := []rune(text)
	if len(runes) <= maxSize {
		return text
	}

	result := TruncateMiddleOut(text, maxSize, cfg)

	sepMarker := "\n... [NENYA: MASSIVE PAYLOAD TRUNCATED] ...\n"
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
