package stream

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"

	"nenya/internal/util"
)

type anthropicBlock struct {
	index       int
	blockType   string
	toolUseID   string
	toolUseName string
}

type AnthropicTransformer struct {
	mu           sync.Mutex
	messageID    string
	model        string
	promptTokens int
	outputTokens int
	hasToolCalls bool
	blockMap     map[int]*anthropicBlock
	streamDone   bool
}

func NewAnthropicTransformer() *AnthropicTransformer {
	return &AnthropicTransformer{
		blockMap: make(map[int]*anthropicBlock),
	}
}

func (t *AnthropicTransformer) TransformSSEChunk(ctx context.Context, data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, ErrEventConsumed
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	var event map[string]interface{}
	if err := json.Unmarshal(data, &event); err != nil {
		slog.Debug("Failed to unmarshal SSE event, passthrough", "error", err, "data", string(data))
		return data, nil
	}

	eventType, _ := event["type"].(string)

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.streamDone {
		slog.Debug("Dropping chunk after stream_done", "event", eventType)
		return nil, ErrEventConsumed
	}

	switch eventType {
	case "message_start":
		return t.handleMessageStart(event)
	case "content_block_start":
		return t.handleContentBlockStart(event)
	case "content_block_delta":
		return t.handleContentBlockDelta(event)
	case "content_block_stop":
		return t.handleContentBlockStop()
	case "message_delta":
		return t.handleMessageDelta(event)
	case "message_stop":
		return t.handleMessageStop()
	case "ping":
		return nil, ErrEventConsumed
	default:
		return data, nil
	}
}

func (t *AnthropicTransformer) handleMessageStart(event map[string]interface{}) ([]byte, error) {
	msg, ok := event["message"].(map[string]interface{})
	if !ok {
		return nil, ErrEventConsumed
	}

	t.messageID, _ = msg["id"].(string)
	t.model, _ = msg["model"].(string)

	if t.messageID == "" {
		t.messageID = "anthropic-" + util.GenerateID()
	}

	if usage, ok := msg["usage"].(map[string]interface{}); ok {
		if it, ok := usage["input_tokens"].(float64); ok {
			t.promptTokens = int(it)
		}
	}

	return nil, ErrEventConsumed
}

func (t *AnthropicTransformer) handleContentBlockStart(event map[string]interface{}) ([]byte, error) {
	idx := getFloat64(event, "index")
	if idx > math.MaxInt32 {
		slog.Warn("Content block index exceeds int32 range", "index", idx)
		return nil, ErrEventConsumed
	}
	index := int(idx)

	blockRaw, ok := event["content_block"].(map[string]interface{})
	if !ok {
		return nil, ErrEventConsumed
	}

	blockType, _ := blockRaw["type"].(string)
	blockTypeLower := strings.ToLower(blockType)
	block := &anthropicBlock{index: index, blockType: blockTypeLower}
	t.blockMap[index] = block

	switch blockTypeLower {
	case "text":
		if text, ok := blockRaw["text"].(string); ok && text != "" {
			return t.marshalChunk(t.makeOpenAIChunk(
				map[string]interface{}{"content": text}, nil))
		}
		return nil, ErrEventConsumed
	case "thinking":
		if thinking, ok := blockRaw["thinking"].(string); ok && thinking != "" {
			return t.marshalChunk(t.makeOpenAIChunk(
				map[string]interface{}{"content": "<thinking>" + thinking + "</thinking>"}, nil))
		}
		return nil, ErrEventConsumed
	case "redacted_thinking":
		return nil, ErrEventConsumed
	case "tool_use":
		block.toolUseID, _ = blockRaw["id"].(string)
		block.toolUseName, _ = blockRaw["name"].(string)
		t.hasToolCalls = true

		tcIndex := indexToToolCall(index, t.blockMap)
		tc := map[string]interface{}{
			"index": tcIndex,
			"id":    block.toolUseID,
			"type":  "function",
			"function": map[string]interface{}{
				"name":      block.toolUseName,
				"arguments": "",
			},
		}
		return t.marshalChunk(t.makeOpenAIChunk(
			map[string]interface{}{"tool_calls": []interface{}{tc}}, nil))
	default:
		slog.Debug("Unknown Anthropic content block type", "type", blockType, "normalized", blockTypeLower, "index", index)
		return nil, ErrEventConsumed
	}
}

func (t *AnthropicTransformer) handleContentBlockDelta(event map[string]interface{}) ([]byte, error) {
	idx := getFloat64(event, "index")
	if idx > math.MaxInt32 {
		slog.Warn("Content block delta index exceeds int32 range", "index", idx)
		return nil, ErrEventConsumed
	}
	index := int(idx)

	block, ok := t.blockMap[index]
	if !ok {
		return nil, ErrEventConsumed
	}

	deltaRaw, ok := event["delta"].(map[string]interface{})
	if !ok {
		return nil, ErrEventConsumed
	}

	deltaType, _ := deltaRaw["type"].(string)

	switch block.blockType {
	case "text":
		return t.handleTextDelta(deltaRaw, deltaType, index)
	case "thinking":
		return t.handleThinkingDelta(deltaRaw, deltaType, index)
	case "redacted_thinking":
		return nil, ErrEventConsumed
	case "tool_use":
		return t.handleToolUseDelta(deltaRaw, deltaType, index, t.blockMap)
	default:
		slog.Debug("Unknown Anthropic content block type in delta", "type", block.blockType, "index", index)
		return nil, ErrEventConsumed
	}
}

