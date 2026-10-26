package stream

import (
	"context"
	"encoding/json"
	"testing"
)

// tailStopReason runs one finish chunk through the transformer and returns
// the stop_reason carried by the held message_delta tail.
func tailStopReason(t *testing.T, finishReason string) interface{} {
	t.Helper()
	tr := NewOpenAIToAnthropicTransformer()
	chunk := map[string]interface{}{
		"id":     "chatcmpl-fr",
		"object": "chat.completion.chunk",
		"model":  "m",
		"choices": []interface{}{
			map[string]interface{}{
				"index":         0,
				"delta":         map[string]interface{}{},
				"finish_reason": finishReason,
			},
		},
	}
	data, _ := json.Marshal(chunk)
	if _, err := tr.TransformSSEChunk(context.Background(), data); err != nil {
		t.Fatalf("TransformSSEChunk: %v", err)
	}
	tail := parseAnthropicEvents(t, tr.FinishTail())
	for _, evt := range tail {
		if evt["type"] != "message_delta" {
			continue
		}
		delta, _ := evt["delta"].(map[string]interface{})
		if delta == nil {
			continue
		}
		return delta["stop_reason"]
	}
	t.Fatalf("no message_delta in tail: %v", eventTypes(tail))
	return nil
}

// TestFinishReasonReverseClosedEnum pins AUD-32 (NENYA-251): the reverse
// transformer must never forward an unknown OpenAI finish reason onto the
// Anthropic stop_reason field — it is a CLOSED enum and strict clients
// reject foreign values. Known semantic mappings apply (content_filter →
// refusal, function_call → tool_use); anything unknown lands on end_turn,
// Anthropic's neutral natural-stop. Pre-fix the default branch returned
// the provider's string verbatim.
func TestFinishReasonReverseClosedEnum(t *testing.T) {
	cases := []struct {
		finish string
		want   string
	}{
		{"stop", "end_turn"},
		{"tool_calls", "tool_use"},
		{"length", "max_tokens"},
		{"content_filter", "refusal"},
		{"function_call", "tool_use"},
		{"eos", "end_turn"},  // aggregator/custom
		{"STOP", "end_turn"}, // case drift
		{"weird_reason", "end_turn"},
	}
	for _, tc := range cases {
		if got := tailStopReason(t, tc.finish); got != tc.want {
			t.Errorf("finish_reason %q → stop_reason %v, want %q", tc.finish, got, tc.want)
		}
	}
}
