package adapter

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nenya/internal/jsonschema"
	"github.com/nenya/internal/stream"
	"github.com/nenya/internal/util"
)

func (a *AnthropicAdapter) convertAnthropicToOpenAI(anthropic map[string]interface{}, stream bool, report *DropReport) map[string]interface{} {
	openai := map[string]interface{}{
		"id":      "anthropic-" + util.GenerateID(),
		"object":  "chat.completion",
		"created": 0,
		"model":   anthropic["model"],
	}

	choice := map[string]interface{}{
		"index": 0,
	}

	if stream {
		delta := map[string]interface{}{}
		a.processAnthropicContent(anthropic, delta, choice, report)
		a.processStopReason(anthropic, choice)
		choice["delta"] = delta
		openai["object"] = "chat.completion.chunk"
	} else {
		message := map[string]interface{}{
			"role": "assistant",
		}
		a.processAnthropicContent(anthropic, message, choice, report)
		a.processStopReason(anthropic, choice)
		choice["message"] = message
	}

	openai["choices"] = []interface{}{choice}
	a.processUsage(anthropic, openai)

	return openai
}

// reportThinkingBlock classifies one thinking-family block in a reverse
// translation (NENYA-176): its reasoning text is the surfaced
// representation, the opaque signature blob and redacted thinking are
// recorded by kind on the report (never dropped silently).
func reportThinkingBlock(block map[string]interface{}, report *DropReport) (string, bool) {
	reason, _ := block["thinking"].(string)
	if sig, _ := block["signature"].(string); sig != "" {
		report.Add("thinking_signature")
	}
	return reason, reason != ""
}

// untypedBlockKind names a block whose type is missing, non-string, or
// empty, so it is still reported instead of vanishing silently.
func untypedBlockKind(block map[string]interface{}) string {
	bType, _ := block["type"].(string)
	if bType == "" {
		return "unknown"
	}
	return bType
}

// extractContentBlocks extracts text parts, tool calls, and reasoning text
// from Anthropic content blocks. Content the OpenAI shape cannot represent is
// recorded on the report (nil-safe) rather than dropped silently (NENYA-176):
// text-bearing reasoning blocks surface as reasoning_content, redacted
// thinking and unknown block types are counted by kind.
func (a *AnthropicAdapter) extractContentBlocks(blocks []interface{}, report *DropReport) ([]string, []interface{}, []string) {
	var textParts []string
	var toolCalls []interface{}
	var reasonParts []string
	for _, block := range blocks {
		bm, ok := block.(map[string]interface{})
		if !ok {
			report.Add("unknown")
			continue
		}
		bType, _ := bm["type"].(string)
		switch bType {
		case "text":
			if t, ok := bm["text"].(string); ok {
				textParts = append(textParts, t)
			} else {
				// A text block without a text value is a malformed
				// fidelity loss: report, never drop silently.
				report.Add("text")
			}
		case "tool_use":
			toolCalls = append(toolCalls, a.convertToolUseBlock(bm))
		case "tool_result":
			if t, ok := bm["content"].(string); ok {
				textParts = append(textParts, t)
			} else {
				// Array-valued tool results have no flat OpenAI
				// representation here: report the loss.
				report.Add("tool_result")
			}
		case "thinking":
			if reason, ok := reportThinkingBlock(bm, report); ok {
				reasonParts = append(reasonParts, reason)
			} else if _, has := bm["signature"]; !has {
				// Empty thinking text with no signature: nothing was
				// surfaced at all — report rather than vanish.
				report.Add("thinking")
			}
		case "redacted_thinking":
			report.Add("redacted_thinking")
		default:
			report.Add(untypedBlockKind(bm))
			slog.Debug("Unknown Anthropic content block type", "type", bType)
		}
	}
	return textParts, toolCalls, reasonParts
}

