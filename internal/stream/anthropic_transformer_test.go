package stream

import (
	"encoding/json"
	"testing"
)

func TestNewAnthropicTransformer(t *testing.T) {
	tr := NewAnthropicTransformer()
	if tr == nil {
		t.Fatal("expected non-nil transformer")
	}
	if tr.version != "2023-06-01" {
		t.Errorf("expected version 2023-06-01, got %s", tr.version)
	}
}

func TestAnthropicTransformer_TransformSSEChunk_TextContent(t *testing.T) {
	tr := NewAnthropicTransformer()
	input := map[string]interface{}{
		"id":          "msg_123",
		"type":        "message",
		"role":        "assistant",
		"content":     []interface{}{map[string]interface{}{"type": "text", "text": "Hello"}},
		"model":       "claude-3",
		"stop_reason": "end_turn",
		"usage":       map[string]interface{}{"input_tokens": float64(10), "output_tokens": float64(5)},
	}

	data, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	out, err := tr.TransformSSEChunk(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	choices := m["choices"].([]interface{})
	choice := choices[0].(map[string]interface{})
	delta := choice["delta"].(map[string]interface{})

	if delta["content"] != "Hello" {
		t.Errorf("expected content 'Hello', got %v", delta["content"])
	}
	if choice["finish_reason"] != "stop" {
		t.Errorf("expected finish_reason stop, got %v", choice["finish_reason"])
	}
}

func TestAnthropicTransformer_TransformSSEChunk_ToolUse(t *testing.T) {
	tr := NewAnthropicTransformer()
	input := map[string]interface{}{
		"id":      "msg_123",
		"type":    "message",
		"role":    "assistant",
		"content": []interface{}{
			map[string]interface{}{
				"type":  "tool_use",
				"id":    "tu1",
				"name":  "get_weather",
				"input": map[string]interface{}{"city": "London"},
			},
		},
		"model":       "claude-3",
		"stop_reason": "tool_use",
		"usage":       map[string]interface{}{"input_tokens": float64(10), "output_tokens": float64(2)},
	}

	data, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	out, err := tr.TransformSSEChunk(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	choices := m["choices"].([]interface{})
	choice := choices[0].(map[string]interface{})
	delta := choice["delta"].(map[string]interface{})

	tcs := delta["tool_calls"].([]interface{})
	tc := tcs[0].(map[string]interface{})
	if tc["id"] != "tu1" {
		t.Errorf("expected tool_call id 'tu1', got %v", tc["id"])
	}
	fn := tc["function"].(map[string]interface{})
	if fn["name"] != "get_weather" {
		t.Errorf("expected function name 'get_weather', got %v", fn["name"])
	}
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("expected finish_reason 'tool_calls', got %v", choice["finish_reason"])
	}
}

func TestAnthropicTransformer_TransformSSEChunk_StopReason(t *testing.T) {
	tr := NewAnthropicTransformer()

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
			input := map[string]interface{}{
				"id": "msg_123", "type": "message", "role": "assistant",
				"content":     []interface{}{map[string]interface{}{"type": "text", "text": "ok"}},
				"model":       "claude-3",
				"stop_reason": tt.stopReason,
			}
			data, _ := json.Marshal(input)
			out, err := tr.TransformSSEChunk(data)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			var m map[string]interface{}
			json.Unmarshal(out, &m)
			choices := m["choices"].([]interface{})
			choice := choices[0].(map[string]interface{})
			if choice["finish_reason"] != tt.wantFinish {
				t.Errorf("expected finish_reason %s, got %v", tt.wantFinish, choice["finish_reason"])
			}
		})
	}
}

func TestAnthropicTransformer_TransformSSEChunk_Usage(t *testing.T) {
	tr := NewAnthropicTransformer()
	input := map[string]interface{}{
		"id": "msg_123", "type": "message", "role": "assistant",
		"content": "Hello", "model": "claude-3", "stop_reason": "end_turn",
		"usage": map[string]interface{}{"input_tokens": float64(50), "output_tokens": float64(20)},
	}
	data, _ := json.Marshal(input)
	out, _ := tr.TransformSSEChunk(data)
	var m map[string]interface{}
	json.Unmarshal(out, &m)
	usage := m["usage"].(map[string]interface{})
	if usage["prompt_tokens"] != float64(50) {
		t.Errorf("expected prompt_tokens 50, got %v", usage["prompt_tokens"])
	}
	if usage["completion_tokens"] != float64(20) {
		t.Errorf("expected completion_tokens 20, got %v", usage["completion_tokens"])
	}
	if usage["total_tokens"] != float64(70) {
		t.Errorf("expected total_tokens 70, got %v", usage["total_tokens"])
	}
}

func TestAnthropicTransformer_TransformSSEChunk_InvalidJSON(t *testing.T) {
	tr := NewAnthropicTransformer()
	data := []byte("{invalid")
	out, err := tr.TransformSSEChunk(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != string(data) {
		t.Errorf("expected passthrough for invalid JSON, got %s", string(out))
	}
}

func TestAnthropicTransformer_TransformSSEChunk_EmptyBody(t *testing.T) {
	tr := NewAnthropicTransformer()
	data := []byte("")
	out, err := tr.TransformSSEChunk(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != "" {
		t.Errorf("expected empty, got %s", string(out))
	}
}

func TestAnthropicTransformer_TransformSSEChunk_ContentArray(t *testing.T) {
	tr := NewAnthropicTransformer()
	input := map[string]interface{}{
		"id": "msg_123", "type": "message", "role": "assistant",
		"content": []interface{}{map[string]interface{}{"type": "text", "text": "Hello world"}},
		"model": "claude-3", "stop_reason": "end_turn",
	}
	data, _ := json.Marshal(input)
	out, _ := tr.TransformSSEChunk(data)
	var m map[string]interface{}
	json.Unmarshal(out, &m)
	choices := m["choices"].([]interface{})
	choice := choices[0].(map[string]interface{})
	delta := choice["delta"].(map[string]interface{})
	if delta["content"] != "Hello world" {
		t.Errorf("expected content 'Hello world', got %v", delta["content"])
	}
}

func TestAnthropicTransformer_TransformSSEChunk_ToolResult(t *testing.T) {
	tr := NewAnthropicTransformer()
	input := map[string]interface{}{
		"id": "msg_123", "type": "message", "role": "assistant",
		"content": []interface{}{map[string]interface{}{"type": "tool_result", "content": "Sunny"}},
		"model": "claude-3", "stop_reason": "end_turn",
	}
	data, _ := json.Marshal(input)
	out, _ := tr.TransformSSEChunk(data)
	var m map[string]interface{}
	json.Unmarshal(out, &m)
	choices := m["choices"].([]interface{})
	choice := choices[0].(map[string]interface{})
	delta := choice["delta"].(map[string]interface{})
	if delta["content"] != "Sunny" {
		t.Errorf("expected content 'Sunny', got %v", delta["content"])
	}
}

func TestAnthropicTransformer_TransformSSEChunk_MessageStop(t *testing.T) {
	tr := NewAnthropicTransformer()
	input := map[string]interface{}{
		"id": "msg_123", "type": "message", "role": "assistant",
		"content": "done", "model": "claude-3", "stop_reason": "end_turn",
	}
	data, _ := json.Marshal(input)
	out, _ := tr.TransformSSEChunk(data)
	var m map[string]interface{}
	json.Unmarshal(out, &m)
	choices := m["choices"].([]interface{})
	choice := choices[0].(map[string]interface{})
	if choice["finish_reason"] != "stop" {
		t.Errorf("expected finish_reason 'stop', got %v", choice["finish_reason"])
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
