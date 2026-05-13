package adapter

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

type AnthropicAdapter struct {
	version string
}

var (
	anthropicOnce    sync.Once
	anthropicAdapter *AnthropicAdapter
)

// GetAnthropicAdapter returns a singleton AnthropicAdapter instance.
func GetAnthropicAdapter() *AnthropicAdapter {
	anthropicOnce.Do(func() {
		anthropicAdapter = NewAnthropicAdapter()
	})
	return anthropicAdapter
}

// ConvertOpenAIToAnthropicBody converts an OpenAI-format request body
// (as a parsed map) to the Anthropic Messages API format.
func (a *AnthropicAdapter) ConvertOpenAIToAnthropicBody(openai map[string]interface{}, model string, stream bool) map[string]interface{} {
	return a.convertOpenAIToAnthropic(openai, model, stream)
}

// ConvertAnthropicToOpenAIBody converts an Anthropic-format response
// (as a parsed map) to the OpenAI chat completions format.
func (a *AnthropicAdapter) ConvertAnthropicToOpenAIBody(anthropic map[string]interface{}) map[string]interface{} {
	return a.convertAnthropicToOpenAI(anthropic)
}

// NewAnthropicAdapter creates a new AnthropicAdapter instance.
func NewAnthropicAdapter() *AnthropicAdapter {
	return &AnthropicAdapter{
		version: "2023-06-01",
	}
}

// InjectAuth adds the Anthropic-specific authentication headers (x-api-key and anthropic-version).
func (a *AnthropicAdapter) InjectAuth(req *http.Request, apiKey string) error {
	if err := verifyAPIKey(apiKey, "anthropic"); err != nil {
		return err
	}
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", a.version)
	return nil
}

// MutateRequest converts an OpenAI-format request body to Anthropic format.
func (a *AnthropicAdapter) MutateRequest(body []byte, model string, stream bool) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}

	var openai map[string]interface{}
	if err := json.Unmarshal(body, &openai); err != nil {
		return body, nil
	}

	anthropic := a.convertOpenAIToAnthropic(openai, model, stream)

	out, err := json.Marshal(anthropic)
	if err != nil {
		return body, fmt.Errorf("anthropic: failed to marshal converted request: %w", err)
	}
	return out, nil
}

// MutateResponse converts an Anthropic-format response to OpenAI format.
func (a *AnthropicAdapter) MutateResponse(body []byte) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}

	var anthropic map[string]interface{}
	if err := json.Unmarshal(body, &anthropic); err != nil {
		return body, nil
	}

	openai := a.convertAnthropicToOpenAI(anthropic)

	out, err := json.Marshal(openai)
	if err != nil {
		return body, fmt.Errorf("anthropic: failed to marshal converted response: %w", err)
	}
	return out, nil
}

// NormalizeError classifies Anthropic HTTP errors into retryable, rate-limited, quota-exhausted, or permanent.
func (a *AnthropicAdapter) NormalizeError(statusCode int, body []byte) ErrorClass {
	switch statusCode {
	case 429:
		return ErrorRateLimited
	case 500, 502, 503, 504:
		return ErrorRetryable
	case 400:
		lower := strings.ToLower(string(body))
		if strings.Contains(lower, "overloaded") || strings.Contains(lower, "rate") {
			return ErrorRetryable
		}
		return ErrorPermanent
	case 529:
		return ErrorRateLimited
	default:
		return ErrorPermanent
	}
}

func (a *AnthropicAdapter) convertOpenAIToAnthropic(openai map[string]interface{}, model string, stream bool) map[string]interface{} {
	anthropic := map[string]interface{}{
		"model":      model,
		"stream":     stream,
		"max_tokens": 8192,
	}

	a.copyOpenAIFields(openai, anthropic)

	if msgs, ok := openai["messages"].([]interface{}); ok {
		systemParts := a.extractSystemMessages(msgs)
		if len(systemParts) > 0 {
			anthropic["system"] = strings.Join(systemParts, "\n\n")
		}
		anthropic["messages"] = a.convertMessages(msgs)
	}

	if tools, ok := openai["tools"].([]interface{}); ok && len(tools) > 0 {
		anthropic["tools"] = a.convertTools(tools)
	}

	if tc, ok := openai["tool_choice"]; ok {
		a.convertToolChoice(tc, anthropic)
	}

	return anthropic
}