func (a *AnthropicAdapter) processAnthropicContent(anthropic, delta, choice map[string]interface{}, report *DropReport) {
	content, ok := anthropic["content"]
	if !ok {
		return
	}

	switch c := content.(type) {
	case string:
		delta["content"] = c
		choice["finish_reason"] = "stop"
	case []interface{}:
		textParts, toolCalls, reasonParts := a.extractContentBlocks(c, report)
		if len(textParts) > 0 {
			delta["content"] = strings.Join(textParts, "")
		}
		if len(reasonParts) > 0 {
			delta["reasoning_content"] = strings.Join(reasonParts, "\n\n")
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
	case "refusal":
		choice["finish_reason"] = "content_filter"
	case "pause_turn":
		// Resumable terminal (NENYA-174): OpenAI semantics have no resume
		// verb, so a buffered OpenAI-facing client sees the turn end.
		choice["finish_reason"] = "stop"
	default:
		slog.Debug("Unknown Anthropic stop_reason, defaulting to 'stop'", "reason", stopReason)
		choice["finish_reason"] = "stop"
	}
}

// processUsage converts an Anthropic usage object into the OpenAI shape
// (NENYA-167). Anthropic reports input_tokens EXCLUDING the cache dimensions,
// while the OpenAI shape's prompt_tokens INCLUDES them with the cache counters
// as subsets; the conversion therefore folds the cache dimensions into the
// prompt total and re-publishes them as subsets under both the OpenAI-style
// keys and the native Anthropic spellings (matching the streaming
// transformer), so a client sees each dimension exactly once instead of
// losing the cache counts, whichever key family it reads.
func (a *AnthropicAdapter) processUsage(anthropic, openai map[string]interface{}) {
	usage, ok := anthropic["usage"].(map[string]interface{})
	if !ok {
		return
	}

	totals := stream.NormalizeUsageTotals(usage)
	// float64 values keep the converted map type-identical to a JSON-decoded
	// upstream usage object (NENYA-167).
	converted := map[string]interface{}{
		"prompt_tokens":     float64(totals.Prompt),
		"completion_tokens": float64(totals.Completion),
		"total_tokens":      float64(totals.Total),
	}
	if totals.CacheHit > 0 {
		converted["prompt_cache_hit_tokens"] = float64(totals.CacheHit)
		// Native Anthropic spelling, mirroring the streaming transformer
		// (internal/stream/anthropic_transformer.go) so buffered and
		// streaming clients see the same dimensions under the same keys.
		// Only the canonical totals are republished; re-emitting whatever
		// spelling the upstream used would leak provider dialects.
		converted["cache_read_input_tokens"] = float64(totals.CacheHit)
	}
	if totals.CacheMiss > 0 {
		converted["prompt_cache_miss_tokens"] = float64(totals.CacheMiss)
	}
	if totals.CacheCreation > 0 {
		converted["cache_creation_tokens"] = float64(totals.CacheCreation)
		converted["cache_creation_input_tokens"] = float64(totals.CacheCreation)
	}
	// The per-TTL breakdown mirrors the streaming transformer
	// (internal/stream/anthropic_transformer.go) so buffered and streaming
	// clients see the same object (NENYA-310); omitted when unreported.
	if totals.CacheCreation5m > 0 || totals.CacheCreation1h > 0 {
		converted["cache_creation"] = map[string]interface{}{
			"ephemeral_5m_input_tokens": float64(totals.CacheCreation5m),
			"ephemeral_1h_input_tokens": float64(totals.CacheCreation1h),
		}
	}
	openai["usage"] = converted
}

func (a *AnthropicAdapter) convertAnthropicToOpenAIRequest(anthropic map[string]interface{}, report *DropReport) map[string]interface{} {
	openai := map[string]interface{}{}

	if model, ok := anthropic["model"].(string); ok {
		openai["model"] = model
	}

	if msgs, ok := anthropic["messages"].([]interface{}); ok {
		openai["messages"] = a.reverseConvertMessages(anthropic, msgs, report)
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

func (a *AnthropicAdapter) reverseConvertMessages(anthropic map[string]interface{}, msgs []interface{}, report *DropReport) []interface{} {
	var result []interface{}

	if systemContent := anthropic["system"]; systemContent != nil {
		sysMsg := a.buildOpenAISystemMessage(systemContent, report)
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
			result = a.processUserMessage(result, msgs, msg, &i, report)
			continue
		}

		if role == "assistant" {
			result = append(result, a.buildOpenAIAssistantMessage(msg, report))
			continue
		}

		oMsg := map[string]interface{}{"role": role}
		if content, ok := msg["content"]; ok {
			if role == "system" {
				// System reminders move through the OpenAI pipeline and
				// back on Anthropic-format egress; preserve blocks
				// verbatim so cache_control breakpoints are not lost.
				oMsg["content"] = preserveSystemContent(content)
			} else {
				oMsg["content"] = a.contentBlocksToString(content)
				// NENYA-207: the flattening dropped any block-level mark —
				// hoist its presence onto the message (see hoistedCacheControl).
				hoistOntoMessage(oMsg, content)
			}
		}
		result = append(result, oMsg)
	}

	return result
}

func (a *AnthropicAdapter) processUserMessage(result []interface{}, msgs []interface{}, msg map[string]interface{}, i *int, report *DropReport) []interface{} {
	toolMsgs := a.extractToolResultMessages(msgs, i, report)
	if len(toolMsgs) > 0 {
		result = append(result, toolMsgs...)
		result = a.appendRemainingUserMessage(result, msgs, i, report)
		return result
	}
	return append(result, a.buildOpenAIUserMessage(msg, report))
}

func (a *AnthropicAdapter) appendRemainingUserMessage(result []interface{}, msgs []interface{}, i *int, report *DropReport) []interface{} {
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
		return append(result, a.buildOpenAIUserMessage(msg, report))
	}
	(*i)--
	return result
}

// buildOpenAISystemMessage flattens Anthropic system content to an OpenAI
// system message. Blocks without a text value carry nothing into the target
// shape: when the report is set, each such block (or a wholly
// unrepresentable system turn) is recorded rather than dropped silently
// (NENYA-176). Returns nil when there is nothing to send.
func (a *AnthropicAdapter) buildOpenAISystemMessage(systemContent interface{}, report *DropReport) map[string]interface{} {
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
			block, ok := blockRaw.(map[string]interface{})
			if !ok {
				report.Add("unknown")
				continue
			}
			bType, _ := block["type"].(string)
			if t, ok := block["text"].(string); ok && t != "" {
				parts = append(parts, t)
				continue
			}
			if bType == "text" {
				// A text block carrying no text value.
				report.Add("text")
				continue
			}
			// A non-text system block (a document attachment, a
			// foreign blob) has no OpenAI system representation.
			report.Add(untypedBlockKind(block))
		}
		if len(parts) > 0 {
			oMsg := map[string]interface{}{
				"role":    "system",
				"content": strings.Join(parts, "\n\n"),
			}
			// NENYA-207: the flattened system string dropped any block-level
			// mark — hoist its presence onto the message.
			hoistOntoMessage(oMsg, sc)
			return oMsg
		}
	default:
		// Neither string nor array: the whole system content has no
		// representation.
		report.Add("system")
	}
	return nil
}

