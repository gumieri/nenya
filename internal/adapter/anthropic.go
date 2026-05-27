package adapter

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"sync"

	"git.0ur.uk/nenya/internal/util"
)

const (
	maxPreFlightBodyBytes = 1 * 1024 * 1024
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

// ConvertAnthropicRequestToOpenAIBody converts an Anthropic-format request body
// (as a parsed map) to the OpenAI chat completions format. This is the reverse
// of ConvertOpenAIToAnthropicBody and is used when clients send Anthropic-format
// requests to the gateway.
func (a *AnthropicAdapter) ConvertAnthropicRequestToOpenAIBody(anthropic map[string]interface{}) map[string]interface{} {
	return a.convertAnthropicToOpenAIRequest(anthropic)
}

// ConvertOpenAIResponseToAnthropicBody converts an OpenAI-format response
// (as a parsed map) to the Anthropic Messages API format. This is the reverse
// of ConvertAnthropicToOpenAIBody and is used when the gateway needs to send
// Anthropic-format responses to clients that sent Anthropic-format requests.
func (a *AnthropicAdapter) ConvertOpenAIResponseToAnthropicBody(openai map[string]interface{}) map[string]interface{} {
	return a.convertOpenAIToAnthropicResponse(openai)
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

	if len(body) > maxPreFlightBodyBytes {
		return nil, fmt.Errorf("anthropic: request body too large (%d bytes, max %d)", len(body), maxPreFlightBodyBytes)
	}

	var openai map[string]interface{}
	if err := json.Unmarshal(body, &openai); err != nil {
		slog.Debug("anthropic: request body is not valid JSON, passing through", "error", err)
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
		slog.Debug("anthropic: response body is not valid JSON, passing through", "error", err)
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
		if v > math.MaxInt32 {
			v = math.MaxInt32
		}
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
	result := make([]interface{}, 0, len(msgs))
	var lastAssistantToolIDs map[string]bool

	for i := 0; i < len(msgs); i++ {
		msg, ok := msgs[i].(map[string]interface{})
		if !ok {
			continue
		}

		role, _ := msg["role"].(string)

		if role == "system" {
			continue
		}

		if role == "tool" {
			toolBatch := a.collectToolMessages(msgs, &i)
			toolResults := a.buildValidatedToolResults(toolBatch, lastAssistantToolIDs)
			lastAssistantToolIDs = nil
			if len(toolResults) == 0 {
				continue
			}
			anthMsg := map[string]interface{}{
				"role":    "user",
				"content": toolResults,
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

		if role == "assistant" {
			ids := a.extractToolUseIDsFromBlocks(contentBlocks)
			if len(ids) > 0 {
				lastAssistantToolIDs = ids
			} else {
				lastAssistantToolIDs = nil
			}
		} else {
			lastAssistantToolIDs = nil
		}
	}
	return a.sanitizeOrphanedToolUse(result)
}

// sanitizeOrphanedToolUse strips tool_use blocks from assistant messages
// when the immediately following user message lacks matching tool_result blocks.
// Anthropic's API requires every tool_use to have a corresponding tool_result
// in the next message. This handles cases where message pruning or other
// preprocessing has broken the pairing.
func (a *AnthropicAdapter) sanitizeOrphanedToolUse(result []interface{}) []interface{} {
	if len(result) < 2 {
		return result
	}

	for i := 0; i < len(result); i++ {
		msg, ok := result[i].(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "assistant" {
			continue
		}

		content, ok := msg["content"].([]interface{})
		if !ok || len(content) == 0 {
			continue
		}

		toolUseIDs := a.collectToolUseIDs(content)
		if len(toolUseIDs) == 0 {
			continue
		}

		nextResultIDs := a.collectNextToolResultIDs(result, i)
		orphaned := a.findOrphanedIDs(toolUseIDs, nextResultIDs)
		if len(orphaned) == 0 {
			continue
		}

		for _, id := range orphaned {
			slog.Warn("anthropic: stripping orphaned tool_use block (no matching tool_result)", "tool_use_id", id)
		}

		result[i] = a.stripToolUseBlocks(msg, content, orphaned)
	}

	return result
}

// collectToolUseIDs returns the tool_use IDs present in the given content blocks.
func (a *AnthropicAdapter) collectToolUseIDs(blocks []interface{}) []string {
	var ids []string
	for _, block := range blocks {
		bm, ok := block.(map[string]interface{})
		if !ok || bm["type"] != "tool_use" {
			continue
		}
		if id, _ := bm["id"].(string); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// collectNextToolResultIDs returns the set of tool_use_id values in the message
// immediately following result[i]. Returns nil if there is no next message or
// the next message is not a user message with tool_result content.
func (a *AnthropicAdapter) collectNextToolResultIDs(result []interface{}, i int) map[string]bool {
	if i+1 >= len(result) {
		return nil
	}
	nextMsg, ok := result[i+1].(map[string]interface{})
	if !ok {
		return nil
	}
	if role, _ := nextMsg["role"].(string); role != "user" {
		return nil
	}
	nextContent, ok := nextMsg["content"].([]interface{})
	if !ok {
		return nil
	}
	ids := make(map[string]bool, len(nextContent))
	for _, block := range nextContent {
		bm, ok := block.(map[string]interface{})
		if !ok || bm["type"] != "tool_result" {
			continue
		}
		if id, _ := bm["tool_use_id"].(string); id != "" {
			ids[id] = true
		}
	}
	return ids
}

// findOrphanedIDs returns tool_use IDs that are not present in nextResultIDs.
// Returns nil when no IDs are orphaned.
func (a *AnthropicAdapter) findOrphanedIDs(toolUseIDs []string, nextResultIDs map[string]bool) []string {
	if nextResultIDs == nil {
		// No next message at all — all tool_use IDs are orphaned
		result := make([]string, len(toolUseIDs))
		copy(result, toolUseIDs)
		return result
	}
	var orphaned []string
	for _, id := range toolUseIDs {
		if !nextResultIDs[id] {
			orphaned = append(orphaned, id)
		}
	}
	return orphaned
}

// stripToolUseBlocks removes orphaned tool_use content blocks from the message
// and returns a new map. If all blocks are removed, a fallback text block is added.
func (a *AnthropicAdapter) stripToolUseBlocks(msg map[string]interface{}, content []interface{}, orphaned []string) map[string]interface{} {
	orphanedSet := make(map[string]bool, len(orphaned))
	for _, id := range orphaned {
		orphanedSet[id] = true
	}

	newContent := make([]interface{}, 0, len(content))
	for _, block := range content {
		bm, ok := block.(map[string]interface{})
		if !ok {
			newContent = append(newContent, block)
			continue
		}
		if bm["type"] == "tool_use" {
			if id, _ := bm["id"].(string); orphanedSet[id] {
				continue
			}
		}
		newContent = append(newContent, block)
	}

	if len(newContent) == 0 {
		newContent = []interface{}{emptyTextBlock}
	}

	updated := make(map[string]interface{}, len(msg))
	for k, v := range msg {
		updated[k] = v
	}
	updated["content"] = newContent
	return updated
}

// collectToolMessages gathers consecutive tool-role messages starting at index *i.
// It advances *i past all tool messages consumed. If it encounters a non-tool
// message, it decrements *i so the caller's i++ lands on it. Returns the batch
// of raw tool messages for validation.
func (a *AnthropicAdapter) collectToolMessages(msgs []interface{}, i *int) []map[string]interface{} {
	var batch []map[string]interface{}
	for *i < len(msgs) {
		msg, ok := msgs[*i].(map[string]interface{})
		if !ok {
			break
		}
		role, _ := msg["role"].(string)
		if role != "tool" {
			(*i)--
			break
		}
		batch = append(batch, msg)
		(*i)++
	}
	return batch
}

// extractToolUseIDsFromBlocks scans Anthropic content blocks and returns a set
// of all tool_use block IDs. Returns a non-nil map only when tool_use blocks exist.
func (a *AnthropicAdapter) extractToolUseIDsFromBlocks(blocks []interface{}) map[string]bool {
	ids := make(map[string]bool)
	for _, block := range blocks {
		bm, ok := block.(map[string]interface{})
		if !ok {
			continue
		}
		if typ, _ := bm["type"].(string); typ == "tool_use" {
			if id, ok := bm["id"].(string); ok && id != "" {
				ids[id] = true
			}
		}
	}
	return ids
}

// buildValidatedToolResults validates each tool message's tool_call_id against the
// set of known tool_use IDs from the preceding assistant message. Orphaned results
// (nil validIDs, missing tool_call_id, or ID not in validIDs) are dropped with a warning.
func (a *AnthropicAdapter) buildValidatedToolResults(batch []map[string]interface{}, validIDs map[string]bool) []interface{} {
	var results []interface{}
	for _, msg := range batch {
		toolCallID, _ := msg["tool_call_id"].(string)
		if toolCallID == "" {
			slog.Warn("anthropic: dropping tool message without tool_call_id")
			continue
		}
		if validIDs == nil {
			slog.Warn("anthropic: dropping orphaned tool result (no preceding assistant)", "tool_use_id", toolCallID)
			continue
		}
		if !validIDs[toolCallID] {
			slog.Warn("anthropic: dropping orphaned tool result (no matching tool_use)", "tool_use_id", toolCallID)
			continue
		}
		results = append(results, a.buildToolResultBlock(msg["content"], toolCallID))
	}
	return results
}

// buildToolResultBlock constructs a single Anthropic tool_result content block
// from a tool message's content and the validated tool_use_id.
func (a *AnthropicAdapter) buildToolResultBlock(content interface{}, toolUseID string) map[string]interface{} {
	toolResult := map[string]interface{}{
		"type":        "tool_result",
		"tool_use_id": toolUseID,
	}

	if content == nil {
		toolResult["content"] = ""
	} else {
		switch c := content.(type) {
		case string:
			toolResult["content"] = c
		case []interface{}:
			anthropicBlocks := a.convertToolContentBlocks(c)
			if len(anthropicBlocks) > 0 {
				toolResult["content"] = anthropicBlocks
			} else {
				toolResult["content"] = ""
			}
		default:
			slog.Debug("anthropic: tool message content has unexpected type, treating as string", "type", fmt.Sprintf("%T", content))
			toolResult["content"] = fmt.Sprintf("%v", content)
		}
	}

	return toolResult
}

// emptyTextBlock is a minimal Anthropic text content block used as a fallback
// when a message has no text or tool_use content after sanitization. Using a minimal
// placeholder satisfies Anthropic's API requirement that assistant messages have
// non-empty content. The "..." placeholder indicates content was removed.
var emptyTextBlock = map[string]interface{}{"type": "text", "text": "..."}

// buildContentBlocks constructs an Anthropic content array from an OpenAI-format
// message. It converts text content into typed text blocks and OpenAI tool_calls
// into Anthropic tool_use blocks. Returns a non-empty slice guaranteed to have at
// least one content block to satisfy Anthropic's API requirement.
func (a *AnthropicAdapter) buildContentBlocks(msg map[string]interface{}) []interface{} {
	blocks := make([]interface{}, 0, 2)

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

func (a *AnthropicAdapter) convertToolContentBlocks(blocks []interface{}) []interface{} {
	var result []interface{}
	for _, blockRaw := range blocks {
		block, ok := blockRaw.(map[string]interface{})
		if !ok {
			continue
		}

		blockType, _ := block["type"].(string)
		switch blockType {
		case "text":
			if text, ok := block["text"].(string); ok {
				result = append(result, map[string]interface{}{
					"type": "text",
					"text": text,
				})
			}
		case "image_url":
			if imageURL, ok := block["image_url"].(map[string]interface{}); ok {
				if url, ok := imageURL["url"].(string); ok {
					parts := strings.SplitN(url, ",", 2)
					if len(parts) == 2 && strings.HasPrefix(parts[0], "data:") {
						mediaType := strings.TrimPrefix(parts[0], "data:")
						mediaType = strings.TrimSuffix(mediaType, ";base64")
						result = append(result, map[string]interface{}{
							"type": "image",
							"source": map[string]interface{}{
								"type":       "base64",
								"media_type": mediaType,
								"data":       parts[1],
							},
						})
					}
				}
			}
		case "image":
			if source, ok := block["source"].(map[string]interface{}); ok {
				result = append(result, map[string]interface{}{
					"type":   "image",
					"source": source,
				})
			}
		case "tool_result":
			slog.Debug("anthropic: tool_result block inside tool message content, forwarding as-is")
			result = append(result, block)
		}
	}
	return result
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
		"id":      "anthropic-" + util.GenerateID(),
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
		default:
			slog.Debug("Unknown Anthropic content block type", "type", bType)
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
		slog.Debug("Unknown Anthropic stop_reason, defaulting to 'stop'", "reason", stopReason)
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
		"total_tokens":      util.AddFloat64(usage["input_tokens"], usage["output_tokens"]),
	}
}

func (a *AnthropicAdapter) convertAnthropicToOpenAIRequest(anthropic map[string]interface{}) map[string]interface{} {
	openai := map[string]interface{}{}

	if model, ok := anthropic["model"].(string); ok {
		openai["model"] = model
	}

	if msgs, ok := anthropic["messages"].([]interface{}); ok {
		openai["messages"] = a.reverseConvertMessages(anthropic, msgs)
	}

	if tools, ok := anthropic["tools"].([]interface{}); ok && len(tools) > 0 {
		openai["tools"] = a.reverseConvertTools(tools)
	}

	if tc, ok := anthropic["tool_choice"]; ok {
		a.reverseConvertToolChoice(tc, openai)
	}

	a.copyAnthropicFields(anthropic, openai)

	return openai
}

func (a *AnthropicAdapter) copyAnthropicFields(anthropic, openai map[string]interface{}) {
	if v, ok := anthropic["max_tokens"]; ok {
		openai["max_tokens"] = v
	}
	if v, ok := anthropic["temperature"]; ok {
		openai["temperature"] = v
	}
	if v, ok := anthropic["top_p"]; ok {
		openai["top_p"] = v
	}
	if v, ok := anthropic["top_k"]; ok {
		openai["top_k"] = v
	}
	if v, ok := anthropic["stop_sequences"]; ok {
		openai["stop"] = v
	}
	if v, ok := anthropic["stream"].(bool); ok {
		openai["stream"] = v
	}
	if meta, ok := anthropic["metadata"].(map[string]interface{}); ok {
		if uid, ok := meta["user_id"].(string); ok {
			openai["user"] = uid
		}
	}
}

func (a *AnthropicAdapter) reverseConvertMessages(anthropic map[string]interface{}, msgs []interface{}) []interface{} {
	var result []interface{}

	if systemContent := anthropic["system"]; systemContent != nil {
		sysMsg := a.buildOpenAISystemMessage(systemContent)
		if sysMsg != nil {
			result = append(result, sysMsg)
		}
	}

	for i := 0; i < len(msgs); i++ {
		msg, ok := msgs[i].(map[string]interface{})
		if !ok {
			continue
		}

		role, _ := msg["role"].(string)

		if role == "user" {
			result = a.processUserMessage(result, msgs, msg, &i)
			continue
		}

		if role == "assistant" {
			result = append(result, a.buildOpenAIAssistantMessage(msg))
			continue
		}

		oMsg := map[string]interface{}{"role": role}
		if content, ok := msg["content"]; ok {
			oMsg["content"] = a.contentBlocksToString(content)
		}
		result = append(result, oMsg)
	}

	return result
}

func (a *AnthropicAdapter) processUserMessage(result []interface{}, msgs []interface{}, msg map[string]interface{}, i *int) []interface{} {
	toolMsgs := a.extractToolResultMessages(msgs, i)
	if len(toolMsgs) > 0 {
		result = append(result, toolMsgs...)
		result = a.appendRemainingUserMessage(result, msgs, i)
		return result
	}
	return append(result, a.buildOpenAIUserMessage(msg))
}

func (a *AnthropicAdapter) appendRemainingUserMessage(result []interface{}, msgs []interface{}, i *int) []interface{} {
	if *i >= len(msgs) {
		(*i)--
		return result
	}
	msg, ok := msgs[*i].(map[string]interface{})
	if !ok {
		(*i)--
		return result
	}
	role, _ := msg["role"].(string)
	if role == "user" {
		return append(result, a.buildOpenAIUserMessage(msg))
	}
	(*i)--
	return result
}

func (a *AnthropicAdapter) buildOpenAISystemMessage(systemContent interface{}) map[string]interface{} {
	switch sc := systemContent.(type) {
	case string:
		if sc != "" {
			return map[string]interface{}{
				"role":    "system",
				"content": sc,
			}
		}
	case []interface{}:
		var parts []string
		for _, blockRaw := range sc {
			if block, ok := blockRaw.(map[string]interface{}); ok {
				if t, ok := block["text"].(string); ok && t != "" {
					parts = append(parts, t)
				}
			}
		}
		if len(parts) > 0 {
			return map[string]interface{}{
				"role":    "system",
				"content": strings.Join(parts, "\n\n"),
			}
		}
	}
	return nil
}

// extractToolResultMessages checks if a user message contains only tool_result
// blocks. If so, it converts them to OpenAI tool-role messages and peeks ahead
// for more consecutive user messages that are purely tool_results. It advances
// *i past consumed messages and decrements if a non-tool-result user message
// is encountered so the caller can process it.
func (a *AnthropicAdapter) extractToolResultMessages(msgs []interface{}, i *int) []interface{} {
	msg, ok := msgs[*i].(map[string]interface{})
	if !ok {
		return nil
	}

	content := msg["content"]
	if !a.isPureToolResults(content) {
		return nil
	}

	var result []interface{}
	for *i < len(msgs) {
		msg, ok := msgs[*i].(map[string]interface{})
		if !ok {
			break
		}
		role, _ := msg["role"].(string)
		if role != "user" {
			(*i)--
			break
		}
		content := msg["content"]
		if !a.isPureToolResults(content) {
			(*i)--
			break
		}
		result = append(result, a.convertToolResultContent(content)...)
		(*i)++
	}
	return result
}

func (a *AnthropicAdapter) isPureToolResults(content interface{}) bool {
	blocks, ok := content.([]interface{})
	if !ok {
		return false
	}
	if len(blocks) == 0 {
		return false
	}
	for _, blockRaw := range blocks {
		block, ok := blockRaw.(map[string]interface{})
		if !ok {
			return false
		}
		if typ, _ := block["type"].(string); typ != "tool_result" {
			return false
		}
	}
	return true
}

func (a *AnthropicAdapter) convertToolResultContent(content interface{}) []interface{} {
	blocks, ok := content.([]interface{})
	if !ok {
		return nil
	}
	var result []interface{}
	for _, blockRaw := range blocks {
		block, ok := blockRaw.(map[string]interface{})
		if !ok {
			continue
		}
		toolUseID, _ := block["tool_use_id"].(string)
		toolContent := a.extractToolResultText(block["content"])
		result = append(result, map[string]interface{}{
			"role":         "tool",
			"tool_call_id": toolUseID,
			"content":      toolContent,
		})
	}
	return result
}

func (a *AnthropicAdapter) extractToolResultText(content interface{}) string {
	if content == nil {
		return ""
	}
	switch c := content.(type) {
	case string:
		return c
	case []interface{}:
		var parts []string
		for _, blockRaw := range c {
			if block, ok := blockRaw.(map[string]interface{}); ok {
				if typ, _ := block["type"].(string); typ == "text" {
					if t, ok := block["text"].(string); ok {
						parts = append(parts, t)
					}
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return fmt.Sprintf("%v", content)
}

func (a *AnthropicAdapter) buildOpenAIUserMessage(msg map[string]interface{}) map[string]interface{} {
	content := msg["content"]
	switch c := content.(type) {
	case string:
		return map[string]interface{}{
			"role":    "user",
			"content": c,
		}
	case []interface{}:
		parts := a.convertContentBlocksToOpenAI(c)
		if text := a.extractSingleText(parts); text != nil {
			return map[string]interface{}{
				"role":    "user",
				"content": *text,
			}
		}
		return map[string]interface{}{
			"role":    "user",
			"content": parts,
		}
	default:
		return map[string]interface{}{
			"role":    "user",
			"content": fmt.Sprintf("%v", content),
		}
	}
}

func (a *AnthropicAdapter) extractSingleText(parts []interface{}) *string {
	if len(parts) != 1 {
		return nil
	}
	text, ok := parts[0].(map[string]interface{})
	if !ok {
		return nil
	}
	if typ, _ := text["type"].(string); typ != "text" {
		return nil
	}
	t, ok := text["text"].(string)
	if !ok {
		return nil
	}
	return &t
}

func (a *AnthropicAdapter) convertContentBlocksToOpenAI(blocks []interface{}) []interface{} {
	var result []interface{}
	for _, blockRaw := range blocks {
		block, ok := blockRaw.(map[string]interface{})
		if !ok {
			continue
		}
		bType, _ := block["type"].(string)
		switch bType {
		case "text":
			if text, ok := block["text"].(string); ok {
				result = append(result, map[string]interface{}{
					"type": "text",
					"text": text,
				})
			}
		case "image":
			if source, ok := block["source"].(map[string]interface{}); ok {
				result = append(result, a.convertImageSourceToOpenAI(source))
			}
		case "tool_use", "tool_result":
			continue
		}
	}
	return result
}

func (a *AnthropicAdapter) convertImageSourceToOpenAI(source map[string]interface{}) map[string]interface{} {
	if srcType, _ := source["type"].(string); srcType == "base64" {
		mediaType, _ := source["media_type"].(string)
		data, _ := source["data"].(string)
		if mediaType != "" && data != "" {
			return map[string]interface{}{
				"type": "image_url",
				"image_url": map[string]interface{}{
					"url": fmt.Sprintf("data:%s;base64,%s", mediaType, data),
				},
			}
		}
	}
	return map[string]interface{}{
		"type":   "image",
		"source": source,
	}
}

func (a *AnthropicAdapter) buildOpenAIAssistantMessage(msg map[string]interface{}) map[string]interface{} {
	textParts, toolCalls := a.extractAssistantContent(msg["content"])

	oMsg := map[string]interface{}{
		"role": "assistant",
	}
	if len(textParts) > 0 {
		oMsg["content"] = strings.Join(textParts, "")
	} else if len(toolCalls) > 0 {
		oMsg["content"] = ""
	}

	if len(toolCalls) > 0 {
		oMsg["tool_calls"] = toolCalls
	}

	return oMsg
}

func (a *AnthropicAdapter) extractAssistantContent(content interface{}) ([]string, []interface{}) {
	var textParts []string
	var toolCalls []interface{}

	blocks, ok := content.([]interface{})
	if !ok {
		if text, ok := content.(string); ok {
			return []string{text}, nil
		}
		return nil, nil
	}

	for _, blockRaw := range blocks {
		block, ok := blockRaw.(map[string]interface{})
		if !ok {
			continue
		}
		bType, _ := block["type"].(string)
		switch bType {
		case "text":
			if t, ok := block["text"].(string); ok && t != "" {
				textParts = append(textParts, t)
			}
		case "tool_use":
			tc := a.reverseConvertToolUseBlock(block)
			if tc != nil {
				toolCalls = append(toolCalls, tc)
			}
		case "thinking", "redacted_thinking":
			continue
		}
	}
	return textParts, toolCalls
}

func (a *AnthropicAdapter) reverseConvertToolUseBlock(block map[string]interface{}) map[string]interface{} {
	id, _ := block["id"].(string)
	name, _ := block["name"].(string)

	var argsStr string
	if input, ok := block["input"]; ok {
		argsBytes, err := json.Marshal(input)
		if err == nil {
			argsStr = string(argsBytes)
		}
	}
	if argsStr == "" {
		argsStr = "{}"
	}

	return map[string]interface{}{
		"id":   id,
		"type": "function",
		"function": map[string]interface{}{
			"name":      name,
			"arguments": argsStr,
		},
	}
}

func (a *AnthropicAdapter) reverseConvertTools(tools []interface{}) []interface{} {
	var result []interface{}
	for _, toolRaw := range tools {
		tool, ok := toolRaw.(map[string]interface{})
		if !ok {
			continue
		}

		name, _ := tool["name"].(string)
		desc, _ := tool["description"].(string)

		var params interface{}
		if schema, ok := tool["input_schema"]; ok {
			params = schema
		} else {
			params = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
		}

		result = append(result, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        name,
				"description": desc,
				"parameters":  params,
			},
		})
	}
	return result
}

func (a *AnthropicAdapter) reverseConvertToolChoice(tc interface{}, openai map[string]interface{}) {
	if m, ok := tc.(map[string]interface{}); ok {
		tcType, _ := m["type"].(string)
		switch tcType {
		case "auto", "required":
			openai["tool_choice"] = tcType
		case "tool":
			if name, ok := m["name"].(string); ok {
				openai["tool_choice"] = map[string]interface{}{
					"type": "function",
					"function": map[string]interface{}{
						"name": name,
					},
				}
			}
		}
		return
	}
	if s, ok := tc.(string); ok {
		openai["tool_choice"] = s
	}
}

func (a *AnthropicAdapter) contentBlocksToString(content interface{}) interface{} {
	switch c := content.(type) {
	case string:
		return c
	case []interface{}:
		var parts []string
		for _, blockRaw := range c {
			if block, ok := blockRaw.(map[string]interface{}); ok {
				if typ, _ := block["type"].(string); typ == "text" {
					if t, ok := block["text"].(string); ok {
						parts = append(parts, t)
					}
				}
			}
		}
		if len(parts) == 1 {
			return parts[0]
		}
		if len(parts) > 1 {
			return strings.Join(parts, "")
		}
	}
	return ""
}

func (a *AnthropicAdapter) convertOpenAIToAnthropicResponse(openai map[string]interface{}) map[string]interface{} {
	anthropic := map[string]interface{}{
		"id":   a.extractResponseID(openai),
		"type": "message",
		"role": "assistant",
	}

	if model, ok := openai["model"]; ok {
		anthropic["model"] = model
	}

	var contentBlocks []interface{}
	var stopReason string

	choices, ok := openai["choices"].([]interface{})
	if ok && len(choices) > 0 {
		choice, ok := choices[0].(map[string]interface{})
		if ok {
			contentBlocks, stopReason = a.extractOpenAIChoiceContent(choice)
		}
	}

	if len(contentBlocks) == 0 {
		contentBlocks = []interface{}{map[string]interface{}{"type": "text", "text": ""}}
	}
	anthropic["content"] = contentBlocks

	if stopReason != "" {
		anthropic["stop_reason"] = stopReason
	}

	if usage, ok := openai["usage"].(map[string]interface{}); ok {
		anthropic["usage"] = map[string]interface{}{
			"input_tokens":  usage["prompt_tokens"],
			"output_tokens": usage["completion_tokens"],
		}
	}

	return anthropic
}

func (a *AnthropicAdapter) extractResponseID(openai map[string]interface{}) string {
	if id, ok := openai["id"].(string); ok && id != "" {
		return id
	}
	return "msg_" + util.GenerateID()
}

func (a *AnthropicAdapter) extractOpenAIChoiceContent(choice map[string]interface{}) ([]interface{}, string) {
	var contentBlocks []interface{}
	var stopReason string

	message := choice
	if msg, ok := choice["message"].(map[string]interface{}); ok {
		message = msg
	}

	if content, ok := message["content"].(string); ok && content != "" {
		contentBlocks = append(contentBlocks, map[string]interface{}{
			"type": "text",
			"text": content,
		})
	}

	if toolCalls, ok := message["tool_calls"].([]interface{}); ok {
		for _, tcRaw := range toolCalls {
			tc, ok := tcRaw.(map[string]interface{})
			if !ok {
				continue
			}
			block := a.convertToolCallToAnthropic(tc)
			if block != nil {
				contentBlocks = append(contentBlocks, block)
			}
		}
	}

	if fr, ok := choice["finish_reason"].(string); ok {
		stopReason = a.mapFinishReason(fr)
	}

	return contentBlocks, stopReason
}

func (a *AnthropicAdapter) convertToolCallToAnthropic(tc map[string]interface{}) map[string]interface{} {
	id, _ := tc["id"].(string)
	fn, ok := tc["function"].(map[string]interface{})
	if !ok {
		return nil
	}
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

func (a *AnthropicAdapter) mapFinishReason(reason string) string {
	switch reason {
	case "stop":
		return "end_turn"
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	default:
		return reason
	}
}
