package stream

import (
	"encoding/json"
	"strings"
)

type AnthropicTransformer struct {
	version string
}

func NewAnthropicTransformer() *AnthropicTransformer {
	return &AnthropicTransformer{
		version: "2023-06-01",
	}
}

func (t *AnthropicTransformer) TransformSSEChunk(data []byte) ([]byte, error) {
	var anthropicChunk map[string]interface{}
	if err := json.Unmarshal(data, &anthropicChunk); err != nil {
		return data, nil
	}

	openaiChunk := t.convertAnthropicToOpenAI(anthropicChunk)
	if openaiChunk == nil {
		return data, nil
	}

	return json.Marshal(openaiChunk)
}

func (t *AnthropicTransformer) convertAnthropicToOpenAI(anthropic map[string]interface{}) map[string]interface{} {
	openai := map[string]interface{}{
		"id":      "anthropic-" + generateID(),
		"object":  "chat.completion",
		"created": 0,
		"model":   anthropic["model"],
	}

	choice := map[string]interface{}{
		"index": 0,
	}
	delta := map[string]interface{}{}

	t.processAnthropicContent(anthropic, delta, choice)

	t.processStopReason(anthropic, choice)

	choice["delta"] = delta
	openai["choices"] = []interface{}{choice}

	t.processUsage(anthropic, openai)

	return openai
}

func (t *AnthropicTransformer) processAnthropicContent(anthropic, delta, choice map[string]interface{}) {
	content, ok := anthropic["content"]
	if !ok {
		return
	}

	switch c := content.(type) {
	case string:
		delta["content"] = c
		choice["finish_reason"] = "stop"
	case []interface{}:
		textParts, toolCalls := t.extractContentBlocks(c)
		if len(textParts) > 0 {
			delta["content"] = joinStrings(textParts)
		}
		if len(toolCalls) > 0 {
			delta["tool_calls"] = toolCalls
			choice["finish_reason"] = "tool_calls"
		}
		if choice["finish_reason"] == nil {
			choice["finish_reason"] = "stop"
		}
	}
}

func (t *AnthropicTransformer) extractContentBlocks(blocks []interface{}) ([]string, []interface{}) {
	var textParts []string
	var toolCalls []interface{}
	for _, block := range blocks {
		bm, ok := block.(map[string]interface{})
		if !ok {
			continue
		}
		bType, _ := bm["type"].(string)
		switch bType {
		case "text":
			if text, ok := bm["text"].(string); ok {
				textParts = append(textParts, text)
			}
		case "tool_use":
			toolCalls = append(toolCalls, t.convertToolUseBlock(bm))
		case "tool_result":
			if text, ok := bm["content"].(string); ok {
				textParts = append(textParts, text)
			}
		}
	}
	return textParts, toolCalls
}

func (t *AnthropicTransformer) convertToolUseBlock(bm map[string]interface{}) map[string]interface{} {
	tc := map[string]interface{}{
		"id":   bm["id"],
		"type": "function",
		"function": map[string]interface{}{
			"name":      bm["name"],
			"arguments": "{}",
		},
	}
	if inp, ok := bm["input"]; ok {
		argsBytes, _ := json.Marshal(inp)
		tc["function"].(map[string]interface{})["arguments"] = string(argsBytes)
	}
	return tc
}

func (t *AnthropicTransformer) processStopReason(anthropic, choice map[string]interface{}) {
	stopReason, ok := anthropic["stop_reason"].(string)
	if !ok {
		return
	}

	switch stopReason {
	case "end_turn":
		choice["finish_reason"] = "stop"
	case "tool_use":
		choice["finish_reason"] = "tool_calls"
	case "max_tokens":
		choice["finish_reason"] = "length"
	default:
		choice["finish_reason"] = "stop"
	}
}

func (t *AnthropicTransformer) processUsage(anthropic, openai map[string]interface{}) {
	usage, ok := anthropic["usage"].(map[string]interface{})
	if !ok {
		return
	}

	openai["usage"] = map[string]interface{}{
		"prompt_tokens":     usage["input_tokens"],
		"completion_tokens": usage["output_tokens"],
		"total_tokens":      addFloat64(usage["input_tokens"], usage["output_tokens"]),
	}
}

func addFloat64(a, b interface{}) float64 {
	af, _ := a.(float64)
	bf, _ := b.(float64)
	return af + bf
}

func generateID() string {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 24)
	for i := range b {
		b[i] = charset[i%len(charset)]
	}
	return string(b)
}

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
