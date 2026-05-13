package stream

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func joinStrings(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	if len(parts) == 1 {
		return parts[0]
	}
	var sb strings.Builder
	for _, p := range parts {
		sb.WriteString(p)
	}
	return sb.String()
}

func addFloat64(a, b interface{}) float64 {
	af, _ := a.(float64)
	bf, _ := b.(float64)
	return af + bf
}

func generateID() string {
	// Simple deterministic ID for testing (must be 24 chars)
	return "test12345678901234567890"
}

func TestNewAnthropicTransformer(t *testing.T) {
	tr := NewAnthropicTransformer()
	if tr == nil {
		t.Fatal("expected non-nil transformer")
	}
}

func TestAnthropicTransformer_MessageStart(t *testing.T) {
	tr := NewAnthropicTransformer()
	input := map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":    "msg_123",
			"type":  "message",
			"role":  "assistant",
			"model": "claude-3",
			"usage": map[string]interface{}{"input_tokens": float64(10)},
		},
	}
	data, _ := json.Marshal(input)
	out, err := tr.TransformSSEChunk(context.Background(), data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != nil {
		t.Fatalf("expected nil output for message_start, got %s", string(out))
	}
	if tr.messageID != "msg_123" {
		t.Errorf("expected messageID msg_123, got %s", tr.messageID)
	}
	if tr.promptTokens != 10 {
		t.Errorf("expected promptTokens 10, got %d", tr.promptTokens)
	}
}

