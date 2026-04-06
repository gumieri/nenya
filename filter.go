package main

func (g *NenyaGateway) redactSecrets(text string) string {
	if !g.config.SecurityFilter.Enabled || len(g.secretPatterns) == 0 {
		return text
	}

	redacted := text
	for _, re := range g.secretPatterns {
		redacted = re.ReplaceAllString(redacted, g.config.SecurityFilter.RedactionLabel)
	}

	if redacted != text {
		g.logger.Info("Tier-0 filter: secrets redacted from message content")
	}

	return redacted
}

func (g *NenyaGateway) truncateMiddleOut(text string, maxSize int) string {
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

	keepFirst := int(float64(available) * g.config.Governance.KeepFirstPercent / 100.0)
	keepLast := int(float64(available) * g.config.Governance.KeepLastPercent / 100.0)

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
