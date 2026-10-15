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

// TestIsCapacityRejection pins NENYA-168: a capacity/overload rejection is
// recognized from the failure-reporting fields only, never from content — a
// model that quotes "the server is overloaded" in its answer is not a
// rejection, and a rejection that is not shaped as an error envelope still
// releases the NENYA-45 bootstrap buffer for failover.
func TestIsCapacityRejection(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    bool
	}{
		{name: "top-level capacity message", payload: `{"message":"model is at capacity"}`, want: true},
		{name: "overload type without the _error suffix", payload: `{"type":"overload","message":"server overloaded"}`, want: true},
		{name: "nested error message", payload: `{"error":{"message":"model is at capacity"}}`, want: true},
		{name: "nested error code", payload: `{"error":{"code":"overloaded"}}`, want: true},
		{name: "fastapi detail field", payload: `{"detail":"model is at capacity"}`, want: true},
		{name: "content quoting the word is not a rejection", payload: `{"choices":[{"delta":{"content":"the server is overloaded"}}]}`, want: false},
		{name: "anthropic text delta quoting the word is not a rejection", payload: `{"type":"content_block_delta","delta":{"type":"text_delta","text":"overloaded"}}`, want: false},
		{name: "gemini part quoting the word is not a rejection", payload: `{"candidates":[{"content":{"parts":[{"text":"capacity"}]}}]}`, want: false},
		{name: "unrelated message", payload: `{"message":"invalid parameter"}`, want: false},
		{name: "empty payload", payload: `{}`, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed := ParseSSEChunk([]byte(tt.payload))
			if parsed == nil {
				t.Fatalf("payload did not parse: %s", tt.payload)
			}
			if got := IsCapacityRejection(parsed); got != tt.want {
				t.Fatalf("IsCapacityRejection(%s) = %v, want %v", tt.payload, got, tt.want)
			}
		})
	}
	if IsCapacityRejection(nil) {
		t.Error("a nil payload is not a capacity rejection")
	}
}
