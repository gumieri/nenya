package adapter

import "testing"

// TestConvertOpenAIResponseStopReasonClosedEnum pins AUD-32 (NENYA-251) on
// the buffered path: ConvertOpenAIResponseToAnthropicBody must never forward
// an unknown OpenAI finish reason onto Anthropic's CLOSED stop_reason enum.
// content_filter maps to refusal, function_call (legacy) to tool_use, and
// anything unknown to end_turn. Pre-fix the default branch passed the
// provider's string through verbatim.
func TestConvertOpenAIResponseStopReasonClosedEnum(t *testing.T) {
	a := NewAnthropicAdapter()

	body := func(finishReason string) map[string]interface{} {
		return map[string]interface{}{
			"id": "cmpl-1", "object": "chat.completion", "model": "m",
			"choices": []interface{}{
				map[string]interface{}{
					"index":         0,
					"message":       map[string]interface{}{"role": "assistant", "content": "hi"},
					"finish_reason": finishReason,
				},
			},
			"usage": map[string]interface{}{"prompt_tokens": 1, "completion_tokens": 1},
		}
	}

	cases := []struct {
		finish string
		want   string
	}{
		{"stop", "end_turn"},
		{"tool_calls", "tool_use"},
		{"length", "max_tokens"},
		{"content_filter", "refusal"},
		{"function_call", "tool_use"},
		{"eos", "end_turn"},
		{"weird_reason", "end_turn"},
	}
	for _, tc := range cases {
		out := a.ConvertOpenAIResponseToAnthropicBody(body(tc.finish))
		if got := out["stop_reason"]; got != tc.want {
			t.Errorf("buffered: finish_reason %q → stop_reason %v, want %q", tc.finish, got, tc.want)
		}
	}
}
