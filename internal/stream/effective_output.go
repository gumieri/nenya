package stream

// NENYA-178 — effective-output classification for the time-to-first-token clock.
//
// "Effective output" is the protocol-aware first token a client would render.
// The clock must not stop on a keepalive, an empty delta, a role-only handshake,
// a bare/empty block opening, a block close, a usage/metadata frame, or (by
// default) a reasoning-only frame: a slow-thinking model would otherwise look
// slow-to-first-token, which de-prioritizes it in latency-aware routing and,
// once NENYA-153 lands, trips its slow-call breaker.
//
// The same classification drives the bootstrap head probe (NENYA-45), which must
// not release its buffer on a handshake frame: keep the two in sync by using
// this function rather than a second copy of the rules.

// HasEffectiveOutput reports whether an upstream SSE payload carries output a
// client would render:
//
//   - OpenAI chat: the first choice's delta/message with content text (a string
//     or text parts), tool calls, a legacy function_call, a refusal, audio, or
//     annotations (the first choice is the one every other consumer in this
//     package treats as client-visible);
//   - Anthropic: a content_block_delta carrying text or tool input, or a
//     content_block_start opening a tool block (tool_use, server_tool_use,
//     mcp_tool_use) — a bare or empty text/thinking block opening and every
//     block close are not output (a thinking block opens seconds before the
//     first token);
//   - Gemini: a candidate part bearing text, a function call, inline data,
//     executable code, a code-execution result, or file data (a thought-only or
//     signature-only part is not output).
//
// Reasoning/thinking frames count as effective output only when includeReasoning
// is set (the operator opts in to measuring the first byte of any generation);
// the accepted field names are reasoning_content, reasoning and thinking.
func HasEffectiveOutput(parsed map[string]any, includeReasoning bool) bool {
	if parsed == nil {
		return false
	}
	if anthropicHasOutput(parsed, includeReasoning) {
		return true
	}
	if choicesHaveOutput(parsed, includeReasoning) {
		return true
	}
	return geminiCandidatesHaveOutput(parsed, includeReasoning)
}

// anthropicHasOutput reports whether an Anthropic event carries generation
// output. message_start/ping/message_delta/message_stop are metadata, and a
// content_block_stop is a close.
func anthropicHasOutput(parsed map[string]any, includeReasoning bool) bool {
	switch t, _ := parsed["type"].(string); t {
	case "content_block_delta":
		delta, _ := parsed["delta"].(map[string]any)
		return anthropicDeltaHasOutput(delta, includeReasoning)
	case "content_block_start":
		block, _ := parsed["content_block"].(map[string]any)
		if block == nil {
			return false
		}
		switch bt, _ := block["type"].(string); bt {
		case "tool_use", "server_tool_use", "mcp_tool_use":
			// A tool block opening carries the call identity the client renders.
			return true
		case "thinking", "redacted_thinking":
			return includeReasoning
		default:
			// Result blocks and other shapes are judged by their text.
			return nonEmptyString(block["text"])
		}
	}
	return false
}

// anthropicDeltaHasOutput classifies a content_block_delta payload.
func anthropicDeltaHasOutput(delta map[string]any, includeReasoning bool) bool {
	if delta == nil {
		return false
	}
	switch dt, _ := delta["type"].(string); dt {
	case "text_delta":
		return nonEmptyString(delta["text"])
	case "input_json_delta":
		return nonEmptyString(delta["partial_json"])
	case "thinking_delta":
		return includeReasoning && nonEmptyString(delta["thinking"])
	case "signature_delta":
		// A signature alone is not rendered output.
		return false
	}
	// Unknown delta shape: accept it when it carries text, gated on the flag
	// when it carries reasoning text.
	if nonEmptyString(delta["text"]) {
		return true
	}
	return includeReasoning && nonEmptyString(delta["thinking"])
}

