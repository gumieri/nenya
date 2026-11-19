package adapter

// Buffered Responses-API → OpenAI chat-completions response conversion
// (NENYA-337). Its caller is the proxy's buffered-response path
// (convertBufferedResponseForClient); the MCP buffered loop's replay instead
// drives the SSE transformer over the buffered frames (ConvertBufferedForReplay).

import (
	"strings"
	"time"
)

// ConvertResponsesToOpenAIBodyReported converts a Responses-API response
// object (as a parsed map) to the chat-completions response format, with a
// DropReport: content the chat shape cannot represent (unknown output item
// types, reasoning signatures) is recorded by kind so the gateway can surface
// it on the X-Nenya-Dropped-Content header and the unsendable-content metric
// (NENYA-176). Nil report behaves like an unreported conversion.
func (a *ResponsesAdapter) ConvertResponsesToOpenAIBodyReported(response map[string]interface{}, report *DropReport) map[string]interface{} {
	return a.convertResponsesToOpenAI(response, report)
}

// convertResponsesToOpenAI folds the output items into one chat choice:
// output_text parts join into message.content, function_call items become
// tool_calls, reasoning summaries surface as reasoning_content (fidelity
// annotation, never a silent drop).
func (a *ResponsesAdapter) convertResponsesToOpenAI(response map[string]interface{}, report *DropReport) map[string]interface{} {
	out := map[string]interface{}{
		"id":      response["id"],
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   response["model"],
	}

	var content, refusal, reasoning strings.Builder
	var toolCalls []interface{}
	if outputs, ok := response["output"].([]interface{}); ok {
		for _, itemRaw := range outputs {
			item, ok := itemRaw.(map[string]interface{})
			if !ok {
				report.Add("output_item")
				continue
			}
			convertResponsesOutputItem(item, &content, &refusal, &reasoning, &toolCalls, report)
		}
	}

	msg := map[string]interface{}{"role": "assistant"}
	if content.Len() > 0 {
		msg["content"] = content.String()
	} else {
		// Chat clients expect a string content (possibly empty) when there
		// are no tool calls.
		msg["content"] = ""
	}
	if refusal.Len() > 0 {
		msg["refusal"] = refusal.String()
	}
	if reasoning.Len() > 0 {
		msg["reasoning_content"] = reasoning.String()
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}

	out["choices"] = []interface{}{map[string]interface{}{
		"index":         0,
		"message":       msg,
		"finish_reason": responsesFinishReason(response, len(toolCalls)),
	}}

	if u, ok := response["usage"].(map[string]interface{}); ok {
		out["usage"] = responsesUsageToChat(u)
	}
	return out
}

// convertResponsesOutputItem folds one output item into the choice's
// accumulators. Unknown item kinds are reported by their native type.
func convertResponsesOutputItem(item map[string]interface{}, content, refusal, reasoning *strings.Builder, toolCalls *[]interface{}, report *DropReport) {
	switch item["type"] {
	case "message":
		convertResponsesMessageParts(item, content, refusal, report)
	case "function_call":
		args, ok := item["arguments"].(string)
		if !ok {
			// A non-string arguments blob has no chat home; the chat shape
			// requires the raw JSON string.
			report.Add("function_arguments")
			args = ""
		}
		fn := map[string]interface{}{
			"name":      item["name"],
			"arguments": args,
		}
		call := map[string]interface{}{
			"id":       responsesCallID(item),
			"type":     "function",
			"function": fn,
		}
		*toolCalls = append(*toolCalls, call)
	case "reasoning":
		convertResponsesReasoningItem(item, reasoning, report)
	default:
		// Server-side tool items (web_search_call, file_search_call,
		// image_generation_call, mcp_call, ...) have no chat-completions
		// home: record the kind, keep converting the rest.
		report.Add(partType(item))
	}
}

// convertResponsesMessageParts folds a message item's content parts:
// output_text joins into content, refusal parts into the refusal field.
func convertResponsesMessageParts(item map[string]interface{}, content, refusal *strings.Builder, report *DropReport) {
	parts, ok := item["content"].([]interface{})
	if !ok {
		return
	}
	for _, pRaw := range parts {
		p, ok := pRaw.(map[string]interface{})
		if !ok {
			report.Add("content_part")
			continue
		}
		switch p["type"] {
		case "output_text":
			if text, ok := p["text"].(string); ok {
				content.WriteString(text)
			} else {
				// Upstream shape drift: an output_text part with no text —
				// silently losing the answer text is invisible to the client
				// (NENYA-176, mirrors the request-direction report).
				report.Add("content_part_text")
			}
		case "refusal":
			if text, ok := p["refusal"].(string); ok {
				refusal.WriteString(text)
			} else {
				report.Add("content_part_refusal")
			}
		default:
			report.Add("content_part_" + partType(p))
		}
	}
}

// convertResponsesReasoningItem folds a reasoning output item: the summary is
// the human-readable part, surfaced as the reasoning_content annotation
// (NENYA-176 fidelity precedent); the encrypted signature blob has no
// chat-shape home and is recorded.
func convertResponsesReasoningItem(item map[string]interface{}, reasoning *strings.Builder, report *DropReport) {
	if sum, ok := item["summary"].([]interface{}); ok {
		convertResponsesSummaryParts(sum, reasoning, report)
	}
	if _, enc := item["encrypted_content"]; enc {
		report.Add("reasoning_signature")
	}
}

// convertResponsesSummaryParts folds reasoning summary parts into the
// reasoning annotation: summary_text joins, shape drift reports (genuinely
// empty text stays silent — no loss).
func convertResponsesSummaryParts(sum []interface{}, reasoning *strings.Builder, report *DropReport) {
	for _, sRaw := range sum {
		s, ok := sRaw.(map[string]interface{})
		if !ok {
			report.Add("reasoning_summary")
			continue
		}
		text, isText := s["text"].(string)
		if !isText {
			report.Add("reasoning_summary")
			continue
		}
		if text != "" {
			reasoning.WriteString(text)
		}
	}
}

// responsesCallID prefers call_id (the loop-join id) and falls back to the
// item id.
func responsesCallID(item map[string]interface{}) string {
	if id, ok := item["call_id"].(string); ok && id != "" {
		return id
	}
	id, _ := item["id"].(string)
	return id
}

// responsesFinishReason maps the response status and output composition to
// the chat finish_reason: the upstream's explicit truncation/filter statement
// wins (status incomplete — same precedence as the streaming terminal and
// the MCP loop's fold), function_call items next (tool_calls), completed to
// stop.
func responsesFinishReason(response map[string]interface{}, toolCallCount int) string {
	if status, _ := response["status"].(string); status == "incomplete" {
		if d, ok := response["incomplete_details"].(map[string]interface{}); ok {
			switch r, _ := d["reason"].(string); r {
			case "content_filter":
				return "content_filter"
			}
		}
		return "length"
	}
	if toolCallCount > 0 {
		return "tool_calls"
	}
	return "stop"
}
