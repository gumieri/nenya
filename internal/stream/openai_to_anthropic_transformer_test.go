package stream

import (
	"context"
	"encoding/json"
	"strings"
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
	// The tail is held at finish (AUD-29): [DONE] flushes it.
	events = append(events, splitEvents(t, flushStreamTail(t, tr))...)

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
		t.Error("expected error from canceled context")
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
	_, _ = tr.TransformSSEChunk(context.Background(), data)

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

// usageFromEvents returns the usage object of the last message_delta event in a
// batch of transformed events.
func usageFromEvents(t *testing.T, out []byte) map[string]interface{} {
	t.Helper()
	var usage map[string]interface{}
	for _, line := range splitSSEEvents(out) {
		var evt map[string]interface{}
		if err := json.Unmarshal(line, &evt); err != nil {
			continue
		}
		if evt["type"] != "message_delta" {
			continue
		}
		if u, ok := evt["usage"].(map[string]interface{}); ok {
			usage = u
		}
	}
	return usage
}

// flushStreamTail sends the [DONE] sentinel so a held tail
// (message_delta + message_stop) is flushed, then returns the emission
// (AUD-29 / NENYA-241).
func flushStreamTail(t *testing.T, tr *OpenAIToAnthropicTransformer) []byte {
	t.Helper()
	out, err := tr.TransformSSEChunk(context.Background(), []byte("[DONE]"))
	if err != nil {
		t.Fatalf("[DONE]: %v", err)
	}
	return out
}

func splitEvents(t *testing.T, out []byte) []map[string]interface{} {
	t.Helper()
	var events []map[string]interface{}
	for _, line := range splitSSEEvents(out) {
		var evt map[string]interface{}
		if err := json.Unmarshal(line, &evt); err != nil {
			continue
		}
		events = append(events, evt)
	}
	return events
}

// TestOpenAIToAnthropicTransformer_UsageBeforeFinish pins the case the
// capture fixes (NENYA-167): a provider that reports cumulative usage on the
// content chunks (Gemini-over-OpenAI-compat and several gateways do) has its
// real counts published on the final message_delta instead of the
// character-count estimate.
func TestOpenAIToAnthropicTransformer_UsageBeforeFinish(t *testing.T) {
	tr := NewOpenAIToAnthropicTransformer()
	ctx := context.Background()

	content, _ := json.Marshal(map[string]interface{}{
		"id": "chatcmpl-1", "model": "gpt-4",
		"choices": []interface{}{map[string]interface{}{
			"index": 0, "delta": map[string]interface{}{"content": "hello there"}, "finish_reason": nil,
		}},
		"usage": map[string]interface{}{
			"prompt_tokens": 100.0, "completion_tokens": 42.0,
			"prompt_tokens_details": map[string]interface{}{"cached_tokens": 60.0},
			"cache_creation_tokens": 10.0,
		},
	})
	if _, err := tr.TransformSSEChunk(ctx, content); err != nil {
		t.Fatalf("content chunk: %v", err)
	}
	// message_start is emitted before the usage is read from the same chunk,
	// so the real counts land on the closing message_delta below.
	finish, _ := json.Marshal(map[string]interface{}{
		"id": "chatcmpl-1", "model": "gpt-4",
		"choices": []interface{}{map[string]interface{}{
			"index": 0, "delta": map[string]interface{}{}, "finish_reason": "stop",
		}},
	})
	if _, err := tr.TransformSSEChunk(ctx, finish); err != nil {
		t.Fatalf("finish chunk: %v", err)
	}
	// The tail is held at finish; [DONE] flushes it with the captured usage.
	usage := usageFromEvents(t, flushStreamTail(t, tr))
	if usage == nil {
		t.Fatal("expected a message_delta with usage")
	}
	if usage["output_tokens"] != float64(42) {
		t.Errorf("output_tokens = %v, want the upstream-reported 42", usage["output_tokens"])
	}
	if usage["input_tokens"] != float64(30) {
		t.Errorf("input_tokens = %v, want 30 (100 prompt - 60 cached - 10 written)", usage["input_tokens"])
	}
	if usage["cache_read_input_tokens"] != float64(60) {
		t.Errorf("cache_read_input_tokens = %v, want 60", usage["cache_read_input_tokens"])
	}
	if usage["cache_creation_input_tokens"] != float64(10) {
		t.Errorf("cache_creation_input_tokens = %v, want 10", usage["cache_creation_input_tokens"])
	}
}

// TestOpenAIToAnthropicTransformer_UsageAfterFinish pins the documented
// limitation of the standard `stream_options.include_usage` ordering: the
// usage-only chunk follows the finish_reason chunk, and the closing
// message_delta has already been emitted by then, so the client-facing usage
// keeps the character-count estimate. (Nenya's own accounting is unaffected —
// the reader records the real usage before the transformer runs.) NENYA-167
// records this as a known limitation rather than deferring the terminal events.
func TestOpenAIToAnthropicTransformer_UsageAfterFinish(t *testing.T) {
	tr := NewOpenAIToAnthropicTransformer()
	ctx := context.Background()

	content, _ := json.Marshal(map[string]interface{}{
		"id": "chatcmpl-1", "model": "gpt-4",
		"choices": []interface{}{map[string]interface{}{
			"index": 0, "delta": map[string]interface{}{"content": "hello there"}, "finish_reason": nil,
		}},
	})
	if _, err := tr.TransformSSEChunk(ctx, content); err != nil {
		t.Fatalf("content chunk: %v", err)
	}
	finish, _ := json.Marshal(map[string]interface{}{
		"id": "chatcmpl-1", "model": "gpt-4",
		"choices": []interface{}{map[string]interface{}{
			"index": 0, "delta": map[string]interface{}{}, "finish_reason": "stop",
		}},
	})
	finishOut, err := tr.TransformSSEChunk(ctx, finish)
	if err != nil {
		t.Fatalf("finish chunk: %v", err)
	}
	// AUD-29 (NENYA-241): the tail is HELD at finish under the standard
	// include_usage ordering - publishing it here would carry 0/0 (or the
	// char estimate) because the real usage arrives in the NEXT chunk.
	if usage := usageFromEvents(t, finishOut); usage != nil {
		t.Fatalf("the finish chunk must hold the message_delta, got usage %v", usage)
	}

	// The trailing usage-only chunk is captured AND flushes the held tail:
	// the client now sees the real counts, not the estimate.
	usageChunk, _ := json.Marshal(map[string]interface{}{
		"id": "chatcmpl-1", "model": "gpt-4", "choices": []interface{}{},
		"usage": map[string]interface{}{"prompt_tokens": 100.0, "completion_tokens": 42.0, "total_tokens": 142.0},
	})
	out, err := tr.TransformSSEChunk(ctx, usageChunk)
	if err != nil {
		t.Fatalf("usage chunk: %v", err)
	}
	usage := usageFromEvents(t, out)
	if usage == nil {
		t.Fatal("the trailing usage chunk must flush the held message_delta")
	}
	if usage["output_tokens"] != float64(42) {
		t.Errorf("output_tokens = %v, want the upstream-reported 42", usage["output_tokens"])
	}
	if usage["input_tokens"] != float64(100) {
		t.Errorf("input_tokens = %v, want 100", usage["input_tokens"])
	}
	if !strings.Contains(string(out), "message_stop") {
		t.Fatalf("the flushed tail must end with message_stop, got %s", out)
	}
	if tr.outputTokens != 42 {
		t.Errorf("captured outputTokens = %d, want 42", tr.outputTokens)
	}

	// Nothing is pending at [DONE]: the sentinel passes through unchanged.
	if done, err := tr.TransformSSEChunk(ctx, []byte("[DONE]")); err != nil || string(done) != "[DONE]" {
		t.Fatalf("[DONE] = %q, %v", done, err)
	}
}

// TestOpenAIToAnthropicTransformer_UsageEstimateFallback pins that the
// character-count estimate is used only when the stream carried no usage.
func TestOpenAIToAnthropicTransformer_UsageEstimateFallback(t *testing.T) {
	tr := NewOpenAIToAnthropicTransformer()
	ctx := context.Background()
	content, _ := json.Marshal(map[string]interface{}{
		"id": "chatcmpl-1", "model": "gpt-4",
		"choices": []interface{}{map[string]interface{}{
			"index": 0, "delta": map[string]interface{}{"content": "hello there"}, "finish_reason": "stop",
		}},
	})
	if _, err := tr.TransformSSEChunk(ctx, content); err != nil {
		t.Fatalf("chunk: %v", err)
	}
	usage := usageFromEvents(t, flushStreamTail(t, tr))
	if usage == nil {
		t.Fatal("expected a message_delta with usage")
	}
	if usage["output_tokens"] != float64(len("hello there")/4) {
		t.Errorf("output_tokens = %v, want the chars/4 estimate", usage["output_tokens"])
	}
	if usage["input_tokens"] != float64(0) {
		t.Errorf("input_tokens = %v, want 0 when the stream carried no usage", usage["input_tokens"])
	}
}