// choicesHaveOutput reports whether the first OpenAI-style choice carries
// output (a request with n>1 choices renders the first one).
func choicesHaveOutput(parsed map[string]any, includeReasoning bool) bool {
	choices, ok := parsed["choices"].([]any)
	if !ok || len(choices) == 0 {
		return false
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		return false
	}
	if delta, ok := choice["delta"].(map[string]any); ok && objectHasOutput(delta, includeReasoning) {
		return true
	}
	if msg, ok := choice["message"].(map[string]any); ok && objectHasOutput(msg, includeReasoning) {
		return true
	}
	return false
}

// geminiCandidatesHaveOutput reports whether a Gemini candidate carries a part
// a client would render.
func geminiCandidatesHaveOutput(parsed map[string]any, includeReasoning bool) bool {
	cands, ok := parsed["candidates"].([]any)
	if !ok || len(cands) == 0 {
		// The first candidate is the one a client renders, mirroring the
		// first-choice rule for OpenAI-shaped payloads.
		return false
	}
	candidate, ok := cands[0].(map[string]any)
	if !ok {
		return false
	}
	content, ok := candidate["content"].(map[string]any)
	if !ok {
		return false
	}
	parts, ok := content["parts"].([]any)
	if !ok {
		return false
	}
	for _, rawPart := range parts {
		part, ok := rawPart.(map[string]any)
		if !ok {
			continue
		}
		if geminiPartHasOutput(part, includeReasoning) {
			return true
		}
	}
	return false
}

// geminiPartHasOutput classifies one Gemini part. A text part is output even
// when it also carries a thoughtSignature (real Gemini 2.5/3 text parts do);
// only a part with no rendered payload at all (signature-only) is not.
func geminiPartHasOutput(part map[string]any, includeReasoning bool) bool {
	for _, key := range []string{"functionCall", "inlineData", "executableCode", "codeExecutionResult", "fileData"} {
		if _, ok := part[key]; ok {
			return true
		}
	}
	text := nonEmptyString(part["text"])
	if thought, _ := part["thought"].(bool); thought {
		return includeReasoning && text
	}
	if !text {
		// A signature-only part is bookkeeping, not rendered output.
		return false
	}
	return true
}

// objectHasOutput reports whether an OpenAI-style delta/message object carries
// generation output: content text (string or array parts), tool calls, a legacy
// function_call, a refusal, audio, or annotations. A role-only chunk is a
// handshake, not output.
func objectHasOutput(m map[string]any, includeReasoning bool) bool {
	if contentHasText(m["content"]) || hasRenderedExtra(m) {
		return true
	}
	return includeReasoning && hasReasoningText(m)
}

// contentHasText reports whether an OpenAI content value carries text: either a
// bare string or an array of text parts.
func contentHasText(content any) bool {
	switch c := content.(type) {
	case string:
		return c != ""
	case []any:
		for _, raw := range c {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if nonEmptyString(part["text"]) {
				return true
			}
		}
	}
	return false
}

// hasRenderedExtra reports whether an OpenAI-style object carries a non-content
// field a client renders: tool calls, a legacy function_call, a refusal (the
// repo treats refusals as first-class — see the captured-refusal handling in the
// proxy), audio, or annotations.
func hasRenderedExtra(m map[string]any) bool {
	if tc, ok := m["tool_calls"].([]any); ok && len(tc) > 0 {
		return true
	}
	if fc, ok := m["function_call"].(map[string]any); ok && fc != nil {
		return true
	}
	if nonEmptyString(m["refusal"]) {
		return true
	}
	if a, ok := m["audio"].(map[string]any); ok && a != nil {
		return true
	}
	if an, ok := m["annotations"].([]any); ok && len(an) > 0 {
		return true
	}
	return false
}

// hasReasoningText reports whether the object carries reasoning text under any
// of the field names providers use.
func hasReasoningText(m map[string]any) bool {
	for _, key := range []string{"reasoning_content", "reasoning", "thinking"} {
		if nonEmptyString(m[key]) {
			return true
		}
	}
	return false
}

// nonEmptyString reports whether v is a non-empty string.
func nonEmptyString(v any) bool {
	s, ok := v.(string)
	return ok && s != ""
}
