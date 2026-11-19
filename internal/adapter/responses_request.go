package adapter

import (
	"math"
	"strings"
	"sync"
)

// ResponsesAdapter converts between the OpenAI chat-completions wire shape
// and the OpenAI Responses API (/v1/responses) wire shape (the format string
// lives in routing.FormatOpenAIResponses; the adapter package sits left of
// routing in the dependency DAG and cannot import it). It exists for models
// that reject function tools + reasoning_effort on
// /v1/chat/completions (gpt-6-luna, gpt-6.2-sol; NENYA-337): a chat client's
// request is converted to a Responses create request, dispatched to the
// responses endpoint (see the provider's FormatURLs entry), and the
// Responses reply is converted back to the chat shape the client speaks.
type ResponsesAdapter struct{}

var (
	responsesAdapterOnce     sync.Once
	responsesAdapterInstance *ResponsesAdapter
)

// GetResponsesAdapter returns the shared ResponsesAdapter instance (the
// adapter is stateless; conversions never mutate their inputs).
func GetResponsesAdapter() *ResponsesAdapter {
	responsesAdapterOnce.Do(func() {
		responsesAdapterInstance = &ResponsesAdapter{}
	})
	return responsesAdapterInstance
}

// responsesChatOnlyParams lists top-level chat-completions request
// parameters with no Responses-API equivalent (NENYA-337). They are removed
// from the converted body and recorded on the DropReport — surfaced, never
// silently dropped (NENYA-176 fidelity semantics).
var responsesChatOnlyParams = []string{
	"stop", "logprobs", "top_logprobs", "logit_bias",
	"presence_penalty", "frequency_penalty", "n", "stream_options", "seed",
}

// ConvertOpenAIToResponsesBodyReported converts a chat-completions request
// payload into a Responses-API create request, recording request content the
// Responses shape cannot represent on the DropReport (NENYA-176). A nil
// report behaves like an unreported conversion (the caller decides the
// surfacing channel — the forward transform path logs and counts, the
// buffered path can also set the header).
func (a *ResponsesAdapter) ConvertOpenAIToResponsesBodyReported(openai map[string]interface{}, report *DropReport) map[string]interface{} {
	return a.convertOpenAIToResponses(openai, report)
}

// convertOpenAIToResponses builds the Responses create request. The body
// starts as a shallow copy of the chat payload so parameters neither
// converter owns (future or provider-extension fields) survive untouched and
// are validated upstream, then the owned keys are replaced/removed.
func (a *ResponsesAdapter) convertOpenAIToResponses(openai map[string]interface{}, report *DropReport) map[string]interface{} {
	out := make(map[string]interface{}, len(openai))
	for k, v := range openai {
		out[k] = v
	}

	if messages, ok := openai["messages"].([]interface{}); ok {
		out["input"] = convertResponsesInput(messages, report)
		delete(out, "messages")
	} else { // A chat request without a messages array is malformed; without
		// this report it would dispatch verbatim to /v1/responses with
		// nothing surfaced.
		delete(out, "messages")
		report.Add("messages")
	}
	if tools, ok := openai["tools"].([]interface{}); ok {
		if flat := flattenResponsesTools(tools, report); len(flat) > 0 {
			out["tools"] = flat
		} else {
			delete(out, "tools")
		}
	}
	convertResponsesToolChoice(out, openai, report)
	convertResponsesReasoning(out, openai)
	convertResponsesTokenCap(out, openai)
	convertResponsesResponseFormat(out, openai, report)
	stripResponsesChatOnlyParams(out, report)
	return out
}

// convertResponsesToolChoice maps the chat tool_choice spelling to the
// Responses one: string forms pass through; the forced-function form renests
// without the function envelope; anything else (foreign shapes, a function
// selection missing its name) is removed and reported — dispatching it
// verbatim is a guaranteed upstream 400 with nothing surfaced.
func convertResponsesToolChoice(out, openai map[string]interface{}, report *DropReport) {
	tc, ok := openai["tool_choice"].(map[string]interface{})
	if !ok {
		return // string forms (auto/none/required) pass through unchanged
	}
	if fn, ok := tc["function"].(map[string]interface{}); ok {
		named := map[string]interface{}{"type": "function"}
		if name, ok := fn["name"].(string); ok && name != "" {
			named["name"] = name
			out["tool_choice"] = named
			return
		}
	}
	delete(out, "tool_choice")
	report.Add("tool_choice")
}

