package pipeline

import (
	"math"
	"strings"
)

type EntropyFilter struct {
	Threshold   float64 // bits/char threshold for redaction
	MinTokenLen int     // minimum token length to consider
	MaxTokenLen int     // maximum token length to consider (skip absurdly long tokens)
}

type tokenSpan struct {
	offset int
	length int
}

func NewEntropyFilter(threshold float64, minTokenLen int) *EntropyFilter {
	if threshold <= 0 {
		threshold = 4.5
	}
	if minTokenLen < 8 {
		minTokenLen = 20
	}
	return &EntropyFilter{
		Threshold:   threshold,
		MinTokenLen: minTokenLen,
		MaxTokenLen: 512,
	}
}

func ShannonEntropy(token string) float64 {
	if len(token) == 0 {
		return 0.0
	}

	var freq [256]int
	total := 0

	for _, r := range token {
		if r < 256 {
			freq[r]++
			total++
		}
	}

	if total == 0 {
		return 0.0
	}

	entropy := 0.0
	for _, count := range freq {
		if count > 0 {
			p := float64(count) / float64(total)
			entropy -= p * math.Log2(p)
		}
	}

	return entropy
}

func tokenizeForEntropy(text string) []tokenSpan {
	var spans []tokenSpan
	start := -1

	for i, r := range text {
		if isDelimiter(r) {
			if start >= 0 {
				length := i - start
				if length > 0 {
					spans = append(spans, tokenSpan{offset: start, length: length})
				}
				start = -1
			}
		} else if start < 0 {
			start = i
		}
	}

	if start >= 0 {
		length := len(text) - start
		if length > 0 {
			spans = append(spans, tokenSpan{offset: start, length: length})
		}
	}

	return spans
}

func isDelimiter(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' ||
		r == '"' || r == '\'' || r == '`' || r == ',' ||
		r == ';' || r == '(' || r == ')' || r == '[' ||
		r == ']' || r == '{' || r == '}' || r == '=' ||
		r == ':' || r == '|' || r == '<' || r == '>'
}

func (f *EntropyFilter) RedactHighEntropy(text string, label string) string {
	if len(text) == 0 {
		return text
	}

	spans := tokenizeForEntropy(text)
	if len(spans) == 0 {
		return text
	}

	var result strings.Builder
	lastEnd := 0

	for _, span := range spans {
		if span.length < f.MinTokenLen || span.length > f.MaxTokenLen {
			continue
		}

		token := text[span.offset : span.offset+span.length]

		if hasRepeatedChars(token) {
			continue
		}

		entropy := ShannonEntropy(token)
		if entropy < f.Threshold {
			continue
		}

		if span.offset > lastEnd {
			result.WriteString(text[lastEnd:span.offset])
		}
		result.WriteString(label)
		lastEnd = span.offset + span.length
	}

	if lastEnd < len(text) {
		result.WriteString(text[lastEnd:])
	}

	return result.String()
}

func hasRepeatedChars(token string) bool {
	if len(token) < 10 {
		return false
	}

	first := token[0]
	repeatCount := 0
	for _, c := range token {
		if byte(c) == first {
			repeatCount++
		}
	}

	return float64(repeatCount)/float64(len(token)) >= 0.9
}
