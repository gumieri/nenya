package stream

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"

	"github.com/nenya/internal/util"
)

type openaiToolCall struct {
	index int
	id    string
	name  string
	args  strings.Builder
}

// OpenAIToAnthropicTransformer converts OpenAI-format SSE chunks into Anthropic
// Messages API SSE events. It implements the stream.ResponseTransformer interface
// and produces a valid Anthropic SSE event sequence: message_start →
// content_block_start → content_block_delta → content_block_stop → message_delta →
// message_stop.
type OpenAIToAnthropicTransformer struct {
	mu           sync.Mutex
	messageID    string
	model        string
	promptTokens int
	streamDone   bool
	textBuffer   strings.Builder
	toolCalls    []*openaiToolCall
	textEmitted  bool
	hasEmitted   bool
	// pendingFinish holds the mapped finish reason between the finish chunk
	// and the stream tail. The message_delta/message_stop tail is held so
	// the standard `stream_options.include_usage` ordering — the trailing
	// usage-only chunk AFTER the finish chunk — lands in the published
	// usage instead of a 0/0 delta (AUD-29 / NENYA-241). Flushed by the
	// first post-finish event (the usage chunk) or by [DONE].
	pendingFinish string

	// Upstream usage captured from the stream (NENYA-167). OpenAI-format
	// upstreams report usage in a final chunk that often carries no choices,
	// so it must be captured before the no-choices early return. When the
	// upstream reported nothing, the output count falls back to an estimate.
	outputTokens        int
	cacheReadTokens     int
	cacheCreationTokens int
	usageSeen           bool
}

// NewOpenAIToAnthropicTransformer creates a transformer that converts OpenAI SSE
// chunks into Anthropic SSE events for streaming responses to Anthropic-native clients.
func NewOpenAIToAnthropicTransformer() *OpenAIToAnthropicTransformer {
	return &OpenAIToAnthropicTransformer{}
}

// TransformSSEChunk converts a single OpenAI SSE data payload into one or more
// Anthropic SSE event payloads. It implements stream.ResponseTransformer.
func (t *OpenAIToAnthropicTransformer) TransformSSEChunk(ctx context.Context, data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, nil
	}

	if IsDonePayload(data) {
		t.mu.Lock()
		defer t.mu.Unlock()
		t.streamDone = true
		// In the reader pipeline this branch is unreachable — the reader
		// intercepts [DONE] and pulls the tail via TailFinisher — but direct
		// callers (tests, embedding users) flush here: a still-held tail
		// (finish chunk carried no trailing usage chunk) emits
		// message_delta with the captured usage + message_stop
		// (AUD-29 / NENYA-241). The OpenAI [DONE] sentinel is consumed in
		// that case — the converted Anthropic stream terminates at
		// message_stop.
		if t.pendingFinish != "" {
			tail := t.buildStreamTail(t.pendingFinish)
			t.pendingFinish = ""
			return tail, nil
		}
		return []byte(DonePayload), nil
	}

	var chunk map[string]interface{}
	if err := json.Unmarshal(data, &chunk); err != nil {
		slog.Debug("Failed to unmarshal OpenAI SSE chunk, passthrough", "error", err)
		return data, nil
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	// Usage is captured before the streamDone guard: with the standard
	// `stream_options.include_usage` ordering the final usage-only chunk
	// follows the finish_reason chunk, which sets streamDone (NENYA-167).
	t.captureUsage(chunk)

	if t.streamDone {
		// The stream is closed: a trailing usage-only chunk (the standard
		// stream_options.include_usage ordering) is OpenAI-shaped and must
		// not be forwarded to an Anthropic client after message_stop. It was
		// captured above; flush the tail now so the held message_delta
		// carries THIS usage instead of the 0/0 published pre-fix
		// (AUD-29 / NENYA-241).
		if t.pendingFinish != "" {
			tail := t.buildStreamTail(t.pendingFinish)
			t.pendingFinish = ""
			return tail, nil
		}
		return nil, ErrEventConsumed
	}

	var events []byte

	if !t.hasEmitted {
		msgStart := t.buildMessageStart(chunk)
		events = append(events, msgStart...)
		t.hasEmitted = true
	}

	choices, ok := chunk["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return events, nil
	}

	choice, ok := choices[0].(map[string]interface{})
	if !ok {
		return events, nil
	}

	choiceEvents := t.processChoice(choice)
	events = append(events, choiceEvents...)

	return events, nil
}