// convertResponsesReasoning moves the chat reasoning_effort spelling into the
// Responses reasoning object — the chat spelling is what the upstream rejects
// for tools-bearing requests on these models (the NENYA-337 incident).
// "none"/empty mean no reasoning, so no object.
func convertResponsesReasoning(out, openai map[string]interface{}) {
	effort, ok := openai["reasoning_effort"].(string)
	if !ok {
		return
	}
	delete(out, "reasoning_effort")
	if effort != "" && effort != "none" {
		out["reasoning"] = map[string]interface{}{"effort": effort}
	}
}

// convertResponsesTokenCap renames the token cap: both chat spellings
// collapse to max_output_tokens (NENYA-179: never lose the cap across a
// rename).
func convertResponsesTokenCap(out, openai map[string]interface{}) {
	if capv := firstTokenCap(openai); capv != nil {
		out["max_output_tokens"] = capv
	}
	delete(out, "max_tokens")
	delete(out, "max_completion_tokens")
}

// convertResponsesResponseFormat maps chat response_format to text.format
// (json_schema unwraps its envelope; the text default is omitted).
func convertResponsesResponseFormat(out, openai map[string]interface{}, report *DropReport) {
	rf, ok := openai["response_format"].(map[string]interface{})
	if !ok {
		return
	}
	delete(out, "response_format")
	switch t, _ := rf["type"].(string); t {
	case "json_object":
		out["text"] = map[string]interface{}{"format": map[string]interface{}{"type": "json_object"}}
	case "json_schema":
		if js, ok := rf["json_schema"].(map[string]interface{}); ok {
			// The chat envelope is {type, json_schema:{name, schema, strict}};
			// Responses' text.format IS the inner envelope but still requires
			// its own type field — without it every structured-output request
			// 400s upstream.
			// No size hint: the envelope is client-shaped (CWE-190 §7 — the
			// len+1 arithmetic is the allocation-overflow pattern CodeQL
			// flags), and the map is tiny in practice.
			typed := make(map[string]interface{})
			typed["type"] = "json_schema"
			for k, v := range js {
				typed[k] = v
			}
			out["text"] = map[string]interface{}{"format": typed}
		} else {
			report.Add("response_format")
		}
	case "text", "":
		// The Responses default already returns text; omit.
	default:
		report.Add("response_format_" + t)
	}
}

// stripResponsesChatOnlyParams removes chat parameters with no Responses
// equivalent. Presence alone is not a loss: default-valued parameters
// (n=1, false flags, zero penalties, empty collections) are dropped silently
// — reporting them would pollute the DropReport/header with non-losses
// (values the Responses request semantics are identical without).
func stripResponsesChatOnlyParams(out map[string]interface{}, report *DropReport) {
	for _, k := range responsesChatOnlyParams {
		v, present := out[k]
		if !present {
			continue
		}
		delete(out, k)
		if !responsesChatOnlyDefault(k, v) {
			report.Add(k)
		}
	}
	// The Responses create request has no documented user field: any
	// NON-EMPTY value is a real loss (per-caller attribution); an empty
	// string is semantically default — dropped silently like the other
	// defaults above.
	if v, present := out["user"]; present {
		delete(out, "user")
		if s, _ := v.(string); s != "" {
			report.Add("user")
		}
	}
}

// responsesChatOnlyDefault reports whether the chat-only parameter's value is
// semantically its default (dropping it loses nothing).
func responsesChatOnlyDefault(k string, v interface{}) bool {
	switch k {
	case "n":
		return responsesToInt(v) <= 1
	case "logprobs":
		b, ok := v.(bool)
		return !ok || !b
	case "top_logprobs", "presence_penalty", "frequency_penalty":
		return responsesToInt(v) == 0
	case "seed":
		// OpenAI's seed default is null (random): an explicit 0 is a real
		// determinism request, not a default — only absence is silent (the
		// loop above only sees present keys, so every seed here reports).
		return false
	case "logit_bias", "stop":
		switch c := v.(type) {
		case string:
			return c == ""
		case []interface{}:
			return len(c) == 0
		case map[string]interface{}:
			return len(c) == 0
		}
		return v == nil
	}
	return false
}