func (a *AnthropicAdapter) copyOpenAIFields(openai, anthropic map[string]interface{}) {
	if v, ok := openai["max_tokens"].(float64); ok && v > 0 {
		anthropic["max_tokens"] = int(v)
	}
	if v, ok := openai["temperature"].(float64); ok {
		anthropic["temperature"] = v
	}
	if v, ok := openai["top_p"].(float64); ok {
		anthropic["top_p"] = v
	}
	if v, ok := openai["stop"]; ok {
		anthropic["stop_sequences"] = v
	}
	if v, ok := openai["user"].(string); ok {
		anthropic["metadata"] = map[string]interface{}{
			"user_id": v,
		}
	}
	if v, ok := openai["stream"].(bool); ok {
		anthropic["stream"] = v
	}
}

func (a *AnthropicAdapter) extractSystemMessages(msgs []interface{}) []string {
	var systemParts []string
	for _, msgRaw := range msgs {
		msg, ok := msgRaw.(map[string]interface{})
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role == "system" {
			systemParts = append(systemParts, a.extractSystemContent(msg["content"])...)
		}
	}
	return systemParts
}

func (a *AnthropicAdapter) extractSystemContent(content interface{}) []string {
	var parts []string
	switch c := content.(type) {
	case string:
		if c != "" {
			parts = append(parts, c)
		}
	case []interface{}:
		for _, partRaw := range c {
			if part, ok := partRaw.(map[string]interface{}); ok {
				if t, ok := part["text"].(string); ok && t != "" {
					parts = append(parts, t)
				}
			}
		}
	}
	return parts
}

func (a *AnthropicAdapter) convertToolChoice(tc interface{}, anthropic map[string]interface{}) {
	if s, ok := tc.(string); ok {
		switch s {
		case "auto", "required":
			anthropic["tool_choice"] = map[string]interface{}{"type": s}
		case "none":
			delete(anthropic, "tool_choice")
		}
		return
	}
	if m, ok := tc.(map[string]interface{}); ok {
		if fn, ok := m["function"].(map[string]interface{}); ok {
			if name, ok := fn["name"].(string); ok {
				anthropic["tool_choice"] = map[string]interface{}{
					"type": "tool",
					"name": name,
				}
			}
		}
	}
}

func (a *AnthropicAdapter) convertMessages(msgs []interface{}) []interface{} {
	var result []interface{}
	toolUseIDs := a.extractToolUseIDs(msgs)
	nextID := 0
	for _, msgRaw := range msgs {
		msg, ok := msgRaw.(map[string]interface{})
		if !ok {
			continue
		}

		role, _ := msg["role"].(string)

		if role == "system" {
			continue
		}

		if role == "tool" {
			toolUseID := ""
			if nextID < len(toolUseIDs) {
				toolUseID = toolUseIDs[nextID]
				nextID++
			}
			anthMsg := map[string]interface{}{
				"role":    "user",
				"content": a.convertToolMessage(msg["content"], toolUseID),
			}
			result = append(result, anthMsg)
			continue
		}

		contentBlocks := a.buildContentBlocks(msg)
		anthMsg := map[string]interface{}{
			"role":    role,
			"content": contentBlocks,
		}
		result = append(result, anthMsg)
	}
	return result
}

// emptyTextBlock is a minimal Anthropic text content block used as a fallback
// when a message has no text or tool_use content. Anthropic requires every
// user/assistant message to have a non-empty content array.
var emptyTextBlock = map[string]interface{}{"type": "text", "text": " "}

// buildContentBlocks constructs an Anthropic content array from an OpenAI-format
// message. It converts text content into typed text blocks and OpenAI tool_calls
// into Anthropic tool_use blocks. Returns a non-empty slice guaranteed to have at
// least one content block to satisfy Anthropic's API requirement.
func (a *AnthropicAdapter) buildContentBlocks(msg map[string]interface{}) []interface{} {
	var blocks []interface{}

	content := msg["content"]
	switch c := content.(type) {
	case string:
		if strings.TrimSpace(c) != "" {
			blocks = append(blocks, map[string]interface{}{
				"type": "text",
				"text": c,
			})
		}
	case []interface{}:
		for _, part := range c {
			if pm, ok := part.(map[string]interface{}); ok {
				blocks = append(blocks, pm)
			}
		}
	}

	if role, _ := msg["role"].(string); role == "assistant" {
		if tcs, ok := msg["tool_calls"].([]interface{}); ok {
			for _, tc := range tcs {
				toolUse := a.convertToolCallToUse(tc)
				if toolUse != nil {
					blocks = append(blocks, toolUse)
				}
			}
		}
	}

	if len(blocks) == 0 {
		blocks = []interface{}{emptyTextBlock}
	}
	return blocks
}

