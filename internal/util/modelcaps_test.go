package util

import "testing"

func TestSupportsMidConversationSystem(t *testing.T) {
	tests := []struct {
		modelID string
		want    bool
	}{
		// 4.8+ generation and 5-family: supported.
		{"claude-sonnet-4-8", true},
		{"claude-opus-4-8-20260101", true},
		{"claude-haiku-4-9", true},
		{"claude-sonnet-5", true},
		{"claude-opus-5-1", true},
		{"claude-fable-5", true},
		{"anthropic/claude-sonnet-4-8", true}, // provider-qualified segment

		// Older generations: unsupported.
		{"claude-sonnet-4-5", false},
		{"claude-4-5-sonnet", false},
		{"claude-opus-4-1", false},
		{"claude-3-5-sonnet-20241022", false},
		{"claude-3-haiku", false},
		{"claude-2.1", false},
		{"claude-instant-1", false},

		// Non-claude or malformed: unsupported.
		{"gemini-3-pro", false},
		{"gpt-4o", false},
		{"", false},
		{"claude", false},
	}
	for _, tt := range tests {
		if got := SupportsMidConversationSystem(tt.modelID); got != tt.want {
			t.Errorf("SupportsMidConversationSystem(%q) = %v, want %v", tt.modelID, got, tt.want)
		}
	}
}