// captureUsage records the upstream usage totals reported by any chunk, in any
// placement (the standard final usage chunk, a usage-only chunk with no
// choices, or a provider-specific placement). The values are normalized by
// NormalizeUsageTotals, so the synthesized Anthropic usage reports the real
// counts instead of a character-count estimate (NENYA-167).
func (t *OpenAIToAnthropicTransformer) captureUsage(chunk map[string]interface{}) {
	usage, ok := UsageMapFromChunk(chunk)
	if !ok {
		return
	}
	totals := NormalizeUsageTotals(usage)
	if totals.Prompt == 0 && totals.Completion == 0 {
		return
	}
	// Upstream usage is cumulative for the request, and a provider may report
	// the input side in one chunk and the output side in another (or a smaller
	// value after a partial one): keep the high-water mark per dimension so no
	// count is lost or doubled.
	t.promptTokens = max(t.promptTokens, totals.Prompt)
	t.outputTokens = max(t.outputTokens, totals.Completion)
	t.cacheReadTokens = max(t.cacheReadTokens, totals.CacheHit)
	t.cacheCreationTokens = max(t.cacheCreationTokens, totals.CacheCreation)
	t.usageSeen = true
}

func (t *OpenAIToAnthropicTransformer) buildMessageStart(chunk map[string]interface{}) []byte {
	t.messageID = "msg_" + util.GenerateID()
	if id, ok := chunk["id"].(string); ok && id != "" {
		t.messageID = id
	}

	t.model = "unknown-model"
	if model, ok := chunk["model"].(string); ok {
		t.model = model
	}

	msgStart := map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            t.messageID,
			"type":          "message",
			"role":          "assistant",
			"model":         t.model,
			"content":       []interface{}{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			// message_start is emitted before the upstream reports usage (an
			// OpenAI-format stream carries it in a final chunk), so
			// input_tokens is 0 here and the real counts are published on the
			// final message_delta (NENYA-167; see docs/COST_MODEL.md).
			"usage": map[string]interface{}{
				"input_tokens":  t.promptTokens,
				"output_tokens": 0,
			},
		},
	}

	b, err := json.Marshal(msgStart)
	if err != nil {
		slog.Warn("failed to marshal message_start", "error", err)
		return nil
	}
	return b
}

func (t *OpenAIToAnthropicTransformer) processChoice(choice map[string]interface{}) []byte {
	delta, ok := choice["delta"].(map[string]interface{})
	if !ok {
		return nil
	}

	var events []byte

	if content, ok := delta["content"].(string); ok && content != "" {
		if !t.textEmitted {
			startEvent := t.makeContentBlockStart(0, "text", nil)
			events = append(events, startEvent...)
			t.textEmitted = true
		}
		t.textBuffer.WriteString(content)
		deltaEvent := t.makeContentBlockDelta(0, map[string]interface{}{
			"type": "text_delta",
			"text": content,
		})
		events = append(events, deltaEvent...)
	}

	if toolCalls, ok := delta["tool_calls"].([]interface{}); ok {
		for _, tcRaw := range toolCalls {
			tc, ok := tcRaw.(map[string]interface{})
			if !ok {
				continue
			}
			tcEvents := t.processToolCall(tc)
			events = append(events, tcEvents...)
		}
	}

	if finishReason, ok := choice["finish_reason"].(string); ok && finishReason != "" {
		closeEvents := t.buildStreamStops()
		events = append(events, closeEvents...)
		// The tail (message_delta + message_stop) is HELD: the standard
		// include_usage ordering delivers usage in a trailing chunk after
		// this one, and the delta must carry it (AUD-29 / NENYA-241).
		t.pendingFinish = finishReason
		t.streamDone = true
	}

	return events
}

func (t *OpenAIToAnthropicTransformer) processToolCall(tc map[string]interface{}) []byte {
	// An absent or non-numeric index is treated as 0: some providers omit
	// `index` when the stream carries a single tool call, and dropping the
	// delta there lost the ENTIRE call (AUD-31 / NENYA-250). Multi-call
	// streams that omit indices are ambiguous anyway; 0 is the conventional
	// interpretation.
	index := 0
	if indexRaw, ok := tc["index"].(float64); ok {
		index = int(indexRaw)
	}

	for len(t.toolCalls) <= index {
		t.toolCalls = append(t.toolCalls, &openaiToolCall{index: len(t.toolCalls)})
	}

	otc := t.toolCalls[index]
	blockIdx := index + 1
	var events []byte

	if id, ok := tc["id"].(string); ok && id != "" && otc.id == "" {
		otc.id = id

		if fn, ok := tc["function"].(map[string]interface{}); ok {
			if fnName, _ := fn["name"].(string); fnName != "" {
				otc.name = fnName
			}
		}

		startEvent := t.makeContentBlockStart(blockIdx, "tool_use", map[string]interface{}{
			"id":   otc.id,
			"name": otc.name,
		})
		events = append(events, startEvent...)
	}

	if fn, ok := tc["function"].(map[string]interface{}); ok {
		if args, ok := fn["arguments"].(string); ok {
			otc.args.WriteString(args)

			deltaEvent := t.makeContentBlockDelta(blockIdx, map[string]interface{}{
				"type":         "input_json_delta",
				"partial_json": args,
			})
			events = append(events, deltaEvent...)
		}
	}

	return events
}