// convertResponsesInput maps chat messages to Responses input items:
// system/developer/user → role message items with input_* content parts,
// assistant content → an assistant message item with output_text parts,
// assistant tool_calls → function_call items, tool results →
// function_call_output items (the MCP loop's appended shapes).
func convertResponsesInput(messages []interface{}, report *DropReport) []interface{} {
	// The capacity mirrors the client-supplied message count (bounded by the
	// request body limit) — clamped explicitly so the allocation cannot
	// overflow (CWE-190 §7, AGENTS.md §7 guard pattern; CodeQL
	// go/allocation-size-overflow).
	const maxInputItemsCap = 1 << 16
	input := make([]interface{}, 0, min(len(messages), maxInputItemsCap))
	for _, mRaw := range messages {
		m, ok := mRaw.(map[string]interface{})
		if !ok {
			report.Add("message")
			continue
		}
		role, _ := m["role"].(string)
		switch role {
		case "system", "developer", "user":
			item := map[string]interface{}{
				"type": "message",
				"role": role,
			}
			// Responses message items accept plain-string content; passing it
			// through avoids wrapping empty strings into parts (a null-content
			// item is a guaranteed upstream 400).
			if s, ok := m["content"].(string); ok {
				item["content"] = s
			} else if parts := responsesInputContent(m["content"], report); parts != nil {
				item["content"] = parts
			} else if m["content"] == nil {
				// No content at all: nothing to lose, but the item would
				// carry null — skip it.
				report.Add("message_content")
				continue
			} else {
				item["content"] = ""
			}
			input = append(input, item)
		case "assistant":
			input = append(input, convertResponsesAssistantMessage(m, report)...)
		case "tool":
			item := map[string]interface{}{
				"type": "function_call_output",
			}
			if id, ok := m["tool_call_id"].(string); ok && id != "" {
				item["call_id"] = id
			} else {
				// The output cannot be joined to its call without the id —
				// surface it (NENYA-176).
				report.Add("tool_call_id")
			}
			item["output"] = responsesToolOutputText(m["content"], report)
			input = append(input, item)
		default:
			report.Add("message_role_" + role)
		}
	}
	return input
}

// convertResponsesAssistantMessage flattens one assistant chat message into
// Responses input items: content first (mirrors the chat field order), then
// one function_call item per tool call. Assistant reasoning_content in
// history is dropped BY DESIGN (the Responses API does not accept reasoning
// echoes — it reconstructs its own reasoning server-side) and reported per
// message (kind reasoning_history, NENYA-176 never-silent). A fully-empty
// assistant message (no content, no tool calls) vanishes from the input and
// is reported (kind message_content) — the same symmetry as the user/system
// nil-content branch.
func convertResponsesAssistantMessage(m map[string]interface{}, report *DropReport) []interface{} {
	var items []interface{}
	if rc, ok := m["reasoning_content"].(string); ok && rc != "" {
		report.Add("reasoning_history")
	}
	if content := responsesOutputContent(m["content"], report); content != nil {
		items = append(items, map[string]interface{}{
			"type":    "message",
			"role":    "assistant",
			"content": content,
		})
	}
	calls, _ := m["tool_calls"].([]interface{})
	for _, cRaw := range calls {
		c, ok := cRaw.(map[string]interface{})
		if !ok {
			report.Add("tool_call")
			continue
		}
		fn, ok := c["function"].(map[string]interface{})
		if !ok {
			// A tool call without its function envelope degrades to a
			// call_id-only item — the upstream cannot execute it. Surface it.
			report.Add("tool_call")
			continue
		}
		item := map[string]interface{}{
			"type": "function_call",
		}
		if id, ok := c["id"].(string); ok && id != "" {
			item["call_id"] = id
		} else {
			// Without call_id the upstream cannot join the following
			// function_call_output to this call — a guaranteed 400 that must
			// be visible, not silent.
			report.Add("tool_call_id")
		}
		if name, ok := fn["name"].(string); ok && name != "" {
			item["name"] = name
		} else {
			// A nameless function call is a guaranteed upstream 400 (same
			// shape the tool list reports as tool_name) — surface it.
			report.Add("tool_name")
		}
		if args, ok := fn["arguments"].(string); ok {
			item["arguments"] = args
		} else if fn["arguments"] != nil {
			report.Add("tool_call_arguments")
		}
		items = append(items, item)
	}
	if len(items) == 0 {
		// Nothing survived (an assistant message with no content and an
		// empty/absent tool_calls array vanishes from the input): the same
		// silent-loss shape the user branch reports.
		report.Add("message_content")
	}
	return items
}

