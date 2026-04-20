package stream

import (
	"math"
	"testing"
)

func makeTestRedactFunc(threshold float64, minTokenLen int) RedactFunc {
	return func(text string, label string) string {
		spans := tokenizeForEntropy(text)
		if len(spans) == 0 {
			return text
		}

		var result []rune
		lastEnd := 0

		for _, span := range spans {
			if span.length < minTokenLen || span.length > 512 {
				continue
			}

			token := text[span.offset : span.offset+span.length]

			if hasRepeatedChars(token) {
				continue
			}

			entropy := shannonEntropy(token)
			if entropy < threshold {
				continue
			}

			if span.offset > lastEnd {
				result = append(result, []rune(text[lastEnd:span.offset])...)
			}
			result = append(result, []rune(label)...)
			lastEnd = span.offset + span.length
		}

		if lastEnd < len(text) {
			result = append(result, []rune(text[lastEnd:])...)
		}

		return string(result)
	}
}

func TestStreamEntropy_SingleChunk(t *testing.T) {
	redact := makeTestRedactFunc(4.5, 20)
	ef := NewStreamEntropyFilter(redact, "[REDACTED]", 4096)

	input := "password is xJ3kL9mN2pQ8rS5vT1wA7yB4cF6hD0eG2iK end"
	got, action := ef.FilterContent(input)

	if action != ActionRedact {
		t.Errorf("expected ActionRedact, got %v", action)
	}
	if containsStr(got, "xJ3kL9mN2pQ8rS5vT1wA7yB4cF6hD0eG2iK") {
		t.Errorf("expected token redacted, got %q", got)
	}
	if !containsStr(got, "password is ") {
		t.Errorf("expected surrounding text preserved, got %q", got)
	}
}

func TestStreamEntropy_SplitToken(t *testing.T) {
	redact := makeTestRedactFunc(4.5, 20)
	ef := NewStreamEntropyFilter(redact, "[REDACTED]", 4096)

	token := "xJ3kL9mN2pQ8rS5vT1wA7yB4cF6hD0eG2iK"
	chunk1 := "password is " + token[:20]
	chunk2 := token[20:] + " end"

	_, action1 := ef.FilterContent(chunk1)
	if action1 != ActionPass {
		t.Errorf("chunk1: expected ActionPass, got %v", action1)
	}

	_, action2 := ef.FilterContent(chunk2)
	if action2 != ActionPass {
		t.Errorf("chunk2: expected ActionPass (split token fragments are below min_token len), got %v", action2)
	}
}

func TestStreamEntropy_LowEntropyChunks(t *testing.T) {
	redact := makeTestRedactFunc(4.5, 20)
	ef := NewStreamEntropyFilter(redact, "[REDACTED]", 4096)

	chunks := []string{
		"the quick brown fox ",
		"jumps over the lazy ",
		"dog and walks away",
	}

	for i, chunk := range chunks {
		got, action := ef.FilterContent(chunk)
		if action != ActionPass {
			t.Errorf("chunk %d: expected ActionPass, got %v", i, action)
		}
		if got != chunk {
			t.Errorf("chunk %d: expected unchanged, got %q", i, got)
		}
	}
}

func TestStreamEntropy_WindowEviction(t *testing.T) {
	redact := makeTestRedactFunc(4.5, 20)
	ef := NewStreamEntropyFilter(redact, "[REDACTED]", 100)

	lowEntropy := "the quick brown fox jumps over the lazy dog "
	highEntropy := "xJ3kL9mN2pQ8rS5vT1wA7yB4cF6hD0eG2iK"

	for i := 0; i < 10; i++ {
		_, action := ef.FilterContent(lowEntropy)
		if action != ActionPass {
			t.Errorf("iteration %d: expected ActionPass for low entropy", i)
		}
	}

	got, action := ef.FilterContent(highEntropy)
	if action != ActionRedact {
		t.Errorf("expected ActionRedact for high entropy, got %v", action)
	}
	if containsStr(got, highEntropy) {
		t.Errorf("expected high entropy token redacted, got %q", got)
	}
}

