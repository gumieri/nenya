package stream

// Responses-API SSE → OpenAI chat-completions delta transformer (NENYA-337):
// when the gateway dispatches a chat client's request to an openai-responses
// target, the upstream's Responses event stream is converted back into the
// chunk shape the client — and the reader's tool-call/usage machinery —
// understands. Stateful: output items carry their own ids while chat deltas
// carry positional tool-call indexes, so the mapping is tracked here.

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
)

// ResponsesTransformer converts Responses-API SSE events into OpenAI
// chat.completion.chunk frames. It implements ResponseTransformer and (via a
// no-op FinishTail) satisfies TailFinisher — the canonical [DONE] frame is
// composed by the reader at terminal.
type ResponsesTransformer struct {
	mu sync.Mutex

	messageID string
	model     string
	// emittedRole tracks the chat convention of a leading delta.role chunk:
	// the first emitted delta merges the role in.
	emittedRole bool
	// itemIndex maps Responses item ids to their positional chat tool_calls
	// index (assigned on output_item.added in arrival order).
	itemIndex map[string]int
	// nextToolIndex is the next positional index to hand out.
	nextToolIndex int
	// sawToolCall drives finish_reason on the terminal event.
	sawToolCall bool
	// usage caches the terminal event's usage object (already in the chat
	// spelling) so it rides the finish chunk.
	usage map[string]interface{}
	// streamDone latches the terminal event; later frames are dropped.
	streamDone bool
}

// Compile-time interface checks: the reader composes the terminal [DONE]
// from the transformer's tail.
var (
	_ ResponseTransformer = (*ResponsesTransformer)(nil)
	_ TailFinisher        = (*ResponsesTransformer)(nil)
)

// NewResponsesTransformer creates a fresh transformer for one stream.
func NewResponsesTransformer() *ResponsesTransformer {
	return &ResponsesTransformer{itemIndex: make(map[string]int)}
}

// FinishTail implements TailFinisher; the reader composes [DONE] itself, so
// the transformer holds nothing back.
func (t *ResponsesTransformer) FinishTail() []byte { return nil }

// responsesConsumedEvents are pure-state events: they carry information the
// transformer already consumed from sibling events (or needs not at all) and
// produce no client-facing frame.
var responsesConsumedEvents = map[string]bool{
	"response.output_text.done":             true,
	"response.function_call_arguments.done": true,
	"response.reasoning_summary_text.done":  true,
	"response.reasoning_text.done":          true,
	"response.refusal.done":                 true,
	"response.output_item.done":             true,
	"response.content_part.added":           true,
	"response.content_part.done":            true,
	"response.output_text.annotation.added": true,
	"response.output_text.annotation.done":  true,
	"response.reasoning_summary_part.added": true,
	"response.reasoning_summary_part.done":  true,
	"response.queued":                       true,
}

// TransformSSEChunk converts one Responses SSE data line into zero or more
// chat chunk frames. Consumed state events return ErrEventConsumed; a
// terminal event returns the finish chunk with the usage remapped into the
// chat spelling (the reader's usage callback keys on exactly that spelling).
func (t *ResponsesTransformer) TransformSSEChunk(ctx context.Context, data []byte) ([]byte, error) {
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
		slog.Debug("responses: non-JSON SSE line, passthrough", "error", err, "data", string(data))
		return data, nil
	}

	eventType, _ := event["type"].(string)

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.streamDone {
		slog.Debug("responses: dropping frame after terminal event", "event", eventType)
		return nil, ErrEventConsumed
	}

	response, _ := event["response"].(map[string]interface{})

	switch eventType {
	case "response.created", "response.in_progress":
		return t.handleEnvelope(response)
	case "response.completed", "response.incomplete":
		return t.handleTerminal(eventType, response)
	case "response.failed", "error":
		return t.handleFailure(event, response)
	case "response.output_item.added":
		return t.handleOutputItemAdded(event)
	case "response.output_text.delta", "response.refusal.delta",
		"response.reasoning_summary_text.delta", "response.reasoning_text.delta",
		"response.function_call_arguments.delta":
		return t.handleResponsesDelta(eventType, event)
	default:
		// Known state events are consumed; any other "response.*" event is
		// consumed too — a raw Responses event is garbage to a chat client
		// (and to the Anthropic composite's second stage), so unknown
		// response.* shapes never forward. Non-response prefixes pass
		// through (schema-drift resilience, same contract as the Anthropic
		// transformer). A future delta-carrying response.* event would be
		// swallowed here — the Debug line is its only trace.
		if responsesConsumedEvents[eventType] || strings.HasPrefix(eventType, "response.") {
			if !responsesConsumedEvents[eventType] {
				slog.Debug("responses: unknown event consumed (not a chat-frame shape)", "event", eventType)
			}
			return nil, ErrEventConsumed
		}
		return data, nil
	}
}

