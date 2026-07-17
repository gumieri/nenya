package stream

import (
	"context"
	"encoding/json"
	"testing"
)

func TestOpenAIToAnthropicTransformer_BasicStream(t *testing.T) {
	tr := NewOpenAIToAnthropicTransformer()

	firstChunk := map[string]interface{}{
		"id":      "chatcmpl-123",
		"object":  "chat.completion.chunk",
		"created": 0,
		"model":   "gpt-4",
		"choices": []interface{}{
			map[string]interface{}{
				"index": 0,
				"delta": map[string]interface{}{
					"content": "Hello",
				},
				"finish_reason": nil,
			},
		},
	}
	firstData, _ := json.Marshal(firstChunk)

	out, err := tr.TransformSSEChunk(context.Background(), firstData)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var events []map[string]interface{}
	for _, line := range splitSSEEvents(out) {
		var evt map[string]interface{}
		if err := json.Unmarshal(line, &evt); err != nil {
			continue
		}
		events = append(events, evt)
	}

	if len(events) < 2 {
		t.Fatalf("expected at least 2 events (message_start + content_block_start + delta), got %d", len(events))
	}

	if events[0]["type"] != "message_start" {
		t.Errorf("first event should be message_start, got %v", events[0]["type"])
	}

	msg := events[0]["message"].(map[string]interface{})
	if msg["role"] != "assistant" {
		t.Errorf("message role should be assistant, got %v", msg["role"])
	}
}

func TestOpenAIToAnthropicTransformer_FinishReason(t *testing.T) {
	tr := NewOpenAIToAnthropicTransformer()

	initChunk := map[string]interface{}{
		"id":    "chatcmpl-123",
		"model": "gpt-4",
		"choices": []interface{}{
			map[string]interface{}{
				"index":         0,
				"delta":         map[string]interface{}{},
				"finish_reason": "stop",
			},
		},
	}
	initData, _ := json.Marshal(initChunk)

	out, err := tr.TransformSSEChunk(context.Background(), initData)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var events []map[string]interface{}
	for _, line := range splitSSEEvents(out) {
		var evt map[string]interface{}
		if err := json.Unmarshal(line, &evt); err != nil {
			continue
		}
		events = append(events, evt)
	}

	foundDelta := false
	foundStop := false
	for _, evt := range events {
		if evt["type"] == "message_delta" {
			foundDelta = true
			delta := evt["delta"].(map[string]interface{})
			if delta["stop_reason"] != "end_turn" {
				t.Errorf("expected stop_reason 'end_turn', got %v", delta["stop_reason"])
			}
		}
		if evt["type"] == "message_stop" {
			foundStop = true
		}
	}

	if !foundDelta {
		t.Error("expected message_delta event")
	}
	if !foundStop {
		t.Error("expected message_stop event")
	}
}

func TestOpenAIToAnthropicTransformer_DonePassthrough(t *testing.T) {
	tr := NewOpenAIToAnthropicTransformer()

	out, err := tr.TransformSSEChunk(context.Background(), []byte("[DONE]"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != "[DONE]" {
		t.Errorf("expected [DONE], got %q", string(out))
	}
}

func TestOpenAIToAnthropicTransformer_ContextCancellation(t *testing.T) {
	tr := NewOpenAIToAnthropicTransformer()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	chunk := map[string]interface{}{
		"id":    "chatcmpl-123",
		"model": "gpt-4",
		"choices": []interface{}{
			map[string]interface{}{
				"index": 0,
				"delta": map[string]interface{}{"content": "hi"},
			},
		},
	}
	data, _ := json.Marshal(chunk)

	_, err := tr.TransformSSEChunk(ctx, data)
	if err == nil {
		t.Error("expected error from cancelled context")
	}
}

func TestOpenAIToAnthropicTransformer_Reset(t *testing.T) {
	tr := NewOpenAIToAnthropicTransformer()

	chunk := map[string]interface{}{
		"id":    "chatcmpl-123",
		"model": "gpt-4",
		"choices": []interface{}{
			map[string]interface{}{
				"index":         0,
				"delta":         map[string]interface{}{"content": "hi"},
				"finish_reason": "stop",
			},
		},
	}
	data, _ := json.Marshal(chunk)
	tr.TransformSSEChunk(context.Background(), data)

	tr.Reset()

	if tr.messageID != "" {
		t.Error("messageID should be empty after reset")
	}
	if tr.hasEmitted {
		t.Error("hasEmitted should be false after reset")
	}
	if tr.streamDone {
		t.Error("streamDone should be false after reset")
	}
}

func splitSSEEvents(data []byte) [][]byte {
	var events [][]byte
	depth := 0
	start := -1
	for i := 0; i < len(data); i++ {
		if data[i] == '{' {
			if depth == 0 {
				start = i
			}
			depth++
		}
		if data[i] == '}' {
			depth--
			if depth == 0 && start >= 0 {
				events = append(events, data[start:i+1])
				start = -1
			}
		}
	}
	return events
}