func TestAnthropicTransformer_TextContent(t *testing.T) {
	tr := NewAnthropicTransformer()
	tr.messageID = "msg_123"
	tr.model = "claude-3"

	blockStart := map[string]interface{}{
		"type":  "content_block_start",
		"index": 0,
		"content_block": map[string]interface{}{
			"type": "text",
			"text": "Hello",
		},
	}
	data, _ := json.Marshal(blockStart)
	out, err := tr.TransformSSEChunk(context.Background(), data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]interface{}
	json.Unmarshal(out, &m)
	choices := m["choices"].([]interface{})
	delta := choices[0].(map[string]interface{})["delta"].(map[string]interface{})
	if delta["content"] != "Hello" {
		t.Errorf("expected content 'Hello', got %v", delta["content"])
	}
	if m["id"] != "msg_123" {
		t.Errorf("expected id msg_123, got %v", m["id"])
	}
	if m["model"] != "claude-3" {
		t.Errorf("expected model claude-3, got %v", m["model"])
	}

	blockDelta := map[string]interface{}{
		"type":         "content_block_delta",
		"index":        0,
		"delta":        map[string]interface{}{
			"type": "text_delta",
			"text": " world",
		},
	}
	data, _ = json.Marshal(blockDelta)
	out, err = tr.TransformSSEChunk(context.Background(), data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	json.Unmarshal(out, &m)
	choices = m["choices"].([]interface{})
	delta = choices[0].(map[string]interface{})["delta"].(map[string]interface{})
	if delta["content"] != " world" {
		t.Errorf("expected content ' world', got %v", delta["content"])
	}

	blockStop := map[string]interface{}{
		"type":  "content_block_stop",
		"index": 0,
	}
	data, _ = json.Marshal(blockStop)
	out, err = tr.TransformSSEChunk(context.Background(), data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != nil {
		t.Errorf("expected nil for content_block_stop, got %s", string(out))
	}
}

func TestAnthropicTransformer_MessageDeltaStopReasonAndUsage(t *testing.T) {
	tr := NewAnthropicTransformer()
	tr.messageID = "msg_123"
	tr.model = "claude-3"
	tr.promptTokens = 10

	msgDelta := map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{
			"stop_reason": "end_turn",
		},
		"usage": map[string]interface{}{
			"output_tokens": float64(5),
		},
	}
	data, _ := json.Marshal(msgDelta)
	out, err := tr.TransformSSEChunk(context.Background(), data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]interface{}
	json.Unmarshal(out, &m)
	choices := m["choices"].([]interface{})
	choice := choices[0].(map[string]interface{})
	if choice["finish_reason"] != "stop" {
		t.Errorf("expected finish_reason 'stop', got %v", choice["finish_reason"])
	}
	usage := m["usage"].(map[string]interface{})
	if usage["prompt_tokens"] != float64(10) {
		t.Errorf("expected prompt_tokens 10, got %v", usage["prompt_tokens"])
	}
	if usage["completion_tokens"] != float64(5) {
		t.Errorf("expected completion_tokens 5, got %v", usage["completion_tokens"])
	}
	if usage["total_tokens"] != float64(15) {
		t.Errorf("expected total_tokens 15, got %v", usage["total_tokens"])
	}
}

func TestAnthropicTransformer_MessageStop(t *testing.T) {
	tr := NewAnthropicTransformer()
	input := map[string]interface{}{
		"type": "message_stop",
	}
	data, _ := json.Marshal(input)
	out, err := tr.TransformSSEChunk(context.Background(), data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != "[DONE]" {
		t.Errorf("expected [DONE], got %s", string(out))
	}
}

func TestAnthropicTransformer_ToolUseContentBlock(t *testing.T) {
	tr := NewAnthropicTransformer()
	tr.messageID = "msg_123"
	tr.model = "claude-3"

	blockStart := map[string]interface{}{
		"type":  "content_block_start",
		"index": 0,
		"content_block": map[string]interface{}{
			"type": "tool_use",
			"id":   "tu1",
			"name": "get_weather",
		},
	}
	data, _ := json.Marshal(blockStart)
	out, err := tr.TransformSSEChunk(context.Background(), data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]interface{}
	json.Unmarshal(out, &m)
	delta := m["choices"].([]interface{})[0].(map[string]interface{})["delta"].(map[string]interface{})
	tcs := delta["tool_calls"].([]interface{})
	if len(tcs) != 1 {
		t.Fatalf("expected 1 tool_call, got %d", len(tcs))
	}
	tc := tcs[0].(map[string]interface{})
	if tc["id"] != "tu1" {
		t.Errorf("expected tool_call id 'tu1', got %v", tc["id"])
	}
	if tc["index"] != float64(0) {
		t.Errorf("expected index 0, got %v", tc["index"])
	}
	fn := tc["function"].(map[string]interface{})
	if fn["name"] != "get_weather" {
		t.Errorf("expected function name 'get_weather', got %v", fn["name"])
	}
	if fn["arguments"] != "" {
		t.Errorf("expected empty arguments, got %v", fn["arguments"])
	}

	blockDelta := map[string]interface{}{
		"type":  "content_block_delta",
		"index": 0,
		"delta": map[string]interface{}{
			"type":  "input_json_delta",
			"partial_json": `{"city": "`,
		},
	}
	data, _ = json.Marshal(blockDelta)
	out, err = tr.TransformSSEChunk(context.Background(), data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	json.Unmarshal(out, &m)
	delta = m["choices"].([]interface{})[0].(map[string]interface{})["delta"].(map[string]interface{})
	tcs = delta["tool_calls"].([]interface{})
	tc = tcs[0].(map[string]interface{})
	fn = tc["function"].(map[string]interface{})
	if fn["arguments"] != `{"city": "` {
		t.Errorf("expected partial json, got %v", fn["arguments"])
	}
}

func TestAnthropicTransformer_ThinkingContentBlock(t *testing.T) {
	tr := NewAnthropicTransformer()
	tr.messageID = "msg_123"
	tr.model = "claude-3-opus"

	blockStart := map[string]interface{}{
		"type":  "content_block_start",
		"index": 0,
		"content_block": map[string]interface{}{
			"type":     "thinking",
			"thinking": "Let me think...",
		},
	}
	data, _ := json.Marshal(blockStart)
	out, err := tr.TransformSSEChunk(context.Background(), data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]interface{}
	json.Unmarshal(out, &m)
	delta := m["choices"].([]interface{})[0].(map[string]interface{})["delta"].(map[string]interface{})
	if delta["content"] != "<thinking>Let me think...</thinking>" {
		t.Errorf("expected thinking content, got %v", delta["content"])
	}
}

func TestAnthropicTransformer_ThinkingBlockDelta(t *testing.T) {
	tr := NewAnthropicTransformer()
	tr.messageID = "msg_123"

	blockStart := map[string]interface{}{
		"type":  "content_block_start",
		"index": 0,
		"content_block": map[string]interface{}{
			"type":     "thinking",
			"thinking": "start ",
		},
	}
	data, _ := json.Marshal(blockStart)
	_, _ = tr.TransformSSEChunk(context.Background(), data)

	blockDelta := map[string]interface{}{
		"type":  "content_block_delta",
		"index": 0,
		"delta": map[string]interface{}{
			"type":     "thinking_delta",
			"thinking": "more thoughts",
		},
	}
	data, _ = json.Marshal(blockDelta)
	out, err := tr.TransformSSEChunk(context.Background(), data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]interface{}
	json.Unmarshal(out, &m)
	delta := m["choices"].([]interface{})[0].(map[string]interface{})["delta"].(map[string]interface{})
	if delta["content"] != "more thoughts" {
		t.Errorf("expected 'more thoughts', got %v", delta["content"])
	}
}

func TestAnthropicTransformer_StopReasons(t *testing.T) {
	tests := []struct {
		name       string
		stopReason string
		wantFinish string
	}{
		{name: "end_turn", stopReason: "end_turn", wantFinish: "stop"},
		{name: "tool_use", stopReason: "tool_use", wantFinish: "tool_calls"},
		{name: "max_tokens", stopReason: "max_tokens", wantFinish: "length"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := NewAnthropicTransformer()
			tr.messageID = "msg_123"
			tr.model = "claude-3"
			tr.promptTokens = 5

			msgDelta := map[string]interface{}{
				"type":  "message_delta",
				"delta": map[string]interface{}{
					"stop_reason": tt.stopReason,
				},
				"usage": map[string]interface{}{
					"output_tokens": float64(10),
				},
			}
			data, _ := json.Marshal(msgDelta)
			out, err := tr.TransformSSEChunk(context.Background(), data)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			var m map[string]interface{}
			json.Unmarshal(out, &m)
			choice := m["choices"].([]interface{})[0].(map[string]interface{})
			if choice["finish_reason"] != tt.wantFinish {
				t.Errorf("expected finish_reason %s, got %v", tt.wantFinish, choice["finish_reason"])
			}
		})
	}
}

func TestAnthropicTransformer_MultipleContentBlocks(t *testing.T) {
	tr := NewAnthropicTransformer()
	tr.messageID = "msg_123"
	tr.model = "claude-3"

	textBlock := map[string]interface{}{
		"type":  "content_block_start",
		"index": 0,
		"content_block": map[string]interface{}{
			"type": "text",
			"text": "Sure",
		},
	}
	data, _ := json.Marshal(textBlock)
	out, _ := tr.TransformSSEChunk(context.Background(), data)

	var m map[string]interface{}
	json.Unmarshal(out, &m)
	delta := m["choices"].([]interface{})[0].(map[string]interface{})["delta"].(map[string]interface{})
	if delta["content"] != "Sure" {
		t.Errorf("expected 'Sure', got %v", delta["content"])
	}

	textBlockStop := map[string]interface{}{
		"type":  "content_block_stop",
		"index": 0,
	}
	data, _ = json.Marshal(textBlockStop)
	out, _ = tr.TransformSSEChunk(context.Background(), data)

	toolBlock := map[string]interface{}{
		"type":  "content_block_start",
		"index": 1,
		"content_block": map[string]interface{}{
			"type": "tool_use",
			"id":   "tu1",
			"name": "get_weather",
		},
	}
	data, _ = json.Marshal(toolBlock)
	out, _ = tr.TransformSSEChunk(context.Background(), data)

	json.Unmarshal(out, &m)
	delta = m["choices"].([]interface{})[0].(map[string]interface{})["delta"].(map[string]interface{})
	tcs := delta["tool_calls"].([]interface{})
	if len(tcs) != 1 {
		t.Fatalf("expected 1 tool_call, got %d", len(tcs))
	}
	tc := tcs[0].(map[string]interface{})
	if tc["id"] != "tu1" {
		t.Errorf("expected tool_call id 'tu1', got %v", tc["id"])
	}
}

func TestAnthropicTransformer_PingEvent(t *testing.T) {
	tr := NewAnthropicTransformer()
	input := map[string]interface{}{
		"type": "ping",
	}
	data, _ := json.Marshal(input)
	out, err := tr.TransformSSEChunk(context.Background(), data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != nil {
		t.Errorf("expected nil for ping, got %s", string(out))
	}
}

func TestAnthropicTransformer_UnknownEventPassthrough(t *testing.T) {
	tr := NewAnthropicTransformer()
	input := map[string]interface{}{
		"type": "error",
		"error": map[string]interface{}{
			"type":    "overloaded_error",
			"message": "overloaded",
		},
	}
	data, _ := json.Marshal(input)
	out, err := tr.TransformSSEChunk(context.Background(), data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != string(data) {
		t.Errorf("expected passthrough, got %s", string(out))
	}
}

func TestAnthropicTransformer_InvalidJSON(t *testing.T) {
	tr := NewAnthropicTransformer()
	data := []byte("{invalid")
	out, err := tr.TransformSSEChunk(context.Background(), data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != string(data) {
		t.Errorf("expected passthrough for invalid JSON, got %s", string(out))
	}
}

func TestAnthropicTransformer_EmptyBody(t *testing.T) {
	tr := NewAnthropicTransformer()
	data := []byte("")
	out, err := tr.TransformSSEChunk(context.Background(), data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != "" {
		t.Errorf("expected empty, got %s", string(out))
	}
}

func TestGenerateID(t *testing.T) {
	id := generateID()
	if len(id) != 24 {
		t.Errorf("expected ID length 24, got %d", len(id))
	}
}

func TestJoinStrings(t *testing.T) {
	tests := []struct {
		name  string
		parts []string
		want  string
	}{
		{"empty", []string{}, ""},
		{"single", []string{"hello"}, "hello"},
		{"multiple", []string{"hello", " ", "world"}, "hello world"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := joinStrings(tt.parts); got != tt.want {
				t.Errorf("joinStrings() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAddFloat64(t *testing.T) {
	tests := []struct {
		name string
		a, b interface{}
		want float64
	}{
		{"both float64", float64(10), float64(20), float64(30)},
		{"nil a", nil, float64(5), float64(5)},
		{"both nil", nil, nil, float64(0)},
		{"non-float", "10", float64(5), float64(5)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := addFloat64(tt.a, tt.b); got != tt.want {
				t.Errorf("addFloat64() = %v, want %v", got, tt.want)
			}
		})
	}
}