// FinishTail flushes the held message_delta/message_stop tail, or nil when
// nothing is held. Reader-side hook (stream.TailFinisher, AUD-29 /
// NENYA-241): called at the stream terminal so the standard include_usage
// usage chunk lands in the published usage.
func (t *OpenAIToAnthropicTransformer) FinishTail() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.pendingFinish == "" {
		return nil
	}
	tail := t.buildStreamTail(t.pendingFinish)
	t.pendingFinish = ""
	return tail
}

// buildStreamStops emits the content_block_stop events for every open
// content block. The message_delta/message_stop tail is held separately
// (pendingFinish) so the standard include_usage usage chunk lands in the
// published usage (AUD-29 / NENYA-241).
func (t *OpenAIToAnthropicTransformer) buildStreamStops() []byte {
	var events []byte

	if t.textEmitted {
		stopEvent := t.makeContentBlockStop(0)
		events = append(events, stopEvent...)
	}

	for i := range t.toolCalls {
		stopEvent := t.makeContentBlockStop(i + 1)
		events = append(events, stopEvent...)
	}

	return events
}

// buildStreamTail emits the closing message_delta (stop reason plus the
// captured usage) and message_stop. Called once, after the last upstream
// usage has been captured.
func (t *OpenAIToAnthropicTransformer) buildStreamTail(finishReason string) []byte {
	var events []byte

	stopReason := mapFinishReasonReverse(finishReason)

	// Prefer the upstream-reported output total; fall back to a character
	// estimate only when the stream carried no usage at all (NENYA-167).
	outputTokens := t.outputTokens
	if !t.usageSeen {
		outputTokens = 0
		if t.textBuffer.Len() > 0 {
			outputTokens = t.textBuffer.Len() / 4
		}
	}
	// Anthropic's input_tokens excludes the cache dimensions, which are
	// reported as their own fields (NENYA-167).
	usage := map[string]interface{}{
		"input_tokens":  max(0, t.promptTokens-t.cacheReadTokens-t.cacheCreationTokens),
		"output_tokens": outputTokens,
	}
	if t.cacheReadTokens > 0 {
		usage["cache_read_input_tokens"] = t.cacheReadTokens
	}
	if t.cacheCreationTokens > 0 {
		usage["cache_creation_input_tokens"] = t.cacheCreationTokens
	}

	msgDelta := map[string]interface{}{
		"type": "message_delta",
		"delta": map[string]interface{}{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": usage,
	}

	b, err := json.Marshal(msgDelta)
	if err != nil {
		slog.Warn("failed to marshal message_delta", "error", err)
	} else {
		events = append(events, b...)
	}

	msgStop := []byte(`{"type":"message_stop"}`)
	events = append(events, msgStop...)

	return events
}

func (t *OpenAIToAnthropicTransformer) makeContentBlockStart(index int, blockType string, content map[string]interface{}) []byte {
	block := map[string]interface{}{
		"type":  "content_block_start",
		"index": index,
		"content_block": map[string]interface{}{
			"type": blockType,
		},
	}
	if content != nil {
		cb := block["content_block"].(map[string]interface{})
		for k, v := range content {
			cb[k] = v
		}
	}

	b, err := json.Marshal(block)
	if err != nil {
		slog.Warn("failed to marshal content_block_start", "error", err)
		return nil
	}
	return b
}

func (t *OpenAIToAnthropicTransformer) makeContentBlockDelta(index int, delta map[string]interface{}) []byte {
	event := map[string]interface{}{
		"type":  "content_block_delta",
		"index": index,
		"delta": delta,
	}

	b, err := json.Marshal(event)
	if err != nil {
		slog.Warn("failed to marshal content_block_delta", "error", err)
		return nil
	}
	return b
}

func (t *OpenAIToAnthropicTransformer) makeContentBlockStop(index int) []byte {
	event := map[string]interface{}{
		"type":  "content_block_stop",
		"index": index,
	}

	b, err := json.Marshal(event)
	if err != nil {
		slog.Warn("failed to marshal content_block_stop", "error", err)
		return nil
	}
	return b
}

func mapFinishReasonReverse(reason string) string {
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
		// Anthropic's stop_reason is a CLOSED enum (end_turn, max_tokens,
		// stop_sequence, tool_use, refusal, pause_turn). A provider's
		// custom or case-drifted finish reason must never ride onto it
		// verbatim — strict clients reject foreign values; end_turn is
		// the neutral natural-stop (AUD-32 / NENYA-251).
		return "end_turn"
	}
}

// Reset clears all transformer state for reuse with a new stream.
func (t *OpenAIToAnthropicTransformer) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.messageID = ""
	t.model = ""
	t.promptTokens = 0
	t.streamDone = false
	t.textBuffer.Reset()
	t.toolCalls = nil
	t.textEmitted = false
	t.hasEmitted = false
	t.outputTokens = 0
	t.cacheReadTokens = 0
	t.cacheCreationTokens = 0
	t.usageSeen = false
}
