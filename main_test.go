package main

import (
	"testing"
	"unicode/utf8"
)

func TestTruncateMiddleOut(t *testing.T) {
	cfg := Config{
		Interceptor: InterceptorConfig{
			SoftLimit:          4000,
			HardLimit:          24000,
			TruncationStrategy: "middle-out",
			KeepFirstPercent:   15.0,
			KeepLastPercent:    25.0,
		},
	}
	secrets := &SecretsConfig{}
	g := NewNenyaGateway(cfg, secrets)

	// Test short text (no truncation)
	short := "Hello, world!"
	result := g.truncateMiddleOut(short, 100)
	if result != short {
		t.Errorf("Expected no truncation, got: %s", result)
	}

	// Test exact length
	exact := string(make([]rune, 100))
	result = g.truncateMiddleOut(exact, 100)
	if utf8.RuneCountInString(result) != 100 {
		t.Errorf("Expected 100 runes, got %d", utf8.RuneCountInString(result))
	}

	// Test truncation
	long := string(make([]rune, 1000))
	result = g.truncateMiddleOut(long, 100)
	// Result should be <= maxSize and contain separator
	if utf8.RuneCountInString(result) > 100 {
		t.Errorf("Expected at most 100 runes after truncation, got %d", utf8.RuneCountInString(result))
	}

	// Check that separator is present
	if !contains(result, "[NENYA: MASSIVE PAYLOAD TRUNCATED]") {
		t.Errorf("Expected truncation separator")
	}
}

func TestCountTokens(t *testing.T) {
	cfg := Config{}
	secrets := &SecretsConfig{}
	g := NewNenyaGateway(cfg, secrets)

	text := "Hello, world! This is a test."
	tokens := g.countTokens(text)
	// Token count is approximate; just ensure it's positive
	if tokens <= 0 {
		t.Errorf("Expected positive token count, got %d", tokens)
	}
}

func TestDetermineUpstream(t *testing.T) {
	cfg := Config{}
	secrets := &SecretsConfig{}
	g := NewNenyaGateway(cfg, secrets)

	tests := []struct {
		model      string
		expected   string
	}{
		{"gemini-3.1-flash-lite", "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"},
		{"gemini-3-flash", "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"},
		{"deepseek-reasoner", "https://api.deepseek.com/v1/chat/completions"},
		{"deepseek-chat", "https://api.deepseek.com/v1/chat/completions"},
		{"glm-5", "https://api.z.ai/v1/chat/completions"},
		{"unknown", "https://api.z.ai/v1/chat/completions"},
	}

	for _, tt := range tests {
		result := g.determineUpstream(tt.model)
		if result != tt.expected {
			t.Errorf("For model %s expected %s, got %s", tt.model, tt.expected, result)
		}
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && (s[0:len(substr)] == substr || contains(s[1:], substr)))
}