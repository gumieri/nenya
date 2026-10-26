package stream

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// feedOpenAIChunks marshals each chunk and runs it through the transformer,
// returning every emitted Anthropic SSE event as parsed JSON maps.
func feedOpenAIChunks(t *testing.T, tr *OpenAIToAnthropicTransformer, chunks ...map[string]interface{}) []map[string]interface{} {
	t.Helper()
	var events []map[string]interface{}
	for _, chunk := range chunks {
		data, err := json.Marshal(chunk)
		if err != nil {
			t.Fatalf("marshal chunk: %v", err)
		}
		out, err := tr.TransformSSEChunk(context.Background(), data)
		if err != nil {
			t.Fatalf("TransformSSEChunk: %v", err)
		}
		events = append(events, parseAnthropicEvents(t, out)...)
	}
	return events
}

func parseAnthropicEvents(t *testing.T, out []byte) []map[string]interface{} {
	t.Helper()
	var events []map[string]interface{}
	dec := json.NewDecoder(bytes.NewReader(out))
	for dec.More() {
		var evt map[string]interface{}
		if err := dec.Decode(&evt); err != nil {
			t.Fatalf("parsing transformer output %q: %v", out, err)
		}
		events = append(events, evt)
	}
	return events
}

func hasEvent(events []map[string]interface{}, typ string) bool {
	for _, evt := range events {
		if evt["type"] == typ {
			return true
		}
	}
	return false
}

// TestOpenAIToAnthropicToolCallWithoutIndex pins AUD-31 (NENYA-250): a
// tool-call delta whose `index` field is absent (some providers omit it when
// the stream carries a single tool call) must be treated as index 0, not
// silently dropped. Pre-fix processToolCall returned nil on a non-numeric
// index and the whole call vanished from the Anthropic stream.
func TestOpenAIToAnthropicToolCallWithoutIndex(t *testing.T) {
	tr := NewOpenAIToAnthropicTransformer()

	chunk := func(delta map[string]interface{}, finishReason interface{}) map[string]interface{} {
		return map[string]interface{}{
			"id":     "chatcmpl-t",
			"object": "chat.completion.chunk",
			"model":  "m",
			"choices": []interface{}{
				map[string]interface{}{"index": 0, "delta": delta, "finish_reason": finishReason},
			},
		}
	}

	events := feedOpenAIChunks(t, tr,
		chunk(map[string]interface{}{
			"tool_calls": []interface{}{
				map[string]interface{}{
					"id":   "call_1",
					"type": "function",
					"function": map[string]interface{}{
						"name":      "get_weather",
						"arguments": `{"city":`,
					},
					// NO index field.
				},
			},
		}, nil),
		chunk(map[string]interface{}{
			"tool_calls": []interface{}{
				map[string]interface{}{
					"function": map[string]interface{}{
						"arguments": `"Paris"}`,
					},
					// NO index field.
				},
			},
		}, nil),
		chunk(map[string]interface{}{}, "tool_calls"),
	)
	events = append(events, parseAnthropicEvents(t, tr.FinishTail())...)

	if !hasEvent(events, "content_block_start") {
		t.Fatalf("no content_block_start emitted; the index-less tool call was dropped: %v", eventTypes(events))
	}
	var args strings.Builder
	for _, evt := range events {
		if evt["type"] != "content_block_delta" {
			continue
		}
		delta, _ := evt["delta"].(map[string]interface{})
		if delta == nil || delta["type"] != "input_json_delta" {
			continue
		}
		if pj, _ := delta["partial_json"].(string); pj != "" {
			args.WriteString(pj)
		}
	}
	if got := args.String(); got != `{"city":"Paris"}` {
		t.Fatalf("accumulated arguments = %q, want the full JSON payload", got)
	}

	// The stop tail must still close the tool_use block and end the message.
	if !hasEvent(events, "content_block_stop") || !hasEvent(events, "message_stop") {
		t.Fatalf("missing stop events: %v", eventTypes(events))
	}
}

// TestOpenAIToAnthropicToolCallWithIndexStillWorks guards the regression:
// an explicit index keeps its behavior.
func TestOpenAIToAnthropicToolCallWithIndexStillWorks(t *testing.T) {
	tr := NewOpenAIToAnthropicTransformer()

	chunk := func(tc map[string]interface{}, finishReason interface{}) map[string]interface{} {
		return map[string]interface{}{
			"id":     "chatcmpl-t",
			"object": "chat.completion.chunk",
			"model":  "m",
			"choices": []interface{}{
				map[string]interface{}{
					"index":         0,
					"delta":         map[string]interface{}{"tool_calls": []interface{}{tc}},
					"finish_reason": finishReason,
				},
			},
		}
	}

	events := feedOpenAIChunks(t, tr,
		chunk(map[string]interface{}{
			"index": float64(0), "id": "call_x", "type": "function",
			"function": map[string]interface{}{"name": "f", "arguments": "{}"},
		}, nil),
		chunk(map[string]interface{}{}, "tool_calls"),
	)
	events = append(events, parseAnthropicEvents(t, tr.FinishTail())...)

	if !hasEvent(events, "content_block_start") {
		t.Fatalf("explicit-index tool call lost: %v", eventTypes(events))
	}
}

func eventTypes(events []map[string]interface{}) []string {
	out := make([]string, 0, len(events))
	for _, evt := range events {
		if s, ok := evt["type"].(string); ok {
			out = append(out, s)
		}
	}
	return out
}
