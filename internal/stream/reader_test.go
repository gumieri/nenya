package stream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
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

func TestSSETransformingReader_AnthropicStream_SawDone(t *testing.T) {
	input := "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_123\",\"model\":\"claude-sonnet-4-6\",\"usage\":{\"input_tokens\":10}}}\n\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n\n" +
		"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"

	tr := NewAnthropicTransformer()
	reader := NewSSETransformingReader(strings.NewReader(input), tr, context.Background())

	var output bytes.Buffer
	_, err := io.Copy(&output, reader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !reader.SawDone() {
		t.Error("expected SawDone() to be true after Anthropic message_stop was processed by transformer")
	}

	result := output.String()
	if strings.Contains(result, "gateway_error") {
		t.Errorf("unexpected gateway_error in output — sawDone fix not working: %s", result)
	}

	doneCount := strings.Count(result, "[DONE]")
	if doneCount != 1 {
		t.Errorf("expected exactly 1 [DONE] in output, got %d: %s", doneCount, result)
	}
}

func TestSSETransformingReader_UpstreamDone_SawDone(t *testing.T) {
	input := "data: {\"chunk\":1}\n\ndata: [DONE]\n\n"

	reader := NewSSETransformingReader(strings.NewReader(input), nil, context.Background())

	var output bytes.Buffer
	_, err := io.Copy(&output, reader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !reader.SawDone() {
		t.Error("expected SawDone() to be true after upstream data: [DONE]")
	}

	result := output.String()
	if strings.Contains(result, "gateway_error") {
		t.Errorf("unexpected gateway_error in output: %s", result)
	}
}

func TestSSETransformingReader_NoDone_InjectsError(t *testing.T) {
	input := "data: {\"chunk\":1}\n\ndata: {\"chunk\":2}\n\n"

	reader := NewSSETransformingReader(strings.NewReader(input), nil, context.Background())

	var output bytes.Buffer
	_, err := io.Copy(&output, reader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if reader.SawDone() {
		t.Error("expected SawDone() to be false when no [DONE] was received")
	}

	result := output.String()
	if !strings.Contains(result, "gateway_error") {
		t.Errorf("expected gateway_error injection when stream ends without [DONE]: %s", result)
	}
}

func TestSSETransformingReader_AnthropicMessageStartWithCacheFields(t *testing.T) {
	event := map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"model":         "claude-sonnet-4-6",
			"id":            "msg_01Q4djwS6AqPt2B16UeHmeQC",
			"type":          "message",
			"role":          "assistant",
			"content":       []interface{}{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"stop_details":  nil,
			"usage": map[string]interface{}{
				"input_tokens":                float64(5668),
				"cache_creation_input_tokens": float64(54947),
				"cache_read_input_tokens":     float64(0),
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

func TestSSETransformingReader_LogsMalformedJSON(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	malformedData := "data: {\"id\":\"123\",\"incomplete\ndata: {\"valid\":\"json\"}\n\ndata: [DONE]\n\n"
	reader := NewSSETransformingReader(strings.NewReader(malformedData), nil, context.Background())
	reader.SetLogger(logger)

	var output bytes.Buffer
	_, err := io.Copy(&output, reader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	outputStr := output.String()
	if !strings.Contains(outputStr, "incomplete\ndata:") {
		t.Errorf("output should contain malformed data: %s", outputStr)
	}

	logStr := logBuf.String()
	if !strings.Contains(logStr, "malformed JSON in SSE data line") {
		t.Errorf("expected malformed JSON warning in logs, got: %s", logStr)
	}
	if !strings.Contains(logStr, "data_len") {
		t.Errorf("expected data_len in log, got: %s", logStr)
	}
}

func TestSSETransformingReader_ValidJSONNoLog(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	validData := "data: {\"id\":\"123\",\"chunk\":\"hello\"}\n\ndata: [DONE]\n\n"
	reader := NewSSETransformingReader(strings.NewReader(validData), nil, context.Background())
	reader.SetLogger(logger)

	var output bytes.Buffer
	_, err := io.Copy(&output, reader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	logStr := logBuf.String()
	if strings.Contains(logStr, "malformed JSON") {
		t.Errorf("unexpected malformed JSON warning for valid data: %s", logStr)
	}
}

func TestSSETransformingReader_UsageCallbackWithCacheFields(t *testing.T) {
	var receivedCompletion, receivedPrompt, receivedTotal, receivedCacheHit, receivedCacheMiss, receivedCacheCreation, receivedReasoning int

	cb := func(completion, prompt, total, cacheHit, cacheMiss, cacheCreation, reasoning int) {
		receivedCompletion = completion
		receivedPrompt = prompt
		receivedTotal = total
		receivedCacheHit = cacheHit
		receivedCacheMiss = cacheMiss
		receivedCacheCreation = cacheCreation
		receivedReasoning = reasoning
	}

	input := "data: {\"id\":\"msg_1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"}}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":10,\"total_tokens\":110,\"prompt_cache_hit_tokens\":5000,\"prompt_cache_miss_tokens\":2000,\"cache_creation_tokens\":54947}}\n\ndata: [DONE]\n\n"

	reader := NewSSETransformingReader(strings.NewReader(input), nil, context.Background())
	reader.SetOnUsage(cb)

	var output bytes.Buffer
	_, err := io.Copy(&output, reader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if receivedCompletion != 10 {
		t.Errorf("expected completion_tokens=10, got %d", receivedCompletion)
	}
	if receivedPrompt != 100 {
		t.Errorf("expected prompt_tokens=100, got %d", receivedPrompt)
	}
	if receivedTotal != 110 {
		t.Errorf("expected total_tokens=110, got %d", receivedTotal)
	}
	if receivedCacheHit != 5000 {
		t.Errorf("expected cache_hit_tokens=5000, got %d", receivedCacheHit)
	}
	if receivedCacheMiss != 2000 {
		t.Errorf("expected cache_miss_tokens=2000, got %d", receivedCacheMiss)
	}
	if receivedCacheCreation != 54947 {
		t.Errorf("expected cache_creation_tokens=54947, got %d", receivedCacheCreation)
	}
	if receivedReasoning != 0 {
		t.Errorf("expected reasoning_tokens=0, got %d", receivedReasoning)
	}
}

func TestSSETransformingReader_UsageCallbackDeltaCalculation(t *testing.T) {
	var callCount int
	var lastDCompletion, lastDPrompt, lastDCacheCreation int

	cb := func(completion, prompt, total, cacheHit, cacheMiss, cacheCreation, reasoning int) {
		callCount++
		lastDCompletion = completion
		lastDPrompt = prompt
		lastDCacheCreation = cacheCreation
	}

	input := "data: {\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":10,\"total_tokens\":110,\"cache_creation_tokens\":54947}}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":200,\"completion_tokens\":20,\"total_tokens\":220,\"cache_creation_tokens\":54947}}\n\n" +
		"data: [DONE]\n\n"

	reader := NewSSETransformingReader(strings.NewReader(input), nil, context.Background())
	reader.SetOnUsage(cb)

	var output bytes.Buffer
	_, err := io.Copy(&output, reader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if callCount != 2 {
		t.Errorf("expected 2 usage callbacks (1 per usage event), got %d", callCount)
	}

	if lastDCompletion != 10 {
		t.Errorf("expected final delta completion=10 (20-10), got %d", lastDCompletion)
	}
	if lastDPrompt != 100 {
		t.Errorf("expected final delta prompt=100 (200-100), got %d", lastDPrompt)
	}

	if lastDCacheCreation != 0 {
		t.Errorf("expected delta cache_creation=0 (54947-54947, no new creation), got %d", lastDCacheCreation)
	}
}

func TestSSETransformingReader_UsageCallbackWithReasoningTokens(t *testing.T) {
	var receivedCompletion, receivedPrompt, receivedReasoning int

	cb := func(completion, prompt, total, cacheHit, cacheMiss, cacheCreation, reasoning int) {
		receivedCompletion = completion
		receivedPrompt = prompt
		receivedReasoning = reasoning
	}

	input := "data: {\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":10,\"total_tokens\":110,\"reasoning_tokens\":500}}\n\ndata: [DONE]\n\n"

	reader := NewSSETransformingReader(strings.NewReader(input), nil, context.Background())
	reader.SetOnUsage(cb)

	var output bytes.Buffer
	_, err := io.Copy(&output, reader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if receivedCompletion != 10 {
		t.Errorf("expected completion_tokens=10, got %d", receivedCompletion)
	}
	if receivedPrompt != 100 {
		t.Errorf("expected prompt_tokens=100, got %d", receivedPrompt)
	}
	if receivedReasoning != 500 {
		t.Errorf("expected reasoning_tokens=500, got %d", receivedReasoning)
	}
}

func TestSSETransformingReader_UsageCallbackReasoningDecrease(t *testing.T) {
	var callCount int
	var receivedCompletion, receivedReasoning int

	cb := func(completion, prompt, total, cacheHit, cacheMiss, cacheCreation, reasoning int) {
		callCount++
		receivedCompletion = completion
		receivedReasoning = reasoning
	}

	input := "data: {\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":10,\"total_tokens\":110,\"reasoning_tokens\":500}}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":200,\"completion_tokens\":20,\"total_tokens\":220,\"reasoning_tokens\":400}}\n\n" +
		"data: [DONE]\n\n"

	reader := NewSSETransformingReader(strings.NewReader(input), nil, context.Background())
	reader.SetOnUsage(cb)

	var output bytes.Buffer
	_, err := io.Copy(&output, reader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if callCount != 2 {
		t.Fatalf("expected 2 callbacks, got %d", callCount)
	}

	if receivedCompletion != 10 {
		t.Errorf("expected final delta completion=10, got %d", receivedCompletion)
	}

	if receivedReasoning != 0 {
		t.Errorf("expected final callback reasoning=0 when reasoning decreased (500→400), got %d", receivedReasoning)
	}
}

func TestSSETransformingReader_TransformedSizeLimit(t *testing.T) {
	// Generate SSE data that exceeds configured limit (256KB)
	chunkSize := 1024
	numChunks := 300
	var sb strings.Builder
	for i := 0; i < numChunks; i++ {
		content := strings.Repeat("x", chunkSize)
		sb.WriteString("data: {\"choices\":[{\"delta\":{\"content\":\"")
		sb.WriteString(content)
		sb.WriteString("\"}}]}\n\n")
	}
	sb.WriteString("data: [DONE]\n\n")

	reader := NewSSETransformingReader(strings.NewReader(sb.String()), nil, context.Background())
	reader.SetMaxTransformedBytes(256 * 1024)

	var output bytes.Buffer
	_, err := io.Copy(&output, reader)

	if !errors.Is(err, ErrTransformedSizeExceeded) {
		t.Fatalf("expected ErrTransformedSizeExceeded, got %v", err)
	}

	result := output.String()
	if !strings.Contains(result, "gateway_error") {
		t.Error("expected gateway_error in output when size limit exceeded")
	}
	if !strings.Contains(result, "[DONE]") {
		t.Error("expected [DONE] in output after size limit error")
	}
}

func TestSSETransformingReader_NormalSizeDoesNotTrip(t *testing.T) {
	input := "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_123\",\"model\":\"claude-sonnet-4-6\",\"usage\":{\"input_tokens\":10}}}\n\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello world this is a test\"}}\n\n" +
		"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"

	tr := NewAnthropicTransformer()
	reader := NewSSETransformingReader(strings.NewReader(input), tr, context.Background())

	var output bytes.Buffer
	_, err := io.Copy(&output, reader)
	if err != nil {
		t.Fatalf("unexpected error for normal-sized stream: %v", err)
	}

	result := output.String()
	if strings.Contains(result, "gateway_error") {
		t.Errorf("unexpected gateway_error for normal stream: %s", result)
	}
	if !strings.Contains(result, "[DONE]") {
		t.Errorf("expected [DONE] in normal stream output, got: %s", result)
	}
}

func TestSSETransformingReader_ResetCounters(t *testing.T) {
	reader := NewSSETransformingReader(strings.NewReader(""), nil, context.Background())
	reader.transformedBytes = 50000
	reader.discarding = true
	reader.warnedAtThreshold = true

	reader.ResetCounters()

	if reader.transformedBytes != 0 {
		t.Errorf("expected transformedBytes=0 after reset, got %d", reader.transformedBytes)
	}
	if reader.discarding {
		t.Error("expected discarding=false after reset")
	}
	if reader.warnedAtThreshold {
		t.Error("expected warnedAtThreshold=false after reset")
	}
}

func TestSSETransformingReader_SetMaxTransformedBytes(t *testing.T) {
	reader := NewSSETransformingReader(strings.NewReader(""), nil, context.Background())
	if reader.maxTransformedBytes != DefaultMaxTransformedEventBytes {
		t.Errorf("expected default %d, got %d", DefaultMaxTransformedEventBytes, reader.maxTransformedBytes)
	}

	customLimit := 10 * 1024 * 1024
	reader.SetMaxTransformedBytes(customLimit)
	if reader.maxTransformedBytes != customLimit {
		t.Errorf("expected %d, got %d", customLimit, reader.maxTransformedBytes)
	}

	reader.SetMaxTransformedBytes(-1)
	if reader.maxTransformedBytes != DefaultMaxTransformedEventBytes {
		t.Errorf("expected reset to default %d on negative input, got %d", DefaultMaxTransformedEventBytes, reader.maxTransformedBytes)
	}

	reader.SetMaxTransformedBytes(0)
	if reader.maxTransformedBytes != DefaultMaxTransformedEventBytes {
		t.Errorf("expected reset to default %d on zero input, got %d", DefaultMaxTransformedEventBytes, reader.maxTransformedBytes)
	}
}