// extractToolResultMessages checks if a user message contains only tool_result
// blocks. If so, it converts them to OpenAI tool-role messages and peeks ahead
// for more consecutive user messages that are purely tool_results. It advances
// *i past consumed messages and decrements if a non-tool-result user message
// is encountered so the caller can process it.
func (a *AnthropicAdapter) extractToolResultMessages(msgs []interface{}, i *int, report *DropReport) []interface{} {
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
		result = append(result, a.convertToolResultContent(content, report)...)
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

func (a *AnthropicAdapter) convertToolResultContent(content interface{}, report *DropReport) []interface{} {
	blocks, ok := content.([]interface{})
	if !ok {
		return nil
	}
	var result []interface{}
	for _, blockRaw := range blocks {
		block, ok := blockRaw.(map[string]interface{})
		if !ok {
			report.Add("unknown")
			continue
		}
		a.reportToolResultSubblocks(block["content"], report)
		toolUseID, _ := block["tool_use_id"].(string)
		toolContent := a.extractToolResultText(block["content"])
		oMsg := map[string]interface{}{
			"role":         "tool",
			"tool_call_id": toolUseID,
			"content":      toolContent,
		}
		// NENYA-207: the flattening destroyed a client mark on the
		// tool_result block — hoist its presence onto the converted tool
		// message so the stand-down scan sees the placement intent.
		if mark, ok := block["cache_control"]; ok {
			oMsg["cache_control"] = mark
		}
		result = append(result, oMsg)
	}
	return result
}

// reportToolResultSubblocks records the sub-block fidelity of an
// array-valued tool_result: only its text reaches the target shape, so
// non-text sub-blocks (images, documents, foreign blobs) are reported by
// kind rather than dropped silently (NENYA-176).
func (a *AnthropicAdapter) reportToolResultSubblocks(content interface{}, report *DropReport) {
	blocks, ok := content.([]interface{})
	if !ok {
		return
	}
	for _, blockRaw := range blocks {
		block, ok := blockRaw.(map[string]interface{})
		if !ok {
			report.Add("unknown")
			continue
		}
		bType, _ := block["type"].(string)
		switch bType {
		case "text":
			if _, isStr := block["text"].(string); !isStr {
				report.Add("text")
			}
		default:
			report.Add(untypedBlockKind(block))
		}
	}
}

// extractToolResultText reduces tool_result content (a string or an array of
// blocks) to plain text: string content passes through verbatim, array
// content keeps the text blocks, and anything else is stringified.
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

func (a *AnthropicAdapter) buildOpenAIUserMessage(msg map[string]interface{}, report *DropReport) map[string]interface{} {
	content := msg["content"]
	switch c := content.(type) {
	case string:
		oMsg := map[string]interface{}{
			"role":    "user",
			"content": c,
		}
		if m, ok := msg["cache_control"]; ok {
			oMsg["cache_control"] = m
		}
		return oMsg
	case []interface{}:
		parts := a.convertContentBlocksToOpenAI(c, report)
		var oMsg map[string]interface{}
		if text := a.extractSingleText(parts); text != nil {
			// NENYA-207: the flattened string dropped any block-level mark.
			oMsg = map[string]interface{}{
				"role":    "user",
				"content": *text,
			}
		} else {
			// The parts were rebuilt without cache_control keys
			// (convertContentBlocksToOpenAI) — same hoist.
			oMsg = map[string]interface{}{
				"role":    "user",
				"content": parts,
			}
		}
		// A message-level mark on the input rides along and wins over a
		// hoisted block mark.
		if m, ok := msg["cache_control"]; ok {
			oMsg["cache_control"] = m
		}
		hoistOntoMessage(oMsg, content)
		return oMsg
	default:
		oMsg := map[string]interface{}{
			"role":    "user",
			"content": fmt.Sprintf("%v", content),
		}
		// A message-level mark on the input rides along (parity with the
		// string and array paths, NENYA-207).
		if m, ok := msg["cache_control"]; ok {
			oMsg["cache_control"] = m
		}
		return oMsg
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

// convertContentBlocksToOpenAI maps Anthropic content blocks to OpenAI parts.
// Foreign block types (documents, attachments, anything the OpenAI shape
// cannot carry) are recorded on the report — never dropped silently
// (NENYA-176).
func (a *AnthropicAdapter) convertContentBlocksToOpenAI(blocks []interface{}, report *DropReport) []interface{} {
	var result []interface{}
	for _, blockRaw := range blocks {
		block, ok := blockRaw.(map[string]interface{})
		if !ok {
			report.Add("unknown")
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
			} else {
				report.Add("text")
			}
		case "image":
			if source, ok := block["source"].(map[string]interface{}); ok {
				result = append(result, a.convertImageSourceToOpenAI(source))
			} else {
				// An image block without a source map has no
				// representation: report the loss.
				report.Add("image")
			}
		case "tool_result":
			// Mixed content: extractToolResultMessages only drains PURE
			// tool-result turns, so a tool_result inside a text-bearing
			// user turn lands here — keep its text instead of dropping it
			// (NENYA-176).
			a.reportToolResultSubblocks(block["content"], report)
			if text := a.extractToolResultText(block["content"]); text != "" {
				result = append(result, map[string]interface{}{
					"type": "text",
					"text": text,
				})
			} else {
				report.Add("tool_result")
			}
		case "tool_use":
			// A tool_use block cannot appear in a client-provided user
			// turn; report rather than drop silently.
			report.Add("tool_use")
		default:
			report.Add(untypedBlockKind(block))
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

func (a *AnthropicAdapter) buildOpenAIAssistantMessage(msg map[string]interface{}, report *DropReport) map[string]interface{} {
	textParts, toolCalls, reasonParts := a.extractAssistantContent(msg["content"], report)

	oMsg := map[string]interface{}{
		"role": "assistant",
	}
	// A message-level mark on the input rides along; a block-level one is
	// hoisted when the flattening drops it (NENYA-207).
	if m, ok := msg["cache_control"]; ok {
		oMsg["cache_control"] = m
	}
	hoistOntoMessage(oMsg, msg["content"])
	if len(textParts) > 0 {
		oMsg["content"] = strings.Join(textParts, "")
	} else if len(toolCalls) > 0 {
		oMsg["content"] = ""
	}
	if len(reasonParts) > 0 {
		oMsg["reasoning_content"] = strings.Join(reasonParts, "\n\n")
	}

	if len(toolCalls) > 0 {
		oMsg["tool_calls"] = toolCalls
	}

	return oMsg
}

// extractAssistantContent maps an Anthropic assistant content array to OpenAI
// text parts and tool calls. Thinking blocks surface as OpenAI reasoning
// content (their signatures are opaque to the OpenAI shape and recorded on
// the report); redacted thinking and unknown block types are reported rather
// than dropped silently (NENYA-176).
func (a *AnthropicAdapter) extractAssistantContent(content interface{}, report *DropReport) ([]string, []interface{}, []string) {
	var textParts []string
	var toolCalls []interface{}
	var reasonParts []string

	blocks, ok := content.([]interface{})
	if !ok {
		if text, ok := content.(string); ok {
			return []string{text}, nil, nil
		}
		// Neither array nor string: the whole content has no
		// representation — report rather than vanish (NENYA-176).
		report.Add("unknown")
		return nil, nil, nil
	}

	for _, blockRaw := range blocks {
		block, ok := blockRaw.(map[string]interface{})
		if !ok {
			report.Add("unknown")
			continue
		}
		bType, _ := block["type"].(string)
		switch bType {
		case "text":
			if t, ok := block["text"].(string); ok {
				if t != "" {
					textParts = append(textParts, t)
				}
			} else {
				// Non-string text value: a malformed fidelity loss.
				report.Add("text")
			}
		case "tool_use":
			tc := a.reverseConvertToolUseBlock(block)
			if tc != nil {
				toolCalls = append(toolCalls, tc)
			}
		case "thinking":
			if reason, ok := reportThinkingBlock(block, report); ok {
				reasonParts = append(reasonParts, reason)
			} else if _, has := block["signature"]; !has {
				report.Add("thinking")
			}
		case "redacted_thinking":
			report.Add("redacted_thinking")
		default:
			report.Add(untypedBlockKind(block))
		}
	}
	return textParts, toolCalls, reasonParts
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

		result = append(result, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        name,
				"description": desc,
				// Sanitized both ways (NENYA-172).
				"parameters": jsonschema.Sanitize(tool["input_schema"]),
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

// preserveSystemContent keeps system-message content intact when it is
// already structured: Anthropic-native clients attach cache_control
// breakpoints to mid-conversation system reminders, and flattening them
// to a string would drop the breakpoint and freeze provider prompt
// caching at the static prefix. Strings pass through untouched; any
// other shape falls back to the lossy text extraction.
func preserveSystemContent(content interface{}) interface{} {
	switch content.(type) {
	case string, []interface{}:
		return content
	default:
		return ""
	}
}

// hoistedCacheControl preserves a client cache_control breakpoint's PRESENCE
// when the reverse conversion flattens Anthropic content blocks (NENYA-207):
// the OpenAI shape carries a string where Anthropic carried typed blocks, so
// a block-level mark cannot survive positionally. Hoisting the first marked
// block's cache_control onto the message keeps the auto-anchor stand-down
// scan (hasClientCacheControl walks the whole payload, message level
// included) honest — without it the gateway would anchor its own placement
// over the client's destroyed placement and record "injected". nil when no
// block is marked.
func hoistedCacheControl(content interface{}) interface{} {
	blocks, ok := content.([]interface{})
	if !ok {
		return nil
	}
	for _, blockRaw := range blocks {
		block, ok := blockRaw.(map[string]interface{})
		if !ok {
			continue
		}
		if mark, ok := block["cache_control"]; ok {
			return mark
		}
	}
	return nil
}

// hoistOntoMessage attaches a hoisted cache_control mark to an OpenAI-shaped
// message unless it already carries one (a client-placed message-level mark
// wins over a hoisted block mark).
func hoistOntoMessage(oMsg map[string]interface{}, content interface{}) {
	if _, exists := oMsg["cache_control"]; exists {
		return
	}
	if mark := hoistedCacheControl(content); mark != nil {
		oMsg["cache_control"] = mark
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

func (a *AnthropicAdapter) convertOpenAIToAnthropicResponse(openai map[string]interface{}, report *DropReport) map[string]interface{} {
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
			contentBlocks, stopReason = a.extractOpenAIChoiceContent(choice, report)
		}
	}

	if len(contentBlocks) == 0 {
		contentBlocks = []interface{}{map[string]interface{}{"type": "text", "text": ""}}
	}
	anthropic["content"] = contentBlocks

	if stopReason != "" {
		anthropic["stop_reason"] = stopReason
	}

	// Anthropic's input_tokens excludes the cache dimensions, which are
	// reported separately (NENYA-167): the OpenAI prompt total is split so a
	// consumer that sums input + cache_read + cache_creation gets the total
	// exactly once.
	if usage, ok := openai["usage"].(map[string]interface{}); ok {
		totals := stream.NormalizeUsageTotals(usage)
		converted := map[string]interface{}{
			"input_tokens":  float64(max(0, totals.Prompt-totals.CacheHit-totals.CacheCreation)),
			"output_tokens": float64(totals.Completion),
		}
		if totals.CacheHit > 0 {
			converted["cache_read_input_tokens"] = float64(totals.CacheHit)
		}
		if totals.CacheCreation > 0 {
			converted["cache_creation_input_tokens"] = float64(totals.CacheCreation)
		}
		// The per-TTL breakdown survives the reverse conversion too
		// (NENYA-310): an OpenAI-format source that carried it (an
		// Anthropic-relaying aggregator) keeps the split in the Anthropic
		// shape.
		if totals.CacheCreation5m > 0 || totals.CacheCreation1h > 0 {
			converted["cache_creation"] = map[string]interface{}{
				"ephemeral_5m_input_tokens": float64(totals.CacheCreation5m),
				"ephemeral_1h_input_tokens": float64(totals.CacheCreation1h),
			}
		}
		anthropic["usage"] = converted
	}

	return anthropic
}

func (a *AnthropicAdapter) extractResponseID(openai map[string]interface{}) string {
	if id, ok := openai["id"].(string); ok && id != "" {
		return id
	}
	return "msg_" + util.GenerateID()
}

// extractOpenAIChoiceContent extracts the Anthropic content blocks and stop
// reason for one OpenAI-format choice. Reasoning rides as visible text
// (NENYA-176), array content is flattened via appendOpenAIContentArray.
func (a *AnthropicAdapter) extractOpenAIChoiceContent(choice map[string]interface{}, report *DropReport) ([]interface{}, string) {
	var contentBlocks []interface{}
	var stopReason string

	message := choice
	if msg, ok := choice["message"].(map[string]interface{}); ok {
		message = msg
	}

	if reason, _ := message["reasoning_content"].(string); reason != "" {
		// Foreign reasoning cannot ride an unsigned `thinking` block:
		// replaying such a block as history to an Anthropic upstream is a
		// guaranteed 400 (signature required — see ADAPTERS.md's
		// invariant table). The reasoning rides as visible, sendable text
		// instead of being dropped silently (NENYA-176).
		contentBlocks = append(contentBlocks, map[string]interface{}{
			"type": "text",
			"text": "[reasoning] " + reason,
		})
	}

	switch c := message["content"].(type) {
	case string:
		if c != "" {
			contentBlocks = append(contentBlocks, map[string]interface{}{
				"type": "text",
				"text": c,
			})
		}
	case []interface{}:
		contentBlocks = a.appendOpenAIContentArray(contentBlocks, c, report)
	default:
		// Non-string/non-array content has no representation; leave it
		// unrepresented rather than guessing.
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

// appendOpenAIContentArray flattens an array-shaped OpenAI response content
// into Anthropic blocks (multi-part / OpenRouter-style). Text parts are
// extracted; unsendable non-text parts carry the visible
// "[unsendable content: <type>]" marker instead of vanishing (NENYA-176).
func (a *AnthropicAdapter) appendOpenAIContentArray(contentBlocks []interface{}, parts []interface{}, report *DropReport) []interface{} {
	for _, partRaw := range parts {
		part, ok := partRaw.(map[string]interface{})
		if !ok {
			contentBlocks = append(contentBlocks, map[string]interface{}{
				"type": "text",
				"text": "[unsendable content: unknown]",
			})
			report.Add("unknown") // AUD-33 / NENYA-252
			continue
		}
		partType, _ := part["type"].(string)
		if text, ok := part["text"].(string); ok && partType == "text" {
			contentBlocks = append(contentBlocks, map[string]interface{}{
				"type": "text",
				"text": text,
			})
			continue
		}
		if partType == "" {
			partType = "unknown"
		}
		contentBlocks = append(contentBlocks, map[string]interface{}{
			"type": "text",
			"text": "[unsendable content: " + partType + "]",
		})
		// The marker alone is not enough: the kind must reach the DropReport
		// surfaces (header + metric) or the fidelity loss stays silent
		// (AUD-33 / NENYA-252). Nil-safe.
		report.Add(partType)
	}
	return contentBlocks
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
	case "tool_calls", "function_call":
		// function_call is OpenAI's legacy pre-tool_calls spelling.
		return "tool_use"
	case "length":
		return "max_tokens"
	case "content_filter":
		return "refusal"
	default:
		// Anthropic's stop_reason is a CLOSED enum. A provider's custom or
		// case-drifted finish reason must never ride onto it verbatim;
		// end_turn is the neutral natural-stop (AUD-32 / NENYA-251).
		return "end_turn"
	}
}