func (t *AnthropicTransformer) handleTextDelta(deltaRaw map[string]interface{}, deltaType string, index int) ([]byte, error) {
	if deltaType != "text_delta" {
		return nil, ErrEventConsumed
	}

	payload := map[string]interface{}{}
	if text, ok := deltaRaw["text"].(string); ok && text != "" {
		payload["content"] = text
	}

	if len(payload) == 0 {
		return nil, ErrEventConsumed
	}

	return t.marshalChunk(t.makeOpenAIChunk(payload, nil))
}

func (t *AnthropicTransformer) handleThinkingDelta(deltaRaw map[string]interface{}, deltaType string, index int) ([]byte, error) {
	if deltaType != "thinking_delta" {
		return nil, ErrEventConsumed
	}

	payload := map[string]interface{}{}
	if thinking, ok := deltaRaw["thinking"].(string); ok && thinking != "" {
		payload["content"] = thinking
	}

	if len(payload) == 0 {
		return nil, ErrEventConsumed
	}

	return t.marshalChunk(t.makeOpenAIChunk(payload, nil))
}

func (t *AnthropicTransformer) handleToolUseDelta(deltaRaw map[string]interface{}, deltaType string, index int, blocks map[int]*anthropicBlock) ([]byte, error) {
	if deltaType != "input_json_delta" {
		return nil, ErrEventConsumed
	}

	payload := map[string]interface{}{}
	if partial, ok := deltaRaw["partial_json"].(string); ok && partial != "" {
		tcIndex := indexToToolCall(index, blocks)
		payload["tool_calls"] = []interface{}{
			map[string]interface{}{
				"index": tcIndex,
				"function": map[string]interface{}{
					"arguments": partial,
				},
			},
		}
	}

	if len(payload) == 0 {
		return nil, ErrEventConsumed
	}

	return t.marshalChunk(t.makeOpenAIChunk(payload, nil))
}

func (t *AnthropicTransformer) handleContentBlockStop() ([]byte, error) {
	return nil, ErrEventConsumed
}

func (t *AnthropicTransformer) handleMessageDelta(event map[string]interface{}) ([]byte, error) {
	delta, ok := event["delta"].(map[string]interface{})
	if !ok {
		return nil, ErrEventConsumed
	}

	stopReason, _ := delta["stop_reason"].(string)

	if usage, ok := event["usage"].(map[string]interface{}); ok {
		if ot, ok := usage["output_tokens"].(float64); ok {
			t.outputTokens = int(ot)
		}
	}

	finishReason := mapStopReason(stopReason)

	chunk := t.makeOpenAIChunk(map[string]interface{}{}, &finishReason)
	chunk["usage"] = map[string]interface{}{
		"prompt_tokens":     t.promptTokens,
		"completion_tokens": t.outputTokens,
		"total_tokens":      t.promptTokens + t.outputTokens,
	}

	return t.marshalChunk(chunk)
}

func (t *AnthropicTransformer) handleMessageStop() ([]byte, error) {
	t.streamDone = true
	t.blockMap = make(map[int]*anthropicBlock)
	return []byte("[DONE]"), nil
}

// Reset clears all transformer state for reuse with a new stream.
func (t *AnthropicTransformer) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.messageID = ""
	t.model = ""
	t.promptTokens = 0
	t.outputTokens = 0
	t.hasToolCalls = false
	t.blockMap = make(map[int]*anthropicBlock)
	t.streamDone = false
}

func (t *AnthropicTransformer) makeOpenAIChunk(delta map[string]interface{}, finishReason *string) map[string]interface{} {
	chunk := map[string]interface{}{
		"id":      t.messageID,
		"object":  "chat.completion.chunk",
		"created": 0,
		"model":   t.model,
	}

	choice := map[string]interface{}{"index": 0}
	if delta != nil {
		choice["delta"] = delta
	} else {
		choice["delta"] = map[string]interface{}{}
	}
	if finishReason != nil {
		choice["finish_reason"] = *finishReason
	} else {
		choice["finish_reason"] = nil
	}
	chunk["choices"] = []interface{}{choice}

	return chunk
}

func (t *AnthropicTransformer) marshalChunk(chunk map[string]interface{}) ([]byte, error) {
	b, err := json.Marshal(chunk)
	if err != nil {
		return nil, fmt.Errorf("anthropic: failed to marshal streaming chunk: %w", err)
	}
	return b, nil
}

func getFloat64(m map[string]interface{}, key string) float64 {
	if v, ok := m[key].(float64); ok {
		return v
	}
	return 0
}

func mapStopReason(reason string) string {
	switch reason {
	case "end_turn":
		return "stop"
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	default:
		slog.Debug("Unknown Anthropic stop_reason, defaulting to 'stop'", "reason", reason)
		return "stop"
	}
}

func indexToToolCall(idx int, blocks map[int]*anthropicBlock) int {
	if idx < 0 {
		return -1
	}
	tcCount := 0
	for i := 0; i <= idx; i++ {
		if b, ok := blocks[i]; ok && b.blockType == "tool_use" {
			tcCount++
		}
	}
	return tcCount - 1
}