func TestStreamEntropy_EmptyChunk(t *testing.T) {
	redact := makeTestRedactFunc(4.5, 20)
	ef := NewStreamEntropyFilter(redact, "[REDACTED]", 4096)

	got, action := ef.FilterContent("")
	if action != ActionPass {
		t.Errorf("expected ActionPass for empty chunk, got %v", action)
	}
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestStreamEntropy_WindowSize(t *testing.T) {
	redact := makeTestRedactFunc(4.5, 20)
	ef := NewStreamEntropyFilter(redact, "[REDACTED]", 50)

	longText := "the quick brown fox jumps over the lazy dog and walks away from the park "
	for i := 0; i < 5; i++ {
		ef.FilterContent(longText)
	}

	if ef.WindowLen() > 50 {
		t.Errorf("expected window size <= 50, got %d", ef.WindowLen())
	}
}

func TestStreamEntropy_WindowContent(t *testing.T) {
	redact := makeTestRedactFunc(4.5, 20)
	ef := NewStreamEntropyFilter(redact, "[REDACTED]", 4096)

	chunk1 := "hello "
	chunk2 := "world"

	ef.FilterContent(chunk1)
	content := ef.WindowContent()
	if !containsStr(content, "hello ") {
		t.Errorf("expected window to contain first chunk, got %q", content)
	}

	ef.FilterContent(chunk2)
	content = ef.WindowContent()
	if !containsStr(content, "hello ") || !containsStr(content, "world") {
		t.Errorf("expected window to contain both chunks, got %q", content)
	}
}

func TestStreamEntropy_MultipleRedactions(t *testing.T) {
	redact := makeTestRedactFunc(4.5, 20)
	ef := NewStreamEntropyFilter(redact, "[REDACTED]", 4096)

	input := "key1=xJ3kL9mN2pQ8rS5vT1wA7yB4cF6hD0eG2iK key2=yZ8mN4pQ7rS2vT5wA1kL9jB3fD6hG0eI2oK"
	got, action := ef.FilterContent(input)

	if action != ActionRedact {
		t.Errorf("expected ActionRedact, got %v", action)
	}
	if containsStr(got, "xJ3kL9mN2pQ8rS5vT1wA7yB4cF6hD0eG2iK") {
		t.Errorf("expected first token redacted, got %q", got)
	}
	if containsStr(got, "yZ8mN4pQ7rS2vT5wA1kL9jB3fD6hG0eI2oK") {
		t.Errorf("expected second token redacted, got %q", got)
	}
}

func TestStreamEntropy_KeyValueIsolation(t *testing.T) {
	redact := makeTestRedactFunc(4.5, 20)
	ef := NewStreamEntropyFilter(redact, "[REDACTED]", 4096)

	input := "API_KEY=xJ3kL9mN2pQ8rS5vT1wA7yB4cF6hD0eG2iK"
	got, action := ef.FilterContent(input)

	if action != ActionRedact {
		t.Errorf("expected ActionRedact, got %v", action)
	}
	if !containsStr(got, "API_KEY=") {
		t.Errorf("expected key preserved, got %q", got)
	}
	if containsStr(got, "xJ3kL9mN2pQ8rS5vT1wA7yB4cF6hD0eG2iK") {
		t.Errorf("expected value redacted, got %q", got)
	}
}

func TestStreamEntropy_JWT(t *testing.T) {
	redact := makeTestRedactFunc(4.5, 20)
	ef := NewStreamEntropyFilter(redact, "[REDACTED]", 4096)

	jwt := "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIiwiYWRtaW4iOnRydWUsImlhdCI6MTUxNjIzOTAyMn0"
	input := "Authorization: Bearer " + jwt
	got, action := ef.FilterContent(input)

	if action != ActionRedact {
		t.Errorf("expected ActionRedact, got %v", action)
	}
	if containsStr(got, jwt) {
		t.Errorf("expected JWT redacted, got %q", got)
	}
	if !containsStr(got, "Authorization: Bearer ") {
		t.Errorf("expected header preserved, got %q", got)
	}
}

func TestStreamEntropy_NewStreamEntropyFilter_Defaults(t *testing.T) {
	ef := NewStreamEntropyFilter(nil, "[REDACTED]", 0)

	if ef.windowSize != 4096 {
		t.Errorf("expected default window size 4096, got %d", ef.windowSize)
	}
	if ef.redactLabel != "[REDACTED]" {
		t.Errorf("expected redactLabel [REDACTED], got %q", ef.redactLabel)
	}
}

func TestStreamEntropy_NewStreamEntropyFilter_Custom(t *testing.T) {
	ef := NewStreamEntropyFilter(nil, "***", 2048)

	if ef.windowSize != 2048 {
		t.Errorf("expected window size 2048, got %d", ef.windowSize)
	}
	if ef.redactLabel != "***" {
		t.Errorf("expected redactLabel ***, got %q", ef.redactLabel)
	}
}

func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

type tokenSpan struct {
	offset int
	length int
}

func tokenizeForEntropy(text string) []tokenSpan {
	var spans []tokenSpan
	start := -1

	for i, r := range text {
		if isDelim(r) {
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

func isDelim(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' ||
		r == '"' || r == '\'' || r == '`' || r == ',' ||
		r == ';' || r == '(' || r == ')' || r == '[' ||
		r == ']' || r == '{' || r == '}' || r == '=' ||
		r == ':' || r == '|' || r == '<' || r == '>'
}

func shannonEntropy(token string) float64 {
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
