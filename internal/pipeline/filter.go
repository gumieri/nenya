package pipeline

import (
	"regexp"

	"nenya/internal/config"
)

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

	keepFirst := int(float64(available) * cfg.KeepFirstPercent / 100.0)
	keepLast := int(float64(available) * cfg.KeepLastPercent / 100.0)

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