// handleEnvelope captures the response id/model from the envelope events.
func (t *ResponsesTransformer) handleEnvelope(response map[string]interface{}) ([]byte, error) {
	if response != nil {
		if id, ok := response["id"].(string); ok && id != "" {
			t.messageID = id
		}
		if m, ok := response["model"].(string); ok && m != "" {
			t.model = m
		}
	}
	return nil, ErrEventConsumed
}

// handleOutputItemAdded emits the leading chat tool_calls delta (id + name)
// when a function_call item opens; message items are state-only.
func (t *ResponsesTransformer) handleOutputItemAdded(event map[string]interface{}) ([]byte, error) {
	item, _ := event["item"].(map[string]interface{})
	if item == nil || item["type"] != "function_call" {
		return nil, ErrEventConsumed
	}
	t.sawToolCall = true
	idx := t.nextToolIndex
	t.nextToolIndex++
	if id, ok := item["id"].(string); ok && id != "" {
		t.itemIndex[id] = idx
	}
	// The first chat delta for a call carries id and name; arguments
	// stream as increments afterwards. The chat id prefers call_id (the
	// loop-join id) with the item-id fallback — mirroring the buffered
	// converter's responsesCallID, so streamed and replayed calls join the
	// conversation under the same identity.
	name, _ := item["name"].(string)
	callID, _ := item["call_id"].(string)
	if callID == "" {
		callID, _ = item["id"].(string)
	}
	delta := map[string]interface{}{
		"tool_calls": []interface{}{map[string]interface{}{
			"index": idx,
			"id":    callID,
			"type":  "function",
			"function": map[string]interface{}{
				"name":      name,
				"arguments": "",
			},
		}},
	}
	return t.marshalChunk(t.makeChunk(delta, nil))
}

// handleResponsesDelta maps the delta event family onto the chat delta
// fields: text/refusal → content, reasoning summaries → reasoning_content,
// function-call arguments → positional tool_calls increments.
func (t *ResponsesTransformer) handleResponsesDelta(eventType string, event map[string]interface{}) ([]byte, error) {
	deltaText, _ := event["delta"].(string)
	if eventType == "response.function_call_arguments.delta" {
		idx, known := t.toolIndexFor(event)
		if !known {
			// Mid-stream attach (the added event was never seen): allocate a
			// fallback positional index so the arguments still stream — chat
			// clients tolerate a call that starts with arguments only.
			// Silently dropping them would execute the call with {}.
			itemID, _ := event["item_id"].(string)
			if itemID == "" {
				slog.Debug("responses: arguments delta with no item id, dropped", "event", eventType)
				return nil, ErrEventConsumed
			}
			idx = t.nextToolIndex
			t.nextToolIndex++
			t.itemIndex[itemID] = idx
			// The fallback allocation IS a tool call: the stream must close
			// with finish_reason tool_calls, not stop.
			t.sawToolCall = true
			slog.Debug("responses: arguments delta for an unseen item, allocating fallback index", "item_id", itemID)
		}
		delta := map[string]interface{}{
			"tool_calls": []interface{}{map[string]interface{}{
				"index":    idx,
				"function": map[string]interface{}{"arguments": deltaText},
			}},
		}
		return t.marshalChunk(t.makeChunk(delta, nil))
	}
	field := "content"
	if eventType == "response.reasoning_summary_text.delta" || eventType == "response.reasoning_text.delta" {
		// Reasoning deltas surface as the reasoning_content annotation
		// (NENYA-176 reasoning-fidelity precedent; the buffered path uses
		// the same field).
		field = "reasoning_content"
	}
	// Documented streaming limitation (NENYA-176 class): custom-tool
	// call_input deltas have no case here — custom tools are not supported
	// for this format at all (flattenResponsesTools drops them with a
	// report), so the events cannot legitimately occur; the default branch
	// consumes them as chat-garbage.
	return t.marshalChunk(t.makeChunk(map[string]interface{}{field: deltaText}, nil))
}

// handleTerminal emits the finish chunk: function_call items seen →
// tool_calls, truncation → length, otherwise stop. The terminal response's
// usage rides the chunk in the chat spelling.
func (t *ResponsesTransformer) handleTerminal(eventType string, response map[string]interface{}) ([]byte, error) {
	reason := t.terminalFinishReason(eventType, response)
	t.cacheUsage(response)
	t.streamDone = true
	return t.marshalChunk(t.makeChunk(map[string]interface{}{}, &reason))
}

// terminalFinishReason maps the terminal event to the chat finish_reason.
func (t *ResponsesTransformer) terminalFinishReason(eventType string, response map[string]interface{}) string {
	switch eventType {
	case "response.completed":
		if t.sawToolCall {
			return "tool_calls"
		}
		return "stop"
	default: // response.incomplete
		if response != nil {
			if d, ok := response["incomplete_details"].(map[string]interface{}); ok {
				if r, _ := d["reason"].(string); r == "content_filter" {
					return "content_filter"
				}
			}
		}
		return "length"
	}
}

