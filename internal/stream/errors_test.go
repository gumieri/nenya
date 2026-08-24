package stream

import "testing"

func TestIsStreamErrorPayload(t *testing.T) {
	tests := []struct {
		name   string
		input  map[string]any
		expect bool
	}{
		{
			name:   "openai error object",
			input:  map[string]any{"error": map[string]any{"message": "overloaded", "type": "server_error"}},
			expect: true,
		},
		{
			name:   "openai string error",
			input:  map[string]any{"error": "rate limited"},
			expect: true,
		},
		{
			name:   "anthropic error type",
			input:  map[string]any{"type": "error", "error": map[string]any{"type": "overloaded_error"}},
			expect: true,
		},
		{
			name:   "flat server_error type",
			input:  map[string]any{"message": "Streaming response failed: [502] Upstream error from Nvidia: Service temporarily overloaded", "type": "server_error"},
			expect: true,
		},
		{
			name:   "flat overloaded_error type",
			input:  map[string]any{"type": "overloaded_error", "message": "busy"},
			expect: true,
		},
		{
			name: "openai content chunk",
			input: map[string]any{
				"choices": []any{map[string]any{"delta": map[string]any{"content": "Hi"}}},
			},
			expect: false,
		},
		{
			name:   "anthropic message_start",
			input:  map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_1"}},
			expect: false,
		},
		{
			name:   "gemini candidates",
			input:  map[string]any{"candidates": []any{map[string]any{"content": "hi"}}},
			expect: false,
		},
		{
			name:   "empty map",
			input:  map[string]any{},
			expect: false,
		},
		{
			name:   "nil payload",
			input:  nil,
			expect: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsStreamErrorPayload(tc.input); got != tc.expect {
				t.Errorf("IsStreamErrorPayload(%v) = %v, want %v", tc.input, got, tc.expect)
			}
		})
	}
}
