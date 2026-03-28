package main

import "log"

// redactSecrets applies Tier‑0 regex filtering to remove common secret patterns.
// Runs on every request before the 3‑tier pipeline.
func (g *NenyaGateway) redactSecrets(text string) string {
	if !g.config.Filter.Enabled || len(g.secretPatterns) == 0 {
		return text
	}

	redacted := text
	for _, re := range g.secretPatterns {
		redacted = re.ReplaceAllString(redacted, g.config.Filter.RedactionLabel)
	}

	// Log only if something was actually redacted (avoid noise)
	if redacted != text {
		log.Printf("[INFO] Tier‑0 filter redacted secrets from payload")
	}

	return redacted
}

// truncateMiddleOut reduces text to maxSize runes, keeping first KeepFirstPercent% and last KeepLastPercent%.
// Implements UTF-8 safe middle‑out truncation for massive payloads.
func (g *NenyaGateway) truncateMiddleOut(text string, maxSize int) string {
	runes := []rune(text)
	if len(runes) <= maxSize {
		return text
	}

	separator := "\n... [NENYA: MASSIVE PAYLOAD TRUNCATED] ...\n"
	separatorRunes := []rune(separator)
	separatorLen := len(separatorRunes)

	// Available space for content after reserving space for separator
	available := maxSize - separatorLen
	if available <= 0 {
		// Not enough space for separator; just return separator truncated to maxSize
		return string(separatorRunes[:maxSize])
	}

	keepFirst := int(float64(available) * g.config.Interceptor.KeepFirstPercent / 100.0)
	keepLast := int(float64(available) * g.config.Interceptor.KeepLastPercent / 100.0)

	// Adjust if keepFirst + keepLast exceeds available space
	if keepFirst+keepLast > available {
		// Scale down proportionally
		total := keepFirst + keepLast
		keepFirst = keepFirst * available / total
		keepLast = available - keepFirst
	}

	// Ensure at least some content from both ends.
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

	// Final length is ≤ maxSize (integer truncation of percentages may yield slightly less).
	return string(result)
}