// convertToolCallToUse converts a single OpenAI tool_call object to an Anthropic
// tool_use content block. Returns nil if the input is not a valid function tool call.
func (a *AnthropicAdapter) convertToolCallToUse(tc interface{}) map[string]interface{} {
	tcm, ok := tc.(map[string]interface{})
	if !ok {
		return nil
	}
	fn, ok := tcm["function"].(map[string]interface{})
	if !ok {
		return nil
	}

	id, _ := tcm["id"].(string)
	name, _ := fn["name"].(string)

	var input interface{}
	if args, ok := fn["arguments"].(string); ok && args != "" {
		_ = json.Unmarshal([]byte(args), &input)
	}
	if input == nil {
		input = map[string]interface{}{}
	}

	return map[string]interface{}{
		"type":  "tool_use",
		"id":    id,
		"name":  name,
		"input": input,
	}
}

// extractToolUseIDs scans the messages for assistant messages with tool_calls
// and returns a slice of the Anthropic tool_use.id values in order of appearance.
// These are extracted from the client's OpenAI-format tool_calls[].id field
// (which should contain the Anthropic tool_use.id from the original response).
func (a *AnthropicAdapter) extractToolUseIDs(msgs []interface{}) []string {
	var ids []string
	for _, msgRaw := range msgs {
		msg, ok := msgRaw.(map[string]interface{})
		if !ok {
			continue
		}
		if msg["role"] != "assistant" {
			continue
		}
		tcs, ok := msg["tool_calls"].([]interface{})
		if !ok {
			continue
		}
		for _, tc := range tcs {
			tcm, ok := tc.(map[string]interface{})
			if !ok {
				continue
			}
			if id, ok := tcm["id"].(string); ok && id != "" {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

func (a *AnthropicAdapter) convertToolMessage(content interface{}, toolUseID string) []interface{} {
	toolContent := ""
	if content != nil {
		if s, ok := content.(string); ok {
			toolContent = s
		}
	}
	result := map[string]interface{}{
		"type":    "tool_result",
		"content": toolContent,
	}
	if toolUseID != "" {
		result["tool_use_id"] = toolUseID
	}
	return []interface{}{result}
}

func (a *AnthropicAdapter) convertTools(tools []interface{}) []interface{} {
	var result []interface{}
	for _, toolRaw := range tools {
		tool, ok := toolRaw.(map[string]interface{})
		if !ok {
			continue
		}

		if tool["type"] != "function" {
			continue
		}

		fn, ok := tool["function"].(map[string]interface{})
		if !ok {
			continue
		}

		name, _ := fn["name"].(string)
		desc, _ := fn["description"].(string)
		params := fn["parameters"]

		anthTool := map[string]interface{}{
			"name":        name,
			"description": desc,
		}

		if params != nil {
			anthTool["input_schema"] = params
		} else {
			anthTool["input_schema"] = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
		}

		result = append(result, anthTool)
	}
	return result
}

func (a *AnthropicAdapter) convertAnthropicToOpenAI(anthropic map[string]interface{}) map[string]interface{} {
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

	a.processAnthropicContent(anthropic, delta, choice)

	a.processStopReason(anthropic, choice)

	choice["delta"] = delta
	openai["choices"] = []interface{}{choice}

	a.processUsage(anthropic, openai)

	return openai
}

// extractContentBlocks extracts text parts and tool calls from Anthropic content blocks.
func (a *AnthropicAdapter) extractContentBlocks(blocks []interface{}) ([]string, []interface{}) {
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
			if t, ok := bm["text"].(string); ok {
				textParts = append(textParts, t)
			}
		case "tool_use":
			toolCalls = append(toolCalls, a.convertToolUseBlock(bm))
		case "tool_result":
			if t, ok := bm["content"].(string); ok {
				textParts = append(textParts, t)
			}
		}
	}
	return textParts, toolCalls
}

func (a *AnthropicAdapter) processAnthropicContent(anthropic, delta, choice map[string]interface{}) {
	content, ok := anthropic["content"]
	if !ok {
		return
	}

	switch c := content.(type) {
	case string:
		delta["content"] = c
		choice["finish_reason"] = "stop"
	case []interface{}:
		textParts, toolCalls := a.extractContentBlocks(c)
		if len(textParts) > 0 {
			delta["content"] = strings.Join(textParts, "")
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

func (a *AnthropicAdapter) convertToolUseBlock(bm map[string]interface{}) map[string]interface{} {
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

func (a *AnthropicAdapter) processStopReason(anthropic, choice map[string]interface{}) {
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

func (a *AnthropicAdapter) processUsage(anthropic, openai map[string]interface{}) {
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
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = charset[b[i]%byte(len(charset))]
	}
	return string(b)
}
