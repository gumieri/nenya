package proxy

import (
	"strings"
	"testing"
)

// TestClassifyBootstrapLine pins the event-type allow-list semantics of the
// bootstrap buffer (NENYA-45): only provable generation output or a rejection
// payload is decisive; handshakes, metadata, and unknown shapes keep the
// buffer open.
func TestClassifyBootstrapLine(t *testing.T) {
	tests := []struct {
		name string
		data string
		want bootstrapVerdict
	}{
		// OpenAI chat-completions chunks.
		{"delta content", `{"choices":[{"delta":{"content":"ok"}}]}`, bootstrapOutput},
		{"delta tool_calls", `{"choices":[{"delta":{"tool_calls":[{"id":"c1"}]}}]}`, bootstrapOutput},
		{"delta reasoning", `{"choices":[{"delta":{"reasoning_content":"thinking"}}]}`, bootstrapOutput},
		{"message content", `{"choices":[{"message":{"content":"ok"}}]}`, bootstrapOutput},
		{"role-only handshake", `{"choices":[{"delta":{"role":"assistant"}}]}`, bootstrapUndetermined},
		{"empty delta", `{"choices":[{"delta":{}}]}`, bootstrapUndetermined},
		{"finish-only chunk", `{"choices":[{"delta":{},"finish_reason":"stop"}]}`, bootstrapUndetermined},
		// Anthropic events.
		{"anthropic block start", `{"type":"content_block_start"}`, bootstrapOutput},
		{"anthropic block delta", `{"type":"content_block_delta","delta":{"text":"ok"}}`, bootstrapOutput},
		{"anthropic message_start", `{"type":"message_start","message":{}}`, bootstrapUndetermined},
		{"anthropic ping", `{"type":"ping"}`, bootstrapUndetermined},
		{"anthropic message_delta usage", `{"type":"message_delta","usage":{"output_tokens":3}}`, bootstrapUndetermined},
		// Gemini SSE candidates.
		{"gemini text part", `{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}`, bootstrapOutput},
		{"gemini empty candidates", `{"candidates":[]}`, bootstrapUndetermined},
		// Rejections.
		{"nested error object", `{"error":{"message":"server_is_overloaded"}}`, bootstrapReject},
		{"flat string error", `{"error":"overloaded"}`, bootstrapReject},
		{"typed error event", `{"type":"error","code":"server_is_overloaded"}`, bootstrapReject},
		{"server_error suffix", `{"message":"upstream down","type":"server_error"}`, bootstrapReject},
		{"response.failed", `{"type":"response.failed"}`, bootstrapReject},
		// Non-decisive.
		{"done marker", `[DONE]`, bootstrapUndetermined},
		{"usage frame", `{"usage":{"total_tokens":10}}`, bootstrapUndetermined},
		{"handshake metadata", `{"type":"response.created","response":{"id":"r1"}}`, bootstrapUndetermined},
		{"partial json", `{"choices":[{"delta":{"con`, bootstrapUndetermined},
		{"not json", `hello`, bootstrapUndetermined},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyBootstrapLine([]byte(tt.data)); got != tt.want {
				t.Errorf("classifyBootstrapLine(%s) = %v, want %v", tt.data, got, tt.want)
			}
		})
	}
}

// TestScanBootstrapBuffer covers multi-event scanning: decisive events win in
// stream order, incomplete trailing lines are left for the next read, and
// comments/field lines are skipped.
func TestScanBootstrapBuffer(t *testing.T) {
	t.Run("metadata then rejection", func(t *testing.T) {
		acc := []byte(": keep-alive\n\n" +
			"data: {\"type\":\"response.created\",\"response\":{}}\n\n" +
			"data: {\"type\":\"error\",\"code\":\"server_is_overloaded\"}\n\n")
		v, offset := scanBootstrapBuffer(acc, 0)
		if v != bootstrapReject {
			t.Fatalf("expected reject, got %v", v)
		}
		if !strings.Contains(string(acc[:offset]), "server_is_overloaded") {
			t.Errorf("offset %d should be past the rejection line", offset)
		}
	})
	t.Run("output wins over later error", func(t *testing.T) {
		acc := []byte("data: {\"type\":\"response.created\"}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n" +
			"data: {\"error\":{\"message\":\"late boom\"}}\n\n")
		if v, _ := scanBootstrapBuffer(acc, 0); v != bootstrapOutput {
			t.Fatalf("expected output (first decisive event), got %v", v)
		}
	})
	t.Run("incomplete tail not scanned", func(t *testing.T) {
		acc := []byte("data: {\"type\":\"response.created\"}\n\ndata: {\"choices\":[{\"delta\":{\"con")
		v, offset := scanBootstrapBuffer(acc, 0)
		if v != bootstrapUndetermined {
			t.Fatalf("expected undetermined, got %v", v)
		}
		if got := string(acc[offset:]); !strings.HasPrefix(got, "data: {\"choices\"") {
			t.Errorf("incomplete tail should remain unscanned, got remainder: %q", got)
		}
	})
	t.Run("resume after partial scan", func(t *testing.T) {
		first := []byte("data: {\"type\":\"response.created\"}\n\ndata: {\"choices\":[{\"del")
		v, offset := scanBootstrapBuffer(first, 0)
		if v != bootstrapUndetermined {
			t.Fatalf("first scan should be undetermined, got %v", v)
		}
		second := append(first, []byte("ta\":{\"content\":\"ok\"}}]}\n\n")...)
		v, _ = scanBootstrapBuffer(second, offset)
		if v != bootstrapOutput {
			t.Fatalf("resumed scan should see output, got %v", v)
		}
	})
	t.Run("only metadata stays open", func(t *testing.T) {
		acc := []byte(": ping\n\ndata: {\"type\":\"ping\"}\n\ndata: {\"usage\":{}}\n\n")
		if v, _ := scanBootstrapBuffer(acc, 0); v != bootstrapUndetermined {
			t.Fatalf("expected undetermined, got %v", v)
		}
	})
}