// responsesInputContent converts a chat message content (string or parts
// array) into Responses input parts: text → input_text, image_url →
// input_image (remote URLs carried — the vision gate lets non-anthropic
// formats through).
func responsesInputContent(content interface{}, report *DropReport) []interface{} {
	if s, ok := content.(string); ok {
		if s == "" {
			return nil
		}
		return []interface{}{map[string]interface{}{"type": "input_text", "text": s}}
	}
	parts, ok := content.([]interface{})
	if !ok {
		if content == nil {
			return nil
		}
		report.Add("content")
		return nil
	}
	// Same capacity clamp as convertResponsesInput (CWE-190 §7): the parts
	// slice is client-shaped.
	const maxInputPartsCap = 1 << 16
	out := make([]interface{}, 0, min(len(parts), maxInputPartsCap))
	for _, pRaw := range parts {
		p, ok := pRaw.(map[string]interface{})
		if !ok {
			report.Add("content_part")
			continue
		}
		switch p["type"] {
		case "text":
			if text, ok := p["text"].(string); ok {
				out = append(out, map[string]interface{}{"type": "input_text", "text": text})
			} else {
				// A text part without text carries nothing the Responses
				// shape can represent — surface the drop (NENYA-176).
				report.Add("content_part_text")
			}
		case "image_url":
			if img := responsesImagePart(p, report); img != nil {
				out = append(out, img)
			}
		default:
			report.Add("content_part_" + partType(p))
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// responsesImagePart maps one chat image_url content part to a Responses
// input_image part; a malformed URL shape (non-string, missing url) reports
// and yields nil — an input_image without a URL is a guaranteed upstream 400.
func responsesImagePart(p map[string]interface{}, report *DropReport) map[string]interface{} {
	img := map[string]interface{}{"type": "input_image"}
	switch iu := p["image_url"].(type) {
	case string:
		if iu == "" {
			report.Add("image_url")
			return nil
		}
		img["image_url"] = iu
	case map[string]interface{}:
		url, _ := iu["url"].(string)
		if url == "" {
			report.Add("image_url")
			return nil
		}
		img["image_url"] = url
		if detail, ok := iu["detail"].(string); ok && detail != "" {
			img["detail"] = detail
		}
	default:
		report.Add("image_url")
		return nil
	}
	return img
}

// responsesOutputContent converts assistant text content into output_text
// input parts (the Responses history spelling for assistant messages). Parts
// that cannot convert are reported — assistant history loss is invisible to
// the client otherwise (NENYA-176).
func responsesOutputContent(content interface{}, report *DropReport) []interface{} {
	if s, ok := content.(string); ok {
		if s == "" {
			return nil
		}
		return []interface{}{map[string]interface{}{"type": "output_text", "text": s}}
	}
	parts, ok := content.([]interface{})
	if !ok || len(parts) == 0 {
		if content != nil && !ok {
			report.Add("assistant_content")
		}
		return nil
	}
	var out []interface{}
	for _, pRaw := range parts {
		p, ok := pRaw.(map[string]interface{})
		if !ok {
			report.Add("content_part")
			continue
		}
		switch pt := partType(p); pt {
		case "text", "output_text":
			if text, ok := p["text"].(string); ok {
				if text != "" {
					out = append(out, map[string]interface{}{"type": "output_text", "text": text})
				}
				// Genuinely empty strings stay silent (no loss — same
				// convention as responsesToolOutputText); only shape drift
				// (missing/non-string) reports.
			} else {
				report.Add("content_part_text")
			}
		default:
			report.Add("content_part_" + pt)
		}
	}
	return out
}

// responsesToolOutputText flattens a tool result to the string the
// function_call_output item requires (strings pass through, content arrays
// join their text parts, nil becomes empty — a missing output string is an
// upstream 400). Non-text parts (image_url parts the vision gate now
// legitimately produces for openai-responses targets) cannot ride the string
// output and are reported — never silent (NENYA-176).
func responsesToolOutputText(content interface{}, report *DropReport) string {
	if s, ok := content.(string); ok {
		return s
	}
	parts, ok := content.([]interface{})
	if !ok {
		if content != nil {
			report.Add("tool_result")
		}
		return ""
	}
	var b strings.Builder
	for _, pRaw := range parts {
		p, ok := pRaw.(map[string]interface{})
		if !ok {
			report.Add("content_part")
			continue
		}
		switch pt := partType(p); pt {
		case "text":
			if text, ok := p["text"].(string); ok && text != "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(text)
			} else if !ok {
				// Non-string text drift — same report as the content-part
				// siblings; genuinely empty strings stay silent (no loss).
				report.Add("content_part_text")
			}
		default:
			// image_url and other non-text result parts flatten away here.
			report.Add(pt)
		}
	}
	return b.String()
}

// flattenResponsesTools converts chat-completions function tools to the
// flat Responses tool shape. Non-function tool entries are reported.
func flattenResponsesTools(tools []interface{}, report *DropReport) []interface{} {
	// Same capacity clamp as convertResponsesInput (CWE-190 §7): the tools
	// slice is client-shaped.
	const maxToolsCap = 1 << 12
	flat := make([]interface{}, 0, min(len(tools), maxToolsCap))
	for _, tRaw := range tools {
		t, ok := tRaw.(map[string]interface{})
		if !ok {
			report.Add("tool")
			continue
		}
		if t["type"] != "function" {
			report.Add("tool_" + partType(t))
			continue
		}
		fn, ok := t["function"].(map[string]interface{})
		if !ok {
			report.Add("tool")
			continue
		}
		item := map[string]interface{}{"type": "function"}
		if name, ok := fn["name"].(string); ok && name != "" {
			item["name"] = name
		} else {
			// A function tool without a name is a guaranteed upstream 400 —
			// surface it.
			report.Add("tool_name")
		}
		if desc, ok := fn["description"].(string); ok {
			item["description"] = desc
		}
		if params, ok := fn["parameters"]; ok {
			item["parameters"] = params
		}
		if strict, ok := fn["strict"]; ok {
			item["strict"] = strict
		}
		flat = append(flat, item)
	}
	return flat
}

// partType extracts a map's "type" string for drop-kind labeling (empty when
// absent or non-string; Add sanitizes).
func partType(m map[string]interface{}) string {
	t, _ := m["type"].(string)
	return t
}

// responsesUsageToChat remaps the Responses usage object into the chat
// spelling (the usage tracker, calibration loop, and cache-state tracker all
// key on prompt_tokens/completion_tokens plus the *_details decompositions).
func responsesUsageToChat(u map[string]interface{}) map[string]interface{} {
	in := responsesToInt(u["input_tokens"])
	on := responsesToInt(u["output_tokens"])
	total := responsesToInt(u["total_tokens"])
	if total == 0 {
		total = in + on
	}
	out := map[string]interface{}{
		"prompt_tokens":     in,
		"completion_tokens": on,
		"total_tokens":      total,
	}
	if d, ok := u["input_tokens_details"].(map[string]interface{}); ok {
		if c := responsesToInt(d["cached_tokens"]); c > 0 {
			out["prompt_tokens_details"] = map[string]interface{}{"cached_tokens": c}
		}
	}
	if d, ok := u["output_tokens_details"].(map[string]interface{}); ok {
		if r := responsesToInt(d["reasoning_tokens"]); r > 0 {
			out["completion_tokens_details"] = map[string]interface{}{"reasoning_tokens": r}
		}
	}
	return out
}

// responsesToInt coerces a JSON-decoded number (float64) or int to int,
// returning 0 when the value overflows ±MaxInt (CWE-190 §7: int(huge float)
// is implementation-dependent — the exact 2⁶³ float boundary is the guard —
// and this runs on upstream usage objects and client-supplied values).
func responsesToInt(v interface{}) int {
	switch n := v.(type) {
	case float64:
		// 1<<63 is exactly representable as float64: every float ≥ it is out
		// of int range on 64-bit (int(2⁶³) itself is implementation-defined).
		if n >= 1<<63 || n < math.MinInt {
			return 0
		}
		return int(n)
	case int:
		return n
	default:
		return 0
	}
}
