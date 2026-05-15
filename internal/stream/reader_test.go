package stream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestSSETransformingReader_AnthropicMessageStartSuppressed(t *testing.T) {
	input := "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_123\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-4-6\",\"content\":[],\"usage\":{\"input_tokens\":5668}}}\n\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n" +
		"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":10}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"

	tr := NewAnthropicTransformer()
	reader := NewSSETransformingReader(strings.NewReader(input), tr, context.Background())

	var output bytes.Buffer
	_, err := io.Copy(&output, reader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	result := output.String()

	if strings.Contains(result, "message_start") {
		t.Errorf("raw message_start event leaked to client: %s", result)
	}
	if strings.Contains(result, "content_block_stop") {
		t.Errorf("raw content_block_stop event leaked to client: %s", result)
	}
	if !strings.Contains(result, "chat.completion.chunk") {
		t.Errorf("expected OpenAI chat.completion.chunk in output, got: %s", result)
	}
	if !strings.Contains(result, "[DONE]") {
		t.Errorf("expected [DONE] in output, got: %s", result)
	}
}

func TestSSETransformingReader_AnthropicMessageStartWithCacheFields(t *testing.T) {
	event := map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"model":        "claude-sonnet-4-6",
			"id":           "msg_01Q4djwS6AqPt2B16UeHmeQC",
			"type":         "message",
			"role":         "assistant",
			"content":      []interface{}{},
			"stop_reason":  nil,
			"stop_sequence": nil,
			"stop_details":  nil,
			"usage": map[string]interface{}{
				"input_tokens": float64(5668),
				"cache_creation_input_tokens": float64(54947),
				"cache_read_input_tokens":    float64(0),
				"cache_creation": map[string]interface{}{
					"ephemeral_5m_input_tokens": float64(54947),
					"ephemeral_1h_input_tokens": float64(0),
				},
				"output_tokens": float64(1),
			},
		},
	}
	data, _ := json.Marshal(event)
	tr := NewAnthropicTransformer()
	out, err := tr.TransformSSEChunk(context.Background(), data)
	if !errors.Is(err, ErrEventConsumed) {
		t.Errorf("expected ErrEventConsumed, got: out=%s err=%v", string(out), err)
	}
}

func TestSSETransformingReader_SkipsConsumedEvents(t *testing.T) {
	input := "data: {\"type\":\"ping\"}\n\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"claude-3\",\"usage\":{\"input_tokens\":10}}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"

	tr := NewAnthropicTransformer()
	reader := NewSSETransformingReader(strings.NewReader(input), tr, context.Background())
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, reader)

	result := buf.String()
	if strings.Contains(result, "message_start") {
		t.Errorf("consumed message_start leaked: %s", result)
	}
	if strings.Contains(result, "ping") {
		t.Errorf("consumed ping leaked: %s", result)
	}
	if !strings.Contains(result, "[DONE]") {
		t.Errorf("expected [DONE] in output, got: %s", result)
	}
}
