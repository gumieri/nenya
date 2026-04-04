package main

import (
	"log/slog"
	"strings"
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
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	tests := []struct {
		name    string
		input   string
		maxSize int
		wantMax int
		wantSep bool
	}{
		{"short text no truncation", "Hello, world!", 100, 13, false},
		{"exact length", string(make([]rune, 100)), 100, 100, false},
		{"long text truncated", string(make([]rune, 1000)), 100, 100, true},
		{"zero max returns truncated separator", string(make([]rune, 1000)), 10, 10, false},
		{"one rune max", string(make([]rune, 100)), 1, 1, false},
		{"empty text", "", 100, 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := g.truncateMiddleOut(tt.input, tt.maxSize)
			runes := utf8.RuneCountInString(result)
			if runes > tt.wantMax {
				t.Errorf("expected at most %d runes, got %d", tt.wantMax, runes)
			}
			if tt.wantSep && !strings.Contains(result, "[NENYA: MASSIVE PAYLOAD TRUNCATED]") {
				t.Error("expected truncation separator")
			}
		})
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
			}
			secrets := &SecretsConfig{}
			g := NewNenyaGateway(cfg, secrets, slog.Default())

			result := g.redactSecrets(tt.input)
			if result != tt.expectedOutput {
				t.Errorf("Expected %q, got %q", tt.expectedOutput, result)
			}
		})
	}
}