// handleFailure surfaces the upstream failure as a chat error frame — the
// reader's error taxonomy detects a top-level "error" key (stream/errors.go)
// and the client must not see a clean empty stream.
func (t *ResponsesTransformer) handleFailure(event, response map[string]interface{}) ([]byte, error) {
	t.streamDone = true
	errFrame := map[string]interface{}{"error": responsesErrorPayload(event, response)}
	b, err := json.Marshal(errFrame)
	if err != nil {
		return nil, ErrEventConsumed
	}
	return b, nil
}

// toolIndexFor resolves the positional chat index of the item a delta
// belongs to.
func (t *ResponsesTransformer) toolIndexFor(event map[string]interface{}) (int, bool) {
	itemID, _ := event["item_id"].(string)
	idx, ok := t.itemIndex[itemID]
	return idx, ok
}

// cacheUsage snapshots the terminal response's usage in the chat spelling.
func (t *ResponsesTransformer) cacheUsage(response map[string]interface{}) {
	if response == nil {
		return
	}
	u, ok := response["usage"].(map[string]interface{})
	if !ok {
		return
	}
	in := safeFloatToInt(toFloat64(u["input_tokens"]))
	out := safeFloatToInt(toFloat64(u["output_tokens"]))
	total := safeFloatToInt(toFloat64(u["total_tokens"]))
	if total == 0 {
		total = in + out
	}
	usage := map[string]interface{}{
		"prompt_tokens":     in,
		"completion_tokens": out,
		"total_tokens":      total,
	}
	if d, ok := u["input_tokens_details"].(map[string]interface{}); ok {
		if c := safeFloatToInt(toFloat64(d["cached_tokens"])); c > 0 {
			usage["prompt_tokens_details"] = map[string]interface{}{"cached_tokens": c}
		}
	}
	if d, ok := u["output_tokens_details"].(map[string]interface{}); ok {
		if r := safeFloatToInt(toFloat64(d["reasoning_tokens"])); r > 0 {
			usage["completion_tokens_details"] = map[string]interface{}{"reasoning_tokens": r}
		}
	}
	t.usage = usage
}

// responsesErrorPayload extracts the error object from a response.failed
// event (response.error) or a bare error event. The Responses `error` event
// carries its diagnostics at the TOP level (code/message/param, not nested
// under an "error" key) — honor them before falling back to a fabricated
// envelope, so the client sees the real upstream diagnostic.
func responsesErrorPayload(event, response map[string]interface{}) map[string]interface{} {
	if e, ok := event["error"].(map[string]interface{}); ok && e != nil {
		return e
	}
	if response != nil {
		if e, ok := response["error"].(map[string]interface{}); ok && e != nil {
			return e
		}
	}
	// Bare error event: top-level code/message/param (the documented wire
	// shape for the Responses `error` SSE event), gated on string-asserted
	// non-empty values so shape drift yields the fabricated fallback rather
	// than an empty frame.
	msg, _ := event["message"].(string)
	code, _ := event["code"].(string)
	param, _ := event["param"].(string)
	if msg != "" || code != "" || param != "" {
		payload := map[string]interface{}{}
		if msg != "" {
			payload["message"] = msg
		}
		if code != "" {
			payload["code"] = code
		}
		if param != "" {
			payload["param"] = param
		}
		return payload
	}
	return map[string]interface{}{"message": "responses: upstream reported a failed response", "type": "upstream_error"}
}

// makeChunk builds one chat.completion.chunk frame. created is 0 (the
// Anthropic transformer does the same — clients do not rely on it).
func (t *ResponsesTransformer) makeChunk(delta map[string]interface{}, finishReason *string) map[string]interface{} {
	if !t.emittedRole {
		merged := map[string]interface{}{"role": "assistant"}
		for k, v := range delta {
			merged[k] = v
		}
		delta = merged
		t.emittedRole = true
	}
	chunk := map[string]interface{}{
		"id":      t.messageID,
		"object":  "chat.completion.chunk",
		"created": 0,
		"model":   t.model,
	}
	choice := map[string]interface{}{"index": 0, "delta": delta}
	if finishReason != nil {
		choice["finish_reason"] = *finishReason
	} else {
		choice["finish_reason"] = nil
	}
	chunk["choices"] = []interface{}{choice}
	if t.usage != nil {
		chunk["usage"] = t.usage
	}
	return chunk
}

// marshalChunk serializes one frame.
func (t *ResponsesTransformer) marshalChunk(chunk map[string]interface{}) ([]byte, error) {
	b, err := json.Marshal(chunk)
	if err != nil {
		return nil, ErrEventConsumed
	}
	return b, nil
}

// toFloat64 coerces a JSON-decoded number to float64.
func toFloat64(v interface{}) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	default:
		return 0
	}
}
