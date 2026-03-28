package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}

func TestTruncateMiddleOut(t *testing.T) {
	cfg := Config{
		Interceptor: InterceptorConfig{
			SoftLimit:          4000,
			HardLimit:          24000,
			TruncationStrategy: "middle-out",
			KeepFirstPercent:   15.0,
			KeepLastPercent:    25.0,
		},
		Upstream: UpstreamConfig{},
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

func TestRedactSecrets(t *testing.T) {
	tests := []struct {
		name           string
		filterEnabled  bool
		patterns       []string
		input          string
		expectedOutput string
		expectChange   bool
	}{
		{
			name:           "Filter disabled returns original",
			filterEnabled:  false,
			patterns:       []string{`AKIA[0-9A-Z]{16}`},
			input:          "Key: AKIAIOSFODNN7EXAMPLE",
			expectedOutput: "Key: AKIAIOSFODNN7EXAMPLE",
			expectChange:   false,
		},
		{
			name:           "AWS key redaction",
			filterEnabled:  true,
			patterns:       []string{`(?i)AKIA[0-9A-Z]{16}`},
			input:          "AWS key is AKIAIOSFODNN7EXAMPLE",
			expectedOutput: "AWS key is [REDACTED]",
			expectChange:   true,
		},
		{
			name:           "Multiple patterns",
			filterEnabled:  true,
			patterns:       []string{`AKIA[0-9A-Z]{16}`, `ghp_[a-zA-Z0-9]{36,255}`},
			input:          "key1=AKIAIOSFODNN7EXAMPLE token=ghp_abcdef1234567890abcdef1234567890abcdef",
			expectedOutput: "key1=[REDACTED] token=[REDACTED]",
			expectChange:   true,
		},
		{
			name:           "No match",
			filterEnabled:  true,
			patterns:       []string{`AKIA[0-9A-Z]{16}`},
			input:          "No secrets here",
			expectedOutput: "No secrets here",
			expectChange:   false,
		},
		{
			name:           "Empty patterns",
			filterEnabled:  true,
			patterns:       []string{},
			input:          "Key: AKIAIOSFODNN7EXAMPLE",
			expectedOutput: "Key: AKIAIOSFODNN7EXAMPLE",
			expectChange:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{
				Filter: FilterConfig{
					Enabled:        tt.filterEnabled,
					Patterns:       tt.patterns,
					RedactionLabel: "[REDACTED]",
				},
				Upstream: UpstreamConfig{},
			}
			secrets := &SecretsConfig{}
			g := NewNenyaGateway(cfg, secrets)

			result := g.redactSecrets(tt.input)
			if result != tt.expectedOutput {
				t.Errorf("Expected %q, got %q", tt.expectedOutput, result)
			}
		})
	}
}
